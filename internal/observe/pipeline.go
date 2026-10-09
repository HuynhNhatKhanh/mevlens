// Package observe runs the observatory pipeline:
//
//	numbers ──▶ ordered fetch (N in flight) ──▶ single-writer processor ──▶ sink
//
// Fetching is concurrent, but blocks are delivered strictly in order to a single
// processing goroutine that owns all mutable state (pool registry, classifier,
// pricing). The sink receives batches that end with a checkpoint, so a restart
// resumes exactly after the last durable block; writes are idempotent, so the
// at-least-once delivery of the final partial batch is harmless.
package observe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/huynhnhatkhanh/mevlens/internal/classify"
	"github.com/huynhnhatkhanh/mevlens/internal/dex"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
)

// ErrUnavailable marks a temporary inability to serve a block: not produced or
// not propagated yet, rate limited, or an inconsistent answer from a load
// balancer. Sources wrap such errors with it; the pipeline retries them
// indefinitely, while any other error stops the pipeline after a few attempts.
var ErrUnavailable = errors.New("observe: block temporarily unavailable")

// BlockSource provides blocks. Implementations translate their own transient
// failures into errors wrapping ErrUnavailable, which keeps this package free of
// any transport-specific error types.
type BlockSource interface {
	BlockNumber(ctx context.Context) (uint64, error)
	Block(ctx context.Context, n uint64) (*eth.Block, error)
}

// Resolver resolves pool metadata. *registry.Registry implements it.
type Resolver interface {
	Resolve(ctx context.Context, block uint64, hash eth.Hash, cands []dex.Candidate) error
	DrainNew() []dex.Pool
}

// Sink persists batches. Write must be idempotent: the same rows may be written
// more than once after a crash, a reorg rewind, or a failed Write, which the
// pipeline retries with the same batch until it succeeds or the run stops.
type Sink interface {
	Write(ctx context.Context, b *Batch) error
}

// Rewinder is implemented by sinks that can delete rows from an abandoned fork.
// Follow calls it before re-ingesting from the rewind point.
type Rewinder interface {
	Rewind(ctx context.Context, checkpoint string, from uint64) error
}

// Checkpoint marks the last block whose rows are durable.
type Checkpoint struct {
	Name  string
	Block uint64
	Hash  eth.Hash
}

// Batch is a unit of persistence. Rows are ordered by block, then position.
type Batch struct {
	Blocks     []classify.BlockInfo
	Swaps      []classify.SwapRow
	Arbs       []classify.Arb
	Pools      []dex.Pool
	Checkpoint Checkpoint
}

// Len returns the number of blocks in the batch.
func (b *Batch) Len() int { return len(b.Blocks) }

func (b *Batch) add(res *classify.Result, pools []dex.Pool) {
	b.Blocks = append(b.Blocks, res.Block)
	b.Swaps = append(b.Swaps, res.Swaps...)
	b.Arbs = append(b.Arbs, res.Arbs...)
	b.Pools = append(b.Pools, pools...)
}

// Config tunes the pipeline. Zero values select sensible defaults.
type Config struct {
	CheckpointName string
	FetchWorkers   int           // blocks fetched concurrently (default 4)
	PollInterval   time.Duration // head polling in follow mode (default 250ms)
	FlushBlocks    int           // flush after this many blocks (default 100)
	FlushInterval  time.Duration // and at least this often (default 2s)
	ReorgDepth     uint64        // blocks to rewind on reorg (default 64)
	RetryBase      time.Duration // default 200ms
	RetryMax       time.Duration // default 5s
	// ResolveAttempts bounds the pool resolution retries of one block (default
	// 10). Past it the block is processed with its unresolved pools skipped, so
	// one contract the resolver keeps failing on cannot halt ingestion.
	ResolveAttempts int
}

func (c *Config) setDefaults() {
	if c.CheckpointName == "" {
		c.CheckpointName = "follow"
	}
	if c.FetchWorkers <= 0 {
		c.FetchWorkers = 4
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 250 * time.Millisecond
	}
	if c.FlushBlocks <= 0 {
		c.FlushBlocks = 100
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 2 * time.Second
	}
	if c.ReorgDepth == 0 {
		c.ReorgDepth = 64
	}
	if c.RetryBase <= 0 {
		c.RetryBase = 200 * time.Millisecond
	}
	if c.RetryMax <= 0 {
		c.RetryMax = 5 * time.Second
	}
	if c.ResolveAttempts <= 0 {
		c.ResolveAttempts = 10
	}
}

// ReorgError reports that block Block does not extend the previously processed block.
type ReorgError struct {
	Block    uint64
	Expected eth.Hash
	Got      eth.Hash
}

func (e *ReorgError) Error() string {
	return fmt.Sprintf("observe: reorg at block %d: parent %s, expected %s", e.Block, e.Got, e.Expected)
}

// Pipeline wires a source, the deterministic core and a sink.
type Pipeline struct {
	cfg  Config
	src  BlockSource
	reg  Resolver
	cl   *classify.Classifier
	sink Sink
	m    Metrics
	log  *slog.Logger
	// carry holds the pools of a batch dropped on reorg, for the next run's
	// first batch. Only process touches it, and runs never overlap.
	carry []dex.Pool
}

// New builds a Pipeline. m and log may be nil.
func New(cfg Config, src BlockSource, reg Resolver, cl *classify.Classifier, sink Sink, m Metrics, log *slog.Logger) *Pipeline {
	cfg.setDefaults()
	if m == nil {
		m = NopMetrics{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Pipeline{cfg: cfg, src: src, reg: reg, cl: cl, sink: sink, m: m, log: log}
}

// Backfill processes blocks [from, to] once. parent, if non-nil, is the expected
// parent hash of block from.
func (p *Pipeline) Backfill(ctx context.Context, from, to uint64, parent *eth.Hash) error {
	if from > to {
		return fmt.Errorf("observe: empty range [%d, %d]", from, to)
	}
	return p.run(ctx, rangeNumbers(from, to), parent)
}

// Follow processes blocks from start onwards, tracking the chain head until ctx
// is cancelled. On a reorg it rewinds ReorgDepth blocks and continues.
func (p *Pipeline) Follow(ctx context.Context, start uint64, parent *eth.Hash) error {
	for {
		err := p.run(ctx, p.followNumbers(start), parent)
		var re *ReorgError
		if !errors.As(err, &re) {
			return err
		}
		start = re.Block - min(p.cfg.ReorgDepth, re.Block)
		parent = nil // rewinding deeper than any plausible reorg; re-ingestion is idempotent
		if rw, ok := p.sink.(Rewinder); ok {
			if err := rw.Rewind(ctx, p.cfg.CheckpointName, start); err != nil {
				return fmt.Errorf("observe: rewind to %d: %w", start, err)
			}
		}
		p.m.Reorg(re.Block)
		p.log.Warn("reorg detected, rewinding", "block", re.Block, "restart", start, "expected", re.Expected, "got", re.Got)
	}
}

type numberSource func(ctx context.Context, out chan<- uint64) error

func rangeNumbers(from, to uint64) numberSource {
	return func(ctx context.Context, out chan<- uint64) error {
		for n := from; n <= to; n++ {
			select {
			case out <- n:
			case <-ctx.Done():
				return ctx.Err()
			}
			if n == to { // avoid overflow when to == MaxUint64
				break
			}
		}
		return nil
	}
}

func (p *Pipeline) followNumbers(start uint64) numberSource {
	return func(ctx context.Context, out chan<- uint64) error {
		next := start
		for attempt := 0; ; {
			head, err := p.src.BlockNumber(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				p.log.Warn("head poll failed", "err", err)
				if err := sleep(ctx, p.backoff(attempt)); err != nil {
					return err
				}
				attempt++
				continue
			}
			attempt = 0
			p.m.Head(head)
			for ; next <= head; next++ {
				select {
				case out <- next:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if err := sleep(ctx, p.cfg.PollInterval); err != nil {
				return err
			}
		}
	}
}

func (p *Pipeline) run(ctx context.Context, numbers numberSource, parent *eth.Hash) error {
	g, ctx := errgroup.WithContext(ctx)
	nums := make(chan uint64)
	blocks := make(chan *eth.Block)
	g.Go(func() error {
		defer close(nums)
		return numbers(ctx, nums)
	})
	g.Go(func() error {
		defer close(blocks)
		return p.fetchOrdered(ctx, nums, blocks)
	})
	g.Go(func() error { return p.process(ctx, blocks, parent) })
	return g.Wait()
}

type fetched struct {
	block *eth.Block
	err   error
}

// fetchOrdered fetches up to FetchWorkers blocks concurrently and emits them in
// the order their numbers arrived. Each number gets a one-slot result channel that
// is queued in order; the bounded queue caps the number of blocks in flight.
func (p *Pipeline) fetchOrdered(ctx context.Context, nums <-chan uint64, out chan<- *eth.Block) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait() // runs after cancel: every fetch goroutine exits before we return
	defer cancel()

	slots := make(chan chan fetched, p.cfg.FetchWorkers)
	wg.Go(func() {
		defer close(slots)
		for {
			var n uint64
			select {
			case <-ctx.Done():
				return
			case v, ok := <-nums:
				if !ok {
					return
				}
				n = v
			}
			slot := make(chan fetched, 1)
			select {
			case slots <- slot:
			case <-ctx.Done():
				return
			}
			wg.Go(func() {
				b, err := p.fetchBlock(ctx, n)
				slot <- fetched{b, err}
			})
		}
	})

	for slot := range slots {
		var f fetched
		select {
		case f = <-slot:
		case <-ctx.Done():
			return ctx.Err()
		}
		if f.err != nil {
			return f.err
		}
		select {
		case out <- f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ctx.Err()
}

// fetchBlock retries until the block is available. Near the head a load-balanced
// provider may not have the block yet (not found) or answer inconsistently; both
// resolve themselves, so they are retried indefinitely. Other errors get a few
// attempts before the pipeline stops.
func (p *Pipeline) fetchBlock(ctx context.Context, n uint64) (*eth.Block, error) {
	const maxPermanent = 3
	start := time.Now()
	permanent := 0
	for attempt := 0; ; attempt++ {
		b, err := p.src.Block(ctx, n)
		if err == nil {
			p.m.Fetched(attempt, time.Since(start))
			return b, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !errors.Is(err, ErrUnavailable) {
			if permanent++; permanent >= maxPermanent {
				return nil, fmt.Errorf("observe: fetch block %d: %w", n, err)
			}
		}
		if attempt > 0 && attempt%20 == 0 {
			p.log.Warn("block still unavailable", "block", n, "attempts", attempt, "err", err)
		}
		if err := sleep(ctx, p.backoff(attempt)); err != nil {
			return nil, err
		}
	}
}

// process is the single writer: it owns the registry, classifier and batch.
func (p *Pipeline) process(ctx context.Context, blocks <-chan *eth.Block, parent *eth.Hash) error {
	var (
		batch      Batch
		lastHash   eth.Hash
		haveParent = parent != nil
	)
	if haveParent {
		lastHash = *parent
	}
	batch.Pools, p.carry = p.carry, nil
	// flush retries until the batch is written or ctx is done: a ClickHouse
	// restart or a transient TOO_MANY_PARTS must not stop ingestion for good.
	// The batch is kept intact between attempts and Write is idempotent, so
	// re-sending a partly written batch is harmless. It fails only once ctx is
	// done.
	flush := func(ctx context.Context) error {
		if batch.Len() == 0 {
			return nil
		}
		for attempt := 0; ; attempt++ {
			start := time.Now()
			err := p.sink.Write(ctx, &batch)
			p.m.Flushed(&batch, time.Since(start), err)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return fmt.Errorf("observe: flush through block %d: %w", batch.Checkpoint.Block, err)
			}
			p.log.Warn("flush failed, retrying", "checkpoint", batch.Checkpoint.Block, "attempt", attempt, "err", err)
			if sleep(ctx, p.backoff(attempt)) != nil {
				return fmt.Errorf("observe: flush through block %d: %w", batch.Checkpoint.Block, err)
			}
		}
		p.log.Debug("flushed", "blocks", batch.Len(), "swaps", len(batch.Swaps), "arbs", len(batch.Arbs), "checkpoint", batch.Checkpoint.Block)
		batch = Batch{}
		return nil
	}
	// stop ends the run once ctx is done. It persists what has been processed
	// using a detached context, whose timeout also bounds flush's retries: a
	// sink that stays down cannot hold up shutdown forever.
	stop := func() error {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := flush(fctx); err != nil {
			// Logged because the errgroup usually reports the cancellation
			// that caused the stop, not this error.
			p.log.Error("final flush failed, unflushed blocks are processed again on restart", "err", err)
			return err
		}
		return ctx.Err()
	}

	ticker := time.NewTicker(p.cfg.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return stop()
		case <-ticker.C:
			if flush(ctx) != nil { // ctx is done
				return stop()
			}
		case b, ok := <-blocks:
			if !ok {
				// The input ended: a backfill completed, or the run is stopping.
				if ctx.Err() == nil && flush(ctx) == nil {
					return nil
				}
				return stop()
			}
			n := uint64(b.Header.Number)
			if haveParent && b.Header.ParentHash != lastHash {
				// The unflushed tail may belong to the abandoned fork: drop it,
				// but keep its pools. The registry keeps them cached across the
				// rewind and never drains them again, so dropping them would
				// leave them unpersisted until a restart.
				p.carry = batch.Pools
				return &ReorgError{Block: n, Expected: lastHash, Got: b.Header.ParentHash}
			}
			if p.resolve(ctx, n, b) != nil { // ctx is done
				// Shutting down mid-resolution: the blocks before n are complete
				// and their pools already drained, so persist them like any stop.
				return stop()
			}
			res := p.cl.Classify(b)
			batch.add(&res, p.reg.DrainNew())
			batch.Checkpoint = Checkpoint{Name: p.cfg.CheckpointName, Block: n, Hash: b.Header.Hash}
			lastHash, haveParent = b.Header.Hash, true

			p.m.Processed(&res)
			if batch.Len() >= p.cfg.FlushBlocks && flush(ctx) != nil { // ctx is done
				return stop()
			}
		}
	}
}

// resolve resolves the block's pool candidates, retrying transient failures. When
// the attempts run out, the block is processed anyway: unresolved pools are not
// cached, so their swaps are skipped here and resolution is tried again the next
// time they appear. It fails only once ctx is done.
func (p *Pipeline) resolve(ctx context.Context, n uint64, b *eth.Block) error {
	cands := p.cl.Candidates(b)
	for attempt := 0; ; attempt++ {
		err := p.reg.Resolve(ctx, n, b.Header.Hash, cands)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt+1 >= p.cfg.ResolveAttempts {
			p.log.Error("pool resolution failed, skipping unresolved pools", "block", n, "attempts", attempt+1, "err", err)
			p.m.ResolveSkipped(n)
			return nil
		}
		p.log.Warn("pool resolution failed, retrying", "block", n, "attempt", attempt, "err", err)
		if err := sleep(ctx, p.backoff(attempt)); err != nil {
			return err
		}
	}
}

func (p *Pipeline) backoff(attempt int) time.Duration {
	ceiling := p.cfg.RetryBase << min(attempt, 16)
	if ceiling <= 0 || ceiling > p.cfg.RetryMax {
		ceiling = p.cfg.RetryMax
	}
	return ceiling/2 + time.Duration(rand.Int64N(int64(ceiling/2)+1)) // equal jitter
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
