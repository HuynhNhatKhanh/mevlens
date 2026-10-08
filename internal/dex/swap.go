// Package dex decodes AMM events into a venue-agnostic, pool-side view of swaps.
//
// Decoding is pure and allocation-free: it never looks up pool metadata. Whether a
// log really comes from a canonical pool is decided by the registry package, because
// any contract can emit a log with a Swap topic.
package dex

import (
	"fmt"

	"github.com/holiman/uint256"

	"github.com/huynhnhatkhanh/mevlens/internal/eth"
)

// Kind is the AMM family of a pool, which determines its event layout.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindV2           // constant product (Uniswap v2 and forks: Sushi, Camelot)
	KindV3           // concentrated liquidity (Uniswap v3 and forks: Sushi v3)
)

func (k Kind) String() string {
	switch k {
	case KindV2:
		return "v2"
	case KindV3:
		return "v3"
	default:
		return "unknown"
	}
}

// MarshalText implements encoding.TextMarshaler ("v2", "v3").
func (k Kind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (k *Kind) UnmarshalText(b []byte) error {
	switch string(b) {
	case "v2":
		*k = KindV2
	case "v3":
		*k = KindV3
	case "unknown":
		*k = KindUnknown
	default:
		return fmt.Errorf("dex: unknown pool kind %q", b)
	}
	return nil
}

// Event topics. They are derived from the signatures at init time instead of being
// pasted as magic constants; tests pin them to their well-known values.
var (
	TopicV2Swap = eth.EventTopic("Swap(address,uint256,uint256,uint256,uint256,address)")
	TopicV2Sync = eth.EventTopic("Sync(uint112,uint112)")
	TopicV3Swap = eth.EventTopic("Swap(address,address,int256,int256,uint160,uint128,int24)")
)

// KindOfTopic returns the pool family that emits a swap with this topic0.
func KindOfTopic(topic0 eth.Hash) Kind {
	switch topic0 {
	case TopicV2Swap:
		return KindV2
	case TopicV3Swap:
		return KindV3
	default:
		return KindUnknown
	}
}

// Swap is the pool-side view of one swap: how much of token0 and token1 entered
// (In) and left (Out) the pool. Both families normalise to this shape.
type Swap struct {
	Pool     eth.Address
	Kind     Kind
	LogIndex uint32
	In0      uint256.Int
	In1      uint256.Int
	Out0     uint256.Int
	Out1     uint256.Int
	// SqrtPriceX96 is the pool price after the swap (v3 only, zero for v2).
	SqrtPriceX96 uint256.Int
}

const (
	v2SwapDataLen = 4 * eth.HashLength // amount0In, amount1In, amount0Out, amount1Out
	v3SwapDataLen = 5 * eth.HashLength // amount0, amount1, sqrtPriceX96, liquidity, tick
)

// DecodeSwap decodes a v2 or v3 Swap log. It returns false for any other log or
// for a log whose shape does not match the event ABI.
func DecodeSwap(l *eth.Log) (Swap, bool) {
	var s Swap
	if l.Removed || len(l.Topics) != 3 {
		return s, false
	}
	s.Pool = l.Address
	s.LogIndex = uint32(l.LogIndex)
	switch l.Topics[0] {
	case TopicV2Swap:
		if len(l.Data) != v2SwapDataLen {
			return s, false
		}
		s.Kind = KindV2
		s.In0.SetBytes32(l.Data[0:32])
		s.In1.SetBytes32(l.Data[32:64])
		s.Out0.SetBytes32(l.Data[64:96])
		s.Out1.SetBytes32(l.Data[96:128])
	case TopicV3Swap:
		if len(l.Data) != v3SwapDataLen {
			return s, false
		}
		s.Kind = KindV3
		// Positive amounts entered the pool, negative amounts left it (two's complement).
		splitSigned(l.Data[0:32], &s.In0, &s.Out0)
		splitSigned(l.Data[32:64], &s.In1, &s.Out1)
		s.SqrtPriceX96.SetBytes32(l.Data[64:96])
	default:
		return s, false
	}
	return s, true
}

// splitSigned decodes an int256 word into its magnitude, stored in pos when the
// value is non-negative and in neg otherwise.
func splitSigned(word []byte, pos, neg *uint256.Int) {
	var v uint256.Int
	v.SetBytes32(word)
	if v.Sign() < 0 { // uint256.Sign interprets the value as two's complement
		neg.Neg(&v)
		return
	}
	pos.Set(&v)
}

// Direction returns which side of the pool the trader paid in (zeroForOne) and the
// net amounts in and out. ok is false for degenerate swaps where the net flow is
// not one-token-in / one-token-out.
func (s *Swap) Direction() (zeroForOne bool, amountIn, amountOut uint256.Int, ok bool) {
	// Net pool-side flow per token; v2 allows both In amounts to be non-zero.
	d0In := s.In0.Cmp(&s.Out0) > 0
	d1In := s.In1.Cmp(&s.Out1) > 0
	switch {
	case d0In && !d1In:
		amountIn.Sub(&s.In0, &s.Out0)
		amountOut.Sub(&s.Out1, &s.In1)
		return true, amountIn, amountOut, !amountOut.IsZero()
	case d1In && !d0In:
		amountIn.Sub(&s.In1, &s.Out1)
		amountOut.Sub(&s.Out0, &s.In0)
		return false, amountIn, amountOut, !amountOut.IsZero()
	default:
		return false, amountIn, amountOut, false
	}
}
