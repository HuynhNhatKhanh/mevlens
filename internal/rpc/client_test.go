package rpc

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huynhnhatkhanh/mevlens/internal/eth"
)

type rpcReq struct {
	ID     uint64         `json:"id"`
	Method string         `json:"method"`
	Params jsontext.Value `json:"params"`
}

type rpcResp struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      uint64         `json:"id"`
	Result  jsontext.Value `json:"result,omitzero"`
	Error   *Error         `json:"error,omitzero"`
}

// fakeNode is a scriptable JSON-RPC server. handle is called once per request
// object and may return a result or an error.
type fakeNode struct {
	t      *testing.T
	mu     sync.Mutex
	calls  [][]string // methods per HTTP round trip
	status func(round int) int
	handle func(round int, r rpcReq) (result string, rpcErr *Error)
}

func (f *fakeNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var reqs []rpcReq
	batch := len(body) > 0 && body[0] == '['
	if batch {
		if err := json.Unmarshal(body, &reqs); err != nil {
			f.t.Errorf("bad batch: %v", err)
		}
	} else {
		var one rpcReq
		if err := json.Unmarshal(body, &one); err != nil {
			f.t.Errorf("bad request: %v", err)
		}
		reqs = []rpcReq{one}
	}
	f.mu.Lock()
	round := len(f.calls)
	methods := make([]string, len(reqs))
	for i, q := range reqs {
		methods[i] = q.Method
	}
	f.calls = append(f.calls, methods)
	f.mu.Unlock()

	if f.status != nil {
		if s := f.status(round); s != http.StatusOK {
			w.WriteHeader(s)
			_, _ = w.Write([]byte("slow down"))
			return
		}
	}
	out := make([]rpcResp, len(reqs))
	for i, q := range reqs {
		res, rerr := f.handle(round, q)
		out[i] = rpcResp{JSONRPC: "2.0", ID: q.ID, Error: rerr}
		if rerr == nil {
			out[i].Result = jsontext.Value(res)
		}
	}
	slices.Reverse(out) // responses may legally arrive in any order
	var enc []byte
	if batch {
		enc, _ = json.Marshal(out)
	} else {
		enc, _ = json.Marshal(out[0])
	}
	_, _ = w.Write(enc)
}

func (f *fakeNode) rounds() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func newClient(t *testing.T, urls ...string) *Client {
	t.Helper()
	cfg := Config{BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, MaxAttempts: 4}
	for _, u := range urls {
		cfg.Endpoints = append(cfg.Endpoints, EndpointConfig{URL: u})
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestBatchMatchesOutOfOrderResponses(t *testing.T) {
	node := &fakeNode{t: t, handle: func(_ int, r rpcReq) (string, *Error) {
		return `"` + r.Method + `"`, nil
	}}
	srv := httptest.NewServer(node)
	defer srv.Close()

	c := newClient(t, srv.URL)
	var a, b, d string
	reqs := []Request{{Method: "a", Result: &a}, {Method: "b", Result: &b}, {Method: "d", Result: &d}}
	if err := c.Batch(context.Background(), reqs); err != nil {
		t.Fatal(err)
	}
	for _, r := range reqs {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
	}
	if a != "a" || b != "b" || d != "d" {
		t.Fatalf("results mismatched: %q %q %q", a, b, d)
	}
}

func TestBatchRetriesOnlyFailedRequests(t *testing.T) {
	node := &fakeNode{t: t, handle: func(round int, r rpcReq) (string, *Error) {
		if r.Method == "flaky" && round == 0 {
			return "", &Error{Code: -32005, Message: "limit exceeded"}
		}
		return `1`, nil
	}}
	srv := httptest.NewServer(node)
	defer srv.Close()

	c := newClient(t, srv.URL)
	reqs := []Request{{Method: "stable"}, {Method: "flaky"}}
	if err := c.Batch(context.Background(), reqs); err != nil {
		t.Fatal(err)
	}
	if reqs[0].Err != nil || reqs[1].Err != nil {
		t.Fatalf("errs: %v %v", reqs[0].Err, reqs[1].Err)
	}
	got := node.rounds()
	want := [][]string{{"stable", "flaky"}, {"flaky"}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("rounds = %v, want %v", got, want)
	}
}

func TestFailoverOnHTTP429(t *testing.T) {
	limited := &fakeNode{t: t,
		status: func(int) int { return http.StatusTooManyRequests },
		handle: func(int, rpcReq) (string, *Error) { return `"never"`, nil },
	}
	healthy := &fakeNode{t: t, handle: func(int, rpcReq) (string, *Error) { return `"ok"`, nil }}
	s1, s2 := httptest.NewServer(limited), httptest.NewServer(healthy)
	defer s1.Close()
	defer s2.Close()

	c := newClient(t, s1.URL, s2.URL)
	for range 4 { // round-robin starts at different endpoints; every call must succeed
		var out string
		if err := c.Call(context.Background(), &out, "x"); err != nil || out != "ok" {
			t.Fatalf("Call = %q, %v", out, err)
		}
	}
}

func TestPermanentErrorIsNotRetried(t *testing.T) {
	node := &fakeNode{t: t, handle: func(int, rpcReq) (string, *Error) {
		return "", &Error{Code: 3, Message: "execution reverted"}
	}}
	srv := httptest.NewServer(node)
	defer srv.Close()

	c := newClient(t, srv.URL)
	err := c.Call(context.Background(), nil, "eth_call")
	var re *Error
	if !errors.As(err, &re) || re.Code != 3 {
		t.Fatalf("err = %v, want rpc error 3", err)
	}
	if n := len(node.rounds()); n != 1 {
		t.Fatalf("permanent error retried: %d rounds", n)
	}
}

func TestNullResultIsNotFound(t *testing.T) {
	node := &fakeNode{t: t, handle: func(int, rpcReq) (string, *Error) { return `null`, nil }}
	srv := httptest.NewServer(node)
	defer srv.Close()

	var h eth.Header
	err := newClient(t, srv.URL).Call(context.Background(), &h, "eth_getBlockByNumber", "0x1", false)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestWholeBatchErrorObject(t *testing.T) {
	var rounds int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		rounds++
		n := rounds
		mu.Unlock()
		if n == 1 {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32005,"message":"rate limited"}}`))
			return
		}
		var reqs []rpcReq
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &reqs)
		out := make([]rpcResp, len(reqs))
		for i, q := range reqs {
			out[i] = rpcResp{JSONRPC: "2.0", ID: q.ID, Result: jsontext.Value(`true`)}
		}
		enc, _ := json.Marshal(out)
		_, _ = w.Write(enc)
	}))
	defer srv.Close()

	reqs := []Request{{Method: "a"}, {Method: "b"}}
	if err := newClient(t, srv.URL).Batch(context.Background(), reqs); err != nil {
		t.Fatal(err)
	}
	if reqs[0].Err != nil || reqs[1].Err != nil {
		t.Fatalf("batch-level rate limit not retried: %v %v", reqs[0].Err, reqs[1].Err)
	}
}

func TestExhaustedRetriesReportLastError(t *testing.T) {
	node := &fakeNode{t: t,
		status: func(int) int { return http.StatusServiceUnavailable },
		handle: func(int, rpcReq) (string, *Error) { return "", nil },
	}
	srv := httptest.NewServer(node)
	defer srv.Close()

	err := newClient(t, srv.URL).Call(context.Background(), nil, "x")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v", err)
	}
	if n := len(node.rounds()); n != 4 {
		t.Fatalf("attempts = %d, want MaxAttempts=4", n)
	}
}

func TestContextCancelStopsRetries(t *testing.T) {
	node := &fakeNode{t: t,
		status: func(int) int { return http.StatusBadGateway },
		handle: func(int, rpcReq) (string, *Error) { return "", nil },
	}
	srv := httptest.NewServer(node)
	defer srv.Close()

	c, _ := New(Config{
		Endpoints:   []EndpointConfig{{URL: srv.URL}},
		BaseBackoff: time.Hour, MaxBackoff: time.Hour, MaxAttempts: 10,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, nil, "x"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestTransportErrorDoesNotLeakURL(t *testing.T) {
	c, _ := New(Config{
		Endpoints:   []EndpointConfig{{Name: "alchemy", URL: "http://127.0.0.1:1/v2/SECRET-KEY"}},
		MaxAttempts: 1,
	})
	err := c.Call(context.Background(), nil, "x")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "SECRET-KEY") {
		t.Fatalf("error leaks API key: %v", err)
	}
	if !IsRetryable(err) {
		t.Fatalf("connection refused should be retryable: %v", err)
	}
}

func TestBlockDetectsInconsistentReceipts(t *testing.T) {
	header := `{"number":"0x10","hash":"0x` + h64("aa") + `","parentHash":"0x` + h64("bb") + `",
		"timestamp":"0x1","baseFeePerGas":"0x989680","gasUsed":"0x0","l1BlockNumber":"0x1",
		"transactions":["0x` + h64("01") + `"]}`
	node := &fakeNode{t: t, handle: func(_ int, r rpcReq) (string, *Error) {
		if r.Method == "eth_getBlockByNumber" {
			return header, nil
		}
		return `[]`, nil // lagging backend: no receipts yet
	}}
	srv := httptest.NewServer(node)
	defer srv.Close()

	_, err := newClient(t, srv.URL).BlockWithReceipts(context.Background(), 16)
	if !errors.Is(err, ErrInconsistent) {
		t.Fatalf("err = %v, want ErrInconsistent", err)
	}
}

func h64(b string) string { return strings.Repeat(b, 32) }

func TestUnsupportedMethodFailsOverWithoutBackoff(t *testing.T) {
	// Some free endpoints refuse eth_call; the client must learn that and route
	// eth_call elsewhere without cooling the endpoint down for other methods.
	noCall := &fakeNode{t: t, handle: func(_ int, r rpcReq) (string, *Error) {
		if r.Method == "eth_call" {
			return "", &Error{Code: -32000, Message: "The method eth_call is not supported."}
		}
		return `"meow"`, nil
	}}
	full := &fakeNode{t: t, handle: func(int, rpcReq) (string, *Error) { return `"full"`, nil }}
	s1, s2 := httptest.NewServer(noCall), httptest.NewServer(full)
	defer s1.Close()
	defer s2.Close()

	c, _ := New(Config{
		Endpoints:   []EndpointConfig{{URL: s1.URL}, {URL: s2.URL}},
		BaseBackoff: time.Hour, MaxBackoff: time.Hour, // any backoff would hang the test
	})
	for range 6 {
		var out string
		if err := c.Call(context.Background(), &out, "eth_call"); err != nil || out != "full" {
			t.Fatalf("eth_call = %q, %v", out, err)
		}
	}
	if n := len(noCall.rounds()); n != 1 {
		t.Fatalf("endpoint without eth_call was asked %d times, want 1", n)
	}
	var out string
	for range 2 { // other methods still use both endpoints
		if err := c.Call(context.Background(), &out, "eth_blockNumber"); err != nil {
			t.Fatal(err)
		}
	}
	if len(noCall.rounds()) != 2 {
		t.Fatal("endpoint must keep serving the methods it supports")
	}
}

func TestNoEndpointSupportsMethod(t *testing.T) {
	node := &fakeNode{t: t, handle: func(int, rpcReq) (string, *Error) {
		return "", &Error{Code: -32601, Message: "method not found"}
	}}
	srv := httptest.NewServer(node)
	defer srv.Close()
	err := newClient(t, srv.URL).Call(context.Background(), nil, "debug_traceCall")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestIsRevert(t *testing.T) {
	if !IsRevert(&Error{Code: 3, Message: "execution reverted"}) || !IsRevert(&Error{Code: -32000, Message: "execution reverted: STF"}) {
		t.Fatal("revert not recognised")
	}
	if IsRevert(&Error{Code: -32000, Message: "The method eth_call is not supported."}) || IsRevert(errors.New("x")) {
		t.Fatal("non-revert classified as revert")
	}
}

func TestLogRangeRefusalFailsOverPerCall(t *testing.T) {
	// dRPC's free plan refuses eth_getLogs over 10k blocks while Arbitrum's public
	// endpoint accepts 10M: the wide query must go to the endpoint that accepts it,
	// without blacklisting the narrow one for other calls.
	narrow := &fakeNode{t: t, handle: func(_ int, r rpcReq) (string, *Error) {
		if r.Method == "eth_getLogs" {
			return "", &Error{Code: 35, Message: "ranges over 10000 blocks are not supported on free plan"}
		}
		return `"0x1"`, nil
	}}
	wide := &fakeNode{t: t, handle: func(int, rpcReq) (string, *Error) { return `[]`, nil }}
	s1, s2 := httptest.NewServer(narrow), httptest.NewServer(wide)
	defer s1.Close()
	defer s2.Close()

	c, _ := New(Config{
		Endpoints:   []EndpointConfig{{URL: s1.URL}, {URL: s2.URL}},
		BaseBackoff: time.Hour, MaxBackoff: time.Hour, // no backoff may happen
	})
	for range 4 {
		if _, err := c.Logs(context.Background(), LogQuery{From: 1, To: 10_000_000}); err != nil {
			t.Fatalf("Logs: %v", err)
		}
	}
	var out string
	if err := c.Call(context.Background(), &out, "eth_blockNumber"); err != nil {
		t.Fatal(err)
	}
	if len(narrow.rounds()) < 2 {
		t.Fatal("narrow endpoint must stay eligible for other calls")
	}
}

func TestLogRangeRefusedEverywhereIsReported(t *testing.T) {
	node := &fakeNode{t: t, handle: func(int, rpcReq) (string, *Error) {
		return "", &Error{Code: -32602, Message: "query spans 20000000 blocks, but only 10000000 are allowed"}
	}}
	srv := httptest.NewServer(node)
	defer srv.Close()
	_, err := newClient(t, srv.URL).Logs(context.Background(), LogQuery{From: 1, To: 20_000_000})
	if !IsLogRangeError(err) || errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want a range error the caller can narrow", err)
	}
	if IsLogRangeError(&Error{Code: -32005, Message: "limit exceeded"}) {
		t.Fatal("a rate limit must not be mistaken for a range refusal")
	}
}

func TestErrorClassification(t *testing.T) {
	revertNotSupported := &Error{Code: 3, Message: "execution reverted: function not supported"}
	pruned := &Error{Code: -32000, Message: "historical state 79ff2b is not available"}
	for _, e := range []*Error{revertNotSupported, pruned, {Code: -32000, Message: "header not found"}} {
		if IsUnsupported(e) {
			t.Errorf("IsUnsupported(%q) = true: says nothing about the method", e.Message)
		}
	}
	for _, msg := range []string{"execution reverted", "invalid opcode: INVALID", "out of gas", "stack underflow (0 <=> 1)"} {
		if !IsExecutionError(&Error{Code: -32000, Message: msg}) {
			t.Errorf("IsExecutionError(%q) = false", msg)
		}
	}
	if IsExecutionError(&Error{Code: -32000, Message: "The method eth_call is not supported."}) {
		t.Error("a missing method is not an execution error")
	}
	if IsRetryable(&Error{Code: -32000, Message: "execution reverted: try again later"}) {
		t.Error("a revert is final whatever its reason says")
	}
	if !IsRetryable(&Error{Code: -32000, Message: "header not found"}) {
		t.Error("a block the backend does not have yet is retryable")
	}
	if !IsMissingState(pruned) || IsMissingState(revertNotSupported) {
		t.Error("IsMissingState misclassifies")
	}
}

func TestRevertReasonDoesNotDisableMethod(t *testing.T) {
	// Regression: a revert reason containing "not supported" marked eth_call
	// unsupported on every endpoint for the life of the process.
	node := &fakeNode{t: t, handle: func(round int, _ rpcReq) (string, *Error) {
		if round == 0 {
			return "", &Error{Code: 3, Message: "execution reverted: function not supported"}
		}
		return `"0x01"`, nil
	}}
	srv := httptest.NewServer(node)
	defer srv.Close()
	c := newClient(t, srv.URL)

	err := c.Call(context.Background(), nil, "eth_call")
	if !IsRevert(err) || errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want the revert itself", err)
	}
	var out string
	if err := c.Call(context.Background(), &out, "eth_call"); err != nil || out != "0x01" {
		t.Fatalf("eth_call after a revert = %q, %v; the method must stay usable", out, err)
	}
}

func TestEndpointErrorsFailOver(t *testing.T) {
	// Regression: HTTP 401/404 and malformed bodies were returned to the caller
	// without trying the other endpoints, and reset the bad endpoint's health.
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusBadRequest} {
		bad := &fakeNode{t: t, status: func(int) int { return status }, handle: func(int, rpcReq) (string, *Error) { return `"never"`, nil }}
		healthy := &fakeNode{t: t, handle: func(int, rpcReq) (string, *Error) { return `"ok"`, nil }}
		s1, s2 := httptest.NewServer(bad), httptest.NewServer(healthy)
		c := newClient(t, s1.URL, s2.URL)
		for range 6 {
			var out string
			if err := c.Call(context.Background(), &out, "eth_blockNumber"); err != nil || out != "ok" {
				t.Fatalf("HTTP %d: Call = %q, %v; want failover to the healthy endpoint", status, out, err)
			}
		}
		if c.endpoints[0].failureCount() == 0 {
			t.Errorf("HTTP %d: failing endpoint has no recorded failure", status)
		}
		s1.Close()
		s2.Close()
	}

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>gateway</html>"))
	}))
	defer garbage.Close()
	healthy := httptest.NewServer(&fakeNode{t: t, handle: func(int, rpcReq) (string, *Error) { return `"ok"`, nil }})
	defer healthy.Close()
	c := newClient(t, garbage.URL, healthy.URL)
	for range 4 {
		var out string
		if err := c.Call(context.Background(), &out, "eth_blockNumber"); err != nil || out != "ok" {
			t.Fatalf("malformed body: Call = %q, %v; want failover", out, err)
		}
	}
}

func TestForbiddenIsReprobedAfterTTL(t *testing.T) {
	// Regression: one HTTP 403 blacklisted the method on the endpoint forever.
	node := &fakeNode{t: t,
		status: func(round int) int {
			if round == 0 {
				return http.StatusForbidden
			}
			return http.StatusOK
		},
		handle: func(int, rpcReq) (string, *Error) { return `"0x10"`, nil },
	}
	srv := httptest.NewServer(node)
	defer srv.Close()
	c := newClient(t, srv.URL)

	if err := c.Call(context.Background(), nil, "eth_blockNumber"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported while the refusal is remembered", err)
	}
	ep := c.endpoints[0]
	ep.mu.Lock()
	ep.unsupported["eth_blockNumber"] = time.Now().Add(-time.Second) // the TTL has elapsed
	ep.mu.Unlock()
	var out string
	if err := c.Call(context.Background(), &out, "eth_blockNumber"); err != nil || out != "0x10" {
		t.Fatalf("after the TTL: Call = %q, %v; want the endpoint asked again", out, err)
	}
}

func TestForbiddenMixedBatchBlacklistsNothing(t *testing.T) {
	node := &fakeNode{t: t,
		status: func(round int) int {
			if round == 0 {
				return http.StatusForbidden
			}
			return http.StatusOK
		},
		handle: func(int, rpcReq) (string, *Error) { return `true`, nil },
	}
	srv := httptest.NewServer(node)
	defer srv.Close()
	reqs := []Request{{Method: "eth_getBlockByNumber"}, {Method: "eth_getBlockReceipts"}}
	if err := newClient(t, srv.URL).Batch(context.Background(), reqs); err != nil {
		t.Fatal(err)
	}
	if reqs[0].Err != nil || reqs[1].Err != nil {
		t.Fatalf("a 403 to a mixed batch must be retried, not pinned on its methods: %v, %v", reqs[0].Err, reqs[1].Err)
	}
}

func TestUnknownBlockFailsOverPerCall(t *testing.T) {
	// A backend lagging behind the requested block is skipped for that call only.
	lagging := &fakeNode{t: t, handle: func(_ int, r rpcReq) (string, *Error) {
		if strings.Contains(string(r.Params), "0x64") {
			return "", &Error{Code: -32000, Message: "header not found"}
		}
		return `"lagging"`, nil
	}}
	synced := &fakeNode{t: t, handle: func(int, rpcReq) (string, *Error) { return `"synced"`, nil }}
	s1, s2 := httptest.NewServer(lagging), httptest.NewServer(synced)
	defer s1.Close()
	defer s2.Close()
	c, _ := New(Config{
		Endpoints:   []EndpointConfig{{URL: s1.URL}, {URL: s2.URL}},
		BaseBackoff: time.Hour, MaxBackoff: time.Hour, // any backoff would hang the test
	})
	for range 4 {
		var out string
		if err := c.Call(context.Background(), &out, "eth_call", "0x64"); err != nil || out != "synced" {
			t.Fatalf("Call = %q, %v", out, err)
		}
	}
	seen := false
	for range 4 { // the lagging endpoint still serves other heights
		var out string
		if err := c.Call(context.Background(), &out, "eth_call", "0x63"); err != nil {
			t.Fatal(err)
		}
		seen = seen || out == "lagging"
	}
	if !seen {
		t.Fatal("lagging endpoint was blacklisted for the method")
	}
}

func TestUnsupportedNeedsAMethodMessage(t *testing.T) {
	for _, e := range []*Error{
		{Code: -32603, Message: "upstream service not available"},
		{Code: -32005, Message: "capacity not available, try later"},
		{Code: -32000, Message: "receipts not available for block 0x10"},
		{Code: 35, Message: "ranges over 10000 blocks are not supported on free plan"},
	} {
		if IsUnsupported(e) {
			t.Errorf("IsUnsupported(%d %q) = true: says nothing about the method", e.Code, e.Message)
		}
	}
	for _, e := range []*Error{
		{Code: -32601, Message: "whatever"},
		{Code: -32000, Message: "The method eth_call is not supported."},
		{Code: -32000, Message: "the method eth_getLogs is not available on the free tier"},
		{Code: -32000, Message: "Unsupported method: eth_getBlockReceipts"},
		{Code: -32000, Message: "method not whitelisted"},
	} {
		if !IsUnsupported(e) {
			t.Errorf("IsUnsupported(%d %q) = false", e.Code, e.Message)
		}
	}
}

func TestRateLimitIsNotARangeError(t *testing.T) {
	if IsLogRangeError(&Error{Code: -32000, Message: "you sent more than 10 requests per second"}) {
		t.Error("a rate limit worded \"more than\" is not a range refusal")
	}
	if !IsLogRangeError(&Error{Code: -32005, Message: "query returned more than 10000 results"}) {
		t.Error("Infura's result cap is a range refusal")
	}
	if tooWide("eth_getLogs", &HTTPError{Status: http.StatusTooManyRequests, Body: "block range limit"}) {
		t.Error("an HTTP 429 is never a range refusal")
	}
	if tooWide("eth_getBlockByNumber", &Error{Message: "block range too large"}) {
		t.Error("only eth_getLogs can be too wide")
	}

	// End to end: a 429 to eth_getLogs backs off and retries instead of being
	// reported at once as a range the caller should narrow.
	node := &fakeNode{t: t,
		status: func(round int) int {
			if round == 0 {
				return http.StatusTooManyRequests
			}
			return http.StatusOK
		},
		handle: func(int, rpcReq) (string, *Error) { return `[]`, nil },
	}
	srv := httptest.NewServer(node)
	defer srv.Close()
	if _, err := newClient(t, srv.URL).Logs(context.Background(), LogQuery{From: 1, To: 2}); err != nil {
		t.Fatalf("Logs after a 429 = %v, want a retry", err)
	}
}

func TestExhaustedEndpointFailureIsRetryable(t *testing.T) {
	// Regression: roundTrip retried HTTP 4xx and malformed bodies as endpoint
	// faults, but once attempts ran out the caller got an error IsRetryable
	// called permanent, and the pipeline stopped after three of them.
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>challenge</html>"))
	}))
	defer html.Close()
	err := newClient(t, html.URL).Call(context.Background(), nil, "eth_blockNumber")
	if err == nil || !IsRetryable(err) || !errors.Is(err, ErrEndpoints) {
		t.Fatalf("err = %v, want a retryable ErrEndpoints", err)
	}

	unauthorized := &fakeNode{t: t, status: func(int) int { return http.StatusUnauthorized },
		handle: func(int, rpcReq) (string, *Error) { return `"never"`, nil }}
	srv := httptest.NewServer(unauthorized)
	defer srv.Close()
	err = newClient(t, srv.URL).Call(context.Background(), nil, "eth_blockNumber")
	var he *HTTPError
	if !IsRetryable(err) || !errors.As(err, &he) || he.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want a retryable error still carrying the HTTP 401", err)
	}
}

func TestOversizedLogsResponseIsARangeError(t *testing.T) {
	node := &fakeNode{t: t, handle: func(int, rpcReq) (string, *Error) {
		return `[` + strings.Repeat(`"0x00",`, 100) + `"0x00"]`, nil
	}}
	srv := httptest.NewServer(node)
	defer srv.Close()
	c, _ := New(Config{Endpoints: []EndpointConfig{{URL: srv.URL}}, MaxBodySize: 64})
	if _, err := c.Logs(context.Background(), LogQuery{From: 1, To: 2}); !IsLogRangeError(err) {
		t.Fatalf("err = %v, want a range error the caller can narrow", err)
	}
}

func TestRefusalSurvivesOtherEndpointsFailing(t *testing.T) {
	// Found on mainnet: the full nodes answered "historical state ... is not
	// available", the endpoint left kept failing the batch (HTTP 500, a free-plan
	// batch cap), and the caller got the 500 instead of the pruned-state answer
	// it falls back on, so a backfill of old blocks resolved no pool at all.
	pruned := &fakeNode{t: t, handle: func(int, rpcReq) (string, *Error) {
		return "", &Error{Code: -32000, Message: "historical state 4c5f is not available"}
	}}
	capped := &fakeNode{t: t, status: func(int) int { return http.StatusInternalServerError },
		handle: func(int, rpcReq) (string, *Error) { return `"never"`, nil }}
	s1, s2 := httptest.NewServer(pruned), httptest.NewServer(capped)
	defer s1.Close()
	defer s2.Close()
	reqs := []Request{{Method: "eth_call"}, {Method: "eth_call"}}
	if err := newClient(t, s1.URL, s2.URL).Batch(context.Background(), reqs); err != nil {
		t.Fatal(err)
	}
	for i, q := range reqs {
		if !IsMissingState(q.Err) {
			t.Errorf("reqs[%d].Err = %v, want the pruned-state refusal", i, q.Err)
		}
	}
}
