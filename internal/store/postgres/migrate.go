package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// OpenSQLDB opens a database/sql handle over the pgx stdlib driver for dsn.
// The multi-tenant SQL stores (org / apikey / grant) require a *sql.DB, whereas
// the metadata store uses the pgxpool directly; this gives them a handle against
// the same database without disturbing the pool. The caller owns Close().
//
// The ported org/apikey/grant SQL is written with "?" placeholders (SQLite
// dialect) and rebound to "$N" for PostgreSQL by internal/dbx.
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
// postgres dialect. Safe to run at every startup, and — critically — safe to run
// from every HA replica at the same time.
//
// Concurrency: this takes a PostgreSQL **advisory session lock** for the duration
// of the migration, so replicas starting simultaneously serialise instead of
// racing. This is not goose's default. goose's own docs are explicit
// (provider_options.go): "If WithSessionLocker is not called, locking is
// disabled." An unlocked concurrent Up() is how you get two replicas applying the
// same migration at once — duplicate DDL, a poisoned goose_db_version table, or a
// half-migrated schema on rollout. It must be opted into, and it is, here.
//
// This also uses the Provider API rather than the package-level goose.SetBaseFS /
// SetDialect / Up helpers: those mutate global state, which is both racy and
// wrong in a binary that can also open SQLite (each call would clobber the
// other's dialect).
func Migrate(db *sql.DB) error {
	fsys, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("postgres: migrations fs: %w", err)
	}

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("postgres: goose session locker: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys,
		goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("postgres: goose provider: %w", err)
	}
	if _, err := provider.Up(context.Background()); err != nil {
		return fmt.Errorf("postgres: run migrations: %w", err)
	}
	return nil
}
