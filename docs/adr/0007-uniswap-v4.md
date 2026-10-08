# ADR 0007 — Uniswap v4 support

**Status:** accepted · 2026-10-08

## Context
Uniswap v4 runs every pool inside one singleton `PoolManager`
(`0x360e68fa…fb32` on Arbitrum One). In a 4,000-block sample there were 372 v4
swap logs next to about 1,400 v2/v3 swap transactions, and arbitrage that routes
a leg through v4 was invisible to the classifier. Three facts, verified on
mainnet, shape the design:

1. **Pools are identified by a `bytes32` PoolId**, not by an address.
2. **`Swap` amounts use the opposite sign of v3.** They are the swapper's balance
   delta, so a negative amount entered the pool. The NatSpec ("delta of the pool")
   suggests otherwise, but the code (`emit Swap(id, sender, delta.amount0(), …)`)
   and ERC-20 transfers to the PoolManager confirm it.
3. **`Swap` logs carry no tokens.** Tokens come only from the pool's `Initialize`
   log. Native ETH is `address(0)`.

## Decision
- Pool identity is `dex.PoolID` (32 bytes) everywhere. v2/v3 pools use their
  address, left-padded. Rows also keep the emitting contract (the PoolManager for
  v4). Balancer-style vaults will fit the same model.
- v4 swaps are decoded with the swapper sign convention, documented next to the
  decoder and pinned by a test built from a mainnet swap.
- **Pool index.** At startup, `Initialize` logs are indexed from the
  PoolManager's first block. That is one `eth_getLogs` per ten million blocks:
  about 19,400 pools in about 25 s on public endpoints. The index is persisted
  after every range so an interrupted sync resumes. After startup, new pools are
  taken from the `Initialize` logs in the blocks being processed, at zero RPC cost.
- **Trust.** A v4 pool is canonical only if a configured PoolManager emitted its
  `Initialize`. A v4 swap counts only if the same PoolManager emitted it.
- **Native ETH is netted as WETH.** Arbitrage routinely enters as WETH on v2/v3
  and leaves as native ETH on v4, or the reverse. Without the alias such legs look
  like a loss in one asset and a gain in another. Pools keep a `native` flag.
- **Hooks are recorded, not interpreted.** The `Swap` event is emitted before
  `afterSwap`, so hooks that return deltas can make the pool-side view differ
  slightly from what the trader actually paid. `pools.hooks` lets analyses
  exclude hooked pools.
- **RPC capability.** Free endpoints cap `eth_getLogs` differently: Arbitrum's
  public RPC allows 10M blocks, dRPC's free plan 10k. A range refusal fails over
  to the next endpoint *for that call only*. The range is halved only when every
  endpoint refuses.

## Consequences
- Migration 0004 adds `swaps.pool_id` and `arbitrages.pool_ids`; existing rows
  derive them from their addresses. It rebuilds the `pools` cache with the new key.
- Two independently verified mainnet arbitrages are now golden tests. One of
  them (block 512820224) had been invisible before v4 support. In the other
  (block 512850323), the executing contract's own ERC-20 and ETH balances do not
  change at all: profit is forwarded elsewhere, which pool-side netting (ADR 0004)
  still catches.
- `inspect` without a database rebuilds the index each run (~20–30 s).
