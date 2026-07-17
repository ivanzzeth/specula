package verify

// sumdb_rollback_test.go — BUG D regression: sumdb anti-rollback fail-closes on
// CN CDN edge lag (live-observed), bricking `go get` until the DB is wiped.
//
// Live observation: sum.golang.google.cn served tree size 57543803 while the
// persisted high-water — obtained from THE SAME HOST minutes earlier — was
// 57546088. A 2285-entry regression from the same origin is CDN edge lag, not an
// attack. Anti-rollback is correct in principle (DESIGN-REVIEW §2 H5) but as
// written it intermittently bricks CN users.
//
// Measured 2026-07-16 (see sumdb_client.go for the full reasoning):
//   - sum.golang.org grows ~15–80 entries/min
//   - GET https://sum.golang.google.cn/latest → `cache-control: public, max-age=300`
//     with `age: 168` — the CDN is EXPLICITLY permitted to serve a stale head.
//
// The policy must distinguish lag from attack, not stop caring.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

// signedTreeNote builds a signed sumdb tree-head note for an arbitrary tree size,
// so the policy can be exercised at the sizes actually observed in production.
func signedTreeNote(t *testing.T, signerKey string, n int64) []byte {
	t.Helper()
	signer, err := note.NewSigner(signerKey)
	require.NoError(t, err)
	var h tlog.Hash
	for i := range h {
		h[i] = byte(n >> (uint(i%8) * 8))
	}
	msg, err := note.Sign(&note.Note{Text: string(tlog.FormatTree(tlog.Tree{N: n, Hash: h}))}, signer)
	require.NoError(t, err)
	return msg
}

// Live-observed values from the incident.
const (
	observedHighWater = int64(57546088)
	observedLaggedN   = int64(57543803) // same host, minutes later — CDN edge lag
)

// TestSumDBWriteConfig_CDNEdgeLag_ToleratedWithWarn is the primary RED test for
// BUG D: the exact live-observed regression must NOT brick go get.
func TestSumDBWriteConfig_CDNEdgeLag_ToleratedWithWarn(t *testing.T) {
	db := newTestSumDB(t)
	store := newMemTreeSizeStore()
	ctx := context.Background()
	require.NoError(t, store.SetTreeSize(ctx, db.name, observedHighWater))

	ops, err := newSpecOps(db.vkeyText, db.httpSrv.URL, store, nil)
	require.NoError(t, err)

	err = ops.WriteConfig(db.name+"/latest", nil, signedTreeNote(t, db.signerKey, observedLaggedN))

	assert.NoError(t, err,
		"BUG D: a 2285-entry regression from the SAME host is CDN edge lag (the CDN serves "+
			"/latest with max-age=300 by design), not a rollback attack. Hard-failing here "+
			"bricks every CN `go get` until the DB is wiped.")
	assert.Nil(t, ops.securityError(),
		"CDN edge lag within the tolerance window must not be recorded as a security error")
}

// TestSumDBWriteConfig_LagDoesNotAdvanceHighWater: the ratchet must not slip. A
// tolerated lagged observation must never lower (or advance) the persisted
// high-water, otherwise a sustained freeze could walk the window backwards
// indefinitely, one tolerated step at a time.
func TestSumDBWriteConfig_LagDoesNotAdvanceHighWater(t *testing.T) {
	db := newTestSumDB(t)
	store := newMemTreeSizeStore()
	ctx := context.Background()
	require.NoError(t, store.SetTreeSize(ctx, db.name, observedHighWater))

	ops, err := newSpecOps(db.vkeyText, db.httpSrv.URL, store, nil)
	require.NoError(t, err)
	require.NoError(t, ops.WriteConfig(db.name+"/latest", nil, signedTreeNote(t, db.signerKey, observedLaggedN)))

	got, err := store.GetTreeSize(ctx, db.name)
	require.NoError(t, err)
	assert.Equal(t, observedHighWater, got,
		"the high-water mark is a ratchet: a tolerated lagged observation must never move it")
}

// TestSumDBWriteConfig_RealRollback_StillRejected is the anti-regression guard:
// the fix must NOT silently drop anti-rollback. A regression far beyond any
// plausible CDN lag is a rollback/freeze attack and must still hard-fail.
func TestSumDBWriteConfig_RealRollback_StillRejected(t *testing.T) {
	db := newTestSumDB(t)
	store := newMemTreeSizeStore()
	ctx := context.Background()
	require.NoError(t, store.SetTreeSize(ctx, db.name, observedHighWater))

	ops, err := newSpecOps(db.vkeyText, db.httpSrv.URL, store, nil)
	require.NoError(t, err)

	// Roll back by 5 million entries — months of log history.
	err = ops.WriteConfig(db.name+"/latest", nil, signedTreeNote(t, db.signerKey, observedHighWater-5_000_000))

	require.Error(t, err, "a rollback far beyond CDN lag must still fail closed (DESIGN-REVIEW §2 H5)")
	assert.Contains(t, err.Error(), "rollback")
	assert.NotNil(t, ops.securityError(), "a real rollback must be recorded as a security error")
}

// TestSumDBWriteConfig_JustOutsideWindow_Rejected pins the boundary: tolerance is
// bounded, not open-ended.
func TestSumDBWriteConfig_JustOutsideWindow_Rejected(t *testing.T) {
	db := newTestSumDB(t)
	store := newMemTreeSizeStore()
	ctx := context.Background()
	require.NoError(t, store.SetTreeSize(ctx, db.name, observedHighWater))

	ops, err := newSpecOps(db.vkeyText, db.httpSrv.URL, store, nil)
	require.NoError(t, err)

	err = ops.WriteConfig(db.name+"/latest", nil,
		signedTreeNote(t, db.signerKey, observedHighWater-defaultRollbackToleranceEntries-1))
	require.Error(t, err, "one entry beyond the tolerance window must fail closed")
}

// TestSumDBWriteConfig_AtWindowEdge_Tolerated pins the other side of the boundary.
func TestSumDBWriteConfig_AtWindowEdge_Tolerated(t *testing.T) {
	db := newTestSumDB(t)
	store := newMemTreeSizeStore()
	ctx := context.Background()
	require.NoError(t, store.SetTreeSize(ctx, db.name, observedHighWater))

	ops, err := newSpecOps(db.vkeyText, db.httpSrv.URL, store, nil)
	require.NoError(t, err)

	err = ops.WriteConfig(db.name+"/latest", nil,
		signedTreeNote(t, db.signerKey, observedHighWater-defaultRollbackToleranceEntries))
	assert.NoError(t, err, "exactly at the tolerance window edge must be tolerated")
}

// TestSumDBWriteConfig_StrictMode_ZeroTolerance: an operator who wants the old
// zero-tolerance behaviour must be able to have it.
func TestSumDBWriteConfig_StrictMode_ZeroTolerance(t *testing.T) {
	db := newTestSumDB(t)
	store := newMemTreeSizeStore()
	ctx := context.Background()
	require.NoError(t, store.SetTreeSize(ctx, db.name, observedHighWater))

	ops, err := newSpecOps(db.vkeyText, db.httpSrv.URL, store, nil)
	require.NoError(t, err)
	ops.rollbackTolerance = 0 // strict

	err = ops.WriteConfig(db.name+"/latest", nil, signedTreeNote(t, db.signerKey, observedHighWater-1))
	require.Error(t, err, "strict mode (tolerance 0) must reject any regression at all")
}

// TestSumDBWriteConfig_Advance_PersistsHighWater: normal forward progress still
// ratchets the high-water.
func TestSumDBWriteConfig_Advance_PersistsHighWater(t *testing.T) {
	db := newTestSumDB(t)
	store := newMemTreeSizeStore()
	ctx := context.Background()
	require.NoError(t, store.SetTreeSize(ctx, db.name, observedHighWater))

	ops, err := newSpecOps(db.vkeyText, db.httpSrv.URL, store, nil)
	require.NoError(t, err)

	next := observedHighWater + 500
	require.NoError(t, ops.WriteConfig(db.name+"/latest", nil, signedTreeNote(t, db.signerKey, next)))

	got, err := store.GetTreeSize(ctx, db.name)
	require.NoError(t, err)
	assert.Equal(t, next, got, "a newer tree head must advance the high-water mark")
}
