# ADR 0002 — Deterministic single-writer core

**Status:** accepted · 2026-10-08

## Context
Analytics, golden tests and future backtests all need the same input to always
produce the same output. Concurrency is still needed to fetch blocks fast enough.

## Decision
- Blocks are fetched concurrently but delivered **strictly in order** to one
  processing goroutine. That goroutine owns all mutable state (pool registry,
  classifier, oracle), so no locks are needed.
- `classify`, `dex` and `pricing` perform no I/O and never read the clock. The
  `pure-core` depguard rule in `.golangci.yml` enforces this in CI.
- Results never depend on map iteration order. Output follows block, transaction
  and log order.
- **Dependency rule** (ports & adapters): domain types (`dex.Pool`,
  `dex.Candidate`, `eth.*`) live in the core. The pipeline (`observe`) talks to
  infrastructure only through interfaces it owns (`BlockSource`, `Resolver`,
  `Sink`) and through its own error vocabulary (`observe.ErrUnavailable`).
  Adapters (`rpc`, `registry`, `store/clickhouse`) depend inward, never on each
  other. Wiring happens only in `cmd/mevlens`, including the `rpcSource`
  adapter that maps RPC errors to `ErrUnavailable`.
- `internal/archtest` enforces the rule on the **transitive** import graph
  (`go list -deps`). depguard only sees direct imports. This test exists because
  an earlier version let `classify` reach `rpc` through `registry`.

Resulting graph:

```
classify -> eth, dex, pricing
observe  -> eth, dex, pricing, classify
registry -> eth, dex, rpc            (adapter)
store    -> eth, dex, classify, observe (adapter)
cmd      -> everything (composition root)
```

## Consequences
- Replaying the same blocks yields the same rows (golden tests on real blocks).
- Pipeline tests use `testing/synctest` with fake time and leak detection.
- Processing throughput is bounded by one core. That is orders of magnitude
  above Arbitrum's block rate. Fetching, the real bottleneck, is concurrent.
