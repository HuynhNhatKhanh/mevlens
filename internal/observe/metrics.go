package observe

import (
	"time"

	"github.com/huynhnhatkhanh/mevlens/internal/classify"
)

// Metrics receives pipeline events. Implementations must be cheap; Processed is
// called on the hot processing goroutine for every block.
type Metrics interface {
	Head(head uint64)
	Fetched(retries int, d time.Duration)
	Processed(res *classify.Result, lagBlocks uint64)
	Flushed(b *Batch, d time.Duration, err error)
	Reorg(block uint64)
}

// NopMetrics discards all events.
type NopMetrics struct{}

func (NopMetrics) Head(uint64)                          {}
func (NopMetrics) Fetched(int, time.Duration)           {}
func (NopMetrics) Processed(*classify.Result, uint64)   {}
func (NopMetrics) Flushed(*Batch, time.Duration, error) {}
func (NopMetrics) Reorg(uint64)                         {}
