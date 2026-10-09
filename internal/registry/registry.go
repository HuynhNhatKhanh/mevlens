// Package registry decides which pools are canonical and resolves their metadata.
//
// Any contract can emit a log that looks like a Uniswap Swap (honeypots do exactly
// that).
//   - v2/v3: a pool is canonical only if a configured factory, asked for the pool
//     of the pool's own (token0, token1[, fee]), answers with the pool's address.
//     Algebra factories (Camelot v3) have no fee tiers and are asked
//     poolByPair(token0, token1) instead.
//     Results (positive and negative) are cached, so each address costs two batched
//     round trips at most once in its lifetime.
//   - v4: pools live inside a singleton PoolManager and are identified by a bytes32
//     id. Swap logs carry no tokens; they come from the pool's Initialize log, which
//     is read either from the blocks being processed or, for older pools, from a
//     one-off index sync (SyncV4). Only logs emitted by a configured PoolManager count.
//
// A Registry is not safe for concurrent use: it is owned by the single processing
// goroutine of the observatory pipeline (and by startup code before it runs).
package registry

import (
	"context"
	"fmt"
	"slices"

	"github.com/huynhnhatkhanh/mevlens/internal/dex"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
	"github.com/huynhnhatkhanh/mevlens/internal/rpc"
)

// Factory is a trusted pool factory, or for v4 a trusted PoolManager.
type Factory struct {
	Name       string // e.g. "uniswap-v3"; used as the pool's venue label
	Address    eth.Address
	Kind       dex.Kind
	StartBlock uint64 // v4: first block to scan for Initialize logs
	// Algebra marks a v3 factory of Algebra pools (Camelot v3). They emit the
	// Uniswap v3 Swap event but have no fee tier (fee() reverts; the fee is set per
	// swap), so they are looked up with poolByPair(token0, token1).
	Algebra bool
}

// Caller is the subset of *rpc.Client the registry needs.
type Caller interface {
	Batch(ctx context.Context, reqs []rpc.Request) error
	Logs(ctx context.Context, q rpc.LogQuery) ([]eth.Log, error)
}

var (
	selFactory = eth.NewSelector("factory()")
	selToken0  = eth.NewSelector("token0()")
	selToken1  = eth.NewSelector("token1()")
	selFee     = eth.NewSelector("fee()")
	selGetPair = eth.NewSelector("getPair(address,address)")
	selGetPool = eth.NewSelector("getPool(address,address,uint24)")
	// Algebra factories: one pool per token pair.
	selPoolByPair = eth.NewSelector("poolByPair(address,address)")
)

// callGas caps every eth_call to a candidate pool or factory. The getters cost a
// few thousand gas; the cap turns a contract that loops forever into a fast,
// deterministic out-of-gas instead of a node-side timeout.
const callGas = 5_000_000

// Registry caches pool metadata.
type Registry struct {
	caller      Caller
	factories   map[eth.Address]Factory // v2/v3 factories
	managers    map[eth.Address]Factory // v4 PoolManagers
	nativeAlias eth.Address             // token that native ETH is netted as (WETH)
	pools       map[dex.PoolID]dex.Pool
	v4Synced    map[eth.Address]uint64 // highest block scanned for Initialize logs
	fresh       []dex.Pool             // resolved since the last DrainNew, for persistence
}

// New builds an empty registry trusting the given factories and PoolManagers.
// nativeAlias (WETH) is the token native ETH is netted as; it must be set when a
// v4 manager is configured, so that WETH→ETH arbitrage legs cancel out.
func New(caller Caller, factories []Factory, nativeAlias eth.Address) (*Registry, error) {
	r := &Registry{
		caller:      caller,
		factories:   make(map[eth.Address]Factory),
		managers:    make(map[eth.Address]Factory),
		nativeAlias: nativeAlias,
		pools:       make(map[dex.PoolID]dex.Pool),
		v4Synced:    make(map[eth.Address]uint64),
	}
	for _, f := range factories {
		if _, dup := r.factories[f.Address]; dup {
			return nil, fmt.Errorf("registry: duplicate factory %s", f.Address)
		}
		if _, dup := r.managers[f.Address]; dup {
			return nil, fmt.Errorf("registry: duplicate factory %s", f.Address)
		}
		if f.Algebra && f.Kind != dex.KindV3 {
			return nil, fmt.Errorf("registry: Algebra factory %s must be of kind v3", f.Name)
		}
		switch f.Kind {
		case dex.KindV2, dex.KindV3:
			r.factories[f.Address] = f
		case dex.KindV4:
			if nativeAlias.IsZero() {
				return nil, fmt.Errorf("registry: v4 manager %s requires a native ETH alias (WETH)", f.Name)
			}
			r.managers[f.Address] = f
		default:
			return nil, fmt.Errorf("registry: factory %s has unsupported kind %s", f.Name, f.Kind)
		}
	}
	return r, nil
}

// Load seeds the cache, e.g. from persistent storage at startup.
func (r *Registry) Load(pools []dex.Pool) {
	for _, p := range pools {
		r.pools[p.ID] = p
	}
}

// Lookup returns cached metadata for a pool.
func (r *Registry) Lookup(id dex.PoolID) (dex.Pool, bool) {
	p, ok := r.pools[id]
	return p, ok
}

// Len returns the number of cached pools and how many of them are canonical.
func (r *Registry) Len() (total, canonical int) {
	for _, p := range r.pools {
		if p.Canonical {
			canonical++
		}
	}
	return len(r.pools), canonical
}

// DrainNew returns pools resolved since the previous call, in ID order.
func (r *Registry) DrainNew() []dex.Pool {
	out := r.fresh
	r.fresh = nil
	slices.SortFunc(out, func(a, b dex.Pool) int { return eth.Hash(a.ID).Compare(eth.Hash(b.ID)) })
	return out
}

// Resolve resolves every unknown candidate. It returns an error only for transient
// RPC failures, in which case nothing is cached and the caller should retry.
//
// v2/v3 pools are read at the block being processed, pinned by its hash (EIP-1898)
// when hash is not zero, never at "latest": an endpoint lagging behind the block,
// or following another fork, then fails ("header not found", retried elsewhere)
// instead of answering from a state where a brand-new pool has no code yet, which
// would cache it as not canonical forever. Immutables and factory mappings never
// change once set, so when an endpoint has pruned that state (non-archive nodes
// during a backfill) the pools are read at "latest" instead.
//
// v4 pools are resolved from Initialize candidates only. A swap on a v4 pool that
// is still unknown (initialized before the index sync) is left unresolved and not
// cached, so classification skips it rather than guessing.
func (r *Registry) Resolve(ctx context.Context, block uint64, hash eth.Hash, cands []dex.Candidate) error {
	var todo []dex.Candidate
	seen := make(map[dex.PoolID]bool, len(cands))
	for _, c := range cands {
		if _, known := r.pools[c.ID]; known || seen[c.ID] {
			continue
		}
		switch c.Kind {
		case dex.KindV4:
			// Not marked seen: an Initialize-shaped log from an untrusted contract
			// must not shadow a later candidate with the same id (the genuine
			// Initialize, or a v2/v3 pool whose address-form id it copied). A pool
			// addV4 accepts is in r.pools, which skips its duplicates.
			if c.Init != nil {
				r.addV4(*c.Init)
			}
		case dex.KindV2, dex.KindV3:
			seen[c.ID] = true
			todo = append(todo, c)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	var at any = eth.FormatBlock(block)
	if !hash.IsZero() {
		at = rpc.BlockHash{Hash: hash}
	}
	pools, err := r.resolveAt(ctx, at, block, todo)
	if rpc.IsMissingState(err) {
		pools, err = r.resolveAt(ctx, rpc.Latest, block, todo)
	}
	if err != nil {
		return err
	}
	for _, p := range pools {
		r.pools[p.ID] = p
		r.fresh = append(r.fresh, p)
	}
	return nil
}

// addV4 records a pool decoded from an Initialize log, if it comes from a trusted
// PoolManager. Native ETH is aliased so netting treats ETH and WETH as one asset.
func (r *Registry) addV4(p dex.Pool) {
	m, ok := r.managers[p.Contract]
	if !ok {
		return // an Initialize-shaped log from an untrusted contract: ignore, do not cache
	}
	if _, known := r.pools[p.ID]; known {
		return
	}
	p.Kind, p.Canonical, p.Venue, p.Factory = dex.KindV4, true, m.Name, m.Address
	if p.Token0.IsZero() { // Currency.wrap(address(0)) is native ETH and always sorts first
		p.Token0, p.Native = r.nativeAlias, true
	}
	r.pools[p.ID] = p
	r.fresh = append(r.fresh, p)
}

// resolveAt reads the candidates' immutables and verifies them with their
// factories, both at the given block (a tag string or an rpc.BlockHash).
func (r *Registry) resolveAt(ctx context.Context, tag any, block uint64, todo []dex.Candidate) ([]dex.Pool, error) {
	pools, err := r.readImmutables(ctx, tag, block, todo)
	if err != nil {
		return nil, err
	}
	if err := r.verifyWithFactories(ctx, tag, pools); err != nil {
		return nil, err
	}
	return pools, nil
}

func call(to eth.Address, data eth.Data, tag any, out *eth.Data) rpc.Request {
	return rpc.NewCallMsg(rpc.CallMsg{To: to, Data: data, Gas: callGas}, tag, out)
}

// readImmutables reads factory(), token0(), token1() and, for v3, fee() of each candidate.
func (r *Registry) readImmutables(ctx context.Context, tag any, block uint64, todo []dex.Candidate) ([]dex.Pool, error) {
	type slot struct{ factory, token0, token1, fee eth.Data }
	slots := make([]slot, len(todo))
	reqs := make([]rpc.Request, 0, 4*len(todo))
	feeCalls := make(map[int]bool) // indexes in reqs of the fee() calls
	for i, c := range todo {
		s := &slots[i]
		reqs = append(reqs,
			call(c.Contract, selFactory.Calldata(), tag, &s.factory),
			call(c.Contract, selToken0.Calldata(), tag, &s.token0),
			call(c.Contract, selToken1.Calldata(), tag, &s.token1),
		)
		if c.Kind == dex.KindV3 {
			feeCalls[len(reqs)] = true
			reqs = append(reqs, call(c.Contract, selFee.Calldata(), tag, &s.fee))
		}
	}
	if err := r.caller.Batch(ctx, reqs); err != nil {
		return nil, err
	}
	// Only a failure inside the EVM (a revert, an invalid opcode, out of gas, ...)
	// proves "not a pool": it is a property of the contract. Any other error (rate
	// limits, an endpoint lacking eth_call, ...) aborts the resolution so nothing
	// wrong is cached; the caller retries. A reverting fee() alone is not proof:
	// Algebra pools have none, and leave the verdict to their factory.
	failed := make(map[eth.Address]bool)
	noFee := make(map[eth.Address]bool)
	for k, q := range reqs {
		if q.Err == nil {
			continue
		}
		if !rpc.IsExecutionError(q.Err) {
			return nil, fmt.Errorf("registry: resolve pools at block %d: %w", block, q.Err)
		}
		if to := q.Params[0].(rpc.CallMsg).To; feeCalls[k] {
			noFee[to] = true
		} else {
			failed[to] = true
		}
	}

	pools := make([]dex.Pool, len(todo))
	for i, c := range todo {
		p := dex.Pool{ID: c.ID, Contract: c.Contract, Kind: c.Kind, FirstSeen: block}
		s := &slots[i]
		if !failed[c.Contract] {
			p.Factory, _ = wordAddress(s.factory)
			p.Token0, _ = wordAddress(s.token0)
			p.Token1, _ = wordAddress(s.token1)
			if c.Kind == dex.KindV3 && !noFee[c.Contract] {
				p.FeePips = wordUint24(s.fee)
			}
		}
		pools[i] = p
	}
	return pools, nil
}

// verifyWithFactories marks pools canonical when their factory maps their tokens back to them.
func (r *Registry) verifyWithFactories(ctx context.Context, tag any, pools []dex.Pool) error {
	var reqs []rpc.Request
	var idx []int
	answers := make([]eth.Data, len(pools))
	for i := range pools {
		p := &pools[i]
		f, ok := r.factories[p.Factory]
		if !ok || f.Kind != p.Kind || p.Token0.IsZero() || p.Token1.IsZero() {
			continue
		}
		var data eth.Data
		switch {
		case p.Kind == dex.KindV2:
			data = selGetPair.Calldata(p.Token0.Word(), p.Token1.Word())
		case f.Algebra:
			data = selPoolByPair.Calldata(p.Token0.Word(), p.Token1.Word())
		default:
			data = selGetPool.Calldata(p.Token0.Word(), p.Token1.Word(), eth.Uint64Word(uint64(p.FeePips)))
		}
		reqs = append(reqs, call(f.Address, data, tag, &answers[i]))
		idx = append(idx, i)
	}
	if len(reqs) == 0 {
		return nil
	}
	if err := r.caller.Batch(ctx, reqs); err != nil {
		return err
	}
	for k, q := range reqs {
		i := idx[k]
		if q.Err != nil {
			if !rpc.IsExecutionError(q.Err) {
				return fmt.Errorf("registry: verify pool %s: %w", pools[i].Contract, q.Err)
			}
			continue
		}
		if got, ok := wordAddress(answers[i]); ok && got == pools[i].Contract {
			f := r.factories[pools[i].Factory]
			pools[i].Canonical = true
			pools[i].Venue = f.Name
			if f.Algebra { // the fee is set per swap: there is no tier to record
				pools[i].FeePips, pools[i].DynamicFee = 0, true
			}
		}
	}
	return nil
}

func wordAddress(d eth.Data) (eth.Address, bool) {
	w, ok := eth.WordAt(d, 0)
	if !ok {
		return eth.Address{}, false
	}
	// A well-formed address word has 12 zero bytes of padding.
	for _, b := range w[:eth.HashLength-eth.AddressLength] {
		if b != 0 {
			return eth.Address{}, false
		}
	}
	return w.Address(), true
}

func wordUint24(d eth.Data) uint32 {
	w, ok := eth.WordAt(d, 0)
	if !ok {
		return 0
	}
	return uint32(w[29])<<16 | uint32(w[30])<<8 | uint32(w[31])
}
