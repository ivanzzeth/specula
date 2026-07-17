package verify

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// The default (in-memory) AptPinStore must fail closed when one pool path is
// pinned to DIFFERENT hashes by different repos under the same trust anchor.
//
// A pool ref carries no repo (the handler drops it), so lookup is
// (scope, pool_path). If two repos signed by the same keyring disagree about
// what bytes that path commits to, we cannot tell which signature vouches for
// the artifact we are about to serve — so we must refuse, not pick one. Picking
// one would let repo A's InRelease vouch for repo B's bytes.
//
// This test exists because gremlins found the guard at aptpins.go:174 SURVIVED
// mutation: negating the condition failed nothing. The SQL store's identical
// guard was mutation-proofed by hand when it was written; the default store was
// not. That is the structural weakness of hand-picked mutation proofs — the
// author covers the path they are thinking about.
func TestMemAptPinStore_AmbiguousPoolPinFailsClosed(t *testing.T) {
	ctx := context.Background()
	s := NewMemAptPinStore()
	const scope, pool = "anchor-1", "pool/main/h/hello/hello_2.10_amd64.deb"

	require.NoError(t, s.PutPoolPins(ctx, scope, "repoA", map[string]string{pool: "aaaa"}))
	require.NoError(t, s.PutPoolPins(ctx, scope, "repoB", map[string]string{pool: "bbbb"}))

	_, err := s.PoolPin(ctx, scope, pool)
	require.ErrorIs(t, err, ErrAmbiguousPoolPin,
		"two repos pinning one pool path to different hashes must REFUSE, not silently pick one")
}

// The unambiguous case must still resolve: two repos agreeing on the same hash
// is not a conflict. Without this, "fail closed" could be implemented as "always
// fail" and the test above would still pass.
func TestMemAptPinStore_AgreeingReposResolve(t *testing.T) {
	ctx := context.Background()
	s := NewMemAptPinStore()
	const scope, pool = "anchor-1", "pool/main/h/hello/hello_2.10_amd64.deb"

	require.NoError(t, s.PutPoolPins(ctx, scope, "repoA", map[string]string{pool: "aaaa"}))
	require.NoError(t, s.PutPoolPins(ctx, scope, "repoB", map[string]string{pool: "aaaa"}))

	got, err := s.PoolPin(ctx, scope, pool)
	require.NoError(t, err)
	require.Equal(t, "aaaa", got)
}
