-- A Uniswap v4 pool whose hooks set the fee per swap has the PoolKey fee
-- 0x800000 (LPFeeLibrary.DYNAMIC_FEE_FLAG). It was stored as fee_pips = 8388608,
-- which reads as an 838% fee. The flag gets its own column and such pools a
-- fee_pips of 0.

ALTER TABLE pools ADD COLUMN IF NOT EXISTS dynamic_fee Bool DEFAULT false;

-- Cached v4 pools are never re-derived (the Initialize index resumes from its
-- checkpoint), so existing rows are fixed in place. Static v4 fees are capped at
-- 1,000,000 pips, so the flag value is unambiguous; once applied the WHERE
-- matches nothing, and waiting for the mutation keeps the next LoadPools exact.
ALTER TABLE pools UPDATE dynamic_fee = true, fee_pips = 0
    WHERE kind = 'v4' AND fee_pips = 8388608
    SETTINGS mutations_sync = 2;

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
    dynamic_fee,
    concat('0x', lower(hex(hooks)))        AS hooks,
    native,
    first_seen
FROM pools FINAL;
