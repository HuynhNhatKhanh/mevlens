package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"runtime/trace"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewLogger builds a structured logger writing to w.
func NewLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("telemetry: log level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if strings.EqualFold(format, "text") {
		return slog.New(slog.NewTextHandler(w, opts)), nil
	}
	return slog.New(slog.NewJSONHandler(w, opts)), nil
}

// Server is the admin HTTP server. It must listen on a private interface: pprof
// and trace snapshots expose internals.
type Server struct {
	srv *http.Server
	log *slog.Logger
}

// NewServer exposes /metrics, /healthz, /readyz, /debug/pprof/* and, when fr is
// non-nil, /debug/flightrecorder (a trace of the last few seconds).
func NewServer(addr string, reg *prometheus.Registry, ready func() error, fr *FlightRecorder, log *slog.Logger) *Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if err := ready(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ready\n")
	})
	// pprof.Index serves every runtime profile, including goroutineleak (Go 1.27).
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	if fr != nil {
		mux.HandleFunc("GET /debug/flightrecorder", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="flight.trace"`)
			if _, err := fr.WriteTo(w); err != nil {
				log.Warn("flight recorder snapshot failed", "err", err)
			}
		})
	}
	return &Server{
		srv: &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second},
		log: log,
	}
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("telemetry: listen %s: %w", s.srv.Addr, err)
	}
	s.log.Info("admin server listening", "addr", ln.Addr().String())
	errc := make(chan error, 1)
	go func() { errc <- s.srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		// Graceful first, then forceful: Shutdown treats connections that were
		// opened but never used (clients dial spare connections) as active for up
		// to 5s, so a timeout here is expected and must not fail the process.
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			s.log.Warn("admin server: graceful shutdown timed out, closing connections", "err", err)
			_ = s.srv.Close()
		}
		if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// FlightRecorder keeps the last few seconds of execution trace in memory and dumps
// it when something is slow, so latency incidents can be inspected with
// `go tool trace` after the fact instead of being reproduced.
type FlightRecorder struct {
	fr       *trace.FlightRecorder
	dir      string
	minEvery time.Duration
	log      *slog.Logger

	mu   sync.Mutex
	last time.Time
}

// StartFlightRecorder starts recording. Snapshots go to dir, at most one per minEvery.
func StartFlightRecorder(dir string, minEvery time.Duration, log *slog.Logger) (*FlightRecorder, error) {
	fr := trace.NewFlightRecorder(trace.FlightRecorderConfig{MinAge: 10 * time.Second, MaxBytes: 32 << 20})
	if err := fr.Start(); err != nil {
		return nil, fmt.Errorf("telemetry: start flight recorder: %w", err)
	}
	return &FlightRecorder{fr: fr, dir: dir, minEvery: minEvery, log: log}, nil
}

// WriteTo writes the current window.
func (f *FlightRecorder) WriteTo(w io.Writer) (int64, error) { return f.fr.WriteTo(w) }

// Snapshot dumps the window to a file unless one was written recently. It is safe
// to call from hot paths: the write happens on a separate goroutine.
func (f *FlightRecorder) Snapshot(reason string) {
	f.mu.Lock()
	if time.Since(f.last) < f.minEvery {
		f.mu.Unlock()
		return
	}
	f.last = time.Now()
	f.mu.Unlock()

	go func() {
		name := filepath.Join(f.dir, "mevlens-"+time.Now().UTC().Format("20060102T150405Z")+".trace")
		file, err := os.Create(name)
		if err != nil {
			f.log.Warn("flight recorder: create file", "err", err)
			return
		}
		defer file.Close()
		if _, err := f.fr.WriteTo(file); err != nil {
			f.log.Warn("flight recorder: write", "err", err)
			return
		}
		f.log.Warn("slow operation, execution trace saved", "reason", reason, "file", name)
	}()
}

// Stop stops recording.
func (f *FlightRecorder) Stop() { f.fr.Stop() }
