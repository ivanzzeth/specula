//go:build integration

package e2e

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/store/sqlite"
)

// ageMutableEntries backdates every cached mutable entry by d, making TTL
// expiry deterministic without sleeping.
//
// Why this exists: the mutable-tier caching tests used to set a 1-second TTL
// and time.Sleep(1.1s) to force expiry. That races the wall clock. Under -race
// with CPU contention the *within-TTL* phase could itself take longer than the
// 1s TTL, expiring the entry early and failing the "must NOT re-contact
// upstream" assertion — a genuine flake (reproduced on a clean tree under load,
// not a regression from any change).
//
// Backdating fetched_at drives the very same staleness predicate that a real
// expiry would (cache.isMutableFresh: time.Since(FetchedAt) < TTL), but with no
// timing assumptions whatsoever. That lets the tests use a comfortably long TTL
// for the "fresh" assertions — which can then never expire spuriously — while
// still exercising the real expiry path for the "stale" assertions.
//
// fetched_at is stored as unix seconds (INTEGER), so subtracting is exact.
func ageMutableEntries(t *testing.T, ms *sqlite.SQLiteStore, d time.Duration) {
	t.Helper()

	res, err := ms.DB().Exec(
		`UPDATE mutable_entries SET fetched_at = fetched_at - ?`,
		int64(d/time.Second),
	)
	require.NoError(t, err, "backdate mutable_entries.fetched_at")

	n, err := res.RowsAffected()
	require.NoError(t, err, "rows affected")
	require.Greater(t, n, int64(0),
		"expected at least one mutable entry to backdate — the cache should have been populated first")
}
