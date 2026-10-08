-- Core tables. Every table is a ReplacingMergeTree keyed so that re-ingesting a
-- block (after a crash or a reorg rewind) replaces rows instead of duplicating them.
-- Query with FINAL (or the *_v views) to read deduplicated data.

CREATE TABLE IF NOT EXISTS blocks
(
    number          UInt64,
    hash            FixedString(32),
    parent_hash     FixedString(32),
    timestamp       DateTime('UTC'),
    base_fee        UInt64,
    gas_used        UInt64,
    l1_block        UInt64,
    tx_count        UInt32,
    timeboosted_txs UInt32,
    swaps           UInt32,
    arbitrages      UInt32,
    reverted_arbs   UInt32,
    regime          LowCardinality(String),
    ingested_at     DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree(ingested_at)
PARTITION BY toYYYYMM(timestamp)
ORDER BY number;

CREATE TABLE IF NOT EXISTS swaps
(
    block      UInt64,
    block_hash FixedString(32),
    timestamp  DateTime('UTC'),
    tx_index   UInt32,
    log_index  UInt32,
    tx_hash    FixedString(32),
    pool       FixedString(20),
    venue      LowCardinality(String),
    token_in   FixedString(20),
    token_out  FixedString(20),
    amount_in  UInt256,
    amount_out UInt256
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (block, log_index);

CREATE TABLE IF NOT EXISTS arbitrages
(
    block               UInt64,
    block_hash          FixedString(32),
    timestamp           DateTime('UTC'),
    regime              LowCardinality(String),
    tx_index            UInt32,
    tx_hash             FixedString(32),
    sender              FixedString(20),
    contract            FixedString(20),
    status              Enum8('success' = 1, 'reverted' = 2),
    hops                UInt8,
    pools               Array(FixedString(20)),
    profit_token        FixedString(20),
    profit_raw          UInt256,
    profit_eth          Nullable(Float64),
    gas_used            UInt64,
    gas_used_l1         UInt64,
    effective_gas_price UInt64,
    base_fee            UInt64,
    priority_fee        UInt64,
    cost_eth            Float64,
    timeboosted         Bool
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (block, tx_index);

CREATE TABLE IF NOT EXISTS pools
(
    address    FixedString(20),
    kind       LowCardinality(String),
    canonical  Bool,
    venue      LowCardinality(String),
    factory    FixedString(20),
    token0     FixedString(20),
    token1     FixedString(20),
    fee_pips   UInt32,
    first_seen UInt64,
    updated_at DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY address;

CREATE TABLE IF NOT EXISTS checkpoints
(
    name       String,
    block      UInt64,
    hash       FixedString(32),
    updated_at DateTime64(6, 'UTC')
)
ENGINE = ReplacingMergeTree(updated_at)
ORDER BY name;
