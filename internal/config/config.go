// Package config loads and validates the TOML configuration.
//
// RPC URLs and ClickHouse settings may reference environment variables as ${NAME},
// so secrets (provider API keys, database passwords) never live in the file.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/huynhnhatkhanh/mevlens/internal/classify"
	"github.com/huynhnhatkhanh/mevlens/internal/dex"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
	"github.com/huynhnhatkhanh/mevlens/internal/observe"
	"github.com/huynhnhatkhanh/mevlens/internal/pricing"
	"github.com/huynhnhatkhanh/mevlens/internal/registry"
	"github.com/huynhnhatkhanh/mevlens/internal/rpc"
)

// Config is the root configuration.
type Config struct {
	Chain      Chain      `toml:"chain"`
	RPC        RPC        `toml:"rpc"`
	DEX        DEX        `toml:"dex"`
	Pricing    Pricing    `toml:"pricing"`
	Regime     Regime     `toml:"regime"`
	Pipeline   Pipeline   `toml:"pipeline"`
	ClickHouse ClickHouse `toml:"clickhouse"`
	Telemetry  Telemetry  `toml:"telemetry"`
}

type Chain struct {
	Name    string `toml:"name"`
	ChainID uint64 `toml:"chain_id"`
}

type RPC struct {
	MaxBatch    int           `toml:"max_batch"`
	Timeout     time.Duration `toml:"timeout"`
	MaxAttempts int           `toml:"max_attempts"`
	Endpoints   []Endpoint    `toml:"endpoints"`
}

type Endpoint struct {
	Name     string  `toml:"name"`
	URL      string  `toml:"url"`
	RPS      float64 `toml:"rps"`
	Burst    int     `toml:"burst"`
	MaxBatch int     `toml:"max_batch"` // 0 = rpc.max_batch; for plans that refuse larger batches
}

type DEX struct {
	Factories []Factory `toml:"factories"`
}

type Factory struct {
	Name       string      `toml:"name"`
	Kind       string      `toml:"kind"` // "v2" | "v3" | "algebra" (v3 pools, poolByPair lookup) | "v4" (the PoolManager)
	Address    eth.Address `toml:"address"`
	StartBlock uint64      `toml:"start_block"` // v4: first block to index Initialize logs from
}

type Pricing struct {
	WETH eth.Address `toml:"weth"`
	// The reference pool is looked up at startup as
	// ReferenceFactory.getPool(WETH, ReferenceStable, ReferenceFee).
	ReferenceFactory string      `toml:"reference_factory"`
	ReferenceStable  eth.Address `toml:"reference_stable"`
	ReferenceFee     uint32      `toml:"reference_fee"`
	// MaxPriceAgeBlocks bounds how long an ETH/USD observation values stablecoin
	// profits (default pricing.DefaultMaxAgeBlocks, about one hour).
	MaxPriceAgeBlocks uint64   `toml:"max_price_age_blocks"`
	Stables           []Stable `toml:"stables"`
}

type Stable struct {
	Symbol   string      `toml:"symbol"`
	Address  eth.Address `toml:"address"`
	Decimals uint8       `toml:"decimals"`
}

// Regime holds ordering-policy activation blocks (0 = not yet verified).
type Regime struct {
	TimeboostStartBlock uint64 `toml:"timeboost_start_block"`
	PGAStartBlock       uint64 `toml:"pga_start_block"`
}

type Pipeline struct {
	FetchWorkers  int           `toml:"fetch_workers"`
	PollInterval  time.Duration `toml:"poll_interval"`
	FlushBlocks   int           `toml:"flush_blocks"`
	FlushInterval time.Duration `toml:"flush_interval"`
	ReorgDepth    uint64        `toml:"reorg_depth"`
}

type ClickHouse struct {
	Addr     string `toml:"addr"`
	Database string `toml:"database"`
	Username string `toml:"username"`
	Password string `toml:"password"`
}

type Telemetry struct {
	Listen             string        `toml:"listen"`
	LogLevel           string        `toml:"log_level"`
	LogFormat          string        `toml:"log_format"` // "json" | "text"
	FlightRecorder     bool          `toml:"flight_recorder"`
	SlowBlockThreshold time.Duration `toml:"slow_block_threshold"`
	TraceDir           string        `toml:"trace_dir"`
}

// Load reads, expands and validates a configuration file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(string(raw), os.LookupEnv)
}

// Parse decodes and validates the TOML, then expands ${NAME} references in the
// fields meant to carry secrets or deployment-specific values (RPC URLs and
// ClickHouse settings). Expansion happens after decoding, so comments are never
// touched, and referencing an unset variable is an error, not a silent "".
func Parse(text string, lookup func(string) (string, bool)) (*Config, error) {
	var c Config
	md, err := toml.Decode(text, &c)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("config: unknown keys: %v", undecoded)
	}
	ex := expander{lookup: lookup}
	for i := range c.RPC.Endpoints {
		ex.expand(&c.RPC.Endpoints[i].URL)
	}
	ex.expand(&c.ClickHouse.Addr)
	ex.expand(&c.ClickHouse.Database)
	ex.expand(&c.ClickHouse.Username)
	ex.expand(&c.ClickHouse.Password)
	if len(ex.missing) > 0 {
		return nil, fmt.Errorf("config: unset environment variables: %s", strings.Join(ex.missing, ", "))
	}
	c.setDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

type expander struct {
	lookup  func(string) (string, bool)
	missing []string
}

func (e *expander) expand(s *string) {
	*s = envRef.ReplaceAllStringFunc(*s, func(ref string) string {
		name := ref[2 : len(ref)-1]
		v, ok := e.lookup(name)
		if !ok {
			e.missing = append(e.missing, name)
		}
		return v
	})
}

func (c *Config) setDefaults() {
	if c.Telemetry.Listen == "" {
		c.Telemetry.Listen = "127.0.0.1:9464"
	}
	if c.Telemetry.LogLevel == "" {
		c.Telemetry.LogLevel = "info"
	}
	if c.Telemetry.LogFormat == "" {
		c.Telemetry.LogFormat = "json"
	}
	if c.Telemetry.SlowBlockThreshold == 0 {
		c.Telemetry.SlowBlockThreshold = 2 * time.Second
	}
	if c.Telemetry.TraceDir == "" {
		c.Telemetry.TraceDir = os.TempDir()
	}
	if c.ClickHouse.Database == "" {
		c.ClickHouse.Database = "mevlens"
	}
	if c.Pricing.MaxPriceAgeBlocks == 0 {
		c.Pricing.MaxPriceAgeBlocks = pricing.DefaultMaxAgeBlocks
	}
}

// Validate reports every problem at once.
func (c *Config) Validate() error {
	var errs []error
	if c.Chain.ChainID == 0 {
		errs = append(errs, errors.New("chain.chain_id is required"))
	}
	if len(c.RPC.Endpoints) == 0 {
		errs = append(errs, errors.New("rpc.endpoints: at least one endpoint is required"))
	}
	names := map[string]bool{}
	for i, e := range c.RPC.Endpoints {
		if e.URL == "" {
			errs = append(errs, fmt.Errorf("rpc.endpoints[%d].url is required", i))
		}
		if e.Name == "" || names[e.Name] {
			errs = append(errs, fmt.Errorf("rpc.endpoints[%d].name must be unique and non-empty", i))
		}
		if e.MaxBatch < 0 {
			errs = append(errs, fmt.Errorf("rpc.endpoints[%d].max_batch must not be negative", i))
		}
		names[e.Name] = true
	}
	factories := map[string]Factory{}
	for i, f := range c.DEX.Factories {
		switch f.Kind {
		case "v2", "v3", "algebra":
		case "v4":
			if f.StartBlock == 0 {
				errs = append(errs, fmt.Errorf("dex.factories[%d] (%s): v4 requires start_block", i, f.Name))
			}
		default:
			errs = append(errs, fmt.Errorf("dex.factories[%d] (%s): kind must be v2, v3, algebra or v4", i, f.Name))
		}
		if f.Address.IsZero() || f.Name == "" {
			errs = append(errs, fmt.Errorf("dex.factories[%d]: name and address are required", i))
		}
		factories[f.Name] = f
	}
	if c.Pricing.WETH.IsZero() {
		errs = append(errs, errors.New("pricing.weth is required"))
	}
	if c.Pricing.ReferenceFactory != "" {
		if f, ok := factories[c.Pricing.ReferenceFactory]; !ok || f.Kind != "v3" {
			errs = append(errs, errors.New("pricing.reference_factory must name a v3 factory"))
		}
		found := false
		for _, s := range c.Pricing.Stables {
			found = found || s.Address == c.Pricing.ReferenceStable
		}
		if !found {
			errs = append(errs, errors.New("pricing.reference_stable must be listed in pricing.stables"))
		}
	}
	if r := c.Regime; r.PGAStartBlock != 0 && r.TimeboostStartBlock != 0 && r.PGAStartBlock <= r.TimeboostStartBlock {
		errs = append(errs, errors.New("regime.pga_start_block must be after timeboost_start_block"))
	}
	switch c.Telemetry.LogFormat {
	case "json", "text":
	default:
		errs = append(errs, errors.New("telemetry.log_format must be json or text"))
	}
	if len(errs) > 0 {
		return fmt.Errorf("config: %w", errors.Join(errs...))
	}
	return nil
}

// RPCConfig converts to the rpc package configuration.
func (c *Config) RPCConfig() rpc.Config {
	out := rpc.Config{MaxBatch: c.RPC.MaxBatch, Timeout: c.RPC.Timeout, MaxAttempts: c.RPC.MaxAttempts}
	for _, e := range c.RPC.Endpoints {
		out.Endpoints = append(out.Endpoints, rpc.EndpointConfig{
			Name: e.Name, URL: e.URL, RPS: e.RPS, Burst: e.Burst, MaxBatch: e.MaxBatch,
		})
	}
	return out
}

// Factories converts to registry factories.
func (c *Config) Factories() []registry.Factory {
	out := make([]registry.Factory, 0, len(c.DEX.Factories))
	for _, f := range c.DEX.Factories {
		// Algebra pools (Camelot v3) emit the v3 Swap event: they are v3 pools
		// looked up by pair instead of by fee tier.
		algebra := f.Kind == "algebra"
		var kind dex.Kind
		if algebra {
			kind = dex.KindV3
		} else {
			_ = kind.UnmarshalText([]byte(f.Kind)) // validated in Validate
		}
		out = append(out, registry.Factory{Name: f.Name, Address: f.Address, Kind: kind, StartBlock: f.StartBlock, Algebra: algebra})
	}
	return out
}

// Factory returns the factory with the given name.
func (c *Config) Factory(name string) (registry.Factory, bool) {
	for _, f := range c.Factories() {
		if f.Name == name {
			return f, true
		}
	}
	return registry.Factory{}, false
}

// PricingConfig converts to the pricing package configuration. refPool is the
// reference pool address resolved at startup (zero disables ETH/USD pricing).
func (c *Config) PricingConfig(refPool eth.Address) pricing.Config {
	out := pricing.Config{
		WETH: c.Pricing.WETH, RefPool: refPool, RefStable: c.Pricing.ReferenceStable,
		MaxAgeBlocks: c.Pricing.MaxPriceAgeBlocks,
	}
	for _, s := range c.Pricing.Stables {
		out.Stables = append(out.Stables, pricing.Stable{Address: s.Address, Decimals: s.Decimals})
	}
	return out
}

// RegimeFunc returns the block-to-regime mapping.
func (c *Config) RegimeFunc() classify.RegimeFunc {
	return classify.RegimeBoundaries(c.Regime.TimeboostStartBlock, c.Regime.PGAStartBlock)
}

// PipelineConfig converts to the observe package configuration.
func (c *Config) PipelineConfig(checkpoint string) observe.Config {
	p := c.Pipeline
	return observe.Config{
		CheckpointName: checkpoint,
		FetchWorkers:   p.FetchWorkers,
		PollInterval:   p.PollInterval,
		FlushBlocks:    p.FlushBlocks,
		FlushInterval:  p.FlushInterval,
		ReorgDepth:     p.ReorgDepth,
	}
}
