package events

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/ivanzzeth/specula/internal/dbx"
)

const sqlDefaultCap = 2000

// SQLStore persists verification events in the meta database (sqlite/postgres).
type SQLStore struct {
	db      *sql.DB
	dialect dbx.Dialect
	cap     int
	mu      sync.Mutex
}

// NewSQLStore constructs a SQLite-backed store ("?" placeholders).
func NewSQLStore(db *sql.DB, capacity int) *SQLStore {
	return newSQL(db, dbx.SQLite, capacity)
}

// NewSQLStorePostgres constructs a Postgres-backed store ($N placeholders).
func NewSQLStorePostgres(db *sql.DB, capacity int) *SQLStore {
	return newSQL(db, dbx.Postgres, capacity)
}

func newSQL(db *sql.DB, d dbx.Dialect, capacity int) *SQLStore {
	if capacity < 1 {
		capacity = sqlDefaultCap
	}
	return &SQLStore{db: db, dialect: d, cap: capacity}
}

func (s *SQLStore) rb(q string) string { return dbx.Rebind(s.dialect, q) }

// Record inserts e. Unix defaults to now. ID is assigned by the database.
func (s *SQLStore) Record(ctx context.Context, e Event) {
	if e.Unix == 0 {
		e.Unix = time.Now().UTC().Unix()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, s.rb(
		`INSERT INTO verification_events (unix_ts, protocol, artifact, digest, tier, result, detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`),
		e.Unix, e.Protocol, e.Artifact, e.Digest, e.Tier, e.Result, e.Detail,
	)
	if err != nil {
		return
	}
	s.prune(ctx)
}

func (s *SQLStore) prune(ctx context.Context) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM verification_events`).Scan(&n); err != nil || n <= s.cap {
		return
	}
	excess := n - s.cap
	_, _ = s.db.ExecContext(ctx, s.rb(`
		DELETE FROM verification_events WHERE id IN (
			SELECT id FROM verification_events ORDER BY unix_ts ASC, id ASC LIMIT ?
		)`), excess)
}

// List returns newest-first events up to limit.
func (s *SQLStore) List(ctx context.Context, limit int) []Event {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, s.rb(
		`SELECT id, unix_ts, protocol, artifact, digest, tier, result, detail
		 FROM verification_events ORDER BY unix_ts DESC, id DESC LIMIT ?`), limit)
	if err != nil {
		return []Event{}
	}
	defer rows.Close()
	out := make([]Event, 0, limit)
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Unix, &e.Protocol, &e.Artifact, &e.Digest, &e.Tier, &e.Result, &e.Detail); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Fanout writes to memory and durable store; List prefers durable when non-empty.
type Fanout struct {
	Primary Store
	Memory  *Memory
}

func (f *Fanout) Record(ctx context.Context, e Event) {
	if f.Memory != nil {
		f.Memory.Record(ctx, e)
	}
	if f.Primary != nil {
		f.Primary.Record(ctx, e)
	}
}

func (f *Fanout) List(ctx context.Context, limit int) []Event {
	if f.Primary != nil {
		if got := f.Primary.List(ctx, limit); len(got) > 0 {
			return got
		}
	}
	if f.Memory != nil {
		return f.Memory.List(ctx, limit)
	}
	return []Event{}
}
