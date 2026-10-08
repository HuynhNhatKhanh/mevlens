# ADR 0004 — Pool-side netting classifier

**Status:** accepted · 2026-10-08

## Context
A common way to detect arbitrage is to sum the ERC-20 transfers of a guessed
beneficiary (usually `tx.to`). That breaks when bots route through helper
contracts or forward profit to another address in the same transaction.

## Decision
A successful transaction is an atomic arbitrage if it swaps on at least two
**canonical** pools and, summing the pool-side flows (what the pools paid out
minus what they took in), every token nets to at least zero and at least one
token is positive. The positive amounts are the gross profit: `profit_token` is
the first profitable token, `profit_tokens` counts them, and `profit_eth` sums all
of them. It is reported only when every profitable token can be valued, so
multi-token profits are never silently understated.

A reverted transaction to a contract previously seen arbitraging counts as a
failed attempt.

## Consequences
- Detection is independent of the beneficiary's address.
- Aggregator trades (A→B→C) and round trips at a loss are correctly excluded.
- Legs on venues we do not decode (Uniswap v4, PancakeSwap v3, Algebra) make an
  arbitrage invisible or partial. This is a known gap, tracked in the README.
