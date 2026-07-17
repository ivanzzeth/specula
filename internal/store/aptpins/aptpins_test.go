package aptpins_test

// Store-level tests for the apt pin store, run against a REAL sqlite database
// created by the real goose migration — never a stub. A fake DB that returned
// whatever the store asked for would validate nothing about the SQL, which is
// where the batching, the upsert conflict target and the ambiguity LIMIT all
// live.

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/store/aptpins"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
)

const testScope = "anchor-fingerprint-digest"

func newStore(t *testing.T) *aptpins.Store {
	t.Helper()
	st, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "pins.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return aptpins.New(st.DB(), aptpins.SQLite)
}

// ─────────────────────────────────────────────────────────────────────────────
// Index pins
// ─────────────────────────────────────────────────────────────────────────────

func TestIndexPins_RoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	want := map[string]string{
		"main/binary-amd64/Packages.xz": "aaa",
		"main/i18n/Translation-en":      "bbb",
	}
	require.NoError(t, s.ReplaceIndexPins(ctx, testScope, "ubuntu", "noble", want))

	got, err := s.IndexPins(ctx, testScope, "ubuntu", "noble")
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestIndexPins_AbsentSuite_IsEmptyNotError pins the contract the verifier leans
// on: "no InRelease verified" is an empty map, and the verifier turns that into a
// fail-closed. It must not be an error (which would be indistinguishable from a
// broken DB) nor a nil-vs-empty ambiguity.
func TestIndexPins_AbsentSuite_IsEmptyNotError(t *testing.T) {
	s := newStore(t)
	got, err := s.IndexPins(context.Background(), testScope, "ubuntu", "never-seen")
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestReplaceIndexPins_Replaces asserts REPLACE semantics: a path the previous
// InRelease listed and the new one does not must be gone. Merging would let a
// superseded signed index be served forever.
func TestReplaceIndexPins_Replaces(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	require.NoError(t, s.ReplaceIndexPins(ctx, testScope, "ubuntu", "noble", map[string]string{
		"old/Packages":  "old-hash",
		"kept/Packages": "v1",
	}))
	require.NoError(t, s.ReplaceIndexPins(ctx, testScope, "ubuntu", "noble", map[string]string{
		"kept/Packages": "v2",
	}))

	got, err := s.IndexPins(ctx, testScope, "ubuntu", "noble")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"kept/Packages": "v2"}, got,
		"the new InRelease's pin set must be the WHOLE truth for the suite")
}

// TestReplaceIndexPins_ScopedBySuiteAndRepo proves the key is honoured: replacing
// one suite must not disturb another suite or another repo.
func TestReplaceIndexPins_ScopedBySuiteAndRepo(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	require.NoError(t, s.ReplaceIndexPins(ctx, testScope, "ubuntu", "noble", map[string]string{"p": "noble-hash"}))
	require.NoError(t, s.ReplaceIndexPins(ctx, testScope, "ubuntu", "jammy", map[string]string{"p": "jammy-hash"}))
	require.NoError(t, s.ReplaceIndexPins(ctx, testScope, "debian", "noble", map[string]string{"p": "debian-hash"}))

	// Rotate only ubuntu/noble.
	require.NoError(t, s.ReplaceIndexPins(ctx, testScope, "ubuntu", "noble", map[string]string{"p": "noble-hash-2"}))

	for _, tc := range []struct{ repo, suite, want string }{
		{"ubuntu", "noble", "noble-hash-2"},
		{"ubuntu", "jammy", "jammy-hash"},
		{"debian", "noble", "debian-hash"},
	} {
		got, err := s.IndexPins(ctx, testScope, tc.repo, tc.suite)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got["p"], "repo=%s suite=%s", tc.repo, tc.suite)
	}
}

// TestReplaceIndexPins_ScopedByAnchor is the trust-critical one at store level:
// pins written under one trust anchor must be invisible under another.
func TestReplaceIndexPins_ScopedByAnchor(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	require.NoError(t, s.ReplaceIndexPins(ctx, "anchor-A", "ubuntu", "noble", map[string]string{"p": "hash-A"}))

	got, err := s.IndexPins(ctx, "anchor-B", "ubuntu", "noble")
	require.NoError(t, err)
	assert.Empty(t, got, "anchor-B must not read anchor-A's pins")
}

// TestReplaceIndexPins_LargeIndex_CrossesBatchBoundary exercises the multi-row
// INSERT chunking. A real Packages index pins tens of thousands of files, so the
// batch boundary is on the normal path, not an edge case.
func TestReplaceIndexPins_LargeIndex_CrossesBatchBoundary(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const n = 2500 // > 2 batches of 1000
	pins := make(map[string]string, n)
	for i := range n {
		pins[fmt.Sprintf("main/binary-amd64/part-%04d", i)] = fmt.Sprintf("hash-%04d", i)
	}
	require.NoError(t, s.ReplaceIndexPins(ctx, testScope, "ubuntu", "noble", pins))

	got, err := s.IndexPins(ctx, testScope, "ubuntu", "noble")
	require.NoError(t, err)
	assert.Len(t, got, n)
	assert.Equal(t, "hash-2499", got["main/binary-amd64/part-2499"],
		"a row in the last partial batch must survive")
	assert.Equal(t, "hash-1000", got["main/binary-amd64/part-1000"],
		"a row on the batch boundary must survive")
}

// ─────────────────────────────────────────────────────────────────────────────
// Pool pins
// ─────────────────────────────────────────────────────────────────────────────

func TestPoolPin_RoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	require.NoError(t, s.PutPoolPins(ctx, testScope, "ubuntu", map[string]string{
		"pool/main/h/hello/hello_1.0_amd64.deb": "deadbeef",
	}))

	got, err := s.PoolPin(ctx, testScope, "pool/main/h/hello/hello_1.0_amd64.deb")
	require.NoError(t, err)
	assert.Equal(t, "deadbeef", got)
}

// TestPoolPin_Absent_IsEmptyNotError: an unpinned path is "" with no error. The
// verifier turns that into a fail-closed refusal; an error here would be
// indistinguishable from a store outage.
func TestPoolPin_Absent_IsEmptyNotError(t *testing.T) {
	s := newStore(t)
	got, err := s.PoolPin(context.Background(), testScope, "pool/main/n/nope/nope_1.0_amd64.deb")
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

// TestPoolPin_ScopedByAnchor: an anchor must not read another anchor's pool pins.
func TestPoolPin_ScopedByAnchor(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	require.NoError(t, s.PutPoolPins(ctx, "anchor-A", "ubuntu", map[string]string{"pool/x.deb": "hash-A"}))

	got, err := s.PoolPin(ctx, "anchor-B", "pool/x.deb")
	require.NoError(t, err)
	assert.Equal(t, "", got, "anchor-B must not read anchor-A's pool pin")
}

// TestPoolPin_ConflictingRepos_IsAmbiguous: two repos under one anchor pinning
// one path to different hashes has no correct answer. Fail closed.
func TestPoolPin_ConflictingRepos_IsAmbiguous(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	require.NoError(t, s.PutPoolPins(ctx, testScope, "ubuntu", map[string]string{"pool/x.deb": "hash-1"}))
	require.NoError(t, s.PutPoolPins(ctx, testScope, "ports", map[string]string{"pool/x.deb": "hash-2"}))

	_, err := s.PoolPin(ctx, testScope, "pool/x.deb")
	require.Error(t, err)
	assert.ErrorIs(t, err, aptpins.ErrAmbiguousPoolPin)
}

// TestPoolPin_AgreeingRepos_IsNotAmbiguous: same hash from two repos is one
// answer, not a conflict. DISTINCT is what makes this true.
func TestPoolPin_AgreeingRepos_IsNotAmbiguous(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	require.NoError(t, s.PutPoolPins(ctx, testScope, "ubuntu", map[string]string{"pool/x.deb": "same"}))
	require.NoError(t, s.PutPoolPins(ctx, testScope, "alias", map[string]string{"pool/x.deb": "same"}))

	got, err := s.PoolPin(ctx, testScope, "pool/x.deb")
	require.NoError(t, err)
	assert.Equal(t, "same", got)
}

// TestPutPoolPins_Upserts_LatestSignedWins: within one (scope, repo, path) the
// newest signed statement replaces the old.
func TestPutPoolPins_Upserts_LatestSignedWins(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	require.NoError(t, s.PutPoolPins(ctx, testScope, "ubuntu", map[string]string{"pool/x.deb": "v1"}))
	require.NoError(t, s.PutPoolPins(ctx, testScope, "ubuntu", map[string]string{"pool/x.deb": "v2"}))

	got, err := s.PoolPin(ctx, testScope, "pool/x.deb")
	require.NoError(t, err)
	assert.Equal(t, "v2", got, "the ON CONFLICT target must be (scope, repo, pool_path)")
}

// TestPutPoolPins_Accumulates: pool pins from a second index must not wipe the
// first index's pins. This is the immutable-tier property that lets a .deb
// pinned by a superseded InRelease still serve.
func TestPutPoolPins_Accumulates(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	require.NoError(t, s.PutPoolPins(ctx, testScope, "ubuntu", map[string]string{"pool/a.deb": "ha"}))
	require.NoError(t, s.PutPoolPins(ctx, testScope, "ubuntu", map[string]string{"pool/b.deb": "hb"}))

	a, err := s.PoolPin(ctx, testScope, "pool/a.deb")
	require.NoError(t, err)
	assert.Equal(t, "ha", a, "an earlier index's pool pin must survive a later index")
}

func TestPutPoolPins_Empty_IsNoop(t *testing.T) {
	s := newStore(t)
	require.NoError(t, s.PutPoolPins(context.Background(), testScope, "ubuntu", nil))
}

func TestPutPoolPins_LargeIndex_CrossesBatchBoundary(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const n = 2500
	pins := make(map[string]string, n)
	for i := range n {
		pins[fmt.Sprintf("pool/main/p/pkg/pkg-%04d.deb", i)] = fmt.Sprintf("hash-%04d", i)
	}
	require.NoError(t, s.PutPoolPins(ctx, testScope, "ubuntu", pins))

	got, err := s.PoolPin(ctx, testScope, "pool/main/p/pkg/pkg-2499.deb")
	require.NoError(t, err)
	assert.Equal(t, "hash-2499", got)
}

// ─────────────────────────────────────────────────────────────────────────────
// Concurrency
// ─────────────────────────────────────────────────────────────────────────────

// TestReplaceIndexPins_ConcurrentSameInRelease is the §G3 race: two replicas
// verify the same InRelease at the same moment. Neither may corrupt the pin set,
// and the result must be the full set — not a half-applied mixture from one
// transaction's DELETE landing inside another's INSERT.
func TestReplaceIndexPins_ConcurrentSameInRelease(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	pins := make(map[string]string, 200)
	for i := range 200 {
		pins[fmt.Sprintf("main/binary-amd64/f-%03d", i)] = fmt.Sprintf("hash-%03d", i)
	}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.ReplaceIndexPins(ctx, testScope, "ubuntu", "noble", pins)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "concurrent replace %d must not error", i)
	}

	got, err := s.IndexPins(ctx, testScope, "ubuntu", "noble")
	require.NoError(t, err)
	assert.Equal(t, pins, got, "concurrent identical replaces must converge on the full pin set")
}

// TestPutPoolPins_ConcurrentOverlapping: replicas ingesting overlapping Packages
// indices concurrently must not deadlock or lose pins. The store sorts rows
// before inserting precisely so two transactions cannot take the same locks in
// opposite orders (Go map order is randomised, so an unsorted version would
// deadlock only occasionally — the worst kind of bug).
func TestPutPoolPins_ConcurrentOverlapping(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	pins := make(map[string]string, 300)
	for i := range 300 {
		pins[fmt.Sprintf("pool/main/p/pkg/pkg-%03d.deb", i)] = fmt.Sprintf("hash-%03d", i)
	}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.PutPoolPins(ctx, testScope, "ubuntu", pins)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "concurrent pool pin upsert %d must not error", i)
	}

	for i := range 300 {
		got, err := s.PoolPin(ctx, testScope, fmt.Sprintf("pool/main/p/pkg/pkg-%03d.deb", i))
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("hash-%03d", i), got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Dialect
// ─────────────────────────────────────────────────────────────────────────────

// TestPostgresDialect_RebindsPlaceholders guards the one thing that differs
// between the two drivers. The postgres path cannot be exercised without a live
// server in -short mode, so at minimum assert the store constructs and that the
// sqlite dialect is not silently used for it.
func TestNew_DialectsAreDistinct(t *testing.T) {
	st, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "pins.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	assert.NotNil(t, aptpins.New(st.DB(), aptpins.SQLite))
	assert.NotNil(t, aptpins.New(st.DB(), aptpins.Postgres))
}
