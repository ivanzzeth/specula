package admin

// stats_honesty_test.go — ALSO items: the control-plane wire contract must not
// fabricate numbers or invent trust tiers.
//
//  1. git reports `bytes` but `objects: 0` while being absent from
//     specula_cache_objects. objects:0 claims "we counted, there are none"; the
//     metric's absence says "not applicable". Both describe the same cache. The
//     honest rendering is null (UI: "—"), matching the rule dto.go already states
//     for UpstreamHealth's companion flags.
//  2. git's tier rendered as "mirror" — a fifth label outside the documented
//     four-tier model (PRD §G2: signed > consensus > tofu > checksum) — while
//     startup logs "git tops out at tofu".

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// TestHandleStats_OpaqueProtocol_ObjectsNull pins the git case: bytes reported,
// objects explicitly null — never a fabricated zero.
func TestHandleStats_OpaqueProtocol_ObjectsNull(t *testing.T) {
	h := newHarness(t)
	_, tok := h.mustCreateAdmin(t)

	// git: an opaque bare-mirror cache — bytes are du-measured, objects unknown.
	h.stats.byProto["git"] = artifact.SizeStat{Bytes: 4096, ObjectsCountable: false}

	rr := h.do("GET", "/api/v1/admin/stats", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp StatsResponse
	decodeJSON(t, rr, &resp)

	var git, oci *ProtocolStat
	for i := range resp.PerProtocol {
		switch resp.PerProtocol[i].Protocol {
		case "git":
			git = &resp.PerProtocol[i]
		case "oci":
			oci = &resp.PerProtocol[i]
		}
	}
	require.NotNil(t, git, "git must be reported — it has cached bytes")
	require.NotNil(t, oci)

	assert.Equal(t, int64(4096), git.Bytes, "git bytes are measurable and must be reported")
	assert.Nil(t, git.Objects,
		"git objects live in packfiles inside an opaque bare mirror and are not countable. "+
			"objects:0 fabricates a zero and contradicts specula_cache_objects, which omits "+
			"git entirely. null renders as '—'.")

	// The CAS protocol keeps a real number — the flag must not blanket-null everything.
	require.NotNil(t, oci.Objects, "CAS-backed protocols still report an exact count")
	assert.Equal(t, int64(3), *oci.Objects)
}

// TestHandleStats_ObjectsNull_SerialisesAsJSONNull guards the wire format itself:
// a *int64 must marshal to `null`, not be omitted or coerced to 0.
func TestHandleStats_ObjectsNull_SerialisesAsJSONNull(t *testing.T) {
	h := newHarness(t)
	_, tok := h.mustCreateAdmin(t)
	h.stats.byProto["git"] = artifact.SizeStat{Bytes: 4096, ObjectsCountable: false}

	rr := h.do("GET", "/api/v1/admin/stats", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var raw struct {
		PerProtocol []map[string]json.RawMessage `json:"per_protocol"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))

	found := false
	for _, row := range raw.PerProtocol {
		if string(row["protocol"]) != `"git"` {
			continue
		}
		found = true
		v, present := row["objects"]
		require.True(t, present, "the objects key must be present (explicitly null), not omitted")
		assert.JSONEq(t, `null`, string(v), "objects must serialise as null for an opaque cache")
	}
	require.True(t, found, "git row must be present")
}

// TestListGitMirrors_TierIsNotAFifthTier pins the tier label: a bare-mirror row
// must not invent a tier outside the documented four, and must not claim one it
// has not earned.
//
// This mirror has no TOFU pins recorded against it, so it has earned nothing and
// must report nothing.
//
// NOTE: this test previously asserted the tier is ALWAYS empty, on the reasoning
// that "a bare-mirror row is a repository directory, not a verified artifact".
// That reasoning deleted the fabricated "mirror" tier correctly but overshot:
// when ref→SHA pins DO exist for a repo, force-push / history-rewrite detection
// is live for it, which is precisely PRD §G2's tofu tier, and reporting "" then
// under-claims a real guarantee. Empty is the right answer for an UNPINNED repo,
// which is what this test now says. The pinned case is
// TestListGitMirrors_ReportsTofuTierWhenPinned.
func TestListGitMirrors_TierIsNotAFifthTier(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "github.com", "alice", "hello.git"), 0o755))

	entries, err := listGitMirrors(context.Background(), &fakeMetaStore{}, dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	assert.NotEqual(t, "mirror", entries[0].Tier,
		`"mirror" is a fifth tier outside PRD §G2's four-tier model`)
	assert.Empty(t, entries[0].Tier,
		"no pins recorded against this mirror → no verdict earned → no tier claimed")
}
