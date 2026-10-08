package observe

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/huynhnhatkhanh/mevlens/internal/classify"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
	"github.com/huynhnhatkhanh/mevlens/internal/pricing"
	"github.com/huynhnhatkhanh/mevlens/internal/registry"
)

// fakeChain serves a linear chain of empty blocks with simulated latency. Hashes
// encode (fork, number) so a fork can be switched in mid-test.
type fakeChain struct {
	mu       sync.Mutex
	head     uint64
	fork     map[uint64]byte // block number -> fork id (default 0)
	notFound map[uint64]int  // remaining ErrNotFound answers per block
	latency  func(n uint64) time.Duration
}

func hashOf(fork byte, n uint64) eth.Hash {
	h := eth.Uint64Word(n)
	h[0] = 0xf0 | fork
	return h
}

func (c *fakeChain) BlockNumber(context.Context) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.head, nil
}

func (c *fakeChain) Block(ctx context.Context, n uint64) (*eth.Block, error) {
	if c.latency != nil {
		time.Sleep(c.latency(n))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.notFound[n] > 0 {
		c.notFound[n]--
		return nil, ErrUnavailable
	}
	if n > c.head {
		return nil, ErrUnavailable
	}
	return &eth.Block{Header: eth.Header{
		Number:     eth.Quantity(n),
		Hash:       hashOf(c.fork[n], n),
		ParentHash: hashOf(c.fork[n-1], n-1),
	}}, nil
}

func (c *fakeChain) setFork(from uint64, id byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for n := from; n <= c.head; n++ {
		c.fork[n] = id
	}
}

type memSink struct {
	mu      sync.Mutex
	batches []Batch
	rewinds []uint64
	fail    error
}

func (s *memSink) Rewind(_ context.Context, _ string, from uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rewinds = append(s.rewinds, from)
	return nil
}

func (s *memSink) Write(_ context.Context, b *Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.batches = append(s.batches, *b)
	return nil
}

func (s *memSink) blocks() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uint64
	for _, b := range s.batches {
		for _, bl := range b.Blocks {
			out = append(out, bl.Number)
		}
	}
	return out
}

func (s *memSink) lastCheckpoint() Checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.batches) == 0 {
		return Checkpoint{}
	}
	return s.batches[len(s.batches)-1].Checkpoint
}

func newPipeline(t *testing.T, cfg Config, src BlockSource, sink Sink) *Pipeline {
	t.Helper()
	reg, err := registry.New(nil, nil, eth.Address{})
	if err != nil {
		t.Fatal(err)
	}
	o, err := pricing.New(pricing.Config{WETH: eth.MustAddress("0x82af49447d8a07e3bd95bd0d56f35241523fbab1")})
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, src, reg, classify.New(reg, o), sink, nil, nil)
}

func seq(from, to uint64) []uint64 {
	var out []uint64
	for n := from; n <= to; n++ {
		out = append(out, n)
	}
	return out
}

func TestBackfillDeliversInOrderDespiteRandomLatency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		chain := &fakeChain{head: 200, fork: map[uint64]byte{}, latency: func(n uint64) time.Duration {
			return time.Duration((n*7919)%50) * time.Millisecond // completion order ≠ request order
		}}
		sink := &memSink{}
		p := newPipeline(t, Config{FetchWorkers: 8, FlushBlocks: 25}, chain, sink)

		if err := p.Backfill(t.Context(), 1, 200, nil); err != nil {
			t.Fatal(err)
		}
		if got := sink.blocks(); !slices.Equal(got, seq(1, 200)) {
			t.Fatalf("blocks out of order or missing: %v", got)
		}
		if cp := sink.lastCheckpoint(); cp.Block != 200 || cp.Hash != hashOf(0, 200) || cp.Name != "follow" {
			t.Fatalf("checkpoint = %+v", cp)
		}
		if len(sink.batches) != 8 {
			t.Fatalf("batches = %d, want 200/25 = 8", len(sink.batches))
		}
	})
}

func TestBackfillRetriesNotFound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		chain := &fakeChain{head: 10, fork: map[uint64]byte{}, notFound: map[uint64]int{5: 3}}
		sink := &memSink{}
		if err := newPipeline(t, Config{}, chain, sink).Backfill(t.Context(), 1, 10, nil); err != nil {
			t.Fatal(err)
		}
		if got := sink.blocks(); !slices.Equal(got, seq(1, 10)) {
			t.Fatalf("blocks = %v", got)
		}
	})
}

func TestBackfillDetectsBrokenParent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		chain := &fakeChain{head: 10, fork: map[uint64]byte{}}
		parent := hashOf(9, 0) // wrong expected parent of block 1
		err := newPipeline(t, Config{}, chain, &memSink{}).Backfill(t.Context(), 1, 10, &parent)
		var re *ReorgError
		if !errors.As(err, &re) || re.Block != 1 {
			t.Fatalf("err = %v, want reorg at 1", err)
		}
	})
}

func TestFollowRewindsOnReorgAndConverges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		chain := &fakeChain{head: 100, fork: map[uint64]byte{}}
		sink := &memSink{}
		p := newPipeline(t, Config{FlushBlocks: 10, ReorgDepth: 5}, chain, sink)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- p.Follow(ctx, 1, nil) }()

		// Let the pipeline catch up, then replace blocks 98..100 with a fork and
		// extend the chain on top of it.
		synctest.Wait()
		chain.setFork(98, 1)
		chain.mu.Lock()
		chain.head = 120
		chain.fork[101] = 1
		for n := uint64(102); n <= 120; n++ {
			chain.fork[n] = 1
		}
		chain.mu.Unlock()

		time.Sleep(10 * time.Second)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Follow returned %v", err)
		}
		if len(sink.rewinds) != 1 || sink.rewinds[0] != 101-5 {
			t.Fatalf("rewinds = %v, want [96] (reorg at 101, depth 5)", sink.rewinds)
		}
		cp := sink.lastCheckpoint()
		if cp.Block != 120 || cp.Hash != hashOf(1, 120) {
			t.Fatalf("checkpoint = %+v, want block 120 on fork 1", cp)
		}
		// Every block of the new fork must have been written.
		seen := map[eth.Hash]bool{}
		for _, b := range sink.batches {
			for _, bl := range b.Blocks {
				seen[bl.Hash] = true
			}
		}
		for n := uint64(98); n <= 120; n++ {
			if !seen[hashOf(1, n)] {
				t.Fatalf("fork block %d never written", n)
			}
		}
	})
}

func TestFollowFlushesOnShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		chain := &fakeChain{head: 7, fork: map[uint64]byte{}}
		sink := &memSink{}
		p := newPipeline(t, Config{FlushBlocks: 1000, FlushInterval: time.Hour}, chain, sink)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- p.Follow(ctx, 1, nil) }()
		synctest.Wait() // caught up, idle in head polling
		cancel()
		<-done
		if cp := sink.lastCheckpoint(); cp.Block != 7 {
			t.Fatalf("shutdown lost processed blocks: checkpoint %+v", cp)
		}
	})
}

func TestSinkFailureStopsPipeline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		chain := &fakeChain{head: 50, fork: map[uint64]byte{}}
		boom := errors.New("clickhouse down")
		err := newPipeline(t, Config{FlushBlocks: 10}, chain, &memSink{fail: boom}).Backfill(t.Context(), 1, 50, nil)
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
}
