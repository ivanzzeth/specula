// Package postgres re-exports the PostgreSQL MetadataStore driver.
//
// This package pulls in pgx. Import it only when you need Postgres:
//
//	import "github.com/ivanzzeth/specula/pkg/store/postgres"
//
// Blank-import also registers the "postgres" driver name with pkg/store/meta:
//
//	import _ "github.com/ivanzzeth/specula/pkg/store/postgres"
package postgres

import (
	"context"

	intpg "github.com/ivanzzeth/specula/internal/store/postgres"

	"github.com/ivanzzeth/specula/pkg/store/meta"
)

func init() {
	meta.Register("postgres", func(ctx context.Context, dsn string) (meta.MetadataStore, error) {
		return New(ctx, dsn)
	})
}

type PostgresStore = intpg.PostgresStore

// New opens a Postgres MetadataStore at dsn and runs migrations.
func New(ctx context.Context, dsn string) (meta.MetadataStore, error) {
	return intpg.NewPostgresStore(ctx, dsn)
}

// NewPostgresStore is an alias for New retained for parity with internal API.
func NewPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	return intpg.NewPostgresStore(ctx, dsn)
}
