// Package migrate wraps pressly/goose/v3 to run embedded SQL migrations for the
// SQLite and PostgreSQL metadata stores. Stub only: leaf agents wire in the
// embedded migration FS.
package migrate

import (
	"database/sql"

	"github.com/pressly/goose/v3"
)

// Up applies all pending migrations found in dir using the given goose dialect
// ("sqlite" or "postgres").
func Up(db *sql.DB, dialect, dir string) error {
	if err := goose.SetDialect(dialect); err != nil {
		return err
	}
	return goose.Up(db, dir)
}
