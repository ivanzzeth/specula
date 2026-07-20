// Package sqlite re-exports the SQLite MetadataStore driver (default, light deps).
//
// SQLite is single-instance / node-local only. Use pkg/store/postgres for HA.
package sqlite

import (
	"context"

	intsqlite "github.com/ivanzzeth/specula/internal/store/sqlite"

	"github.com/ivanzzeth/specula/pkg/store/meta"
)

func init() {
	meta.Register("sqlite", func(_ context.Context, dsn string) (meta.MetadataStore, error) {
		return New(dsn)
	})
}

type SQLiteStore = intsqlite.SQLiteStore

// New opens a SQLite MetadataStore at dsn (file path or URI) and runs migrations.
func New(dsn string) (meta.MetadataStore, error) {
	return intsqlite.NewSQLiteStore(dsn)
}

// NewSQLiteStore is an alias for New retained for parity with internal API.
func NewSQLiteStore(dsn string) (*SQLiteStore, error) {
	return intsqlite.NewSQLiteStore(dsn)
}
