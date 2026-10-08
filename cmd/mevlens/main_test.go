package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huynhnhatkhanh/mevlens/internal/observe"
	"github.com/huynhnhatkhanh/mevlens/internal/rpc"
)

func TestCommandDispatch(t *testing.T) {
	tests := []struct {
		args    []string
		wantErr string
		wantOut string
	}{
		{args: nil, wantErr: "missing command"},
		{args: []string{"frobnicate"}, wantErr: `unknown command "frobnicate"`},
		{args: []string{"help"}, wantOut: "Commands:"},
		{args: []string{"version"}, wantOut: "mevlens"},
		{args: []string{"backfill", "-from", "10", "-to", "5"}, wantErr: "-to must be >= -from"},
		{args: []string{"follow", "-config", "/does/not/exist.toml"}, wantErr: "no such file"},
	}
	for _, tt := range tests {
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), tt.args, &stdout, &stderr)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%v: err = %v, want %q", tt.args, err, tt.wantErr)
			}
			continue
		}
		if err != nil || !strings.Contains(stdout.String(), tt.wantOut) {
			t.Errorf("%v: err = %v, stdout = %q", tt.args, err, stdout.String())
		}
	}
}

func TestRPCSourceMarksTransientErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c, err := rpc.New(rpc.Config{Endpoints: []rpc.EndpointConfig{{URL: srv.URL}}, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (rpcSource{c}).Block(context.Background(), 1); !errors.Is(err, observe.ErrUnavailable) {
		t.Fatalf("rate limit must surface as observe.ErrUnavailable, got %v", err)
	}
}
