-- Human-friendly, deduplicated views used by Grafana and ad-hoc analysis.

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
    arrayMap(p -> concat('0x', lower(hex(p))), pools) AS pools,
    concat('0x', lower(hex(profit_token))) AS profit_token,
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

CREATE OR REPLACE VIEW blocks_v AS
SELECT
    number,
    concat('0x', lower(hex(hash))) AS hash,
    timestamp,
    base_fee,
    gas_used,
    l1_block,
    tx_count,
    timeboosted_txs,
    swaps,
    arbitrages,
    reverted_arbs,
    regime
FROM blocks FINAL;

CREATE OR REPLACE VIEW pools_v AS
SELECT
    concat('0x', lower(hex(address))) AS address,
    kind,
    canonical,
    venue,
    concat('0x', lower(hex(token0)))  AS token0,
    concat('0x', lower(hex(token1)))  AS token1,
    fee_pips,
    first_seen
FROM pools FINAL;
