// Package pricing values token amounts in ETH for analytics.
//
// Scope is intentionally narrow: WETH is valued 1:1 and USD stablecoins at $1
// through the ETH/USD price of a single deep reference pool. Every other token is
// reported as unvalued instead of being priced off thin, manipulable pools; the
// dashboards show valuation coverage explicitly.
//
// An Oracle is a pure state machine fed by observed swaps, so replaying the same
// blocks yields the same valuations. A price is only used to value blocks at or
// after the one it was observed at, and only for MaxAgeBlocks blocks: valuation
// never looks ahead and never silently carries a dead feed forward. It is not
// safe for concurrent use.
package pricing

import (
	"errors"
	"fmt"
	"math"

	"github.com/holiman/uint256"

	"github.com/huynhnhatkhanh/mevlens/internal/dex"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
)

// Stable is a USD stablecoin valued at exactly $1.
type Stable struct {
	Address  eth.Address
	Decimals uint8
}

// Config configures an Oracle.
type Config struct {
	WETH      eth.Address
	Stables   []Stable
	RefPool   eth.Address // Uniswap-v3-style WETH/RefStable pool used for ETH/USD
	RefStable eth.Address // must be one of Stables
	// MaxAgeBlocks is how many blocks an ETH/USD observation stays usable; older
	// prices leave stablecoin amounts unvalued. Zero means DefaultMaxAgeBlocks.
	MaxAgeBlocks uint64
}

// DefaultMaxAgeBlocks is about one hour of Arbitrum One blocks (~4 blocks/s). The
// reference pool is the deepest WETH/USDC pool and trades many times a minute, so
// an hour without a swap means the feed is broken (pool migrated, swaps no longer
// decoded), not that the market is quiet. ETH/USD rarely moves more than a few
// percent in an hour, which bounds the error of the oldest price still accepted.
const DefaultMaxAgeBlocks = 14_400

// Sanity bounds for ETH/USD; observations outside are ignored as corrupt.
const (
	minETHUSD = 10
	maxETHUSD = 1_000_000
)

// Oracle tracks the ETH/USD price and values amounts in ETH.
type Oracle struct {
	weth       eth.Address
	stables    map[eth.Address]uint8
	ref        eth.Address
	wethIsTok0 bool
	// scale converts (sqrtP/2^96)^2 into USD per ETH (or ETH per USD) given decimals.
	scale  float64
	maxAge uint64

	ethUSD float64
	block  uint64
	known  bool
}

// New validates cfg.
func New(cfg Config) (*Oracle, error) {
	if cfg.WETH.IsZero() {
		return nil, errors.New("pricing: WETH address is required")
	}
	o := &Oracle{weth: cfg.WETH, stables: make(map[eth.Address]uint8, len(cfg.Stables)), ref: cfg.RefPool, maxAge: cfg.MaxAgeBlocks}
	if o.maxAge == 0 {
		o.maxAge = DefaultMaxAgeBlocks
	}
	for _, s := range cfg.Stables {
		if s.Decimals > 36 {
			return nil, fmt.Errorf("pricing: stable %s has %d decimals", s.Address, s.Decimals)
		}
		o.stables[s.Address] = s.Decimals
	}
	if !cfg.RefPool.IsZero() {
		dec, ok := o.stables[cfg.RefStable]
		if !ok {
			return nil, fmt.Errorf("pricing: reference stable %s is not a configured stable", cfg.RefStable)
		}
		// Uniswap orders pool tokens by address.
		o.wethIsTok0 = cfg.WETH.Compare(cfg.RefStable) < 0
		// raw price = token1 per token0 in base units.
		if o.wethIsTok0 {
			o.scale = math.Pow10(18 - int(dec)) // USD per ETH = raw * 10^(18-dec)
		} else {
			o.scale = math.Pow10(int(dec) - 18) // ETH per USD = raw * 10^(dec-18)
		}
	}
	return o, nil
}

// RefPool returns the reference pool address (zero if none).
func (o *Oracle) RefPool() eth.Address { return o.ref }

// ObserveSwap updates the ETH/USD price when s is a swap on the reference pool.
func (o *Oracle) ObserveSwap(s *dex.Swap, block uint64) {
	if s.Kind != dex.KindV3 || o.ref.IsZero() || s.Contract != o.ref {
		return
	}
	o.SetSqrtPrice(&s.SqrtPriceX96, block)
}

// SetSqrtPrice sets the reference price from a Uniswap v3 sqrtPriceX96 observed in
// the state of block (e.g. slot0 read at startup). Implausible values are ignored.
func (o *Oracle) SetSqrtPrice(sqrtPriceX96 *uint256.Int, block uint64) bool {
	if o.ref.IsZero() || sqrtPriceX96.IsZero() {
		return false
	}
	r := sqrtPriceX96.Float64() / (1 << 96)
	p := r * r * o.scale
	if !o.wethIsTok0 && p != 0 {
		p = 1 / p
	}
	if math.IsNaN(p) || p < minETHUSD || p > maxETHUSD {
		return false
	}
	o.ethUSD, o.block, o.known = p, block, true
	return true
}

// ETHUSD returns the last observed ETH/USD price and the block it was observed at.
func (o *Oracle) ETHUSD() (price float64, block uint64, ok bool) { return o.ethUSD, o.block, o.known }

// ValueETH converts amount of token, moved in block, into ETH. ok is false when the
// token is not covered or no usable ETH/USD price exists for block: none observed
// yet, observed more than MaxAgeBlocks before block, or observed after it.
func (o *Oracle) ValueETH(token eth.Address, amount *uint256.Int, block uint64) (float64, bool) {
	if token == o.weth {
		return amount.Float64() / 1e18, true
	}
	dec, ok := o.stables[token]
	if !ok || !o.usableAt(block) {
		return 0, false
	}
	usd := amount.Float64() / math.Pow10(int(dec))
	return usd / o.ethUSD, true
}

// usableAt reports whether the current price may value amounts moved in block. A
// price from a later block is only possible after a reorg rewind replays older
// blocks; it comes from the abandoned fork and would be look-ahead, so it is
// refused until the replay observes a reference swap of its own.
func (o *Oracle) usableAt(block uint64) bool {
	return o.known && o.block <= block && block-o.block <= o.maxAge
}
