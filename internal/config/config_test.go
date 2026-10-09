package config

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/huynhnhatkhanh/mevlens/internal/rpc"
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

func TestEndpointMaxBatch(t *testing.T) {
	withMaxBatch := func(n string) string {
		return strings.Replace(minimal, `url = "http://localhost"`, `url = "http://localhost"`+"\nmax_batch = "+n, 1)
	}
	c, err := Parse(withMaxBatch("3"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.RPCConfig().Endpoints[0].MaxBatch; got != 3 {
		t.Fatalf("endpoint max_batch = %d, want 3", got)
	}
	_, err = Parse(withMaxBatch("-1"), env(nil))
	if err == nil || !strings.Contains(err.Error(), "rpc.endpoints[0].max_batch") {
		t.Fatalf("negative max_batch not caught: %v", err)
	}

	raw, err := os.ReadFile("../../configs/arbitrum-one.toml")
	if err != nil {
		t.Fatal(err)
	}
	if c, err = Parse(string(raw), testEnv); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(c.RPCConfig().Endpoints, func(e rpc.EndpointConfig) bool { return e.Name == "drpc" })
	if i < 0 || c.RPCConfig().Endpoints[i].MaxBatch != 3 {
		t.Fatal("shipped drpc endpoint needs max_batch = 3: its free plan refuses larger batches")
	}
}

func TestMinimalConfigGetsDefaults(t *testing.T) {
	c, err := Parse(minimal, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Telemetry.Listen != "127.0.0.1:9464" || c.ClickHouse.Database != "mevlens" {
		t.Fatalf("defaults not applied: %+v", c.Telemetry)
	}
}
