//go:build integration

package clickhouse

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"github.com/huynhnhatkhanh/mevlens/internal/classify"
	"github.com/huynhnhatkhanh/mevlens/internal/dex"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
	"github.com/huynhnhatkhanh/mevlens/internal/observe"
)

// Run with a live ClickHouse:
//
//	MEVLENS_CH_ADDR=127.0.0.1:9000 MEVLENS_CH_USER=mevlens MEVLENS_CH_PASSWORD=... \
//	  go test -tags integration ./internal/store/clickhouse/
func openTestStore(t *testing.T) *Store {
	t.Helper()
	addr := os.Getenv("MEVLENS_CH_ADDR")
	if addr == "" {
		t.Skip("MEVLENS_CH_ADDR not set")
	}
	ctx := context.Background()
	db := fmt.Sprintf("mevlens_test_%d", time.Now().UnixNano())
	o := Options{Addr: addr, Database: db, Username: os.Getenv("MEVLENS_CH_USER"), Password: os.Getenv("MEVLENS_CH_PASSWORD")}
	s, err := Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if os.Getenv("MEVLENS_KEEP_DB") == "" {
			_ = s.conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db)
		}
		s.Close()
	})
	return s
}

var (
	poolManager = addr(0x44)
	v3          = dex.PoolIDFromAddress(addr(1))
	v4Pool      = dex.PoolID(eth.MustHash("0x973b2ab0a510c8b3b6ffad2d1e4b1db1d0f0aa88f1d3b6f97a0f6b4e9e1d2c3a"))
)

func addr(b byte) eth.Address { var a eth.Address; a[19] = b; return a }

func sampleBatch(block uint64, cp string) *observe.Batch {
	h := eth.Uint64Word(block)
	big, _ := uint256.FromDecimal("123456789012345678901234567890")

	return &observe.Batch{
		Blocks: []classify.BlockInfo{{Number: block, Hash: h, Timestamp: 1_790_000_000 + block, BaseFee: 10_000_000,
			TxCount: 2, Swaps: 2, Arbs: 1, Regime: classify.RegimePGA}},
		Swaps: []classify.SwapRow{
			{Block: block, BlockHash: h, Timestamp: 1_790_000_000 + block, TxIndex: 0, LogIndex: 1, Pool: v3, Contract: addr(1), Venue: "uniswap-v3",
				TokenIn: addr(9), TokenOut: addr(8), AmountIn: *big, AmountOut: *uint256.NewInt(5)},
			{Block: block, BlockHash: h, Timestamp: 1_790_000_000 + block, TxIndex: 0, LogIndex: 2, Pool: v4Pool, Contract: poolManager, Venue: "uniswap-v4",
				TokenIn: addr(8), TokenOut: addr(9), AmountIn: *uint256.NewInt(5), AmountOut: *uint256.NewInt(7)},
		},
		Arbs: []classify.Arb{{
			Block: block, BlockHash: h, Timestamp: 1_790_000_000 + block, Regime: classify.RegimePGA, TxIndex: 0,
			From: addr(0xe0), To: addr(0xb0), Status: classify.StatusSuccess, Hops: 2,
			Pools: []dex.PoolID{v3, v4Pool}, Contracts: []eth.Address{addr(1), poolManager},
			ProfitToken: addr(9), Profit: *uint256.NewInt(2), ProfitTokens: 2, ProfitETH: 0.002, Valued: true,
			GasUsed: 250_000, EffectiveGasPrice: 30_000_000, BaseFee: 10_000_000, PriorityFeePerGas: 20_000_000, CostETH: 0.0075,
		}},
		Pools: []dex.Pool{
			{ID: v3, Contract: addr(1), Kind: dex.KindV3, Canonical: true, Venue: "uniswap-v3", Token0: addr(8), Token1: addr(9), FeePips: 500, FirstSeen: block},
			{ID: v4Pool, Contract: poolManager, Factory: poolManager, Kind: dex.KindV4, Canonical: true, Venue: "uniswap-v4",
				Token0: addr(8), Token1: addr(9), FeePips: 3000, Hooks: addr(0x77), Native: true, FirstSeen: block},
		},
		Checkpoint: observe.Checkpoint{Name: cp, Block: block, Hash: h},
	}
}

func count(t *testing.T, s *Store, q string) uint64 {
	t.Helper()
	var n uint64
	if err := s.conn.QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStoreRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	applied, err := s.Migrate(ctx)
	if err != nil || len(applied) != 4 {
		t.Fatalf("migrate = %v, %v", applied, err)
	}
	if again, err := s.Migrate(ctx); err != nil || len(again) != 0 {
		t.Fatalf("second migrate = %v, %v", again, err)
	}

	if _, ok, err := s.LoadCheckpoint(ctx, "follow"); err != nil || ok {
		t.Fatalf("empty checkpoint = %v, %v", ok, err)
	}
	for _, n := range []uint64{100, 101, 102} {
		if err := s.Write(ctx, sampleBatch(n, "follow")); err != nil {
			t.Fatal(err)
		}
	}
	// Idempotency: rewriting a block must not duplicate rows.
	if err := s.Write(ctx, sampleBatch(101, "follow")); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, sampleBatch(102, "follow")); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "SELECT count() FROM swaps FINAL"); n != 6 {
		t.Fatalf("swaps = %d, want 6", n)
	}
	if n := count(t, s, "SELECT count() FROM arbitrages_v"); n != 3 {
		t.Fatalf("arbitrages = %d, want 3", n)
	}

	cp, ok, err := s.LoadCheckpoint(ctx, "follow")
	if err != nil || !ok || cp.Block != 102 || cp.Hash != eth.Uint64Word(102) {
		t.Fatalf("checkpoint = %+v %v %v", cp, ok, err)
	}

	// Values survive the round trip, including UInt256 and the bid share view column.
	var (
		amountIn *big.Int
		bidShare *float64
		pools    []string
	)
	if err := s.conn.QueryRow(ctx, "SELECT amount_in FROM swaps FINAL WHERE block = 100 AND log_index = 1").Scan(&amountIn); err != nil {
		t.Fatal(err)
	}
	if amountIn.String() != "123456789012345678901234567890" {
		t.Fatalf("amount_in = %s", amountIn)
	}
	if err := s.conn.QueryRow(ctx, "SELECT bid_share, pools FROM arbitrages_v WHERE block = 100").Scan(&bidShare, &pools); err != nil {
		t.Fatal(err)
	}
	if want := 20_000_000.0 * 250_000 / 1e18 / 0.002; bidShare == nil || *bidShare != want {
		t.Fatalf("bid_share = %v, want %v", bidShare, want)
	}
	if !slices.Equal(pools, []string{addr(1).Hex(), eth.Hash(v4Pool).Hex()}) {
		t.Fatalf("pools = %v", pools)
	}

	if n := count(t, s, "SELECT count() FROM arbitrages_v WHERE profit_tokens = 2"); n != 3 {
		t.Fatalf("profit_tokens not persisted: %d rows", n)
	}

	got, err := s.LoadPools(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("pools = %+v, %v", got, err)
	}
	byID := map[dex.PoolID]dex.Pool{got[0].ID: got[0], got[1].ID: got[1]}
	if p := byID[v3]; p.Contract != addr(1) || p.Kind != dex.KindV3 || p.FeePips != 500 {
		t.Fatalf("v3 pool = %+v", p)
	}
	if p := byID[v4Pool]; p.Contract != poolManager || p.Kind != dex.KindV4 || p.Hooks != addr(0x77) || !p.Native {
		t.Fatalf("v4 pool = %+v", p)
	}
	var v4Rows uint64
	if err := s.conn.QueryRow(ctx, "SELECT count() FROM swaps FINAL WHERE pool_id = ?", string(v4Pool[:])).Scan(&v4Rows); err != nil {
		t.Fatal(err)
	}
	if v4Rows != 3 {
		t.Fatalf("v4 swap rows by pool_id = %d, want 3", v4Rows)
	}
	bots, err := s.LoadBots(ctx)
	if err != nil || len(bots) != 1 || bots[0] != addr(0xb0) {
		t.Fatalf("bots = %v, %v", bots, err)
	}

	// Reorg rewind removes rows >= 101 and resets the checkpoint. It completes
	// even when the caller's context is already cancelled (a SIGTERM midway used
	// to leave rows deleted behind an unmoved checkpoint), and is idempotent, as
	// resume re-runs it after an interruption.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Rewind(cancelled, "follow", 101); err != nil {
		t.Fatal(err)
	}
	if err := s.Rewind(ctx, "follow", 101); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "SELECT count() FROM blocks FINAL"); n != 1 {
		t.Fatalf("blocks after rewind = %d, want 1", n)
	}
	if n := count(t, s, "SELECT count() FROM swaps FINAL WHERE block >= 101"); n != 0 {
		t.Fatalf("swaps after rewind = %d", n)
	}
	cp, _, _ = s.LoadCheckpoint(ctx, "follow")
	if cp.Block != 100 || !cp.Hash.IsZero() {
		t.Fatalf("checkpoint after rewind = %+v", cp)
	}
}

func TestSplitStatements(t *testing.T) {
	got := splitStatements("-- c\nCREATE TABLE a (x UInt8);\n\nCREATE VIEW b AS\nSELECT 1;\n")
	if len(got) != 2 || got[1] != "CREATE VIEW b AS\nSELECT 1" {
		t.Fatalf("statements = %q", got)
	}
}
