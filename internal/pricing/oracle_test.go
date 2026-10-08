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
	s := dex.Swap{Pool: ref, Kind: dex.KindV3, SqrtPriceX96: *sqrtPriceFor(3000e6 / 1e18)}
	o.ObserveSwap(&s, 42)
	p, block, ok := o.ETHUSD()
	if !ok || block != 42 || !near(p, 3000) {
		t.Fatalf("ETHUSD = %v @%d ok=%v", p, block, ok)
	}
	v, ok := o.ValueETH(usdc, uint256.NewInt(1500e6))
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
	if v, ok := o.ValueETH(weth, uint256.NewInt(2e18)); !ok || v != 2 {
		t.Fatalf("WETH = %v %v", v, ok)
	}
	if _, ok := o.ValueETH(usdc, uint256.NewInt(1)); ok {
		t.Fatal("stable must be unvalued before any ETH/USD observation")
	}
	if _, ok := o.ValueETH(ref, uint256.NewInt(1)); ok {
		t.Fatal("unknown token must be unvalued")
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
	other := dex.Swap{Pool: usdc, Kind: dex.KindV3, SqrtPriceX96: *sqrtPriceFor(3000e6 / 1e18)}
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
