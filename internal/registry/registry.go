// Package registry decides which contracts are canonical AMM pools.
//
// Any contract can emit a log that looks like a Uniswap Swap (honeypots do exactly
// that). A pool is canonical only if a configured factory, asked for the pool of
// the pool's own (token0, token1[, fee]), answers with the pool's address. Results
// (positive and negative) are cached, so each address costs two batched round trips
// at most once in its lifetime.
//
// A Registry is not safe for concurrent use: it is owned by the single processing
// goroutine of the observatory pipeline.
package registry

import (
	"context"
	"fmt"
	"slices"

	"github.com/huynhnhatkhanh/mevlens/internal/dex"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
	"github.com/huynhnhatkhanh/mevlens/internal/rpc"
)

// Factory is a trusted pool factory.
type Factory struct {
	Name    string // e.g. "uniswap-v3"; used as the pool's venue label
	Address eth.Address
	Kind    dex.Kind
}

// Caller is the subset of *rpc.Client the registry needs.
type Caller interface {
	Batch(ctx context.Context, reqs []rpc.Request) error
}

var (
	selFactory = eth.NewSelector("factory()")
	selToken0  = eth.NewSelector("token0()")
	selToken1  = eth.NewSelector("token1()")
	selFee     = eth.NewSelector("fee()")
	selGetPair = eth.NewSelector("getPair(address,address)")
	selGetPool = eth.NewSelector("getPool(address,address,uint24)")
)

// Registry caches pool metadata.
type Registry struct {
	caller    Caller
	factories map[eth.Address]Factory
	pools     map[eth.Address]dex.Pool
	fresh     []dex.Pool // resolved since the last DrainNew, for persistence
}

// New builds an empty registry trusting the given factories.
func New(caller Caller, factories []Factory) (*Registry, error) {
	r := &Registry{caller: caller, factories: make(map[eth.Address]Factory, len(factories)), pools: make(map[eth.Address]dex.Pool)}
	for _, f := range factories {
		if f.Kind != dex.KindV2 && f.Kind != dex.KindV3 {
			return nil, fmt.Errorf("registry: factory %s has unsupported kind %s", f.Name, f.Kind)
		}
		if _, dup := r.factories[f.Address]; dup {
			return nil, fmt.Errorf("registry: duplicate factory %s", f.Address)
		}
		r.factories[f.Address] = f
	}
	return r, nil
}

// Load seeds the cache, e.g. from persistent storage at startup.
func (r *Registry) Load(pools []dex.Pool) {
	for _, p := range pools {
		r.pools[p.Address] = p
	}
}

// Lookup returns cached metadata for addr.
func (r *Registry) Lookup(addr eth.Address) (dex.Pool, bool) {
	p, ok := r.pools[addr]
	return p, ok
}

// Len returns the number of cached addresses and how many of them are canonical.
func (r *Registry) Len() (total, canonical int) {
	for _, p := range r.pools {
		if p.Canonical {
			canonical++
		}
	}
	return len(r.pools), canonical
}

// DrainNew returns pools resolved since the previous call, in address order.
func (r *Registry) DrainNew() []dex.Pool {
	out := r.fresh
	r.fresh = nil
	slices.SortFunc(out, func(a, b dex.Pool) int { return a.Address.Compare(b.Address) })
	return out
}

// Resolve resolves every unknown candidate. It returns an error only for transient
// RPC failures, in which case nothing is cached and the caller should retry.
func (r *Registry) Resolve(ctx context.Context, block uint64, cands []dex.Candidate) error {
	todo := r.unknown(cands)
	if len(todo) == 0 {
		return nil
	}
	pools, err := r.readImmutables(ctx, block, todo)
	if err != nil {
		return err
	}
	if err := r.verifyWithFactories(ctx, pools); err != nil {
		return err
	}
	for _, p := range pools {
		r.pools[p.Address] = p
		r.fresh = append(r.fresh, p)
	}
	return nil
}

func (r *Registry) unknown(cands []dex.Candidate) []dex.Candidate {
	var todo []dex.Candidate
	seen := make(map[eth.Address]bool, len(cands))
	for _, c := range cands {
		if _, known := r.pools[c.Address]; known || seen[c.Address] || c.Kind == dex.KindUnknown {
			continue
		}
		seen[c.Address] = true
		todo = append(todo, c)
	}
	return todo
}

// readImmutables reads factory(), token0(), token1() and, for v3, fee() of each candidate.
func (r *Registry) readImmutables(ctx context.Context, block uint64, todo []dex.Candidate) ([]dex.Pool, error) {
	type slot struct{ factory, token0, token1, fee eth.Data }
	slots := make([]slot, len(todo))
	reqs := make([]rpc.Request, 0, 4*len(todo))
	for i, c := range todo {
		s := &slots[i]
		reqs = append(reqs,
			rpc.NewCall(c.Address, selFactory.Calldata(), rpc.Latest, &s.factory),
			rpc.NewCall(c.Address, selToken0.Calldata(), rpc.Latest, &s.token0),
			rpc.NewCall(c.Address, selToken1.Calldata(), rpc.Latest, &s.token1),
		)
		if c.Kind == dex.KindV3 {
			reqs = append(reqs, rpc.NewCall(c.Address, selFee.Calldata(), rpc.Latest, &s.fee))
		}
	}
	if err := r.caller.Batch(ctx, reqs); err != nil {
		return nil, err
	}
	// Only an EVM revert proves "not a pool". Any other error (rate limits, an
	// endpoint lacking eth_call, ...) aborts the resolution so nothing wrong is
	// cached; the caller retries.
	failed := make(map[eth.Address]bool)
	for _, q := range reqs {
		if q.Err == nil {
			continue
		}
		if !rpc.IsRevert(q.Err) {
			return nil, fmt.Errorf("registry: resolve pools at block %d: %w", block, q.Err)
		}
		failed[q.Params[0].(rpc.CallMsg).To] = true
	}

	pools := make([]dex.Pool, len(todo))
	for i, c := range todo {
		p := dex.Pool{Address: c.Address, Kind: c.Kind, FirstSeen: block}
		s := &slots[i]
		if !failed[c.Address] {
			p.Factory, _ = wordAddress(s.factory)
			p.Token0, _ = wordAddress(s.token0)
			p.Token1, _ = wordAddress(s.token1)
			if c.Kind == dex.KindV3 {
				p.FeePips = wordUint24(s.fee)
			}
		}
		pools[i] = p
	}
	return pools, nil
}

// verifyWithFactories marks pools canonical when their factory maps their tokens back to them.
func (r *Registry) verifyWithFactories(ctx context.Context, pools []dex.Pool) error {
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
		if p.Kind == dex.KindV2 {
			data = selGetPair.Calldata(p.Token0.Word(), p.Token1.Word())
		} else {
			data = selGetPool.Calldata(p.Token0.Word(), p.Token1.Word(), eth.Uint64Word(uint64(p.FeePips)))
		}
		reqs = append(reqs, rpc.NewCall(f.Address, data, rpc.Latest, &answers[i]))
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
			if !rpc.IsRevert(q.Err) {
				return fmt.Errorf("registry: verify pool %s: %w", pools[i].Address, q.Err)
			}
			continue
		}
		if got, ok := wordAddress(answers[i]); ok && got == pools[i].Address {
			pools[i].Canonical = true
			pools[i].Venue = r.factories[pools[i].Factory].Name
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
