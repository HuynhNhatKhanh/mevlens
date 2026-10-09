package classify

import (
	"reflect"
	"testing"

	"github.com/holiman/uint256"

	"github.com/huynhnhatkhanh/mevlens/internal/dex"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
	"github.com/huynhnhatkhanh/mevlens/internal/pricing"
)

var (
	weth = eth.MustAddress("0x82af49447d8a07e3bd95bd0d56f35241523fbab1")
	usdc = eth.MustAddress("0xaf88d065e77c8cc2239327c5edb3a432268e5831")
	arb  = eth.MustAddress("0x912ce59144191c1204e64559fe8253a0e49e6548")

	poolA    = addr(0xa1) // v2 WETH/USDC
	poolB    = addr(0xa2) // v3 WETH/USDC (reference pool)
	poolC    = addr(0xa3) // v2 ARB/USDC... tokens ordered below
	poolD    = addr(0xa4) // v2 WETH/ARB
	fakePool = addr(0xee)

	bot  = addr(0xb0)
	eoa  = addr(0xe0)
	user = addr(0xe1)
)

func id(a eth.Address) dex.PoolID { return dex.PoolIDFromAddress(a) }

func addr(b byte) eth.Address {
	var a eth.Address
	a[19] = b
	return a
}

type pools map[dex.PoolID]dex.Pool

func (p pools) Lookup(id dex.PoolID) (dex.Pool, bool) { v, ok := p[id]; return v, ok }

func testPools() pools {
	// token0 < token1 by address: WETH(0x82) < ARB(0x91) < USDC(0xaf)
	return pools{
		id(poolA):    {ID: id(poolA), Contract: poolA, Kind: dex.KindV2, Canonical: true, Venue: "sushi-v2", Token0: weth, Token1: usdc},
		id(poolB):    {ID: id(poolB), Contract: poolB, Kind: dex.KindV3, Canonical: true, Venue: "uniswap-v3", Token0: weth, Token1: usdc, FeePips: 500},
		id(poolC):    {ID: id(poolC), Contract: poolC, Kind: dex.KindV2, Canonical: true, Venue: "camelot-v2", Token0: arb, Token1: usdc},
		id(poolD):    {ID: id(poolD), Contract: poolD, Kind: dex.KindV2, Canonical: true, Venue: "uniswap-v2", Token0: weth, Token1: arb},
		id(fakePool): {ID: id(fakePool), Contract: fakePool, Kind: dex.KindV2, Canonical: false},
	}
}

func word(v *uint256.Int) []byte { b := v.Bytes32(); return b[:] }

func u(v uint64) *uint256.Int { return uint256.NewInt(v) }

func neg(v uint64) *uint256.Int { z := uint256.NewInt(v); return z.Neg(z) }

func v2Swap(pool eth.Address, in0, in1, out0, out1 uint64) eth.Log {
	data := make([]byte, 0, 4*eth.HashLength)
	for _, v := range []uint64{in0, in1, out0, out1} {
		data = append(data, word(u(v))...)
	}
	return eth.Log{Address: pool, Topics: []eth.Hash{dex.TopicV2Swap, {}, {}}, Data: data}
}

// v3Swap takes signed pool-side amounts: positive = into the pool.
func v3Swap(pool eth.Address, amount0, amount1 *uint256.Int, sqrtPrice *uint256.Int) eth.Log {
	data := append(append(append([]byte{}, word(amount0)...), word(amount1)...), word(sqrtPrice)...)
	data = append(data, make([]byte, 64)...)
	return eth.Log{Address: pool, Topics: []eth.Hash{dex.TopicV3Swap, {}, {}}, Data: data}
}

func receipt(idx int, from, to eth.Address, status uint64, logs ...eth.Log) eth.Receipt {
	for i := range logs {
		logs[i].LogIndex = eth.Quantity(idx*10 + i)
		logs[i].TxIndex = eth.Quantity(idx)
	}
	return eth.Receipt{
		TxHash: eth.Hash{byte(idx + 1)}, TxIndex: eth.Quantity(idx), From: from, To: to, Status: eth.Quantity(status),
		GasUsed: 300_000, EffectiveGasPrice: 30_000_000, Logs: logs,
	}
}

func block(receipts ...eth.Receipt) *eth.Block {
	b := &eth.Block{Header: eth.Header{Number: 1000, Hash: eth.Hash{0xbb}, Timestamp: 1_790_000_000, BaseFee: 10_000_000}}
	b.Receipts = receipts
	return b
}

func newOracle(t *testing.T) *pricing.Oracle {
	t.Helper()
	o, err := pricing.New(pricing.Config{WETH: weth, Stables: []pricing.Stable{{Address: usdc, Decimals: 6}}, RefPool: poolB, RefStable: usdc})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// sqrtP3000 is sqrtPriceX96 for $3000/ETH with WETH=token0 (18dp), USDC=token1 (6dp).
var sqrtP3000 = func() *uint256.Int {
	z, _ := uint256.FromDecimal("4339505179874779672736325") // sqrt(3000e6/1e18) * 2^96
	return z
}()

func TestTwoPoolArbitrage(t *testing.T) {
	// Buy USDC with 1 WETH on poolA (v2), sell the USDC on poolB (v3) for 1.01 WETH.
	r := receipt(0, eoa, bot, 1,
		v2Swap(poolA, 1e18, 0, 0, 3_030_000_000),
		v3Swap(poolB, neg(1_010_000_000_000_000_000), u(3_030_000_000), sqrtP3000),
	)
	c := New(testPools(), newOracle(t))
	res := c.Classify(block(r))

	if len(res.Arbs) != 1 {
		t.Fatalf("arbs = %d, want 1", len(res.Arbs))
	}
	a := res.Arbs[0]
	if a.Status != StatusSuccess || a.Hops != 2 || a.ProfitToken != weth || a.Profit.Uint64() != 10_000_000_000_000_000 {
		t.Fatalf("arb = %+v (profit %s)", a, a.Profit.Dec())
	}
	if !a.Valued || a.ProfitETH != 0.01 {
		t.Fatalf("profit ETH = %v valued=%v", a.ProfitETH, a.Valued)
	}
	if a.PriorityFeePerGas != 20_000_000 || a.CostETH != 300_000*30_000_000/1e18 {
		t.Fatalf("fees: prio=%d cost=%v", a.PriorityFeePerGas, a.CostETH)
	}
	if !reflect.DeepEqual(a.Pools, []dex.PoolID{id(poolA), id(poolB)}) || !reflect.DeepEqual(a.Contracts, []eth.Address{poolA, poolB}) {
		t.Fatalf("pools = %v", a.Pools)
	}
	if len(res.Swaps) != 2 || res.Swaps[0].TokenIn != weth || res.Swaps[1].TokenOut != weth || res.Swaps[1].Venue != "uniswap-v3" {
		t.Fatalf("swap rows = %+v", res.Swaps)
	}
	if res.Block.Arbs != 1 || res.Block.Swaps != 2 || c.KnownBots() != 1 {
		t.Fatalf("block stats = %+v bots=%d", res.Block, c.KnownBots())
	}
}

func TestTriangularArbitrageInStablecoin(t *testing.T) {
	// USDC -> WETH (poolA) -> ARB (poolD) -> USDC (poolC), ending with +5 USDC.
	r := receipt(0, eoa, bot, 1,
		v2Swap(poolA, 0, 3000e6, 1e18, 0),        // pay USDC (token1), get WETH (token0)
		v2Swap(poolD, 1e18, 0, 0, 4000e18/1e9),   // pay WETH, get ARB (scaled down to fit uint64)
		v2Swap(poolC, 4000e18/1e9, 0, 0, 3005e6), // pay ARB, get USDC
	)
	o := newOracle(t)
	o.SetSqrtPrice(sqrtP3000, 1)
	res := New(testPools(), o).Classify(block(r))
	if len(res.Arbs) != 1 {
		t.Fatalf("arbs = %d", len(res.Arbs))
	}
	a := res.Arbs[0]
	if a.ProfitToken != usdc || a.Profit.Uint64() != 5e6 || a.Hops != 3 {
		t.Fatalf("arb = %+v", a)
	}
	if want := 5.0 / 3000; !a.Valued || a.ProfitETH < want*0.999999 || a.ProfitETH > want*1.000001 {
		t.Fatalf("profit ETH = %v, want ≈ %v", a.ProfitETH, want)
	}
}

func TestNonArbitrageTrades(t *testing.T) {
	cases := map[string]eth.Receipt{
		"single swap": receipt(0, user, addr(0x77), 1, v2Swap(poolA, 1e18, 0, 0, 3000e6)),
		"multi-hop trade WETH->USDC->ARB": receipt(0, user, addr(0x77), 1,
			v2Swap(poolA, 1e18, 0, 0, 3000e6),
			v2Swap(poolC, 0, 3000e6, 4_000_000, 0),
		),
		"round trip at a loss": receipt(0, user, addr(0x77), 1,
			v2Swap(poolA, 1e18, 0, 0, 3000e6),
			v3Swap(poolB, neg(99e16), u(3000e6), sqrtP3000),
		),
		"cycle through a non-canonical pool": receipt(0, user, addr(0x77), 1,
			v2Swap(poolA, 1e18, 0, 0, 3000e6),
			v2Swap(fakePool, 0, 3000e6, 2e18, 0),
		),
	}
	for name, r := range cases {
		res := New(testPools(), newOracle(t)).Classify(block(r))
		if len(res.Arbs) != 0 {
			t.Errorf("%s: classified as arbitrage: %+v", name, res.Arbs[0])
		}
	}
}

func TestRevertedAttemptsByKnownBots(t *testing.T) {
	c := New(testPools(), newOracle(t), WithKnownBots([]eth.Address{bot}))
	res := c.Classify(block(
		receipt(0, eoa, bot, 0),         // known bot lost the race
		receipt(1, user, addr(0x77), 0), // unrelated revert
	))
	if len(res.Arbs) != 1 || res.Arbs[0].Status != StatusReverted || res.Arbs[0].TxIndex != 0 {
		t.Fatalf("arbs = %+v", res.Arbs)
	}
	if res.Block.RevertedArbs != 1 || res.Block.Arbs != 0 {
		t.Fatalf("block = %+v", res.Block)
	}
}

func TestBotsAreLearnedAcrossBlocks(t *testing.T) {
	c := New(testPools(), newOracle(t))
	c.Classify(block(receipt(0, eoa, bot, 1,
		v2Swap(poolA, 1e18, 0, 0, 3_030_000_000),
		v3Swap(poolB, neg(1_010_000_000_000_000_000), u(3_030_000_000), sqrtP3000),
	)))
	res := c.Classify(block(receipt(0, eoa, bot, 0)))
	if len(res.Arbs) != 1 || res.Arbs[0].Status != StatusReverted {
		t.Fatal("a contract seen arbitraging must be tracked in later blocks")
	}
}

func TestCandidates(t *testing.T) {
	unknown := addr(0x99)
	b := block(receipt(0, user, addr(0x77), 1,
		v2Swap(poolA, 1, 0, 0, 1), v3Swap(unknown, u(1), neg(1), u(1)),
		eth.Log{Address: addr(0x98), Topics: []eth.Hash{dex.TopicV2Sync}},
	))
	got := New(testPools(), newOracle(t)).Candidates(b)
	want := []dex.Candidate{{ID: id(unknown), Contract: unknown, Kind: dex.KindV3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %+v", got)
	}
}

func TestDeterministic(t *testing.T) {
	b := block(
		receipt(0, eoa, bot, 1,
			v2Swap(poolA, 1e18, 0, 0, 3_030_000_000),
			v3Swap(poolB, neg(1_010_000_000_000_000_000), u(3_030_000_000), sqrtP3000)),
		receipt(1, user, addr(0x77), 1, v2Swap(poolA, 1e18, 0, 0, 3000e6)),
		receipt(2, eoa, bot, 0),
	)
	first := New(testPools(), newOracle(t)).Classify(b)
	for range 5 {
		if again := New(testPools(), newOracle(t)).Classify(b); !reflect.DeepEqual(first, again) {
			t.Fatal("classification is not deterministic")
		}
	}
}

func TestRegimeBoundaries(t *testing.T) {
	f := RegimeBoundaries(100, 200)
	for n, want := range map[uint64]Regime{99: RegimeFCFS, 100: RegimeTimeboost, 199: RegimeTimeboost, 200: RegimePGA} {
		if got := f(n); got != want {
			t.Errorf("block %d = %s, want %s", n, got, want)
		}
	}
	if RegimeBoundaries(0, 0)(1) != RegimeUnknown {
		t.Error("unknown boundaries must yield unknown")
	}
}

func BenchmarkClassifyArbBlock(b *testing.B) {
	blk := block(receipt(0, eoa, bot, 1,
		v2Swap(poolA, 1e18, 0, 0, 3_030_000_000),
		v3Swap(poolB, neg(1_010_000_000_000_000_000), u(3_030_000_000), sqrtP3000),
	))
	o, _ := pricing.New(pricing.Config{WETH: weth, Stables: []pricing.Stable{{Address: usdc, Decimals: 6}}, RefPool: poolB, RefStable: usdc})
	c := New(testPools(), o)
	b.ReportAllocs()
	for b.Loop() {
		c.Classify(blk)
	}
}

func TestMultiTokenProfitIsSummed(t *testing.T) {
	// Pay 1 WETH for 3005 USDC on poolA, then 3000 USDC for 1.01 WETH on poolB:
	// the bot ends +0.01 WETH and +5 USDC. Both legs must count.
	r := receipt(0, eoa, bot, 1,
		v2Swap(poolA, 1e18, 0, 0, 3005e6),
		v3Swap(poolB, neg(1_010_000_000_000_000_000), u(3000e6), sqrtP3000),
	)
	o := newOracle(t)
	o.SetSqrtPrice(sqrtP3000, 1)
	res := New(testPools(), o).Classify(block(r))
	if len(res.Arbs) != 1 {
		t.Fatalf("arbs = %d", len(res.Arbs))
	}
	a := res.Arbs[0]
	if a.ProfitTokens != 2 || a.ProfitToken != weth || a.Profit.Uint64() != 1e16 {
		t.Fatalf("arb = %+v", a)
	}
	if want := 0.01 + 5.0/3000; !a.Valued || a.ProfitETH < want*0.999999 || a.ProfitETH > want*1.000001 {
		t.Fatalf("profit ETH = %v, want ≈ %v (both tokens)", a.ProfitETH, want)
	}
}

func TestPartiallyValuedProfitIsUnvalued(t *testing.T) {
	// Profit in WETH and in ARB (not covered by the oracle): reporting only the
	// WETH part would silently understate profit, so the arbitrage is unvalued.
	r := receipt(0, eoa, bot, 1,
		v2Swap(poolD, 1e18, 0, 0, 4100),     // pay 1 WETH, get 4100 ARB
		v2Swap(poolD, 0, 4000, 1_001e15, 0), // pay 4000 ARB, get 1.001 WETH
	)
	res := New(testPools(), newOracle(t)).Classify(block(r))
	if len(res.Arbs) != 1 {
		t.Fatalf("arbs = %d", len(res.Arbs))
	}
	if a := res.Arbs[0]; a.ProfitTokens != 2 || a.Valued || a.ProfitETH != 0 {
		t.Fatalf("arb = %+v", a)
	}
}

func TestStalePriceLeavesStablecoinProfitUnvalued(t *testing.T) {
	// Same +5 USDC triangle as above, but the only ETH/USD price is older than
	// MaxAgeBlocks at block 1000: the profit is reported unvalued, not misvalued.
	r := receipt(0, eoa, bot, 1,
		v2Swap(poolA, 0, 3000e6, 1e18, 0),
		v2Swap(poolD, 1e18, 0, 0, 4000e18/1e9),
		v2Swap(poolC, 4000e18/1e9, 0, 0, 3005e6),
	)
	o, err := pricing.New(pricing.Config{WETH: weth, Stables: []pricing.Stable{{Address: usdc, Decimals: 6}}, RefPool: poolB, RefStable: usdc, MaxAgeBlocks: 10})
	if err != nil {
		t.Fatal(err)
	}
	o.SetSqrtPrice(sqrtP3000, 989)
	res := New(testPools(), o).Classify(block(r))
	if len(res.Arbs) != 1 {
		t.Fatalf("arbs = %d", len(res.Arbs))
	}
	if a := res.Arbs[0]; a.ProfitToken != usdc || a.Valued || a.ProfitETH != 0 {
		t.Fatalf("arb = %+v", a)
	}
}

var (
	manager = addr(0x44)
	v4Pool  = dex.PoolID(eth.Hash{0x3e, 0x0d})
)

// v4Swap takes signed swapper deltas: positive = received by the swapper.
func v4Swap(emitter eth.Address, pool dex.PoolID, amount0, amount1 *uint256.Int) eth.Log {
	data := append(append([]byte{}, word(amount0)...), word(amount1)...)
	data = append(data, make([]byte, 4*32)...)
	return eth.Log{Address: emitter, Topics: []eth.Hash{dex.TopicV4Swap, eth.Hash(pool), {}}, Data: data}
}

func poolsWithV4() pools {
	p := testPools()
	// Native ETH / token pool; the registry aliases native ETH to WETH.
	p[v4Pool] = dex.Pool{ID: v4Pool, Contract: manager, Kind: dex.KindV4, Canonical: true, Venue: "uniswap-v4",
		Token0: weth, Token1: usdc, Native: true, FeePips: 3000}
	return p
}

func TestArbitrageAcrossV3AndV4WithNativeETH(t *testing.T) {
	// Mirrors mainnet tx 0x3b30…: buy native ETH with USDC on v4, sell WETH for
	// USDC on v3. Only the ETH≡WETH alias makes the legs cancel out.
	r := receipt(0, eoa, bot, 1,
		v4Swap(manager, v4Pool, u(1e18), neg(3000e6)),  // swapper pays 3000 USDC, receives 1 ETH
		v3Swap(poolB, u(1e18), neg(3005e6), sqrtP3000), // pool takes 1 WETH, pays 3005 USDC
	)
	res := New(poolsWithV4(), newOracle(t)).Classify(block(r))
	if len(res.Arbs) != 1 {
		t.Fatalf("arbs = %d, want 1", len(res.Arbs))
	}
	a := res.Arbs[0]
	if a.ProfitToken != usdc || a.Profit.Uint64() != 5e6 || a.ProfitTokens != 1 {
		t.Fatalf("arb = %+v (profit %s)", a, a.Profit.Dec())
	}
	if !reflect.DeepEqual(a.Pools, []dex.PoolID{v4Pool, id(poolB)}) || !reflect.DeepEqual(a.Contracts, []eth.Address{manager, poolB}) {
		t.Fatalf("pools = %v contracts = %v", a.Pools, a.Contracts)
	}
}

func TestV4SwapFromWrongEmitterIsIgnored(t *testing.T) {
	// Same pool id, but emitted by a contract that is not the pool's PoolManager.
	r := receipt(0, eoa, bot, 1,
		v4Swap(addr(0x66), v4Pool, u(1e18), neg(3000e6)),
		v3Swap(poolB, u(1e18), neg(3005e6), sqrtP3000),
	)
	res := New(poolsWithV4(), newOracle(t)).Classify(block(r))
	if len(res.Arbs) != 0 || res.Block.Swaps != 1 {
		t.Fatalf("spoofed v4 swap was trusted: arbs=%d swaps=%d", len(res.Arbs), res.Block.Swaps)
	}
}

func TestCandidatesIncludeV4InitializeAndSwaps(t *testing.T) {
	newPool := dex.PoolID(eth.Hash{0x99})
	initData := make([]byte, 5*32)
	b := block(receipt(0, user, addr(0x77), 1,
		eth.Log{Address: manager, Topics: []eth.Hash{dex.TopicV4Initialize, eth.Hash(newPool), {}, usdc.Word()}, Data: initData},
		v4Swap(manager, newPool, u(1), neg(1)),
	))
	got := New(poolsWithV4(), newOracle(t)).Candidates(b)
	if len(got) != 2 || got[0].Init == nil || got[0].ID != newPool || got[1].Kind != dex.KindV4 || got[1].ID != newPool {
		t.Fatalf("candidates = %+v", got)
	}
}
