package runtimestate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ivanzzeth/specula/internal/stats"
)

// SQLSeriesStore implements SeriesStore using database/sql (SQLite or Postgres).
type SQLSeriesStore struct {
	db       *sql.DB
	capacity int
}

// NewSQLSeriesStore constructs a SQL-backed SeriesStore.
func NewSQLSeriesStore(db *sql.DB, capacity int) *SQLSeriesStore {
	if capacity <= 0 {
		capacity = stats.DefaultSeriesCapacity
	}
	return &SQLSeriesStore{db: db, capacity: capacity}
}

var _ SeriesStore = (*SQLSeriesStore)(nil)

// Record inserts one sample and trims each protocol series to capacity.
func (s *SQLSeriesStore) Record(ctx context.Context, protocol string, bytes int64, unix int64) error {
	const insert = `
		INSERT INTO stats_series_samples (protocol, unix_ts, bytes)
		VALUES (?, ?, ?)
		ON CONFLICT (protocol, unix_ts) DO UPDATE SET bytes = excluded.bytes`

	if _, err := s.db.ExecContext(ctx, insert, protocol, unix, bytes); err != nil {
		return fmt.Errorf("runtimestate: record series(%q): %w", protocol, err)
	}

	const trim = `
		DELETE FROM stats_series_samples
		WHERE protocol = ?
		  AND unix_ts NOT IN (
			SELECT unix_ts
			FROM stats_series_samples
			WHERE protocol = ?
			ORDER BY unix_ts DESC
			LIMIT ?
		  )`

	if _, err := s.db.ExecContext(ctx, trim, protocol, protocol, s.capacity); err != nil {
		return fmt.Errorf("runtimestate: trim series(%q): %w", protocol, err)
	}
	return nil
}

// Snapshot returns oldest→newest samples for protocol, at most limit points.
func (s *SQLSeriesStore) Snapshot(ctx context.Context, protocol string, limit int) ([]stats.SeriesPoint, error) {
	if limit <= 0 {
		limit = s.capacity
	}

	const q = `
		SELECT unix_ts, bytes
		FROM (
			SELECT unix_ts, bytes
			FROM stats_series_samples
			WHERE protocol = ?
			ORDER BY unix_ts DESC
			LIMIT ?
		) recent
		ORDER BY unix_ts ASC`

	rows, err := s.db.QueryContext(ctx, q, protocol, limit)
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

// SQLBlockStore implements BlockStore using database/sql.
type SQLBlockStore struct {
	db *sql.DB
}

// NewSQLBlockStore constructs a SQL-backed BlockStore.
func NewSQLBlockStore(db *sql.DB) *SQLBlockStore {
	return &SQLBlockStore{db: db}
}

var _ BlockStore = (*SQLBlockStore)(nil)

// Get returns persisted block state, or zero values when absent.
func (s *SQLBlockStore) Get(ctx context.Context, protocol, upstream string) (BlockState, error) {
	const q = `
		SELECT failures, blocked_until
		FROM upstream_blocks
		WHERE protocol = ? AND upstream = ?`

	var st BlockState
	var blockedUntil sql.NullString
	err := s.db.QueryRowContext(ctx, q, protocol, upstream).Scan(&st.Failures, &blockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return BlockState{}, nil
	}
	if err != nil {
		return BlockState{}, fmt.Errorf("runtimestate: get block(%s/%s): %w", protocol, upstream, err)
	}
	if blockedUntil.Valid && blockedUntil.String != "" {
		t, err := time.Parse(time.RFC3339Nano, blockedUntil.String)
		if err != nil {
			t, err = time.Parse(time.RFC3339, blockedUntil.String)
			if err != nil {
				return BlockState{}, fmt.Errorf("runtimestate: parse blocked_until(%s/%s): %w", protocol, upstream, err)
			}
		}
		st.BlockedUntil = t
	}
	return st, nil
}

// Set upserts block state for (protocol, upstream).
func (s *SQLBlockStore) Set(ctx context.Context, protocol, upstream string, state BlockState) error {
	var blockedUntil any
	if !state.BlockedUntil.IsZero() {
		blockedUntil = state.BlockedUntil.UTC().Format(time.RFC3339Nano)
	}

	const q = `
		INSERT INTO upstream_blocks (protocol, upstream, failures, blocked_until)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (protocol, upstream) DO UPDATE SET
			failures      = excluded.failures,
			blocked_until = excluded.blocked_until`

	if _, err := s.db.ExecContext(ctx, q, protocol, upstream, state.Failures, blockedUntil); err != nil {
		return fmt.Errorf("runtimestate: set block(%s/%s): %w", protocol, upstream, err)
	}
	return nil
}

// Clear removes block state for (protocol, upstream).
func (s *SQLBlockStore) Clear(ctx context.Context, protocol, upstream string) error {
	const q = `DELETE FROM upstream_blocks WHERE protocol = ? AND upstream = ?`
	if _, err := s.db.ExecContext(ctx, q, protocol, upstream); err != nil {
		return fmt.Errorf("runtimestate: clear block(%s/%s): %w", protocol, upstream, err)
	}
	return nil
}
