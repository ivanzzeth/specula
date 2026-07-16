package postgres

import (
	"database/sql"
	"embed"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// OpenSQLDB opens a database/sql handle over the pgx stdlib driver for dsn.
// The multi-tenant SQL stores (org / apikey / grant) require a *sql.DB, whereas
// the metadata store uses the pgxpool directly; this gives them a handle against
// the same database without disturbing the pool. The caller owns Close().
//
// NOTE: the ported org/apikey/grant SQL uses "?" placeholders (SQLite dialect).
// PostgreSQL via database/sql expects "$N"; a rebind wrapper is still required
// before the multi-tenant stores are query-correct on PostgreSQL (R2 hardening).
func OpenSQLDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open sql db: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: ping sql db: %w", err)
	}
	return db, nil
}

// Migrate applies all pending embedded goose migrations against db using the
// postgres dialect. Idempotent (every migration is IF NOT EXISTS); safe to run
// at every startup. This brings a fresh database up to the full schema
// (baseline cache/user tables + the R1 multi-tenant kernel).
func Migrate(db *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("postgres: set goose dialect: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("postgres: run migrations: %w", err)
	}
	return nil
}
