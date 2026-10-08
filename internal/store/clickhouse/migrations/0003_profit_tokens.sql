-- Arbitrages can end with a positive balance in more than one token. profit_eth
-- now sums all of them; profit_tokens records how many there were.
ALTER TABLE arbitrages ADD COLUMN IF NOT EXISTS profit_tokens UInt8 DEFAULT 1;

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
