# ADR 0001 — Read-only, non-commercial observatory first

**Status:** accepted · 2026-10-08

## Context
Cross-DEX arbitrage on Arbitrum is crowded: under Timeboost the mean arbitrage
earned about $1 (arXiv 2509.22143), and large firms dominated the express lane.
In September 2026 Arbitrum switched to per-transaction Priority Gas Auctions
(PGA), and no public data yet shows how competition changed.

## Decision
Phase 0 is an observatory that only reads the chain: it holds no keys and sends
no transactions. A trading component may come later. It would be built on the
same deterministic core, with its own decision record.

## Consequences
- The project produces value (data, dashboards, analysis) before it risks capital.
- Strategy work later starts from measured edge rather than assumptions.
- Running it does not constitute trading activity.
