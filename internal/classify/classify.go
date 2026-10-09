// Package classify turns a block of receipts into analytics rows: every swap on a
// canonical pool, and every atomic arbitrage (successful or reverted).
//
// The package is the deterministic core of the observatory. It performs no I/O and
// never reads the clock: given the same blocks in the same order it produces the
// same output, which makes historical replays and golden tests exact.
//
// # Detection
//
// A successful transaction is an atomic arbitrage when it swaps on at least two
// canonical pools and the pool-side token flows net out to a non-negative amount of
// every token and a strictly positive amount of at least one. Netting on the pool
// side, rather than on the transfers of a guessed beneficiary, keeps detection
// correct when bots route through helper contracts or forward profit elsewhere.
//
// A reverted transaction sent to a contract previously seen performing arbitrage
// is recorded as a failed attempt: under ordering auctions, losing a race still
// lands the transaction on-chain, so reverts measure competition.
package classify

import (
	"github.com/holiman/uint256"

	"github.com/huynhnhatkhanh/mevlens/internal/dex"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
	"github.com/huynhnhatkhanh/mevlens/internal/pricing"
)

// PoolLookup returns cached pool metadata.
type PoolLookup interface {
	Lookup(id dex.PoolID) (dex.Pool, bool)
}

// Classifier holds the state that carries across blocks: the ETH/USD oracle and
// the set of contracts known to perform arbitrage. It is not safe for concurrent use.
type Classifier struct {
	pools  PoolLookup
	prices *pricing.Oracle
	regime RegimeFunc
	bots   map[eth.Address]struct{}

	// scratch buffers reused across transactions to avoid per-tx allocations
	txSwaps []decoded
	flows   []flow
}

type decoded struct {
	swap dex.Swap
	pool dex.Pool
}

// flow accumulates, for one token, what the pools paid out (credit) and took in
// (debit) across a transaction.
type flow struct {
	token  eth.Address
	credit uint256.Int
	debit  uint256.Int
}

// Option customises a Classifier.
type Option func(*Classifier)

// WithRegime sets the function assigning a market regime to each block.
func WithRegime(f RegimeFunc) Option { return func(c *Classifier) { c.regime = f } }

// WithKnownBots seeds the set of known arbitrage contracts (e.g. from storage).
func WithKnownBots(bots []eth.Address) Option {
	return func(c *Classifier) {
		for _, b := range bots {
			c.bots[b] = struct{}{}
		}
	}
}

// New builds a Classifier.
func New(pools PoolLookup, prices *pricing.Oracle, opts ...Option) *Classifier {
	c := &Classifier{pools: pools, prices: prices, bots: make(map[eth.Address]struct{}), regime: func(uint64) Regime { return RegimeUnknown }}
	for _, o := range opts {
		o(c)
	}
	return c
}

// KnownBots returns the number of contracts known to perform arbitrage.
func (c *Classifier) KnownBots() int { return len(c.bots) }

// Candidates lists the pools referenced by swap logs in b that the registry does
// not know yet, plus every v4 pool initialized in b (which carries its own
// metadata), so pools created and traded in the same block resolve immediately.
// Duplicates are left to the registry.
func (c *Classifier) Candidates(b *eth.Block) []dex.Candidate {
	var out []dex.Candidate
	for i := range b.Receipts {
		for j := range b.Receipts[i].Logs {
			l := &b.Receipts[i].Logs[j]
			if len(l.Topics) < 2 || l.Removed {
				continue
			}
			if l.Topics[0] == dex.TopicV4Initialize {
				if p, ok := dex.DecodeV4Initialize(l); ok {
					if _, known := c.pools.Lookup(p.ID); !known {
						p.FirstSeen = uint64(b.Header.Number)
						out = append(out, dex.Candidate{ID: p.ID, Contract: l.Address, Kind: dex.KindV4, Init: &p})
					}
				}
				continue
			}
			var id dex.PoolID
			switch k := dex.KindOfTopic(l.Topics[0]); k {
			case dex.KindV2, dex.KindV3:
				id = dex.PoolIDFromAddress(l.Address)
			case dex.KindV4:
				id = dex.PoolID(l.Topics[1])
			default:
				continue
			}
			if _, known := c.pools.Lookup(id); !known {
				out = append(out, dex.Candidate{ID: id, Contract: l.Address, Kind: dex.KindOfTopic(l.Topics[0])})
			}
		}
	}
	return out
}

// Classify processes one block. Pools referenced by b must have been resolved.
func (c *Classifier) Classify(b *eth.Block) Result {
	h := &b.Header
	res := Result{Block: BlockInfo{
		Number:     uint64(h.Number),
		Hash:       h.Hash,
		ParentHash: h.ParentHash,
		Timestamp:  uint64(h.Timestamp),
		BaseFee:    uint64(h.BaseFee),
		GasUsed:    uint64(h.GasUsed),
		L1Block:    uint64(h.L1BlockNumber),
		TxCount:    uint32(len(b.Receipts)),
		Regime:     c.regime(uint64(h.Number)),
	}}
	for i := range b.Receipts {
		r := &b.Receipts[i]
		if r.Timeboosted {
			res.Block.TimeboostedTxs++
		}
		if !r.Succeeded() {
			if _, bot := c.bots[r.To]; bot && !r.To.IsZero() {
				res.Arbs = append(res.Arbs, c.newArb(&res.Block, r, StatusReverted))
				res.Block.RevertedArbs++
			}
			continue
		}
		c.collectSwaps(&res, r)
		if a, ok := c.detect(&res.Block, r); ok {
			res.Arbs = append(res.Arbs, a)
			res.Block.Arbs++
			if !r.To.IsZero() {
				c.bots[r.To] = struct{}{}
			}
		}
	}
	return res
}

// collectSwaps decodes the canonical swaps of r into c.txSwaps and appends swap rows.
func (c *Classifier) collectSwaps(res *Result, r *eth.Receipt) {
	c.txSwaps = c.txSwaps[:0]
	for j := range r.Logs {
		s, ok := dex.DecodeSwap(&r.Logs[j])
		if !ok {
			continue
		}
		p, ok := c.pools.Lookup(s.Pool)
		// The emitter must match too: a v4 swap is only trusted from the PoolManager
		// that initialized the pool.
		if !ok || !p.Canonical || p.Kind != s.Kind || p.Contract != s.Contract {
			continue
		}
		c.prices.ObserveSwap(&s, res.Block.Number)
		c.txSwaps = append(c.txSwaps, decoded{swap: s, pool: p})

		row := SwapRow{
			Block: res.Block.Number, BlockHash: res.Block.Hash, Timestamp: res.Block.Timestamp,
			TxIndex: uint32(r.TxIndex), LogIndex: s.LogIndex, TxHash: r.TxHash,
			Pool: p.ID, Contract: s.Contract, Venue: p.Venue,
		}
		zeroForOne, in, out, ok := s.Direction()
		if !ok {
			continue // degenerate flow: still used for netting, not reported as a swap row
		}
		row.TokenIn, row.TokenOut = p.Token1, p.Token0
		if zeroForOne {
			row.TokenIn, row.TokenOut = p.Token0, p.Token1
		}
		row.AmountIn, row.AmountOut = in, out
		res.Swaps = append(res.Swaps, row)
		res.Block.Swaps++
	}
}

// detect applies the netting rule to c.txSwaps.
func (c *Classifier) detect(blk *BlockInfo, r *eth.Receipt) (Arb, bool) {
	if len(c.txSwaps) < 2 {
		return Arb{}, false
	}
	c.flows = c.flows[:0]
	for i := range c.txSwaps {
		d := &c.txSwaps[i]
		if !c.addFlow(d.pool.Token0, &d.swap.Out0, &d.swap.In0) || !c.addFlow(d.pool.Token1, &d.swap.Out1, &d.swap.In1) {
			return Arb{}, false // overflow: not a meaningful trade
		}
	}
	profitIdx, profitTokens := -1, 0
	for i := range c.flows {
		f := &c.flows[i]
		switch f.credit.Cmp(&f.debit) {
		case -1:
			return Arb{}, false // the trader is net short some token: a trade, not an arbitrage
		case 1:
			if profitIdx < 0 {
				profitIdx = i // primary profit token: first in order of appearance
			}
			profitTokens++
		}
	}
	if profitIdx < 0 {
		return Arb{}, false
	}

	a := c.newArb(blk, r, StatusSuccess)
	f := &c.flows[profitIdx]
	a.ProfitToken = f.token
	a.Profit.Sub(&f.credit, &f.debit)
	a.ProfitTokens = uint8(min(profitTokens, 255))
	// ProfitETH covers every profitable token; it counts as valued only if all of
	// them could be valued, so multi-token profits are never silently understated.
	a.Valued = true
	for i := range c.flows {
		f := &c.flows[i]
		if f.credit.Cmp(&f.debit) <= 0 {
			continue
		}
		var amount uint256.Int
		amount.Sub(&f.credit, &f.debit)
		v, ok := c.prices.ValueETH(f.token, &amount, blk.Number)
		a.ProfitETH += v
		a.Valued = a.Valued && ok
	}
	if !a.Valued {
		a.ProfitETH = 0
	}
	a.Hops = uint8(min(len(c.txSwaps), 255))
	a.Pools = make([]dex.PoolID, len(c.txSwaps))
	a.Contracts = make([]eth.Address, len(c.txSwaps))
	for i := range c.txSwaps {
		a.Pools[i] = c.txSwaps[i].pool.ID
		a.Contracts[i] = c.txSwaps[i].swap.Contract
	}
	return a, true
}

func (c *Classifier) addFlow(token eth.Address, credit, debit *uint256.Int) bool {
	var f *flow
	for i := range c.flows { // a transaction touches only a handful of tokens
		if c.flows[i].token == token {
			f = &c.flows[i]
			break
		}
	}
	if f == nil {
		c.flows = append(c.flows, flow{token: token})
		f = &c.flows[len(c.flows)-1]
	}
	if _, overflow := f.credit.AddOverflow(&f.credit, credit); overflow {
		return false
	}
	_, overflow := f.debit.AddOverflow(&f.debit, debit)
	return !overflow
}

func (c *Classifier) newArb(blk *BlockInfo, r *eth.Receipt, st Status) Arb {
	a := Arb{
		Block: blk.Number, BlockHash: blk.Hash, Timestamp: blk.Timestamp, Regime: blk.Regime,
		TxIndex: uint32(r.TxIndex), TxHash: r.TxHash, From: r.From, To: r.To, Status: st,
		GasUsed: uint64(r.GasUsed), GasUsedForL1: uint64(r.GasUsedForL1),
		EffectiveGasPrice: uint64(r.EffectiveGasPrice), BaseFee: blk.BaseFee,
		Timeboosted: r.Timeboosted,
	}
	if a.EffectiveGasPrice > blk.BaseFee {
		a.PriorityFeePerGas = a.EffectiveGasPrice - blk.BaseFee
	}
	a.CostETH = float64(a.GasUsed) * float64(a.EffectiveGasPrice) / 1e18
	return a
}
