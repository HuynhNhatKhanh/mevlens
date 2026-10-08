# ADR 0005 — Canonical pools via factory lookup

**Status:** accepted · 2026-10-08

## Context
Any contract can emit a log with Uniswap's `Swap` topic. Honeypots and fake
pools would pollute the data if every emitter were trusted.

## Decision
For each new emitter, read `factory()`, `token0()`, `token1()` (and `fee()` for
v3). The pool is canonical only if a configured factory's `getPair`/`getPool`
for those tokens returns the emitter's own address. Results, both positive and
negative, are cached and persisted. Only an EVM **revert** counts as proof that
an address is not a pool. Any other error (rate limit, an endpoint lacking
`eth_call`) aborts resolution, so nothing wrong is ever cached.

## Consequences
- Each address costs at most two batched round trips in its lifetime.
- The last rule fixes a real bug found on mainnet: an endpoint without
  `eth_call` had caused genuine pools to be cached as "not a pool".
