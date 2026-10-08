package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

var testEnv = env(map[string]string{
	"CLICKHOUSE_ADDR": "localhost:9000", "CLICKHOUSE_USER": "default", "CLICKHOUSE_PASSWORD": "secret",
})

func TestShippedConfigIsValid(t *testing.T) {
	raw, err := os.ReadFile("../../configs/arbitrum-one.toml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(string(raw), testEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.Chain.ChainID != 42161 || len(c.RPC.Endpoints) < 2 || len(c.Factories()) != 6 {
		t.Fatalf("unexpected config: %+v", c.Chain)
	}
	if c.RPC.Timeout != 15*time.Second || c.Pipeline.PollInterval != 250*time.Millisecond {
		t.Fatalf("durations not decoded: %v %v", c.RPC.Timeout, c.Pipeline.PollInterval)
	}
	if c.ClickHouse.Password != "secret" {
		t.Fatal("environment not expanded")
	}
	if f, ok := c.Factory("uniswap-v3"); !ok || f.Kind.String() != "v3" {
		t.Fatalf("factory lookup = %+v", f)
	}
}

func TestUnsetEnvironmentVariableIsAnError(t *testing.T) {
	raw, _ := os.ReadFile("../../configs/arbitrum-one.toml")
	_, err := Parse(string(raw), env(nil))
	if err == nil || !strings.Contains(err.Error(), "CLICKHOUSE_PASSWORD") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownKeysAreRejected(t *testing.T) {
	_, err := Parse(minimal+"\n[pipeline]\nfetch_workerz = 4\n", env(nil))
	if err == nil || !strings.Contains(err.Error(), "fetch_workerz") {
		t.Fatalf("typo not caught: %v", err)
	}
}

func TestValidationReportsAllErrors(t *testing.T) {
	_, err := Parse(`
[[dex.factories]]
name = "x"
kind = "v9"
address = "0x0000000000000000000000000000000000000001"
[[dex.factories]]
name = "y"
kind = "v4"
address = "0x0000000000000000000000000000000000000002"
[telemetry]
log_format = "xml"
`, env(nil))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"chain_id", "rpc.endpoints", "kind must be v2, v3 or v4", "v4 requires start_block", "pricing.weth", "log_format"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in: %v", want, err)
		}
	}
}

const minimal = `
[chain]
chain_id = 1
[[rpc.endpoints]]
name = "a"
url = "http://localhost"
[pricing]
weth = "0x82aF49447D8a07e3bd95BD0d56f35241523fBab1"
`

func TestMinimalConfigGetsDefaults(t *testing.T) {
	c, err := Parse(minimal, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Telemetry.Listen != "127.0.0.1:9464" || c.ClickHouse.Database != "mevlens" {
		t.Fatalf("defaults not applied: %+v", c.Telemetry)
	}
}
