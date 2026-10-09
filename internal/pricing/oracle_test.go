package pricing

import (
	"math"
	"math/big"
	"testing"

	"github.com/holiman/uint256"

	"github.com/huynhnhatkhanh/mevlens/internal/dex"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
)

var (
	weth = eth.MustAddress("0x82af49447d8a07e3bd95bd0d56f35241523fbab1")
	usdc = eth.MustAddress("0xaf88d065e77c8cc2239327c5edb3a432268e5831")
	low  = eth.MustAddress("0x0000000000000000000000000000000000000001") // sorts before WETH
	ref  = eth.MustAddress("0x00000000000000000000000000000000000000ff")
)

// sqrtPriceFor returns sqrtPriceX96 for a raw token1/token0 price.
func sqrtPriceFor(raw float64) *uint256.Int {
	f := new(big.Float).SetFloat64(math.Sqrt(raw))
	f.Mul(f, new(big.Float).SetInt(new(big.Int).Lsh(big.NewInt(1), 96)))
	i, _ := f.Int(nil)
	z, _ := uint256.FromBig(i)
	return z
}

func near(a, b float64) bool { return math.Abs(a-b)/b < 1e-9 }

func TestWETHAsToken0(t *testing.T) {
	// On Arbitrum WETH (0x82af…) < USDC (0xaf88…): WETH is token0, USDC (6 dp) token1.
	o, err := New(Config{WETH: weth, Stables: []Stable{{usdc, 6}}, RefPool: ref, RefStable: usdc})
	if err != nil {
		t.Fatal(err)
	}
	// $3000/ETH => raw = 3000e6 / 1e18 USDC base units per WETH base unit.
	s := dex.Swap{Pool: dex.PoolIDFromAddress(ref), Contract: ref, Kind: dex.KindV3, SqrtPriceX96: *sqrtPriceFor(3000e6 / 1e18)}
	o.ObserveSwap(&s, 42)
	p, block, ok := o.ETHUSD()
	if !ok || block != 42 || !near(p, 3000) {
		t.Fatalf("ETHUSD = %v @%d ok=%v", p, block, ok)
	}
	v, ok := o.ValueETH(usdc, uint256.NewInt(1500e6), 42)
	if !ok || !near(v, 0.5) {
		t.Fatalf("1500 USDC = %v ETH", v)
	}
}

func TestWETHAsToken1(t *testing.T) {
	o, err := New(Config{WETH: weth, Stables: []Stable{{low, 18}}, RefPool: ref, RefStable: low})
	if err != nil {
		t.Fatal(err)
	}
	// token0 = stable (18 dp), token1 = WETH: raw = ETH per USD = 1/2500.
	if !o.SetSqrtPrice(sqrtPriceFor(1.0/2500), 7) {
		t.Fatal("price rejected")
	}
	if p, _, _ := o.ETHUSD(); !near(p, 2500) {
		t.Fatalf("ETHUSD = %v", p)
	}
}

func TestValuationCoverage(t *testing.T) {
	o, _ := New(Config{WETH: weth, Stables: []Stable{{usdc, 6}}, RefPool: ref, RefStable: usdc})
	if v, ok := o.ValueETH(weth, uint256.NewInt(2e18), 1); !ok || v != 2 {
		t.Fatalf("WETH = %v %v", v, ok)
	}
	if _, ok := o.ValueETH(usdc, uint256.NewInt(1), 1); ok {
		t.Fatal("stable must be unvalued before any ETH/USD observation")
	}
	if _, ok := o.ValueETH(ref, uint256.NewInt(1), 1); ok {
		t.Fatal("unknown token must be unvalued")
	}
}

func TestPriceExpiresAfterMaxAge(t *testing.T) {
	o, _ := New(Config{WETH: weth, Stables: []Stable{{usdc, 6}}, RefPool: ref, RefStable: usdc, MaxAgeBlocks: 100})
	o.SetSqrtPrice(sqrtPriceFor(3000e6/1e18), 1000)
	for _, tc := range []struct {
		block uint64
		ok    bool
	}{
		{999, false}, // before the observation: look-ahead after a reorg rewind
		{1000, true},
		{1100, true}, // exactly MaxAgeBlocks old
		{1101, false},
	} {
		if _, ok := o.ValueETH(usdc, uint256.NewInt(3000e6), tc.block); ok != tc.ok {
			t.Errorf("price @1000 valuing block %d: ok=%v, want %v", tc.block, ok, tc.ok)
		}
	}
	if v, ok := o.ValueETH(weth, uint256.NewInt(1e18), 1_000_000); !ok || v != 1 {
		t.Fatalf("WETH needs no price, yet stale price gave %v %v", v, ok)
	}
	// A fresh observation revives valuation.
	o.SetSqrtPrice(sqrtPriceFor(3000e6/1e18), 1101)
	if _, ok := o.ValueETH(usdc, uint256.NewInt(3000e6), 1101); !ok {
		t.Fatal("fresh price rejected")
	}
}

func TestDefaultMaxAge(t *testing.T) {
	o, _ := New(Config{WETH: weth, Stables: []Stable{{usdc, 6}}, RefPool: ref, RefStable: usdc})
	o.SetSqrtPrice(sqrtPriceFor(3000e6/1e18), 0)
	if _, ok := o.ValueETH(usdc, uint256.NewInt(1), DefaultMaxAgeBlocks); !ok {
		t.Fatal("zero MaxAgeBlocks must mean DefaultMaxAgeBlocks")
	}
	if _, ok := o.ValueETH(usdc, uint256.NewInt(1), DefaultMaxAgeBlocks+1); ok {
		t.Fatal("price older than DefaultMaxAgeBlocks accepted")
	}
}

func TestSeedIsRecordedAtItsBlock(t *testing.T) {
	// A startup seed read from the state of block 511 (the parent of the first
	// processed block) must be dated 511, not 0, or a replay of old blocks would
	// treat it as ancient (or, dated "latest", as current).
	o, _ := New(Config{WETH: weth, Stables: []Stable{{usdc, 6}}, RefPool: ref, RefStable: usdc, MaxAgeBlocks: 10})
	if !o.SetSqrtPrice(sqrtPriceFor(3000e6/1e18), 511) {
		t.Fatal("seed rejected")
	}
	if _, block, ok := o.ETHUSD(); !ok || block != 511 {
		t.Fatalf("seed recorded at %d (ok=%v), want 511", block, ok)
	}
	if v, ok := o.ValueETH(usdc, uint256.NewInt(1500e6), 512); !ok || !near(v, 0.5) {
		t.Fatalf("first processed block: %v %v", v, ok)
	}
}

func TestRejectsImplausiblePrices(t *testing.T) {
	o, _ := New(Config{WETH: weth, Stables: []Stable{{usdc, 6}}, RefPool: ref, RefStable: usdc})
	if o.SetSqrtPrice(sqrtPriceFor(1e-9/1e18), 1) { // $0.000000001/ETH
		t.Fatal("accepted implausible price")
	}
	if o.SetSqrtPrice(new(uint256.Int), 1) {
		t.Fatal("accepted zero price")
	}
	other := dex.Swap{Pool: dex.PoolIDFromAddress(usdc), Contract: usdc, Kind: dex.KindV3, SqrtPriceX96: *sqrtPriceFor(3000e6 / 1e18)}
	o.ObserveSwap(&other, 1)
	if _, _, ok := o.ETHUSD(); ok {
		t.Fatal("swaps on non-reference pools must be ignored")
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("missing WETH accepted")
	}
	if _, err := New(Config{WETH: weth, RefPool: ref, RefStable: usdc}); err == nil {
		t.Error("reference stable outside Stables accepted")
	}
}
