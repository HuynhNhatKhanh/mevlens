# ADR 0006 — ClickHouse ReplacingMergeTree with checkpoint-last and rewind

**Status:** accepted · 2026-10-08

## Context
The pipeline delivers at-least-once: a crash between a batch write and its
checkpoint re-sends rows. Reorgs, rare on Arbitrum, require discarding rows from
an abandoned fork.

## Decision
- Every table is a `ReplacingMergeTree` keyed so that a re-sent row replaces
  itself. Readers use `FINAL` or the `*_v` views.
- A batch writes pools, blocks, swaps and arbitrages, then the checkpoint last.
- On a reorg, the pipeline rewinds `reorg_depth` blocks. It deletes every row at
  or above the rewind point (lightweight `DELETE`) and resets the checkpoint
  before re-ingesting.
- Migrations are embedded SQL, idempotent (`IF NOT EXISTS` / `OR REPLACE`) and
  tracked in `schema_migrations`.
- Postgres, planned for the money ledger, is deferred until a phase that has money.

## Consequences
- No duplicates and no orphaned rows. Queries need no canonical-chain join.
- One database to run in Phase 0.
