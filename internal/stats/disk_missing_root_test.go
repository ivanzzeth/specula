package stats

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// TestDuBytes_MissingRoot_IsAnError pins the honesty rule that e181e5a
// established for git's object count and that dto.go states outright:
// render "—", never a fabricated zero.
//
// duBytes walks its root with filepath.WalkDir and swallows every callback
// error (`return nil //nolint:nilerr`) so the walk survives one unreadable
// subdirectory. But WalkDir reports a MISSING ROOT through that same callback:
// it calls fn(root, nil, err) once and, because the callback returns nil,
// WalkDir returns nil too. So a root that does not exist is indistinguishable
// from an empty one, and duBytes answers (0, nil) — a measurement.
//
// "0 bytes" is a claim: we looked, and there is nothing cached. For a root that
// is not there we have not looked at all; the honest answer is "unknown", which
// is an error the caller can skip on (both call sites already do), leaving the
// protocol out of the map so the UI renders "—".
func TestDuBytes_MissingRoot_IsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-git-mirror-dir")

	n, err := duBytes(missing)

	require.Error(t, err,
		"duBytes(%q) on a non-existent root must report that it could not measure, "+
			"not report a measurement of zero: a missing directory rendering as "+
			"0 B is the same fabricated zero e181e5a removed from git's objects count",
		missing)
	assert.Zero(t, n, "no bytes may be claimed alongside an error")
}

// TestDuBytes_EmptyRoot_IsAHonestZero is the other half of the contract: a root
// that EXISTS and is empty genuinely measures zero. The fix must distinguish
// "nothing there" from "nowhere to look", not blanket-error every zero.
func TestDuBytes_EmptyRoot_IsAHonestZero(t *testing.T) {
	n, err := duBytes(t.TempDir())

	require.NoError(t, err, "an existing empty directory is measurable")
	assert.Zero(t, n)
}

// TestByProtocol_MissingOpaqueRoot_OmitsProtocol is the user-visible consequence:
// a git bare-mirror root that has not been created yet (no clone has happened)
// must NOT surface as `git: 0 B` in GET /api/v1/admin/stats. It must be absent
// from the map, which is how the dashboard knows to render "—".
func TestByProtocol_MissingOpaqueRoot_OmitsProtocol(t *testing.T) {
	c := newTestCollector(&fakeStore{stats: map[string]artifact.SizeStat{}})
	missing := filepath.Join(t.TempDir(), "never-cloned")
	c.AddOpaquePath(missing, "git")

	got, err := c.ByProtocol(context.Background())
	require.NoError(t, err)

	_, present := got["git"]
	assert.False(t, present,
		"a git mirror root that does not exist must be omitted (unknown), not "+
			"reported as a measured 0 B; got %+v", got)
}
