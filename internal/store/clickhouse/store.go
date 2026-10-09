// Package clickhouse persists observatory batches to ClickHouse.
//
// Writes are idempotent (ReplacingMergeTree keys), the checkpoint is written last,
// and a reorg rewind deletes every row at or above the rewind block, so the tables
// never retain rows from an abandoned fork.
package clickhouse

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/huynhnhatkhanh/mevlens/internal/classify"
	"github.com/huynhnhatkhanh/mevlens/internal/dex"
	"github.com/huynhnhatkhanh/mevlens/internal/eth"
	"github.com/huynhnhatkhanh/mevlens/internal/observe"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Options configures the connection.
type Options struct {
	Addr     string
	Database string
	Username string
	Password string
}

// Store implements observe.Sink plus the loaders needed at startup.
type Store struct {
	conn driver.Conn
	now  func() time.Time
}

// Open connects, creating the database if needed.
func Open(ctx context.Context, o Options) (*Store, error) {
	if err := ensureDatabase(ctx, o); err != nil {
		return nil, err
	}
	conn, err := dial(o, o.Database)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("clickhouse: ping %s: %w", o.Addr, err)
	}
	return &Store{conn: conn, now: time.Now}, nil
}

func dial(o Options, database string) (driver.Conn, error) {
	conn, err := ch.Open(&ch.Options{
		Addr:        []string{o.Addr},
		Auth:        ch.Auth{Database: database, Username: o.Username, Password: o.Password},
		DialTimeout: 10 * time.Second,
		Compression: &ch.Compression{Method: ch.CompressionLZ4},
		Settings:    ch.Settings{"lightweight_deletes_sync": 2},
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: open %s: %w", o.Addr, err)
	}
	return conn, nil
}

func ensureDatabase(ctx context.Context, o Options) error {
	if !validIdent(o.Database) {
		return fmt.Errorf("clickhouse: invalid database name %q", o.Database)
	}
	conn, err := dial(o, "default")
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.Exec(ctx, "CREATE DATABASE IF NOT EXISTS "+o.Database); err != nil {
		return fmt.Errorf("clickhouse: create database: %w", err)
	}
	return nil
}

func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r == '_', 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		default:
			return false
		}
	}
	return true
}

// Close closes the connection.
func (s *Store) Close() error { return s.conn.Close() }

// Ping checks connectivity (used by the readiness probe).
func (s *Store) Ping(ctx context.Context) error { return s.conn.Ping(ctx) }

// Migrate applies pending migrations in order. Statements use IF NOT EXISTS / OR
// REPLACE, so re-running a partially applied migration is safe.
func (s *Store) Migrate(ctx context.Context) (applied []string, err error) {
	if err := s.conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations
		(version String, applied_at DateTime64(3, 'UTC')) ENGINE = ReplacingMergeTree ORDER BY version`); err != nil {
		return nil, fmt.Errorf("clickhouse: create schema_migrations: %w", err)
	}
	done := map[string]bool{}
	rows, err := s.conn.Query(ctx, "SELECT version FROM schema_migrations FINAL")
	if err != nil {
		return nil, fmt.Errorf("clickhouse: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, err
		}
		done[v] = true
	}
	rows.Close()

	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	slices.Sort(files)
	for _, f := range files {
		version := strings.TrimSuffix(strings.TrimPrefix(f, "migrations/"), ".sql")
		if done[version] {
			continue
		}
		body, err := migrations.ReadFile(f)
		if err != nil {
			return applied, err
		}
		for i, stmt := range splitStatements(string(body)) {
			if err := s.conn.Exec(ctx, stmt); err != nil {
				return applied, fmt.Errorf("clickhouse: migration %s statement %d: %w", version, i+1, err)
			}
		}
		if err := s.conn.Exec(ctx, "INSERT INTO schema_migrations VALUES (?, ?)", version, s.now()); err != nil {
			return applied, fmt.Errorf("clickhouse: record migration %s: %w", version, err)
		}
		applied = append(applied, version)
	}
	return applied, nil
}

// splitStatements splits a migration on ';' at line ends and drops comments.
func splitStatements(sql string) []string {
	var out []string
	var cur strings.Builder
	for line := range strings.Lines(sql) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") || trimmed == "" {
			continue
		}
		cur.WriteString(line)
		if strings.HasSuffix(trimmed, ";") {
			stmt := strings.TrimSuffix(strings.TrimSpace(cur.String()), ";")
			out = append(out, stmt)
			cur.Reset()
		}
	}
	if rest := strings.TrimSpace(cur.String()); rest != "" {
		out = append(out, rest)
	}
	return out
}

// Write implements observe.Sink. The checkpoint is written only after every row.
func (s *Store) Write(ctx context.Context, b *observe.Batch) error {
	now := s.now()
	if err := s.insertPools(ctx, b.Pools, now); err != nil {
		return err
	}
	if err := s.insertBlocks(ctx, b.Blocks, now); err != nil {
		return err
	}
	if err := s.insertSwaps(ctx, b.Swaps); err != nil {
		return err
	}
	if err := s.insertArbs(ctx, b.Arbs); err != nil {
		return err
	}
	return s.SaveCheckpoint(ctx, b.Checkpoint)
}

func (s *Store) insertPools(ctx context.Context, pools []dex.Pool, now time.Time) error {
	if len(pools) == 0 {
		return nil
	}
	return s.batch(ctx, "INSERT INTO pools", func(add func(...any) error) error {
		for i := range pools {
			p := &pools[i]
			if err := add(p.ID[:], p.Contract[:], p.Kind.String(), p.Canonical, p.Venue, p.Factory[:], p.Token0[:], p.Token1[:],
				p.FeePips, p.Hooks[:], p.Native, p.FirstSeen, now); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) insertBlocks(ctx context.Context, blocks []classify.BlockInfo, now time.Time) error {
	if len(blocks) == 0 {
		return nil
	}
	return s.batch(ctx, "INSERT INTO blocks", func(add func(...any) error) error {
		for i := range blocks {
			b := &blocks[i]
			if err := add(b.Number, b.Hash[:], b.ParentHash[:], unix(b.Timestamp), b.BaseFee, b.GasUsed, b.L1Block,
				b.TxCount, b.TimeboostedTxs, b.Swaps, b.Arbs, b.RevertedArbs, b.Regime.String(), now); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) insertSwaps(ctx context.Context, swaps []classify.SwapRow) error {
	if len(swaps) == 0 {
		return nil
	}
	return s.batch(ctx, "INSERT INTO swaps", func(add func(...any) error) error {
		for i := range swaps {
			w := &swaps[i]
			if err := add(w.Block, w.BlockHash[:], unix(w.Timestamp), w.TxIndex, w.LogIndex, w.TxHash[:], w.Contract[:], w.Venue,
				w.TokenIn[:], w.TokenOut[:], w.AmountIn.ToBig(), w.AmountOut.ToBig(), w.Pool[:]); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) insertArbs(ctx context.Context, arbs []classify.Arb) error {
	if len(arbs) == 0 {
		return nil
	}
	return s.batch(ctx, "INSERT INTO arbitrages", func(add func(...any) error) error {
		for i := range arbs {
			a := &arbs[i]
			contracts := make([][]byte, len(a.Contracts))
			for j := range a.Contracts {
				contracts[j] = a.Contracts[j][:]
			}
			ids := make([][]byte, len(a.Pools))
			for j := range a.Pools {
				ids[j] = a.Pools[j][:]
			}
			var profitETH *float64
			if a.Valued {
				profitETH = &a.ProfitETH
			}
			if err := add(a.Block, a.BlockHash[:], unix(a.Timestamp), a.Regime.String(), a.TxIndex, a.TxHash[:],
				a.From[:], a.To[:], a.Status.String(), a.Hops, contracts, a.ProfitToken[:], a.Profit.ToBig(), profitETH,
				a.GasUsed, a.GasUsedForL1, a.EffectiveGasPrice, a.BaseFee, a.PriorityFeePerGas, a.CostETH, a.Timeboosted,
				a.ProfitTokens, ids); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) batch(ctx context.Context, query string, fill func(add func(...any) error) error) error {
	b, err := s.conn.PrepareBatch(ctx, query)
	if err != nil {
		return fmt.Errorf("clickhouse: prepare %q: %w", query, err)
	}
	if err := fill(b.Append); err != nil {
		_ = b.Abort()
		return fmt.Errorf("clickhouse: append %q: %w", query, err)
	}
	if err := b.Send(); err != nil {
		return fmt.Errorf("clickhouse: send %q: %w", query, err)
	}
	return nil
}

// SaveCheckpoint records cp as the latest durable position for cp.Name.
func (s *Store) SaveCheckpoint(ctx context.Context, cp observe.Checkpoint) error {
	if cp.Name == "" {
		return nil
	}
	// Use the native batch protocol: Exec parameter binding does not encode []byte
	// as a FixedString.
	return s.batch(ctx, "INSERT INTO checkpoints", func(add func(...any) error) error {
		return add(cp.Name, cp.Block, cp.Hash[:], s.now())
	})
}

// LoadCheckpoint returns the latest checkpoint for name.
func (s *Store) LoadCheckpoint(ctx context.Context, name string) (observe.Checkpoint, bool, error) {
	var (
		block uint64
		hash  string
	)
	row := s.conn.QueryRow(ctx, "SELECT block, hash FROM checkpoints FINAL WHERE name = ?", name)
	if err := row.Scan(&block, &hash); err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return observe.Checkpoint{}, false, nil
		}
		return observe.Checkpoint{}, false, fmt.Errorf("clickhouse: load checkpoint: %w", err)
	}
	cp := observe.Checkpoint{Name: name, Block: block}
	copy(cp.Hash[:], hash)
	return cp, true, nil
}

// rewindTimeout bounds a rewind, which runs to completion even when the caller's
// context is cancelled.
const rewindTimeout = 2 * time.Minute

// Rewind moves the checkpoint to from-1 with an unknown hash, then deletes every
// row at or above block from, so the next run starts at from without a parent
// check.
//
// The checkpoint moves first: interrupted anywhere after that, the next run
// resumes at from and calls Rewind again (deletes are idempotent), instead of
// resuming past rows that are already gone. The rewind is not cancelled with ctx
// (a SIGTERM midway would otherwise leave exactly that gap). At from = 0 the
// checkpoint is block 0: genesis carries no swaps and is not re-ingested.
func (s *Store) Rewind(ctx context.Context, checkpoint string, from uint64) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rewindTimeout)
	defer cancel()
	if err := s.SaveCheckpoint(ctx, observe.Checkpoint{Name: checkpoint, Block: max(from, 1) - 1}); err != nil {
		return fmt.Errorf("clickhouse: rewind: %w", err)
	}
	for _, q := range []string{
		"DELETE FROM swaps WHERE block >= ?",
		"DELETE FROM arbitrages WHERE block >= ?",
		"DELETE FROM blocks WHERE number >= ?",
	} {
		if err := s.conn.Exec(ctx, q, from); err != nil {
			return fmt.Errorf("clickhouse: rewind: %w", err)
		}
	}
	return nil
}

// LoadPools returns every cached pool.
func (s *Store) LoadPools(ctx context.Context) ([]dex.Pool, error) {
	rows, err := s.conn.Query(ctx, `SELECT id, contract, kind, canonical, venue, factory, token0, token1,
		fee_pips, hooks, native, first_seen FROM pools FINAL`)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: load pools: %w", err)
	}
	defer rows.Close()
	var out []dex.Pool
	for rows.Next() {
		var (
			p                                                dex.Pool
			id, contract, factory, token0, token1, hooks, kd string
		)
		if err := rows.Scan(&id, &contract, &kd, &p.Canonical, &p.Venue, &factory, &token0, &token1,
			&p.FeePips, &hooks, &p.Native, &p.FirstSeen); err != nil {
			return nil, err
		}
		if err := p.Kind.UnmarshalText([]byte(kd)); err != nil {
			return nil, fmt.Errorf("clickhouse: load pools: %w", err)
		}
		copy(p.ID[:], id)
		copy(p.Contract[:], contract)
		copy(p.Factory[:], factory)
		copy(p.Token0[:], token0)
		copy(p.Token1[:], token1)
		copy(p.Hooks[:], hooks)
		out = append(out, p)
	}
	return out, rows.Err()
}

// LoadBots returns contracts that have performed at least one successful arbitrage.
func (s *Store) LoadBots(ctx context.Context) ([]eth.Address, error) {
	rows, err := s.conn.Query(ctx, "SELECT DISTINCT contract FROM arbitrages WHERE status = 'success' ORDER BY contract")
	if err != nil {
		return nil, fmt.Errorf("clickhouse: load bots: %w", err)
	}
	defer rows.Close()
	var out []eth.Address
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var a eth.Address
		copy(a[:], raw)
		if !a.IsZero() {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}

func unix(ts uint64) time.Time { return time.Unix(int64(ts), 0).UTC() }
