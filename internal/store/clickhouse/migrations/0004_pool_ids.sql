-- Uniswap v4 identifies pools by a bytes32 PoolId inside a singleton PoolManager,
-- so pool identity becomes 32 bytes everywhere: v2/v3 pools use their address
-- left-padded with zeros, v4 pools their PoolId.
--
-- swaps.pool and arbitrages.pools keep the emitting contract (the PoolManager for
-- v4). Existing rows get their pool ids derived from those addresses.

ALTER TABLE swaps ADD COLUMN IF NOT EXISTS pool_id FixedString(32)
    DEFAULT toFixedString(concat(unhex(repeat('00', 12)), pool), 32);

ALTER TABLE arbitrages ADD COLUMN IF NOT EXISTS pool_ids Array(FixedString(32))
    DEFAULT arrayMap(p -> toFixedString(concat(unhex(repeat('00', 12)), p), 32), pools);

-- The pool registry is a cache that is fully re-derivable from the chain, so it is
-- rebuilt with the new key instead of migrated in place.
DROP VIEW IF EXISTS pools_v;

DROP TABLE IF EXISTS pools;

CREATE TABLE IF NOT EXISTS pools
(
    id         FixedString(32),
    contract   FixedString(20),
    kind       LowCardinality(String),
    canonical  Bool,
    venue      LowCardinality(String),
    factory    FixedString(20),
    token0     FixedString(20),
    token1     FixedString(20),
    fee_pips   UInt32,
    hooks      FixedString(20),
    native     Bool,
    first_seen UInt64,
    updated_at DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY id;

-- Address-form ids render as 20-byte addresses, v4 ids as 32-byte hex.
CREATE OR REPLACE VIEW pools_v AS
SELECT
    if(substring(id, 1, 12) = unhex(repeat('00', 12)),
       concat('0x', lower(hex(substring(id, 13, 20)))),
       concat('0x', lower(hex(id))))      AS id,
    concat('0x', lower(hex(contract)))     AS contract,
    kind,
    canonical,
    venue,
    concat('0x', lower(hex(token0)))       AS token0,
    concat('0x', lower(hex(token1)))       AS token1,
    fee_pips,
    concat('0x', lower(hex(hooks)))        AS hooks,
    native,
    first_seen
FROM pools FINAL;

CREATE OR REPLACE VIEW arbitrages_v AS
SELECT
    block,
    timestamp,
    regime,
    tx_index,
    concat('0x', lower(hex(tx_hash)))      AS tx_hash,
    concat('0x', lower(hex(sender)))       AS sender,
    concat('0x', lower(hex(contract)))     AS contract,
    status,
    hops,
    arrayMap(p -> if(substring(p, 1, 12) = unhex(repeat('00', 12)),
                     concat('0x', lower(hex(substring(p, 13, 20)))),
                     concat('0x', lower(hex(p)))), pool_ids) AS pools,
    concat('0x', lower(hex(profit_token))) AS profit_token,
    profit_tokens,
    profit_raw,
    profit_eth,
    cost_eth,
    profit_eth - cost_eth                  AS net_eth,
    priority_fee,
    base_fee,
    effective_gas_price,
    gas_used,
    gas_used_l1,
    -- Share of the gross profit paid as ordering bid (priority fee x gas).
    if(profit_eth > 0, toFloat64(priority_fee) * gas_used / 1e18 / profit_eth, NULL) AS bid_share,
    timeboosted
FROM arbitrages FINAL;
