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
