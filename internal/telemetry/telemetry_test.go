package telemetry

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/huynhnhatkhanh/mevlens/internal/classify"
	"github.com/huynhnhatkhanh/mevlens/internal/observe"
)

func TestMetricsRecordPipelineEvents(t *testing.T) {
	var slow []string
	m := NewMetrics(time.Second, func(r string) { slow = append(slow, r) })
	m.now = func() time.Time { return time.Unix(1_000_010, 0) }

	m.Processed(&classify.Result{Block: classify.BlockInfo{
		Number: 42, Timestamp: 1_000_000, Swaps: 3, Arbs: 2, RevertedArbs: 1, Regime: classify.RegimePGA,
	}}, 5)
	m.Fetched(2, 1500*time.Millisecond)
	m.Flushed(&observe.Batch{Blocks: make([]classify.BlockInfo, 4)}, 10*time.Millisecond, nil)
	m.ObserveRoundTrip("drpc", "batch", 2, 50*time.Millisecond, nil)
	m.ResolveSkipped(40)
	m.ResolveSkipped(41)

	checks := map[string]float64{
		"processed_block":              42,
		"head_lag_blocks":              5,
		"processed_block_age_seconds":  10,
		"swaps_total":                  3,
		"block_fetch_retries_total":    2,
		"resolve_skipped_blocks_total": 2,
		"resolve_skipped_last_block":   41,
	}
	for name, want := range checks {
		if got := gather(t, m, "mevlens_"+name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if got := testutil.ToFloat64(m.arbs.WithLabelValues("success", "pga")); got != 2 {
		t.Errorf("success arbs = %v", got)
	}
	if got := testutil.ToFloat64(m.arbs.WithLabelValues("reverted", "pga")); got != 1 {
		t.Errorf("reverted arbs = %v", got)
	}
	if got := testutil.ToFloat64(m.flushRows.WithLabelValues("blocks")); got != 4 {
		t.Errorf("flushed blocks = %v", got)
	}
	if len(slow) != 1 || !strings.Contains(slow[0], "block fetch") {
		t.Errorf("slow callbacks = %v", slow)
	}
}

func gather(t *testing.T, m *Metrics, name string) float64 {
	t.Helper()
	mfs, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			mm := mf.GetMetric()[0]
			switch {
			case mm.Gauge != nil:
				return mm.Gauge.GetValue()
			case mm.Counter != nil:
				return mm.Counter.GetValue()
			}
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

func TestServerEndpoints(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	m := NewMetrics(0, nil)
	log := slog.New(slog.DiscardHandler)
	fr, err := StartFlightRecorder(t.TempDir(), time.Minute, log)
	if err != nil {
		t.Fatal(err)
	}
	defer fr.Stop()
	srv := NewServer(addr, m.Registry, func() error { return nil }, fr, log)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	base := "http://" + addr
	var resp *http.Response
	for range 50 { // wait for the listener
		if resp, err = get(base + "/healthz"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	for path, want := range map[string]string{
		"/metrics":                           "mevlens_blocks_processed_total",
		"/readyz":                            "ready",
		"/debug/pprof/":                      "goroutineleak",
		"/debug/pprof/goroutineleak?debug=1": "goroutineleak profile",
	} {
		resp, err := get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(want)) {
			t.Errorf("%s: status %d, body lacks %q", path, resp.StatusCode, want)
		}
	}
	resp, err = get(base + "/debug/flightrecorder")
	if err != nil {
		t.Fatal(err)
	}
	trace, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(trace) == 0 {
		t.Error("flight recorder returned an empty trace")
	}

	httpClient.CloseIdleConnections()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

func get(url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return httpClient.Do(req)
}

func TestNewLogger(t *testing.T) {
	var buf bytes.Buffer
	log, err := NewLogger(&buf, "debug", "json")
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("hello", "k", 1)
	if !strings.Contains(buf.String(), `"msg":"hello"`) {
		t.Fatalf("log output = %s", buf.String())
	}
	if _, err := NewLogger(&buf, "loud", "json"); err == nil {
		t.Fatal("invalid level accepted")
	}
}
