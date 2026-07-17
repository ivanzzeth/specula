package stats

// git_objects_test.go — ALSO item: git reports `bytes` but `objects: 0`, and is
// absent from `specula_cache_objects` while present in `specula_cache_bytes`.
//
// The two surfaces disagree: the metric says "no data for git" (absent) while the
// JSON API says "git has zero objects" (a fabricated zero). git objects live in
// packfiles inside an opaque bare mirror — they are not CAS cache entries and are
// not countable by the collector. The honest contract is an explicit N/A flag,
// matching the convention dto.go already states: "Render '—', never a fabricated
// zero."

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newGitMirrorDir creates a fake bare-mirror tree with some bytes on disk.
func newGitMirrorDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	objDir := filepath.Join(dir, "github.com", "alice", "hello.git", "objects", "pack")
	require.NoError(t, os.MkdirAll(objDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(objDir, "pack-abc.pack"), make([]byte, 4096), 0o644))
	return dir
}

// TestByProtocol_OpaqueGitMirror_ObjectsNotCountable is the RED test: an
// opaque-cache protocol must report its object count as explicitly NOT countable,
// rather than as a fabricated 0.
func TestByProtocol_OpaqueGitMirror_ObjectsNotCountable(t *testing.T) {
	c := newCollector(nil, prometheus.NewRegistry(), DefaultCollectorConfig())
	c.AddOpaquePath(newGitMirrorDir(t), "git")

	byProto, err := c.ByProtocol(context.Background())
	require.NoError(t, err)

	git, ok := byProto["git"]
	require.True(t, ok, "git must appear in ByProtocol (its bytes are du-computed)")
	assert.Positive(t, git.Bytes, "git bare-mirror bytes are countable and must be reported")

	assert.False(t, git.ObjectsCountable,
		"git objects live in packfiles inside an opaque bare mirror — they are not CAS "+
			"entries and the collector cannot count them. Reporting objects:0 fabricates a "+
			"zero and contradicts specula_cache_objects, which omits git entirely.")
}

// TestByProtocol_CASProtocol_ObjectsCountable pins the other side: a normal CAS
// protocol's object count IS countable and must stay so.
func TestByProtocol_CASProtocol_ObjectsCountable(t *testing.T) {
	c := newCollector(nil, prometheus.NewRegistry(), DefaultCollectorConfig())
	c.RecordPut(context.Background(), "oci", 1234)

	byProto, err := c.ByProtocol(context.Background())
	require.NoError(t, err)

	oci, ok := byProto["oci"]
	require.True(t, ok)
	assert.True(t, oci.ObjectsCountable, "CAS-backed protocols count objects exactly")
	assert.Equal(t, int64(1), oci.Objects)
}

// TestByProtocol_MixedCASAndOpaque_ObjectsNotCountable is the case that actually
// pins the annotation: a protocol with BOTH counted rows AND an opaque root.
//
// Without it, TestByProtocol_OpaqueGitMirror_ObjectsNotCountable passes vacuously
// — a protocol with no rows has a zero-value SizeStat whose ObjectsCountable is
// already false, so it cannot distinguish "explicitly marked unknown" from
// "nobody set it". Here the row count starts countable and MUST be demoted: once
// opaque bytes are merged in, any count is at best partial, and a partial count
// presented as exact is the same fabrication in a smaller costume.
func TestByProtocol_MixedCASAndOpaque_ObjectsNotCountable(t *testing.T) {
	c := newCollector(nil, prometheus.NewRegistry(), DefaultCollectorConfig())
	ctx := context.Background()

	// git has a counted CAS row ...
	require.NoError(t, c.RecordPut(ctx, "git", 100))
	// ... and an opaque bare-mirror root whose objects are not countable.
	c.AddOpaquePath(newGitMirrorDir(t), "git")

	byProto, err := c.ByProtocol(ctx)
	require.NoError(t, err)

	git := byProto["git"]
	assert.Equal(t, int64(100+4096), git.Bytes, "bytes from both sources must be summed")
	assert.False(t, git.ObjectsCountable,
		"once opaque bytes are merged, the object count is at best partial — it must be "+
			"reported as unknown, not as an exact number")
	assert.Zero(t, git.Objects, "an unknown count must not leak a partial number")
}
