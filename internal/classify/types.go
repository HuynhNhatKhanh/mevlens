package classify

import (
	"github.com/holiman/uint256"

	"github.com/huynhnhatkhanh/mevlens/internal/dex"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
)

// Regime is the transaction-ordering policy in force when a block was produced.
type Regime uint8

const (
	RegimeUnknown   Regime = iota
	RegimeFCFS             // first-come-first-served sequencer
	RegimeTimeboost        // express-lane auctions (Arbitrum, Apr 2025 – Sep 2026)
	RegimePGA              // per-transaction priority gas auctions
)

func (r Regime) String() string {
	switch r {
	case RegimeFCFS:
		return "fcfs"
	case RegimeTimeboost:
		return "timeboost"
	case RegimePGA:
		return "pga"
	default:
		return "unknown"
	}
}

// RegimeFunc maps a block number to its ordering regime.
type RegimeFunc func(block uint64) Regime

// RegimeBoundaries builds a RegimeFunc from activation blocks (0 = unknown).
// Blocks before timeboostStart are FCFS only if timeboostStart is known.
func RegimeBoundaries(timeboostStart, pgaStart uint64) RegimeFunc {
	return func(n uint64) Regime {
		switch {
		case pgaStart > 0 && n >= pgaStart:
			return RegimePGA
		case timeboostStart > 0 && n >= timeboostStart:
			return RegimeTimeboost
		case timeboostStart > 0:
			return RegimeFCFS
		default:
			return RegimeUnknown
		}
	}
}

// Status is the execution outcome of an arbitrage transaction.
type Status uint8

const (
	StatusSuccess Status = iota + 1
	StatusReverted
)

func (s Status) String() string {
	if s == StatusReverted {
		return "reverted"
	}
	return "success"
}

// BlockInfo summarises a block.
type BlockInfo struct {
	Number         uint64
	Hash           eth.Hash
	ParentHash     eth.Hash
	Timestamp      uint64
	BaseFee        uint64
	GasUsed        uint64
	L1Block        uint64
	TxCount        uint32
	TimeboostedTxs uint32
	Swaps          uint32
	Arbs           uint32
	RevertedArbs   uint32
	Regime         Regime
}

// SwapRow is one swap on a canonical pool.
type SwapRow struct {
	Block     uint64
	BlockHash eth.Hash
	Timestamp uint64
	TxIndex   uint32
	LogIndex  uint32
	TxHash    eth.Hash
	Pool      dex.PoolID
	Contract  eth.Address // emitting contract (the pool, or the v4 PoolManager)
	Venue     string
	TokenIn   eth.Address
	TokenOut  eth.Address
	AmountIn  uint256.Int
	AmountOut uint256.Int
}

// Arb is one atomic arbitrage transaction (or a reverted attempt by a known bot).
type Arb struct {
	Block     uint64
	BlockHash eth.Hash
	Timestamp uint64
	Regime    Regime
	TxIndex   uint32
	TxHash    eth.Hash
	From      eth.Address // EOA that sent the transaction
	To        eth.Address // contract executing the arbitrage
	Status    Status

	Hops      uint8
	Pools     []dex.PoolID  // pools in log order (empty for reverted attempts)
	Contracts []eth.Address // emitting contract of each hop (the PoolManager for v4)

	ProfitToken  eth.Address // primary profit token (first in order of appearance)
	Profit       uint256.Int // gross profit in ProfitToken base units
	ProfitTokens uint8       // number of tokens with a positive net flow (usually 1)
	ProfitETH    float64     // gross profit across all profit tokens, in ETH, if Valued
	Valued       bool        // every profit token could be valued

	GasUsed           uint64
	GasUsedForL1      uint64
	EffectiveGasPrice uint64 // wei per gas
	BaseFee           uint64 // wei per gas
	PriorityFeePerGas uint64 // wei per gas: the ordering bid under priority auctions
	CostETH           float64
	Timeboosted       bool
}

// NetETH returns profit minus execution cost, if the profit could be valued.
func (a *Arb) NetETH() (float64, bool) {
	if !a.Valued {
		return 0, false
	}
	return a.ProfitETH - a.CostETH, true
}

// Result is the classification of one block.
type Result struct {
	Block BlockInfo
	Swaps []SwapRow
	Arbs  []Arb
}
