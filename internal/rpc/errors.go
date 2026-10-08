package rpc

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

var (
	// ErrNotFound is returned when the node answers with a null result, e.g. a
	// block that does not exist yet on the backend that served the request.
	ErrNotFound = errors.New("rpc: not found")

	// ErrInconsistent is returned when a node returns mutually inconsistent data,
	// typically because a load balancer routed the calls of one batch to backends
	// at different heights. It is retryable.
	ErrInconsistent = errors.New("rpc: inconsistent response")

	// ErrUnsupported is returned when no configured endpoint supports a method.
	ErrUnsupported = errors.New("rpc: no endpoint supports method")

	errMissingResponse = errors.New("rpc: response missing for request id")
)

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    jsontext.Value `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// HTTPError is a non-200 HTTP response.
type HTTPError struct {
	Endpoint string
	Status   int
	Body     string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("rpc: %s returned HTTP %d: %s", e.Endpoint, e.Status, e.Body)
}

// TransportError wraps network-level failures.
type TransportError struct {
	Endpoint string
	Err      error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("rpc: %s transport: %v", e.Endpoint, e.Err)
}
func (e *TransportError) Unwrap() error { return e.Err }

// IsRetryable reports whether err is transient and the request may succeed on a
// later attempt or another endpoint. Application errors (e.g. "execution
// reverted") are not retryable.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	// A per-attempt timeout surfaces as a TransportError wrapping DeadlineExceeded
	// and is retryable; cancellation of the caller's context never is.
	var te *TransportError
	if errors.As(err, &te) {
		return !errors.Is(te.Err, context.Canceled)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrInconsistent) || errors.Is(err, errMissingResponse) {
		return true
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status == http.StatusTooManyRequests || he.Status == http.StatusRequestTimeout || he.Status >= 500
	}
	var re *Error
	if errors.As(err, &re) {
		switch re.Code {
		case -32005, // limit exceeded (EIP-1474)
			-32603, // internal error (often a transient backend failure)
			429:    // some providers reuse the HTTP status as the code
			return true
		}
		msg := strings.ToLower(re.Message)
		return strings.Contains(msg, "rate limit") || strings.Contains(msg, "too many requests") ||
			strings.Contains(msg, "timeout") || strings.Contains(msg, "try again")
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// IsUnsupported reports whether err means the endpoint does not offer the method
// (common on free tiers, e.g. no eth_call or no eth_getLogs). Such errors are
// properties of the endpoint: the client fails over without backing off.
func IsUnsupported(err error) bool {
	var re *Error
	if !errors.As(err, &re) {
		return false
	}
	if re.Code == -32601 { // method not found
		return true
	}
	msg := strings.ToLower(re.Message)
	return strings.Contains(msg, "not supported") || strings.Contains(msg, "not available") ||
		strings.Contains(msg, "not whitelisted") || strings.Contains(msg, "method not found")
}

// IsLogRangeError reports whether an eth_getLogs request was refused because its
// block span or result set is too large for the provider.
func IsLogRangeError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Phrasings seen from providers: Arbitrum ("query spans N blocks"), dRPC
	// ("ranges over N blocks are not supported"), Alchemy ("block range",
	// "response size exceeded"), Infura ("returned more than 10000 results").
	// Deliberately not "limit exceeded": that is the -32005 rate-limit message.
	for _, hint := range []string{"query spans", "ranges over", "block range", "range too large", "more than", "response size", "too many results", "too many logs"} {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

// IsRevert reports whether err is an EVM execution revert returned by eth_call.
func IsRevert(err error) bool {
	var re *Error
	if !errors.As(err, &re) {
		return false
	}
	return re.Code == 3 || strings.Contains(strings.ToLower(re.Message), "execution reverted")
}

// redactURL removes the request URL (which may embed an API key) from net/http errors.
func redactURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}
