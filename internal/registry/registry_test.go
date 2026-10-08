package registry

import (
	"context"
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
	err := r.Resolve(context.Background(), 100, []dex.Candidate{
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
	if err := r.Resolve(context.Background(), 101, []dex.Candidate{cand(pairV2, dex.KindV2), cand(honeypot, dex.KindV2)}); err != nil {
		t.Fatal(err)
	}
	if chain.calls != calls {
		t.Error("cached addresses (positive and negative) must not be re-resolved")
	}
}

func TestKindMismatchIsNotCanonical(t *testing.T) {
	// A v3-shaped Swap log from a genuine v2 pair cannot be trusted.
	r := newRegistry(t, newChain())
	if err := r.Resolve(context.Background(), 1, []dex.Candidate{cand(pairV2, dex.KindV3)}); err != nil {
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
	err := r.Resolve(context.Background(), 1, []dex.Candidate{cand(pairV2, dex.KindV2)})
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
	if err := r.Resolve(context.Background(), 1, []dex.Candidate{cand(pairV2, dex.KindV2)}); err == nil {
		t.Fatal("non-revert error must fail the resolution")
	}
	if _, ok := r.Lookup(dex.PoolIDFromAddress(pairV2)); ok {
		t.Fatal("pool cached after a non-revert error")
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
	err := r.Resolve(context.Background(), 150, []dex.Candidate{
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
