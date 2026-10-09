# ADR 0003 — Minimal Arbitrum-aware JSON-RPC client instead of go-ethereum

**Status:** accepted · 2026-10-08

## Context
Arbitrum blocks contain Arbitrum-specific transaction types that upstream
go-ethereum's `ethclient` cannot decode, and receipts carry extra fields
(`gasUsedForL1`, `timeboosted`). Free endpoints also differ: some lack
`eth_call`, some rate-limit batches, and load balancers can serve a header and
its receipts from backends at different heights.

## Decision
A small client (`internal/rpc`) with its own wire types (`internal/eth`) and
`encoding/json/v2`:
- one HTTP batch per block (`eth_getBlockByNumber` + `eth_getBlockReceipts`),
  validated for consistency (`ErrInconsistent` is retryable)
- a token-bucket rate limiter per endpoint, round-robin across healthy endpoints
- an optional batch cap per endpoint (`max_batch`): some free plans refuse any
  batch over a few requests, so batches sent there are split
- retries only of the failed requests inside a batch, with full-jitter backoff
- runtime learning of unsupported methods per endpoint: fail over immediately,
  with no cooldown
- API keys never appear in errors or logs

## Consequences
- Far fewer dependencies, and the wire types match exactly what we use.
- Signing and transaction building (a later phase) will need a library. The
  dependency will be added then, in an isolated package.
