// Package telemetry provides structured logging, Prometheus metrics, the admin
// HTTP server (metrics, health, pprof) and an execution-trace flight recorder.
package telemetry

import (
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/huynhnhatkhanh/mevlens/internal/classify"
	"github.com/huynhnhatkhanh/mevlens/internal/observe"
)

const ns = "mevlens"

// Metrics implements observe.Metrics and rpc.Observer on a private registry.
type Metrics struct {
	Registry *prometheus.Registry

	head          prometheus.Gauge
	processed     prometheus.Gauge
	blocks        prometheus.Counter
	swaps         prometheus.Counter
	arbs          *prometheus.CounterVec
	fetchDuration prometheus.Histogram
	fetchRetries  prometheus.Counter
	flushDuration prometheus.Histogram
	flushRows     *prometheus.CounterVec
	flushErrors   prometheus.Counter
	reorgs        prometheus.Counter
	skipped       prometheus.Counter
	lastSkipped   prometheus.Gauge
	rpcRequests   *prometheus.CounterVec
	rpcDuration   *prometheus.HistogramVec
	rpcBatchSize  prometheus.Histogram
	arbsByRegime  map[classify.Regime][2]prometheus.Counter // [success, reverted], resolved once
	slowThreshold time.Duration
	onSlow        func(reason string)
	now           func() time.Time

	// Head lag and block age are computed at scrape time from these rather than
	// set per block: a pipeline stalled in fetching or resolution processes
	// nothing, so gauges set in Processed would freeze at their last healthy
	// values (lag 0, age under a second) and dashboards would stay green.
	chainHead atomic.Uint64
	last      atomic.Pointer[processedBlock] // nil until the first block
}

type processedBlock struct {
	number uint64
	time   time.Time
}

// NewMetrics registers all metrics. onSlow (optional) is invoked when a fetch or
// flush exceeds slowThreshold, e.g. to snapshot the flight recorder.
func NewMetrics(slowThreshold time.Duration, onSlow func(reason string)) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := factory{reg}
	latency := []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}

	m := &Metrics{
		Registry:      reg,
		head:          f.gauge("chain_head_block", "Latest chain head observed in follow mode."),
		processed:     f.gauge("processed_block", "Latest block processed by the pipeline."),
		blocks:        f.counter("blocks_processed_total", "Blocks processed."),
		swaps:         f.counter("swaps_total", "Swaps on canonical pools."),
		arbs:          f.counterVec("arbitrages_total", "Arbitrage transactions observed.", "status", "regime"),
		fetchDuration: f.histogram("block_fetch_duration_seconds", "Time to fetch a block, including retries.", latency),
		fetchRetries:  f.counter("block_fetch_retries_total", "Block fetch retries."),
		flushDuration: f.histogram("flush_duration_seconds", "Time to persist a batch.", latency),
		flushRows:     f.counterVec("flushed_rows_total", "Rows persisted.", "table"),
		flushErrors:   f.counter("flush_errors_total", "Failed batch writes."),
		reorgs:        f.counter("reorgs_total", "Chain reorganisations detected."),
		skipped:       f.counter("resolve_skipped_blocks_total", "Blocks processed with unresolved pools skipped (possibly incomplete)."),
		lastSkipped:   f.gauge("resolve_skipped_last_block", "Latest block processed with unresolved pools skipped; re-ingest it to complete it."),
		rpcRequests:   f.counterVec("rpc_round_trips_total", "JSON-RPC HTTP round trips.", "endpoint", "method", "result"),
		rpcDuration:   f.histogramVec("rpc_round_trip_duration_seconds", "JSON-RPC round-trip latency.", latency, "endpoint"),
		rpcBatchSize:  f.histogram("rpc_batch_size", "Requests per JSON-RPC round trip.", prometheus.ExponentialBuckets(1, 2, 8)),
		slowThreshold: slowThreshold,
		onSlow:        onSlow,
		now:           time.Now,
	}
	f.gaugeFunc("head_lag_blocks", "Blocks between the chain head and the last processed block.", m.lagBlocks)
	f.gaugeFunc("processed_block_age_seconds", "Wall-clock age of the last processed block.", m.blockAge)
	m.arbsByRegime = make(map[classify.Regime][2]prometheus.Counter)
	for _, r := range []classify.Regime{classify.RegimeUnknown, classify.RegimeFCFS, classify.RegimeTimeboost, classify.RegimePGA} {
		m.arbsByRegime[r] = [2]prometheus.Counter{
			m.arbs.WithLabelValues(classify.StatusSuccess.String(), r.String()),
			m.arbs.WithLabelValues(classify.StatusReverted.String(), r.String()),
		}
	}
	return m
}

// Head implements observe.Metrics.
func (m *Metrics) Head(head uint64) {
	m.chainHead.Store(head)
	m.head.Set(float64(head))
}

// Fetched implements observe.Metrics.
func (m *Metrics) Fetched(retries int, d time.Duration) {
	m.fetchDuration.Observe(d.Seconds())
	m.fetchRetries.Add(float64(retries))
	m.maybeSlow("block fetch", d)
}

// Processed implements observe.Metrics.
func (m *Metrics) Processed(res *classify.Result) {
	b := &res.Block
	m.last.Store(&processedBlock{number: b.Number, time: time.Unix(int64(b.Timestamp), 0)})
	m.processed.Set(float64(b.Number))
	m.blocks.Inc()
	m.swaps.Add(float64(b.Swaps))
	c := m.arbsByRegime[b.Regime]
	c[0].Add(float64(b.Arbs))
	c[1].Add(float64(b.RevertedArbs))
}

// lagBlocks is 0 until both the head and a processed block are known: backfill
// never polls the head, and before the first block there is nothing to lag.
func (m *Metrics) lagBlocks() float64 {
	head, last := m.chainHead.Load(), m.last.Load()
	if last == nil || head <= last.number {
		return 0
	}
	return float64(head - last.number)
}

// blockAge keeps growing while no block is processed, which is what exposes a
// stall. It is 0 before the first block.
func (m *Metrics) blockAge() float64 {
	last := m.last.Load()
	if last == nil {
		return 0
	}
	return m.now().Sub(last.time).Seconds()
}

// Flushed implements observe.Metrics.
func (m *Metrics) Flushed(b *observe.Batch, d time.Duration, err error) {
	m.flushDuration.Observe(d.Seconds())
	if err != nil {
		m.flushErrors.Inc()
		return
	}
	m.flushRows.WithLabelValues("blocks").Add(float64(len(b.Blocks)))
	m.flushRows.WithLabelValues("swaps").Add(float64(len(b.Swaps)))
	m.flushRows.WithLabelValues("arbitrages").Add(float64(len(b.Arbs)))
	m.flushRows.WithLabelValues("pools").Add(float64(len(b.Pools)))
	m.maybeSlow("flush", d)
}

// Reorg implements observe.Metrics.
func (m *Metrics) Reorg(uint64) { m.reorgs.Inc() }

// ResolveSkipped implements observe.Metrics.
func (m *Metrics) ResolveSkipped(block uint64) {
	m.skipped.Inc()
	m.lastSkipped.Set(float64(block))
}

// ObserveRoundTrip implements rpc.Observer.
func (m *Metrics) ObserveRoundTrip(endpoint, method string, batch int, d time.Duration, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.rpcRequests.WithLabelValues(endpoint, method, result).Inc()
	m.rpcDuration.WithLabelValues(endpoint).Observe(d.Seconds())
	m.rpcBatchSize.Observe(float64(batch))
}

func (m *Metrics) maybeSlow(what string, d time.Duration) {
	if m.onSlow != nil && m.slowThreshold > 0 && d > m.slowThreshold {
		m.onSlow(what + " took " + d.Round(time.Millisecond).String())
	}
}

type factory struct{ reg *prometheus.Registry }

func (f factory) gauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: name, Help: help})
	f.reg.MustRegister(g)
	return g
}

func (f factory) gaugeFunc(name, help string, fn func() float64) {
	f.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Namespace: ns, Name: name, Help: help}, fn))
}

func (f factory) counter(name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: name, Help: help})
	f.reg.MustRegister(c)
	return c
}

func (f factory) counterVec(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: name, Help: help}, labels)
	f.reg.MustRegister(c)
	return c
}

func (f factory) histogram(name, help string, buckets []float64) prometheus.Histogram {
	h := prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: ns, Name: name, Help: help, Buckets: buckets})
	f.reg.MustRegister(h)
	return h
}

func (f factory) histogramVec(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: name, Help: help, Buckets: buckets}, labels)
	f.reg.MustRegister(h)
	return h
}
