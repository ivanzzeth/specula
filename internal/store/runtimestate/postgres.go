package runtimestate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ivanzzeth/specula/internal/stats"
)

// PostgresSeriesStore implements SeriesStore over stats_series_samples.
type PostgresSeriesStore struct {
	pool     *pgxpool.Pool
	capacity int
}

// NewPostgresSeriesStore constructs a Postgres-backed SeriesStore. capacity is
// the maximum samples retained per protocol key; non-positive values default to
// stats.DefaultSeriesCapacity.
func NewPostgresSeriesStore(pool *pgxpool.Pool, capacity int) *PostgresSeriesStore {
	if capacity <= 0 {
		capacity = stats.DefaultSeriesCapacity
	}
	return &PostgresSeriesStore{pool: pool, capacity: capacity}
}

var _ SeriesStore = (*PostgresSeriesStore)(nil)

// Record inserts one sample and trims each protocol series to capacity.
func (s *PostgresSeriesStore) Record(ctx context.Context, protocol string, bytes int64, unix int64) error {
	const insert = `
		INSERT INTO stats_series_samples (protocol, unix_ts, bytes)
		VALUES ($1, $2, $3)
		ON CONFLICT (protocol, unix_ts) DO UPDATE SET bytes = EXCLUDED.bytes`

	if _, err := s.pool.Exec(ctx, insert, protocol, unix, bytes); err != nil {
		return fmt.Errorf("runtimestate: record series(%q): %w", protocol, err)
	}

	const trim = `
		DELETE FROM stats_series_samples
		WHERE protocol = $1
		  AND unix_ts NOT IN (
			SELECT unix_ts
			FROM stats_series_samples
			WHERE protocol = $1
			ORDER BY unix_ts DESC
			LIMIT $2
		  )`

	if _, err := s.pool.Exec(ctx, trim, protocol, s.capacity); err != nil {
		return fmt.Errorf("runtimestate: trim series(%q): %w", protocol, err)
	}
	return nil
}

// Snapshot returns oldest→newest samples for protocol, at most limit points.
func (s *PostgresSeriesStore) Snapshot(ctx context.Context, protocol string, limit int) ([]stats.SeriesPoint, error) {
	if limit <= 0 {
		limit = s.capacity
	}

	const q = `
		SELECT unix_ts, bytes
		FROM (
			SELECT unix_ts, bytes
			FROM stats_series_samples
			WHERE protocol = $1
			ORDER BY unix_ts DESC
			LIMIT $2
		) recent
		ORDER BY unix_ts ASC`

	rows, err := s.pool.Query(ctx, q, protocol, limit)
	if err != nil {
		return nil, fmt.Errorf("runtimestate: snapshot series(%q): %w", protocol, err)
	}
	defer rows.Close()

	var out []stats.SeriesPoint
	for rows.Next() {
		var p stats.SeriesPoint
		if err := rows.Scan(&p.Unix, &p.Bytes); err != nil {
			return nil, fmt.Errorf("runtimestate: scan series(%q): %w", protocol, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runtimestate: snapshot series rows(%q): %w", protocol, err)
	}
	return out, nil
}

// PostgresBlockStore implements BlockStore over upstream_blocks.
type PostgresBlockStore struct {
	pool *pgxpool.Pool
}

// NewPostgresBlockStore constructs a Postgres-backed BlockStore.
func NewPostgresBlockStore(pool *pgxpool.Pool) *PostgresBlockStore {
	return &PostgresBlockStore{pool: pool}
}

var _ BlockStore = (*PostgresBlockStore)(nil)

// Get returns persisted block state, or zero values when absent.
func (s *PostgresBlockStore) Get(ctx context.Context, protocol, upstream string) (BlockState, error) {
	const q = `
		SELECT failures, blocked_until
		FROM upstream_blocks
		WHERE protocol = $1 AND upstream = $2`

	var st BlockState
	var blockedUntil *time.Time
	err := s.pool.QueryRow(ctx, q, protocol, upstream).Scan(&st.Failures, &blockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return BlockState{}, nil
	}
	if err != nil {
		return BlockState{}, fmt.Errorf("runtimestate: get block(%s/%s): %w", protocol, upstream, err)
	}
	if blockedUntil != nil {
		st.BlockedUntil = *blockedUntil
	}
	return st, nil
}

// Set upserts block state for (protocol, upstream).
func (s *PostgresBlockStore) Set(ctx context.Context, protocol, upstream string, state BlockState) error {
	var blockedUntil *time.Time
	if !state.BlockedUntil.IsZero() {
		t := state.BlockedUntil.UTC()
		blockedUntil = &t
	}

	const q = `
		INSERT INTO upstream_blocks (protocol, upstream, failures, blocked_until)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (protocol, upstream) DO UPDATE SET
			failures      = EXCLUDED.failures,
			blocked_until = EXCLUDED.blocked_until`

	if _, err := s.pool.Exec(ctx, q, protocol, upstream, state.Failures, blockedUntil); err != nil {
		return fmt.Errorf("runtimestate: set block(%s/%s): %w", protocol, upstream, err)
	}
	return nil
}

// Clear removes block state for (protocol, upstream).
func (s *PostgresBlockStore) Clear(ctx context.Context, protocol, upstream string) error {
	const q = `DELETE FROM upstream_blocks WHERE protocol = $1 AND upstream = $2`
	if _, err := s.pool.Exec(ctx, q, protocol, upstream); err != nil {
		return fmt.Errorf("runtimestate: clear block(%s/%s): %w", protocol, upstream, err)
	}
	return nil
}
