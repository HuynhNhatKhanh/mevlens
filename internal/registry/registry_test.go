package registry

import (
	"context"
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/huynhnhatkhanh/mevlens/internal/dex"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
	"github.com/huynhnhatkhanh/mevlens/internal/rpc"
)

// fakeChain answers eth_call by (to, calldata) lookup. Missing entries revert.
type fakeChain struct {
	answers map[string]eth.Data
	calls   int
	failAll error
	failTo  map[eth.Address]error // per-contract eth_call error
	failTag func(tag any) error   // per-block-tag eth_call error
	tags    []any                 // block parameter of every eth_call
	msgs    []rpc.CallMsg         // call object of every eth_call
	logs    func(rpc.LogQuery) ([]eth.Log, error)
}

func key(to eth.Address, data eth.Data) string { return to.Hex() + eth.Keccak256(data).Hex() }

func (f *fakeChain) set(to eth.Address, data eth.Data, ret eth.Hash) {
	f.answers[key(to, data)] = ret[:]
}

func (f *fakeChain) Logs(_ context.Context, q rpc.LogQuery) ([]eth.Log, error) {
	if f.logs == nil {
		return nil, nil
	}
	return f.logs(q)
}

func (f *fakeChain) Batch(_ context.Context, reqs []rpc.Request) error {
	for i := range reqs {
		f.calls++
		if f.failAll != nil {
			reqs[i].Err = f.failAll
			continue
		}
		msg := reqs[i].Params[0].(rpc.CallMsg)
		tag := reqs[i].Params[1]
		f.tags, f.msgs = append(f.tags, tag), append(f.msgs, msg)
		if f.failTag != nil {
			if err := f.failTag(tag); err != nil {
				reqs[i].Err = err
				continue
			}
		}
		if err := f.failTo[msg.To]; err != nil {
			reqs[i].Err = err
			continue
		}
		ret, ok := f.answers[key(msg.To, msg.Data)]
		if !ok {
			reqs[i].Err = &rpc.Error{Code: 3, Message: "execution reverted"}
			continue
		}
		*reqs[i].Result.(*eth.Data) = ret
	}
	return nil
}

var (
	v2Factory = eth.MustAddress("0x00000000000000000000000000000000000000f2")
	v3Factory = eth.MustAddress("0x00000000000000000000000000000000000000f3")
	weth      = eth.MustAddress("0x0000000000000000000000000000000000000a01")
	usdc      = eth.MustAddress("0x0000000000000000000000000000000000000a02")
	pairV2    = eth.MustAddress("0x0000000000000000000000000000000000000b01")
	poolV3    = eth.MustAddress("0x0000000000000000000000000000000000000b02")
	honeypot  = eth.MustAddress("0x0000000000000000000000000000000000000b03")
)

func newChain() *fakeChain {
	f := &fakeChain{answers: map[string]eth.Data{}}
	// Genuine v2 pair.
	f.set(pairV2, selFactory.Calldata(), v2Factory.Word())
	f.set(pairV2, selToken0.Calldata(), weth.Word())
	f.set(pairV2, selToken1.Calldata(), usdc.Word())
	f.set(v2Factory, selGetPair.Calldata(weth.Word(), usdc.Word()), pairV2.Word())
	// Genuine v3 pool, fee 500.
	f.set(poolV3, selFactory.Calldata(), v3Factory.Word())
	f.set(poolV3, selToken0.Calldata(), weth.Word())
	f.set(poolV3, selToken1.Calldata(), usdc.Word())
	f.set(poolV3, selFee.Calldata(), eth.Uint64Word(500))
	f.set(v3Factory, selGetPool.Calldata(weth.Word(), usdc.Word(), eth.Uint64Word(500)), poolV3.Word())
	// Honeypot: claims the real v2 factory and real tokens, but the factory maps
	// those tokens to the genuine pair, not to the honeypot.
	f.set(honeypot, selFactory.Calldata(), v2Factory.Word())
	f.set(honeypot, selToken0.Calldata(), weth.Word())
	f.set(honeypot, selToken1.Calldata(), usdc.Word())
	return f
}

func cand(a eth.Address, k dex.Kind) dex.Candidate {
	return dex.Candidate{ID: dex.PoolIDFromAddress(a), Contract: a, Kind: k}
}

func newRegistry(t *testing.T, c Caller) *Registry {
	t.Helper()
	r, err := New(c, []Factory{
		{Name: "uniswap-v2", Address: v2Factory, Kind: dex.KindV2},
		{Name: "uniswap-v3", Address: v3Factory, Kind: dex.KindV3},
	}, weth)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResolveCanonicalAndHoneypot(t *testing.T) {
	chain := newChain()
	r := newRegistry(t, chain)
	notAPool := eth.MustAddress("0x0000000000000000000000000000000000000b04")
	err := r.Resolve(context.Background(), 100, eth.Hash{}, []dex.Candidate{
		cand(pairV2, dex.KindV2), cand(poolV3, dex.KindV3), cand(honeypot, dex.KindV2), cand(notAPool, dex.KindV2),
		cand(pairV2, dex.KindV2), // duplicate in the same batch
	})
	if err != nil {
		t.Fatal(err)
	}

	want := map[eth.Address]struct {
		canonical bool
		venue     string
		fee       uint32
	}{
		pairV2:   {true, "uniswap-v2", 0},
		poolV3:   {true, "uniswap-v3", 500},
		honeypot: {false, "", 0},
		notAPool: {false, "", 0},
	}
	for addr, w := range want {
		p, ok := r.Lookup(dex.PoolIDFromAddress(addr))
		if !ok {
			t.Fatalf("%s not cached", addr)
		}
		if p.Canonical != w.canonical || p.Venue != w.venue || p.FeePips != w.fee || p.FirstSeen != 100 {
			t.Errorf("%s = %+v, want %+v", addr, p, w)
		}
	}
	if total, canonical := r.Len(); total != 4 || canonical != 2 {
		t.Errorf("Len = %d/%d", total, canonical)
	}
	if fresh := r.DrainNew(); len(fresh) != 4 || eth.Hash(fresh[0].ID).Compare(eth.Hash(fresh[1].ID)) > 0 {
		t.Errorf("DrainNew = %d pools (must be sorted)", len(fresh))
	}
	if len(r.DrainNew()) != 0 {
		t.Error("DrainNew must reset")
	}

	calls := chain.calls
	if err := r.Resolve(context.Background(), 101, eth.Hash{}, []dex.Candidate{cand(pairV2, dex.KindV2), cand(honeypot, dex.KindV2)}); err != nil {
		t.Fatal(err)
	}
	if chain.calls != calls {
		t.Error("cached addresses (positive and negative) must not be re-resolved")
	}
}

func TestKindMismatchIsNotCanonical(t *testing.T) {
	// A v3-shaped Swap log from a genuine v2 pair cannot be trusted.
	r := newRegistry(t, newChain())
	if err := r.Resolve(context.Background(), 1, eth.Hash{}, []dex.Candidate{cand(pairV2, dex.KindV3)}); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.Lookup(dex.PoolIDFromAddress(pairV2)); p.Canonical {
		t.Fatal("kind mismatch must not be canonical")
	}
}

func TestTransientErrorCachesNothing(t *testing.T) {
	chain := newChain()
	chain.failAll = &rpc.HTTPError{Status: 429}
	r := newRegistry(t, chain)
	err := r.Resolve(context.Background(), 1, eth.Hash{}, []dex.Candidate{cand(pairV2, dex.KindV2)})
	var he *rpc.HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want transient HTTP error", err)
	}
	if _, ok := r.Lookup(dex.PoolIDFromAddress(pairV2)); ok {
		t.Fatal("transient failure must not be cached")
	}
}

func TestNewRejectsBadFactories(t *testing.T) {
	if _, err := New(nil, []Factory{{Name: "x", Kind: dex.KindUnknown}}, weth); err == nil {
		t.Error("unknown kind accepted")
	}
	dup := []Factory{{Name: "a", Address: v2Factory, Kind: dex.KindV2}, {Name: "b", Address: v2Factory, Kind: dex.KindV2}}
	if _, err := New(nil, dup, weth); err == nil {
		t.Error("duplicate factory accepted")
	}
}

func TestWordAddressRejectsDirtyPadding(t *testing.T) {
	w := weth.Word()
	w[0] = 1
	if _, ok := wordAddress(w[:]); ok {
		t.Fatal("non-zero padding must be rejected")
	}
	if _, ok := wordAddress(nil); ok {
		t.Fatal("empty return data must be rejected")
	}
}

func TestOnlyRevertsAreCachedAsNotAPool(t *testing.T) {
	// Regression: an endpoint lacking eth_call used to get genuine pools cached as
	// "not a pool" forever.
	chain := newChain()
	chain.failAll = &rpc.Error{Code: -32000, Message: "The method eth_call is not supported."}
	r := newRegistry(t, chain)
	if err := r.Resolve(context.Background(), 1, eth.Hash{}, []dex.Candidate{cand(pairV2, dex.KindV2)}); err == nil {
		t.Fatal("non-revert error must fail the resolution")
	}
	if _, ok := r.Lookup(dex.PoolIDFromAddress(pairV2)); ok {
		t.Fatal("pool cached after a non-revert error")
	}
}

func TestExecutionFailureIsNotAPool(t *testing.T) {
	// Regression: a contract emitting Swap-shaped logs whose getters hit INVALID
	// (or loop until out of gas) made Resolve fail forever, halting the pipeline,
	// and blocked the genuine pools of the same block.
	for _, vmErr := range []string{"invalid opcode: INVALID", "out of gas", "stack underflow (0 <=> 1)"} {
		chain := newChain()
		chain.failTo = map[eth.Address]error{honeypot: &rpc.Error{Code: -32000, Message: vmErr}}
		r := newRegistry(t, chain)
		err := r.Resolve(context.Background(), 1, eth.Hash{}, []dex.Candidate{cand(honeypot, dex.KindV2), cand(pairV2, dex.KindV2)})
		if err != nil {
			t.Fatalf("%s: Resolve = %v, want the failing contract classified as not a pool", vmErr, err)
		}
		if p, ok := r.Lookup(dex.PoolIDFromAddress(honeypot)); !ok || p.Canonical {
			t.Fatalf("%s: failing contract = %+v, %v; want cached as not canonical", vmErr, p, ok)
		}
		if p, ok := r.Lookup(dex.PoolIDFromAddress(pairV2)); !ok || !p.Canonical {
			t.Fatalf("%s: genuine pair in the same block = %+v, %v; want canonical", vmErr, p, ok)
		}
	}
}

func TestCallsArePinnedToTheBlock(t *testing.T) {
	// Regression: calls at "latest" on an endpoint lagging behind the processed
	// block saw a brand-new pool without code and cached it as not canonical.
	// Pinning by number alone still let a node on another fork answer from a
	// state without the pool, so the hash is used when known.
	hash := eth.Keccak256([]byte("block 123"))
	for _, tt := range []struct {
		hash eth.Hash
		want any
	}{
		{hash, rpc.BlockHash{Hash: hash}},
		{eth.Hash{}, eth.FormatBlock(123)},
	} {
		chain := newChain()
		r := newRegistry(t, chain)
		if err := r.Resolve(context.Background(), 123, tt.hash, []dex.Candidate{cand(pairV2, dex.KindV2)}); err != nil {
			t.Fatal(err)
		}
		if len(chain.tags) == 0 {
			t.Fatal("no eth_call made")
		}
		for i, tag := range chain.tags {
			if tag != tt.want {
				t.Fatalf("call %d at %v, want %v", i, tag, tt.want)
			}
			if chain.msgs[i].Gas != callGas {
				t.Fatalf("call %d gas = %d, want the cap %d", i, chain.msgs[i].Gas, callGas)
			}
		}
	}
}

func TestBlockHashParam(t *testing.T) {
	b, err := json.Marshal(rpc.BlockHash{Hash: eth.Keccak256([]byte("x"))})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"blockHash":"` + eth.Keccak256([]byte("x")).Hex() + `"}`; string(b) != want {
		t.Fatalf("BlockHash = %s, want the EIP-1898 object %s", b, want)
	}
}

func TestUnknownBlockHashCachesNothing(t *testing.T) {
	// A node following another fork does not know the hash: nothing is cached.
	chain := newChain()
	chain.failAll = &rpc.Error{Code: -32000, Message: "header for hash not found"}
	r := newRegistry(t, chain)
	err := r.Resolve(context.Background(), 1, eth.Keccak256([]byte("orphan")), []dex.Candidate{cand(pairV2, dex.KindV2)})
	if err == nil || !rpc.IsUnknownBlock(err) {
		t.Fatalf("Resolve = %v, want an unknown-block failure", err)
	}
	if _, ok := r.Lookup(dex.PoolIDFromAddress(pairV2)); ok {
		t.Fatal("pool cached although the node did not have the block")
	}
}

func TestLaggingEndpointCachesNothing(t *testing.T) {
	chain := newChain()
	chain.failAll = &rpc.Error{Code: -32000, Message: "header not found"}
	r := newRegistry(t, chain)
	if err := r.Resolve(context.Background(), 1, eth.Hash{}, []dex.Candidate{cand(pairV2, dex.KindV2)}); err == nil {
		t.Fatal("a node without the block must fail the resolution")
	}
	if _, ok := r.Lookup(dex.PoolIDFromAddress(pairV2)); ok {
		t.Fatal("pool cached although no endpoint had the block")
	}
}

func TestPrunedStateFallsBackToLatest(t *testing.T) {
	chain := newChain()
	chain.failTag = func(tag any) error {
		if tag != rpc.Latest {
			return &rpc.Error{Code: -32000, Message: "historical state 79ff2b is not available"}
		}
		return nil
	}
	r := newRegistry(t, chain)
	if err := r.Resolve(context.Background(), 1, eth.Hash{}, []dex.Candidate{cand(pairV2, dex.KindV2)}); err != nil {
		t.Fatal(err)
	}
	if p, ok := r.Lookup(dex.PoolIDFromAddress(pairV2)); !ok || !p.Canonical {
		t.Fatalf("pair = %+v, %v; want canonical, read at latest", p, ok)
	}
}

var (
	manager = eth.MustAddress("0x360e68faccca8ca495c1b759fd9eee466db9fb32")
	token   = eth.MustAddress("0x0000000000000000000000000000000000000c01")
)

func newV4Registry(t *testing.T, c Caller) *Registry {
	t.Helper()
	r, err := New(c, []Factory{{Name: "uniswap-v4", Address: manager, Kind: dex.KindV4, StartBlock: 100}}, weth)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func initLog(id eth.Hash, emitter eth.Address, block uint64) eth.Log {
	data := make([]byte, 5*32)
	data[30], data[31] = 0x0b, 0xb8 // fee 3000 = 0x0bb8
	return eth.Log{
		Address: emitter, BlockNumber: eth.Quantity(block),
		Topics: []eth.Hash{dex.TopicV4Initialize, id, {}, token.Word()}, // currency0 = native ETH
		Data:   data,
	}
}

func TestV4PoolsFromInitializeCandidates(t *testing.T) {
	r := newV4Registry(t, newChain())
	trusted := eth.Hash{0x01}
	spoofed := eth.Hash{0x02}
	unknownSwap := eth.Hash{0x03}

	l := initLog(trusted, manager, 150)
	p, _ := dex.DecodeV4Initialize(&l)
	l2 := initLog(spoofed, eth.MustAddress("0x00000000000000000000000000000000000000ff"), 150)
	q, _ := dex.DecodeV4Initialize(&l2)
	err := r.Resolve(context.Background(), 150, eth.Hash{}, []dex.Candidate{
		{ID: dex.PoolID(unknownSwap), Contract: manager, Kind: dex.KindV4}, // swap on a pool we never saw initialized
		{ID: p.ID, Contract: manager, Kind: dex.KindV4, Init: &p},
		{ID: q.ID, Contract: q.Contract, Kind: dex.KindV4, Init: &q}, // Initialize-shaped log from another contract
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r.Lookup(dex.PoolID(trusted))
	if !ok || !got.Canonical || got.Venue != "uniswap-v4" || got.Token0 != weth || !got.Native || got.Token1 != token || got.FeePips != 3000 {
		t.Fatalf("trusted pool = %+v", got)
	}
	if _, ok := r.Lookup(dex.PoolID(spoofed)); ok {
		t.Fatal("pool initialized by an untrusted contract was accepted")
	}
	if _, ok := r.Lookup(dex.PoolID(unknownSwap)); ok {
		t.Fatal("unknown v4 pool must not be cached (a later index sync may resolve it)")
	}
}

func TestSyncV4NarrowsRangeAndResumes(t *testing.T) {
	var queries []rpc.LogQuery
	chain := newChain()
	chain.logs = func(q rpc.LogQuery) ([]eth.Log, error) {
		queries = append(queries, q)
		if q.To-q.From+1 > 4_000_000 { // this provider accepts at most 4M blocks
			return nil, &rpc.Error{Code: -32602, Message: "query spans too many blocks: block range limit 4000000"}
		}
		if q.From <= 2_000_000 && 2_000_000 <= q.To {
			return []eth.Log{initLog(eth.Hash{0xaa}, manager, 2_000_000)}, nil
		}
		return nil, nil
	}
	r := newV4Registry(t, chain)
	var progress []uint64
	err := r.SyncV4(context.Background(), 9_000_099, func(_ Factory, through uint64, _ int) error {
		progress = append(progress, through)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := r.Lookup(dex.PoolID(eth.Hash{0xaa})); !ok || p.FirstSeen != 2_000_000 {
		t.Fatalf("indexed pool = %+v %v", p, ok)
	}
	if r.V4Synced(manager) != 9_000_099 || progress[len(progress)-1] != 9_000_099 {
		t.Fatalf("synced = %d, progress = %v", r.V4Synced(manager), progress)
	}
	for _, q := range queries[2:] { // after two refusals (10M, 5M) every query fits
		if q.To-q.From+1 > 4_000_000 {
			t.Fatalf("query %d-%d too wide after narrowing", q.From, q.To)
		}
	}

	// Resuming scans only new blocks.
	queries = nil
	if err := r.SyncV4(context.Background(), 9_000_200, nil); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 || queries[0].From != 9_000_100 {
		t.Fatalf("resume queries = %+v", queries)
	}
}

func TestV4RequiresNativeAlias(t *testing.T) {
	if _, err := New(nil, []Factory{{Name: "v4", Address: manager, Kind: dex.KindV4}}, eth.Address{}); err == nil {
		t.Fatal("v4 without a native ETH alias must be rejected")
	}
}

func TestUntrustedInitializeDoesNotShadowGenuineCandidates(t *testing.T) {
	// Regression: a fake Initialize earlier in the block marked its id as seen
	// before the emitter was checked, so the genuine candidate with the same id
	// was skipped: the real v4 pool (or a v2 pair, via its address-form id) then
	// stayed unknown and its swaps invisible.
	chain := newChain()
	r, err := New(chain, []Factory{
		{Name: "uniswap-v2", Address: v2Factory, Kind: dex.KindV2},
		{Name: "uniswap-v4", Address: manager, Kind: dex.KindV4, StartBlock: 100},
	}, weth)
	if err != nil {
		t.Fatal(err)
	}
	attacker := eth.MustAddress("0x00000000000000000000000000000000000000ff")
	genuine := initLog(eth.Hash{0x01}, manager, 150)
	p, _ := dex.DecodeV4Initialize(&genuine)
	fakeV4 := initLog(eth.Hash{0x01}, attacker, 150)
	q, _ := dex.DecodeV4Initialize(&fakeV4)
	fakeV2 := initLog(pairV2.Word(), attacker, 150)
	f, _ := dex.DecodeV4Initialize(&fakeV2)

	err = r.Resolve(context.Background(), 150, eth.Hash{}, []dex.Candidate{
		{ID: q.ID, Contract: attacker, Kind: dex.KindV4, Init: &q},
		{ID: f.ID, Contract: attacker, Kind: dex.KindV4, Init: &f},
		{ID: p.ID, Contract: manager, Kind: dex.KindV4, Init: &p},
		cand(pairV2, dex.KindV2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Lookup(p.ID); !ok || !got.Canonical || got.Contract != manager {
		t.Fatalf("genuine v4 pool = %+v, %v; want canonical from the PoolManager", got, ok)
	}
	if got, ok := r.Lookup(dex.PoolIDFromAddress(pairV2)); !ok || !got.Canonical || got.Kind != dex.KindV2 {
		t.Fatalf("v2 pair = %+v, %v; want canonical", got, ok)
	}
}

func TestAlgebraPoolsAreLookedUpByPair(t *testing.T) {
	// Regression: Camelot v3 (Algebra) pools emit the Uniswap v3 Swap event but
	// have no fee(); the revert cached every one of them as "not a pool".
	algebraFactory := eth.MustAddress("0x00000000000000000000000000000000000000a3")
	algebraPool := eth.MustAddress("0x0000000000000000000000000000000000000b05")
	impostor := eth.MustAddress("0x0000000000000000000000000000000000000b06")
	feeless := eth.MustAddress("0x0000000000000000000000000000000000000b07")
	chain := newChain()
	for _, p := range []eth.Address{algebraPool, impostor} { // no fee() answer: it reverts
		chain.set(p, selFactory.Calldata(), algebraFactory.Word())
		chain.set(p, selToken0.Calldata(), weth.Word())
		chain.set(p, selToken1.Calldata(), usdc.Word())
	}
	chain.set(algebraFactory, selPoolByPair.Calldata(weth.Word(), usdc.Word()), algebraPool.Word())
	// A pool claiming the Uniswap v3 factory without a fee() is not canonical there.
	chain.set(feeless, selFactory.Calldata(), v3Factory.Word())
	chain.set(feeless, selToken0.Calldata(), weth.Word())
	chain.set(feeless, selToken1.Calldata(), usdc.Word())

	r, err := New(chain, []Factory{
		{Name: "uniswap-v3", Address: v3Factory, Kind: dex.KindV3},
		{Name: "camelot-v3", Address: algebraFactory, Kind: dex.KindV3, Algebra: true},
	}, weth)
	if err != nil {
		t.Fatal(err)
	}
	err = r.Resolve(context.Background(), 1, eth.Hash{}, []dex.Candidate{
		cand(algebraPool, dex.KindV3), cand(impostor, dex.KindV3), cand(feeless, dex.KindV3), cand(poolV3, dex.KindV3),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := r.Lookup(dex.PoolIDFromAddress(algebraPool)); !ok || !p.Canonical || p.Venue != "camelot-v3" ||
		!p.DynamicFee || p.FeePips != 0 || p.Token0 != weth || p.Token1 != usdc {
		t.Fatalf("algebra pool = %+v, %v", p, ok)
	}
	for _, a := range []eth.Address{impostor, feeless} {
		if p, ok := r.Lookup(dex.PoolIDFromAddress(a)); !ok || p.Canonical {
			t.Fatalf("%s = %+v, %v; want cached as not canonical", a, p, ok)
		}
	}
	if p, ok := r.Lookup(dex.PoolIDFromAddress(poolV3)); !ok || !p.Canonical || p.FeePips != 500 || p.DynamicFee {
		t.Fatalf("uniswap v3 pool next to them = %+v, %v", p, ok)
	}
	// The fee() revert alone was not proof: the pool's other getters were read.
	if p, _ := r.Lookup(dex.PoolIDFromAddress(feeless)); p.Factory != v3Factory || p.Token0 != weth {
		t.Fatalf("feeless pool immutables = %+v", p)
	}

	if _, err := New(chain, []Factory{{Name: "x", Address: algebraFactory, Kind: dex.KindV2, Algebra: true}}, weth); err == nil {
		t.Fatal("an Algebra factory of kind v2 must be rejected")
	}
}
