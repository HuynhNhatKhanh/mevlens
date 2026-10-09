package observe

import (
	"time"

	"github.com/huynhnhatkhanh/mevlens/internal/classify"
)

// Metrics receives pipeline events. Implementations must be cheap; Processed is
// called on the hot processing goroutine for every block.
type Metrics interface {
	// Head reports the chain head, polled in follow mode only. Implementations
	// derive the head lag from it and Processed.
	Head(head uint64)
	Fetched(retries int, d time.Duration)
	Processed(res *classify.Result)
	Flushed(b *Batch, d time.Duration, err error)
	Reorg(block uint64)
	// ResolveSkipped reports a block processed with unresolved pools skipped
	// after its resolution attempts ran out: its rows may be incomplete.
	ResolveSkipped(block uint64)
}

// NopMetrics discards all events.
type NopMetrics struct{}

func (NopMetrics) Head(uint64)                          {}
func (NopMetrics) Fetched(int, time.Duration)           {}
func (NopMetrics) Processed(*classify.Result)           {}
func (NopMetrics) Flushed(*Batch, time.Duration, error) {}
func (NopMetrics) Reorg(uint64)                         {}
func (NopMetrics) ResolveSkipped(uint64)                {}
