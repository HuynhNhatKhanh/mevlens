package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/holiman/uint256"
	"golang.org/x/sync/errgroup"

	"github.com/huynhnhatkhanh/mevlens/internal/classify"
	"github.com/huynhnhatkhanh/mevlens/internal/config"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
	"github.com/huynhnhatkhanh/mevlens/internal/observe"
	"github.com/huynhnhatkhanh/mevlens/internal/pricing"
	"github.com/huynhnhatkhanh/mevlens/internal/registry"
	"github.com/huynhnhatkhanh/mevlens/internal/rpc"
	"github.com/huynhnhatkhanh/mevlens/internal/store/clickhouse"
	"github.com/huynhnhatkhanh/mevlens/internal/telemetry"
)

// app holds the components shared by the long-running commands.
type app struct {
	cfg     *config.Config
	log     *slog.Logger
	metrics *telemetry.Metrics
	flight  *telemetry.FlightRecorder
	client  *rpc.Client
	store   *clickhouse.Store
	reg     *registry.Registry
	cl      *classify.Classifier
}

func newApp(ctx context.Context, configPath, listen string, stderr io.Writer) (*app, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	if listen != "" {
		cfg.Telemetry.Listen = listen
	}
	log, err := telemetry.NewLogger(stderr, cfg.Telemetry.LogLevel, cfg.Telemetry.LogFormat)
	if err != nil {
		return nil, err
	}
	log = log.With("chain", cfg.Chain.Name)
	log.Info("starting", "version", version())

	a := &app{cfg: cfg, log: log}
	if cfg.Telemetry.FlightRecorder {
		if a.flight, err = telemetry.StartFlightRecorder(cfg.Telemetry.TraceDir, time.Minute, log); err != nil {
			return nil, err
		}
	}
	var onSlow func(string)
	if a.flight != nil {
		onSlow = a.flight.Snapshot
	}
	a.metrics = telemetry.NewMetrics(cfg.Telemetry.SlowBlockThreshold, onSlow)

	if a.client, err = rpc.New(cfg.RPCConfig(), rpc.WithObserver(a.metrics)); err != nil {
		return nil, err
	}
	if err := checkChainID(ctx, a.client, cfg.Chain.ChainID); err != nil {
		return nil, err
	}

	a.store, err = clickhouse.Open(ctx, clickhouse.Options{
		Addr: cfg.ClickHouse.Addr, Database: cfg.ClickHouse.Database,
		Username: cfg.ClickHouse.Username, Password: cfg.ClickHouse.Password,
	})
	if err != nil {
		return nil, err
	}
	applied, err := a.store.Migrate(ctx)
	if err != nil {
		return nil, err
	}
	if len(applied) > 0 {
		log.Info("schema migrated", "applied", applied)
	}

	if a.reg, err = registry.New(a.client, cfg.Factories()); err != nil {
		return nil, err
	}
	pools, err := a.store.LoadPools(ctx)
	if err != nil {
		return nil, err
	}
	a.reg.Load(pools)
	bots, err := a.store.LoadBots(ctx)
	if err != nil {
		return nil, err
	}
	oracle, _, err := newOracle(ctx, cfg, a.client, log)
	if err != nil {
		return nil, err
	}
	a.cl = classify.New(a.reg, oracle, classify.WithRegime(cfg.RegimeFunc()), classify.WithKnownBots(bots))
	total, canonical := a.reg.Len()
	log.Info("state loaded", "pools", total, "canonical_pools", canonical, "known_bots", len(bots))
	return a, nil
}

func (a *app) close() {
	if a.flight != nil {
		a.flight.Stop()
	}
	if a.store != nil {
		_ = a.store.Close()
	}
}

// serve runs the pipeline function next to the admin server and stops both
// when either fails or ctx is cancelled.
func (a *app) serve(ctx context.Context, pipeline func(context.Context) error) error {
	srv := telemetry.NewServer(a.cfg.Telemetry.Listen, a.metrics.Registry, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return a.store.Ping(ctx)
	}, a.flight, a.log)

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(ctx) })
	g.Go(func() error {
		err := pipeline(ctx)
		if err == nil {
			err = errDone // stop the admin server once a finite job completes
		}
		return err
	})
	if err := g.Wait(); err != nil && !errors.Is(err, errDone) {
		return err
	}
	return nil
}

var errDone = errors.New("done")

func follow(ctx context.Context, configPath string, from uint64, listen string, stderr io.Writer) error {
	a, err := newApp(ctx, configPath, listen, stderr)
	if err != nil {
		return err
	}
	defer a.close()

	const checkpoint = "follow"
	start, parent, err := a.resume(ctx, checkpoint, from)
	if err != nil {
		return err
	}
	if start == 0 {
		if start, err = a.client.BlockNumber(ctx); err != nil {
			return err
		}
	}
	a.log.Info("following chain", "start", start, "parent_check", parent != nil)
	p := observe.New(a.cfg.PipelineConfig(checkpoint), rpcSource{a.client}, a.reg, a.cl, a.store, a.metrics, a.log)
	return a.serve(ctx, func(ctx context.Context) error { return p.Follow(ctx, start, parent) })
}

func backfill(ctx context.Context, configPath string, from, to uint64, listen string, stderr io.Writer) error {
	a, err := newApp(ctx, configPath, listen, stderr)
	if err != nil {
		return err
	}
	defer a.close()

	checkpoint := fmt.Sprintf("backfill-%d-%d", from, to)
	start, parent, err := a.resume(ctx, checkpoint, from)
	if err != nil {
		return err
	}
	if start > to {
		a.log.Info("backfill already complete", "from", from, "to", to)
		return nil
	}
	a.log.Info("backfilling", "from", start, "to", to)
	began := time.Now()
	p := observe.New(a.cfg.PipelineConfig(checkpoint), rpcSource{a.client}, a.reg, a.cl, a.store, a.metrics, a.log)
	err = a.serve(ctx, func(ctx context.Context) error { return p.Backfill(ctx, start, to, parent) })
	if err == nil {
		a.log.Info("backfill complete", "blocks", to-start+1, "elapsed", time.Since(began).Round(time.Second).String())
	}
	return err
}

// resume returns the next block to process and its expected parent hash.
func (a *app) resume(ctx context.Context, name string, from uint64) (uint64, *eth.Hash, error) {
	cp, ok, err := a.store.LoadCheckpoint(ctx, name)
	if err != nil || !ok {
		return from, nil, err
	}
	a.log.Info("resuming from checkpoint", "checkpoint", name, "block", cp.Block)
	if cp.Hash.IsZero() { // written by a reorg rewind: no parent to check
		return cp.Block + 1, nil, nil
	}
	return cp.Block + 1, &cp.Hash, nil
}

func migrate(ctx context.Context, configPath string, stdout io.Writer) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	s, err := clickhouse.Open(ctx, clickhouse.Options{
		Addr: cfg.ClickHouse.Addr, Database: cfg.ClickHouse.Database,
		Username: cfg.ClickHouse.Username, Password: cfg.ClickHouse.Password,
	})
	if err != nil {
		return err
	}
	defer s.Close()
	applied, err := s.Migrate(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "applied %d migration(s): %v\n", len(applied), applied)
	return nil
}

func checkChainID(ctx context.Context, c *rpc.Client, want uint64) error {
	got, err := c.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("eth_chainId: %w", err)
	}
	if got != want {
		return fmt.Errorf("RPC chain id %d does not match configured chain id %d", got, want)
	}
	return nil
}

var (
	selGetPool = eth.NewSelector("getPool(address,address,uint24)")
	selSlot0   = eth.NewSelector("slot0()")
)

// newOracle resolves the reference pool through its factory (no hard-coded pool
// address) and seeds the ETH/USD price from slot0. It returns the seed price so
// fixtures can reproduce the valuation offline.
func newOracle(ctx context.Context, cfg *config.Config, c *rpc.Client, log *slog.Logger) (*pricing.Oracle, *uint256.Int, error) {
	p := cfg.Pricing
	if p.ReferenceFactory == "" {
		o, err := pricing.New(cfg.PricingConfig(eth.Address{}))
		return o, nil, err
	}
	f, _ := cfg.Factory(p.ReferenceFactory)
	ret, err := c.CallContract(ctx, f.Address,
		selGetPool.Calldata(p.WETH.Word(), p.ReferenceStable.Word(), eth.Uint64Word(uint64(p.ReferenceFee))), rpc.Latest)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve reference pool: %w", err)
	}
	w, ok := eth.WordAt(ret, 0)
	if !ok || w.Address().IsZero() {
		return nil, nil, errors.New("resolve reference pool: factory returned no pool")
	}
	ref := w.Address()
	o, err := pricing.New(cfg.PricingConfig(ref))
	if err != nil {
		return nil, nil, err
	}
	ret, err = c.CallContract(ctx, ref, selSlot0.Calldata(), rpc.Latest)
	if err != nil {
		return nil, nil, fmt.Errorf("reference pool slot0: %w", err)
	}
	seed := new(uint256.Int)
	if w, ok := eth.WordAt(ret, 0); ok {
		seed.SetBytes32(w[:])
		o.SetSqrtPrice(seed, 0)
	}
	price, _, known := o.ETHUSD()
	log.Info("pricing ready", "reference_pool", ref, "eth_usd", price, "known", known)
	return o, seed, nil
}
