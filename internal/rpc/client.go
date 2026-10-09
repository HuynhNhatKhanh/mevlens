// Package rpc is a small, production-grade Ethereum JSON-RPC 2.0 client over HTTP.
//
// It is built for free-tier providers: every endpoint has its own token-bucket rate
// limiter, requests are batched, transient failures are retried with full-jitter
// backoff across endpoints (failover), and only the requests that actually failed
// inside a batch are retried.
package rpc

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// userAgent identifies the client to providers (some reject the Go default).
const userAgent = "mevlens-observatory/1 (+https://github.com/huynhnhatkhanh/mevlens)"

// EndpointConfig describes one JSON-RPC provider.
type EndpointConfig struct {
	Name  string  // label used in logs and metrics; never contains secrets
	URL   string  // may contain an API key; never logged
	RPS   float64 // sustained requests per second (0 = unlimited)
	Burst int     // token bucket size (defaults to max(1, ceil(RPS)))
}

// Config configures a Client.
type Config struct {
	Endpoints   []EndpointConfig
	MaxBatch    int           // max requests per HTTP call (default 50)
	Timeout     time.Duration // per HTTP attempt (default 15s)
	MaxAttempts int           // attempts per request across endpoints (default 5)
	BaseBackoff time.Duration // default 100ms
	MaxBackoff  time.Duration // default 5s
	MaxBodySize int64         // max response size in bytes (default 64 MiB)
}

// Observer receives one callback per HTTP round trip. Implementations must be
// safe for concurrent use and cheap.
type Observer interface {
	ObserveRoundTrip(endpoint string, method string, batch int, d time.Duration, err error)
}

// Request is one call inside a batch. Result must be a pointer (or nil to discard).
// After Batch returns, Err holds the per-request outcome.
type Request struct {
	Method string
	Params []any
	Result any
	Err    error
}

// Client is safe for concurrent use.
type Client struct {
	cfg       Config
	endpoints []*endpoint
	http      *http.Client
	obs       Observer
	nextID    atomic.Uint64
	rr        atomic.Uint64
}

type endpoint struct {
	name    string
	url     string
	limiter *rate.Limiter

	mu            sync.Mutex
	failures      int
	cooldownUntil time.Time
	unsupported   map[string]bool // methods this provider refuses (learned at runtime)
}

// Option customises a Client.
type Option func(*Client)

// WithObserver installs a metrics observer.
func WithObserver(o Observer) Option { return func(c *Client) { c.obs = o } }

// WithHTTPClient overrides the HTTP client (mainly for tests).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// New validates cfg and builds a Client.
func New(cfg Config, opts ...Option) (*Client, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, errors.New("rpc: at least one endpoint is required")
	}
	setDefaults(&cfg)
	c := &Client{
		cfg: cfg,
		http: &http.Client{Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		}},
	}
	for i, e := range cfg.Endpoints {
		if e.URL == "" {
			return nil, fmt.Errorf("rpc: endpoint %d has no URL", i)
		}
		name := e.Name
		if name == "" {
			name = fmt.Sprintf("endpoint-%d", i)
		}
		limit, burst := rate.Inf, e.Burst
		if e.RPS > 0 {
			limit = rate.Limit(e.RPS)
			if burst <= 0 {
				burst = max(1, int(e.RPS+0.999))
			}
		}
		c.endpoints = append(c.endpoints, &endpoint{name: name, url: e.URL, limiter: rate.NewLimiter(limit, max(1, burst))})
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

func setDefaults(cfg *Config) {
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 50
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = 100 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 5 * time.Second
	}
	if cfg.MaxBodySize <= 0 {
		cfg.MaxBodySize = 64 << 20
	}
}

// Call performs a single request and decodes its result into result.
func (c *Client) Call(ctx context.Context, result any, method string, params ...any) error {
	reqs := []Request{{Method: method, Params: params, Result: result}}
	if err := c.Batch(ctx, reqs); err != nil {
		return err
	}
	return reqs[0].Err
}

// Batch executes reqs, splitting them into HTTP batches of at most MaxBatch.
// The returned error is non-nil only when ctx is done; per-request outcomes
// (including exhausted retries) are reported in reqs[i].Err.
func (c *Client) Batch(ctx context.Context, reqs []Request) error {
	for start := 0; start < len(reqs); start += c.cfg.MaxBatch {
		end := min(start+c.cfg.MaxBatch, len(reqs))
		if err := c.batchWithRetry(ctx, reqs[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// outcome classifies the failures of one round trip.
type outcome uint8

const (
	outcomeDone      outcome = iota // nothing to retry
	outcomeTransient                // retry after a backoff, possibly elsewhere
	outcomeFailover                 // the endpoint lacks a method (remembered): try another now
	outcomeTooWide                  // the endpoint refuses this request's size: try another now
)

func (c *Client) batchWithRetry(ctx context.Context, reqs []Request) error {
	pending := make([]int, len(reqs))
	for i := range reqs {
		pending[i] = i
		reqs[i].Err = nil
	}
	// Endpoints that refused this particular request as too wide (eth_getLogs
	// ranges); other endpoints may accept it, so only this call skips them.
	var tooWide map[*endpoint]bool
	for attempt := 0; ; {
		ep := c.pickEndpoint(reqs, pending, tooWide)
		if ep == nil {
			if len(tooWide) == 0 {
				for _, i := range pending {
					reqs[i].Err = fmt.Errorf("%w: %s", ErrUnsupported, reqs[i].Method)
				}
			}
			return nil // with tooWide, reqs[i].Err holds the refusal: the caller narrows the request
		}
		failed, kind := c.roundTrip(ctx, ep, reqs, pending)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if len(failed) == 0 {
			ep.markSuccess()
			return nil
		}
		pending = failed
		switch kind {
		case outcomeFailover:
			continue
		case outcomeTooWide:
			if tooWide == nil {
				tooWide = make(map[*endpoint]bool)
			}
			tooWide[ep] = true
			continue
		}
		ep.markFailure(c.backoff(ep.failureCount()))
		if attempt++; attempt >= c.cfg.MaxAttempts {
			return nil // reqs[i].Err already holds the last retryable error
		}
		if err := sleep(ctx, c.backoff(attempt-1)); err != nil {
			return err
		}
	}
}

// roundTrip sends the pending requests to ep and returns the indexes that should
// be retried, with the most severe failure kind.
func (c *Client) roundTrip(ctx context.Context, ep *endpoint, reqs []Request, pending []int) (failed []int, kind outcome) {
	method := reqs[pending[0]].Method
	if len(pending) > 1 {
		method = "batch"
	}
	start := time.Now()
	resps, err := c.send(ctx, ep, reqs, pending)
	if c.obs != nil {
		c.obs.ObserveRoundTrip(ep.name, method, len(pending), time.Since(start), err)
	}
	if err != nil {
		for _, i := range pending {
			reqs[i].Err = err
		}
		switch {
		case tooWide(method, err):
			return pending, outcomeTooWide
		case methodRefused(err):
			// e.g. a free endpoint answering eth_getLogs with HTTP 403: a property
			// of the endpoint, not a transient fault. Learn it and fail over.
			for _, i := range pending {
				ep.markUnsupported(reqs[i].Method)
			}
			return pending, outcomeFailover
		case IsRetryable(err):
			return pending, outcomeTransient
		}
		return nil, outcomeDone
	}
	var transient, wide, missing bool
	for _, i := range pending {
		reqs[i].Err = nil // clear the outcome of a previous attempt
		r, ok := resps[i]
		switch {
		case !ok:
			reqs[i].Err = errMissingResponse
		case r.Error != nil:
			reqs[i].Err = r.Error
		case isNull(r.Result):
			reqs[i].Err = ErrNotFound
		case reqs[i].Result != nil:
			if derr := json.Unmarshal(r.Result, reqs[i].Result); derr != nil {
				reqs[i].Err = fmt.Errorf("rpc: decode %s result: %w", reqs[i].Method, derr)
			}
		}
		switch e := reqs[i].Err; {
		case e == nil:
		case tooWide(reqs[i].Method, e):
			failed, wide = append(failed, i), true
		case IsUnsupported(e):
			ep.markUnsupported(reqs[i].Method)
			failed, missing = append(failed, i), true
		case IsRetryable(e):
			failed, transient = append(failed, i), true
		}
	}
	// Transient faults dominate (back off before anything else), then per-call
	// refusals, then learned capability gaps.
	switch {
	case transient:
		return failed, outcomeTransient
	case wide:
		return failed, outcomeTooWide
	case missing:
		return failed, outcomeFailover
	}
	return failed, outcomeDone
}

// tooWide reports an eth_getLogs request refused for its block span or result size.
func tooWide(method string, err error) bool {
	return (method == "eth_getLogs" || method == "batch") && IsLogRangeError(err)
}

type wireRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type wireResponse struct {
	ID     *uint64        `json:"id"`
	Result jsontext.Value `json:"result"`
	Error  *Error         `json:"error"`
}

// send performs one HTTP round trip and maps responses back to request indexes.
func (c *Client) send(ctx context.Context, ep *endpoint, reqs []Request, pending []int) (map[int]wireResponse, error) {
	if err := waitTokens(ctx, ep.limiter, len(pending)); err != nil {
		return nil, err
	}
	wire := make([]wireRequest, len(pending))
	byID := make(map[uint64]int, len(pending))
	for k, i := range pending {
		id := c.nextID.Add(1)
		params := reqs[i].Params
		if params == nil {
			params = []any{}
		}
		wire[k] = wireRequest{JSONRPC: "2.0", ID: id, Method: reqs[i].Method, Params: params}
		byID[id] = i
	}
	var body []byte
	var err error
	if len(wire) == 1 {
		body, err = json.Marshal(wire[0])
	} else {
		body, err = json.Marshal(wire)
	}
	if err != nil {
		return nil, fmt.Errorf("rpc: encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("rpc: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, &TransportError{Endpoint: ep.name, Err: redactURL(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxBodySize+1))
	if err != nil {
		return nil, &TransportError{Endpoint: ep.name, Err: err}
	}
	if int64(len(raw)) > c.cfg.MaxBodySize {
		return nil, fmt.Errorf("rpc: response from %s exceeds %d bytes", ep.name, c.cfg.MaxBodySize)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Endpoint: ep.name, Status: resp.StatusCode, Body: snippet(raw)}
	}
	return decodeResponses(raw, byID)
}

func decodeResponses(raw []byte, byID map[uint64]int) (map[int]wireResponse, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, &TransportError{Err: errors.New("empty response body")}
	}
	var list []wireResponse
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("rpc: decode batch response: %w", err)
		}
	} else {
		var one wireResponse
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, fmt.Errorf("rpc: decode response: %w", err)
		}
		// A single error object with a null id applies to the whole batch
		// (e.g. a provider rejecting the batch for rate limiting).
		if one.ID == nil && one.Error != nil {
			return nil, one.Error
		}
		list = []wireResponse{one}
	}
	out := make(map[int]wireResponse, len(list))
	for _, r := range list {
		if r.ID == nil {
			continue
		}
		if i, ok := byID[*r.ID]; ok {
			out[i] = r
		}
	}
	return out, nil
}

// waitTokens takes one token per request: providers rate-limit the calls inside a
// batch, not HTTP round trips. A batch larger than the bucket waits for it to
// refill as many times as needed.
func waitTokens(ctx context.Context, l *rate.Limiter, n int) error {
	for n > 0 {
		k := min(n, l.Burst())
		if err := l.WaitN(ctx, k); err != nil {
			// WaitN fails early when the wait would outlast ctx's deadline. That
			// is the caller's deadline, not a fault of the endpoint: let it expire
			// so the caller sees its own context error.
			<-ctx.Done()
			return ctx.Err()
		}
		n -= k
	}
	return nil
}

// pickEndpoint round-robins across endpoints that support every pending method,
// are not excluded and are not cooling down. If all eligible endpoints are cooling
// down, the one that recovers first is used. It returns nil when none is eligible.
func (c *Client) pickEndpoint(reqs []Request, pending []int, exclude map[*endpoint]bool) *endpoint {
	n := len(c.endpoints)
	start := int(c.rr.Add(1) % uint64(n))
	now := time.Now()
	var best *endpoint
	var bestUntil time.Time
	for k := range n {
		ep := c.endpoints[(start+k)%n]
		if exclude[ep] || !ep.supportsAll(reqs, pending) {
			continue
		}
		until := ep.cooldown()
		if !until.After(now) {
			return ep
		}
		if best == nil || until.Before(bestUntil) {
			best, bestUntil = ep, until
		}
	}
	return best
}

func (c *Client) backoff(attempt int) time.Duration {
	ceiling := c.cfg.BaseBackoff << min(attempt, 16)
	if ceiling <= 0 || ceiling > c.cfg.MaxBackoff {
		ceiling = c.cfg.MaxBackoff
	}
	return time.Duration(rand.Int64N(int64(ceiling)) + 1) // full jitter
}

func (e *endpoint) cooldown() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cooldownUntil
}

func (e *endpoint) failureCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.failures
}

func (e *endpoint) markFailure(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failures++
	e.cooldownUntil = time.Now().Add(d)
}

func (e *endpoint) supportsAll(reqs []Request, pending []int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, i := range pending {
		if e.unsupported[reqs[i].Method] {
			return false
		}
	}
	return true
}

func (e *endpoint) markUnsupported(method string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.unsupported == nil {
		e.unsupported = make(map[string]bool)
	}
	e.unsupported[method] = true
}

func (e *endpoint) markSuccess() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failures = 0
	e.cooldownUntil = time.Time{}
}

// methodRefused reports HTTP statuses that mean "this endpoint will not serve
// these methods" rather than "try again later".
func methodRefused(err error) bool {
	var he *HTTPError
	if !errors.As(err, &he) {
		return false
	}
	switch he.Status {
	case http.StatusForbidden, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	}
	return false
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

func isNull(v jsontext.Value) bool { return len(v) == 0 || string(v) == "null" }

func snippet(b []byte) string {
	const n = 256
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
