# ADR 0005 — Canonical pools via factory lookup

**Status:** accepted · 2026-10-08

## Context
Any contract can emit a log with Uniswap's `Swap` topic. Honeypots and fake
pools would pollute the data if every emitter were trusted.

## Decision
For each new emitter, read `factory()`, `token0()`, `token1()` (and `fee()` for
v3). The pool is canonical only if a configured factory's `getPair`/`getPool`
for those tokens returns the emitter's own address. Results, both positive and
negative, are cached and persisted. Only a failure **inside the EVM** (a revert,
or a deterministic halt such as an invalid opcode or out of gas under a 5M gas
cap) counts as proof that an address is not a pool. Any other error (rate limit,
an endpoint lacking `eth_call`, a node without the block) aborts resolution, so
nothing wrong is cached.

All calls are made at the block being processed, pinned by its hash (EIP-1898),
not at `latest`: a node lagging behind it, or following another fork, then fails
instead of answering from a state where a new pool has no code yet. Immutables
never change once set, so when the endpoints have pruned that state it is read at
`latest` instead.

## Consequences
- Each address costs at most two batched round trips in its lifetime.
- The "inside the EVM" rule fixes a real bug found on mainnet: an endpoint without
  `eth_call` had caused genuine pools to be cached as "not a pool".
- Treating only reverts as final let any contract with a Swap-shaped log and a
  fallback hitting `INVALID` stall the pipeline at its block forever. Halts are
  now final too, and the pipeline bounds resolution retries per block
  (`ResolveAttempts`), processing the block with unresolved pools skipped. Such
  blocks may be incomplete: they are counted in `mevlens_resolve_skipped_blocks_total`
  and the latest one is exported as `mevlens_resolve_skipped_last_block`, so they
  can be found and re-ingested.
