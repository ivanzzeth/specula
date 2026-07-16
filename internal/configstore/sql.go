package configstore

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ivanzzeth/specula/internal/dbx"
)

// SQLStore is the database-authoritative encrypted config KV: values are stored
// as ciphertext in the system_config table (goose migration 0005_system_config
// / 005_system_config). Being database-backed is the whole point for HA — every
// replica reads the same sealed value, so a secret generated once on any replica
// is immediately shared by all of them.
//
// Like the other hand-written SQL stores in Specula (org / apikey / grant /
// repo), the queries are written with "?" positional placeholders and passed
// through internal/dbx.Rebind, so a single query string is correct on both
// SQLite and PostgreSQL (pgx stdlib needs "$N").
type SQLStore struct {
	db      *sql.DB
	c       *Crypter
	dialect dbx.Dialect
}

// NewSQLStore constructs a database-authoritative store over an already-migrated
// SQLite handle. A disabled Crypter makes every method return ErrConfigDisabled.
func NewSQLStore(db *sql.DB, c *Crypter) *SQLStore {
	return &SQLStore{db: db, c: c, dialect: dbx.SQLite}
}

// NewSQLStorePostgres constructs a database-authoritative store over an
// already-migrated PostgreSQL handle, rebinding "?" placeholders to "$N".
func NewSQLStorePostgres(db *sql.DB, c *Crypter) *SQLStore {
	return &SQLStore{db: db, c: c, dialect: dbx.Postgres}
}

// rb rewrites a query's "?" placeholders for the store's dialect.
func (s *SQLStore) rb(query string) string { return dbx.Rebind(s.dialect, query) }

var _ Store = (*SQLStore)(nil)

func (s *SQLStore) Get(ctx context.Context, key string) (string, bool, error) {
	if !s.c.Enabled() {
		return "", false, ErrConfigDisabled
	}
	var sealed []byte
	err := s.db.QueryRowContext(ctx,
		s.rb(`SELECT value FROM system_config WHERE key = ?`), key).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	pt, err := s.c.Open(sealed)
	if err != nil {
		return "", false, err
	}
	return string(pt), true, nil
}

func (s *SQLStore) Set(ctx context.Context, key, plaintext string) error {
	if !s.c.Enabled() {
		return ErrConfigDisabled
	}
	sealed, err := s.c.Seal([]byte(plaintext))
	if err != nil {
		return err
	}
	// ON CONFLICT ... DO UPDATE is valid on both SQLite and PostgreSQL 9.5+.
	_, err = s.db.ExecContext(ctx, s.rb(
		`INSERT INTO system_config (key, value, updated_at) VALUES (?, ?, ?)
		   ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`),
		key, sealed, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *SQLStore) Delete(ctx context.Context, key string) error {
	if !s.c.Enabled() {
		return ErrConfigDisabled
	}
	_, err := s.db.ExecContext(ctx,
		s.rb(`DELETE FROM system_config WHERE key = ?`), key)
	return err
}

func (s *SQLStore) Keys(ctx context.Context) ([]string, error) {
	if !s.c.Enabled() {
		return nil, ErrConfigDisabled
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM system_config ORDER BY key ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
