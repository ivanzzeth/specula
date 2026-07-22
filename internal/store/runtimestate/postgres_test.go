package runtimestate

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	intpg "github.com/ivanzzeth/specula/internal/store/postgres"
)

const envTestDSN = "SPECULA_TEST_POSTGRES_DSN"

func newTestPool(t *testing.T) (*PostgresSeriesStore, *PostgresBlockStore) {
	t.Helper()
	dsn := os.Getenv(envTestDSN)
	if dsn == "" {
		t.Skipf("skipping live-DB test: set %s to a PostgreSQL DSN to enable", envTestDSN)
	}

	ctx := context.Background()
	store, err := intpg.NewPostgresStore(ctx, dsn)
	require.NoError(t, err)
	require.NoError(t, intpg.ApplySchema(ctx, store.Pool()))

	t.Cleanup(func() {
		_, _ = store.Pool().Exec(context.Background(), "DELETE FROM stats_series_samples")
		_, _ = store.Pool().Exec(context.Background(), "DELETE FROM upstream_blocks")
		store.Close()
	})

	return NewPostgresSeriesStore(store.Pool(), 3), NewPostgresBlockStore(store.Pool())
}

func TestPostgresSeriesStore_RecordAndSnapshot(t *testing.T) {
	series, _ := newTestPool(t)
	ctx := context.Background()

	require.NoError(t, series.Record(ctx, "oci", 100, 1))
	require.NoError(t, series.Record(ctx, "oci", 200, 2))
	require.NoError(t, series.Record(ctx, "oci", 300, 3))
	require.NoError(t, series.Record(ctx, "oci", 400, 4)) // trims unix=1

	pts, err := series.Snapshot(ctx, "oci", 0)
	require.NoError(t, err)
	require.Len(t, pts, 3)
	assert.Equal(t, int64(2), pts[0].Unix)
	assert.Equal(t, int64(400), pts[2].Bytes)
}

func TestPostgresBlockStore_SetGetClear(t *testing.T) {
	_, blocks := newTestPool(t)
	ctx := context.Background()

	until := time.Now().Add(time.Minute).UTC()
	require.NoError(t, blocks.Set(ctx, "oci", "mid", BlockState{Failures: 5, BlockedUntil: until}))

	st, err := blocks.Get(ctx, "oci", "mid")
	require.NoError(t, err)
	assert.Equal(t, 5, st.Failures)
	assert.WithinDuration(t, until, st.BlockedUntil, time.Second)

	require.NoError(t, blocks.Clear(ctx, "oci", "mid"))
	st, err = blocks.Get(ctx, "oci", "mid")
	require.NoError(t, err)
	assert.Zero(t, st.Failures)
	assert.True(t, st.BlockedUntil.IsZero())
}
