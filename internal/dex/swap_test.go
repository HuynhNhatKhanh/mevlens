package dex

import (
	"testing"

	"github.com/holiman/uint256"

	"github.com/huynhnhatkhanh/mevlens/internal/eth"
)

func TestTopicsMatchPublishedValues(t *testing.T) {
	// Values as indexed by Etherscan for UniswapV2Pair / UniswapV3Pool.
	tests := map[string]struct {
		got  eth.Hash
		want string
	}{
		"v2 Swap": {TopicV2Swap, "0xd78ad95fa46c994b6551d0da85fc275fe613ce37657fb8d5e3d130840159d822"},
		"v2 Sync": {TopicV2Sync, "0x1c411e9a96e071241c2f21f7726b17ae89e3cab4c78be50e062b03a9fffbbad1"},
		"v3 Swap": {TopicV3Swap, "0xc42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67"},
	}
	for name, tt := range tests {
		if tt.got.Hex() != tt.want {
			t.Errorf("%s topic = %s, want %s", name, tt.got, tt.want)
		}
	}
}

func word(v int64) []byte {
	var z uint256.Int
	if v < 0 {
		z.SetUint64(uint64(-v))
		z.Neg(&z)
	} else {
		z.SetUint64(uint64(v))
	}
	b := z.Bytes32()
	return b[:]
}

func concat(words ...[]byte) []byte {
	var out []byte
	for _, w := range words {
		out = append(out, w...)
	}
	return out
}

var pool = eth.MustAddress("0x0000000000000000000000000000000000000abc")

func TestDecodeV2Swap(t *testing.T) {
	l := &eth.Log{
		Address:  pool,
		Topics:   []eth.Hash{TopicV2Swap, {}, {}},
		Data:     concat(word(1000), word(0), word(0), word(2990)),
		LogIndex: 7,
	}
	s, ok := DecodeSwap(l)
	if !ok || s.Kind != KindV2 || s.LogIndex != 7 || s.Pool != pool {
		t.Fatalf("decode failed: %+v", s)
	}
	zeroForOne, in, out, ok := s.Direction()
	if !ok || !zeroForOne || in.Uint64() != 1000 || out.Uint64() != 2990 {
		t.Fatalf("direction = %v %d %d %v", zeroForOne, in.Uint64(), out.Uint64(), ok)
	}
}

func TestDecodeV3SwapNegativeAmounts(t *testing.T) {
	// Trader sends 5e17 of token1 into the pool and receives 1234 of token0.
	l := &eth.Log{
		Address: pool,
		Topics:  []eth.Hash{TopicV3Swap, {}, {}},
		Data:    concat(word(-1234), word(500_000_000_000_000_000), word(79228162514264337), word(1), word(-5)),
	}
	s, ok := DecodeSwap(l)
	if !ok || s.Kind != KindV3 {
		t.Fatal("decode failed")
	}
	if s.Out0.Uint64() != 1234 || !s.In0.IsZero() || s.In1.Uint64() != 500_000_000_000_000_000 || !s.Out1.IsZero() {
		t.Fatalf("amounts: in0=%s out0=%s in1=%s out1=%s", s.In0.Dec(), s.Out0.Dec(), s.In1.Dec(), s.Out1.Dec())
	}
	zeroForOne, in, out, ok := s.Direction()
	if !ok || zeroForOne || in.Uint64() != 500_000_000_000_000_000 || out.Uint64() != 1234 {
		t.Fatalf("direction = %v %s %s", zeroForOne, in.Dec(), out.Dec())
	}
	if s.SqrtPriceX96.Uint64() != 79228162514264337 {
		t.Fatalf("sqrtPrice = %s", s.SqrtPriceX96.Dec())
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	cases := map[string]*eth.Log{
		"wrong topic":     {Topics: []eth.Hash{TopicV2Sync, {}, {}}, Data: make([]byte, 128)},
		"short v2 data":   {Topics: []eth.Hash{TopicV2Swap, {}, {}}, Data: make([]byte, 96)},
		"v3 topic v2 len": {Topics: []eth.Hash{TopicV3Swap, {}, {}}, Data: make([]byte, 128)},
		"missing topics":  {Topics: []eth.Hash{TopicV2Swap}, Data: make([]byte, 128)},
		"removed":         {Topics: []eth.Hash{TopicV2Swap, {}, {}}, Data: make([]byte, 128), Removed: true},
	}
	for name, l := range cases {
		if _, ok := DecodeSwap(l); ok {
			t.Errorf("%s: decoded a malformed log", name)
		}
	}
}

func TestDirectionDegenerate(t *testing.T) {
	var s Swap
	s.In0.SetUint64(10)
	s.In1.SetUint64(10)
	if _, _, _, ok := s.Direction(); ok {
		t.Fatal("both sides in must be degenerate")
	}
}

func BenchmarkDecodeV3Swap(b *testing.B) {
	l := &eth.Log{
		Address: pool,
		Topics:  []eth.Hash{TopicV3Swap, {}, {}},
		Data:    concat(word(-1234), word(5e17), word(79228162514264337), word(1), word(-5)),
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := DecodeSwap(l); !ok {
			b.Fatal("decode")
		}
	}
}
