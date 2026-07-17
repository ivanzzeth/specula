package admin

// git_stats_test.go — two git honesty defects on the control plane:
//
//  1. git bytes are invisible to /admin/stats and Prometheus until a human
//     browses the cache-browser UI, because GET /admin/cache/git is the only
//     production caller of AddOpaquePath. A headless replica scraped by
//     Prometheus never reports git at all. Measurement that only exists if
//     someone happens to look is not measurement.
//  2. git mirror rows report tier="" while TOFU is really being enforced —
//     ref→SHA pins are recorded and force-push detection is live. Under-claiming
//     is still mis-reporting.
//
// These tests use the REAL stats collector, not fakeStatsCollector: the fake's
// AddOpaquePath is a no-op and its ByProtocol returns whatever the test preset,
// so it answers "yes, git is there" no matter what the production wiring does —
// it cannot fail for defect 1 and is therefore part of the bug.

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/config"
	githandler "github.com/ivanzzeth/specula/internal/handler/git"
	"github.com/ivanzzeth/specula/internal/stats"
)

// newGitMirrorTree builds a mirror root containing one REAL bare mirror of a
// real repository, at the path layout the git handler uses, and returns the root.
//
// It is a real repo, not a directory of stub files: the tier under test is
// derived by enumerating the mirror's refs with `git for-each-ref` and looking
// each one up in the MetadataStore. A fake tree has no refs, so it would report
// "no tier" for the same reason an unpinned repo does — the test would pass
// against a broken implementation and fail against a correct one, telling us
// nothing either way. (This bit: the first version of this fixture was fake and
// the tier assertion failed against the working code.)
func newGitMirrorTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	// A real upstream with one commit on master.
	work := t.TempDir()
	runGit(t, work, "init", "--quiet", "--initial-branch=master", work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "README"), []byte("hi\n"), 0o644))
	runGit(t, work, "add", "README")
	runGit(t, work, "commit", "--quiet", "-m", "initial")

	mirror := filepath.Join(root, "github.com", "octocat", "Hello-World.git")
	require.NoError(t, os.MkdirAll(filepath.Dir(mirror), 0o755))
	runGit(t, work, "clone", "--bare", "--quiet", "--", work, mirror)
	return root
}

// runGit runs git in dir, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, out)
}

// newGitHarness builds an admin Server exactly the way a fresh headless process
// does — git configured, real stats collector, and NOBODY browsing the UI.
func newGitHarness(t *testing.T, mirrorDir string, meta *fakeMetaStore) *harness {
	t.Helper()
	store := newFakeUserStore()
	verifier := auth.NewHS256Verifier([]byte("test-secret-32-bytes-minimum!!!"))
	hasher := &fakeHasher{}
	svc := auth.NewService(store, hasher, verifier, false, nil)

	cfg := testConfig()
	cfg.Protocols["git"] = config.ProtocolConfig{
		Git: &config.GitConfig{
			MirrorDir:        mirrorDir,
			AllowedUpstreams: []string{"github.com"},
		},
	}

	srv := New(Deps{
		Stats:  stats.NewCollector(), // the real thing
		Meta:   meta,
		Users:  store,
		Auth:   svc,
		Tokens: verifier,
		Config: cfg,
		Blobs:  &fakeBlobReporter{usedBytes: 999},
		Secure: false,
	})
	srv.hasher = hasher

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return &harness{srv: srv, mux: mux, store: store, verifier: verifier, hasher: hasher}
}

// TestStats_GitBytesReportedWithoutBrowsingTheUI is the RED test for defect 1:
// on a fresh process, with no request ever made to /admin/cache/git, the git
// bare mirror's bytes must already be reported by /admin/stats.
//
// Today the only production caller of AddOpaquePath is the cache-browser
// handler, so this passes only after a human clicks the git tab.
func TestStats_GitBytesReportedWithoutBrowsingTheUI(t *testing.T) {
	h := newGitHarness(t, newGitMirrorTree(t), &fakeMetaStore{})
	_, tok := h.mustCreateAdmin(t)

	// NOTE: no GET /api/v1/admin/cache/git here — that is the whole point.
	rr := h.do("GET", "/api/v1/admin/stats", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp StatsResponse
	decodeJSON(t, rr, &resp)

	var git *ProtocolStat
	for i := range resp.PerProtocol {
		if resp.PerProtocol[i].Protocol == "git" {
			git = &resp.PerProtocol[i]
		}
	}
	require.NotNil(t, git,
		"git is absent from /admin/stats on a fresh process. A headless replica scraped "+
			"by Prometheus never reports git at all: opaque-cache registration must happen "+
			"when the control plane is constructed, not as a side effect of a human "+
			"browsing the cache-browser UI.")
	assert.Positive(t, git.Bytes, "the git mirror tree has real bytes on disk")
}

// TestStats_GitBytesUnaffectedByBrowsing pins the other half: registration is
// idempotent, so browsing the UI must not double-count the same bytes.
func TestStats_GitBytesUnaffectedByBrowsing(t *testing.T) {
	h := newGitHarness(t, newGitMirrorTree(t), &fakeMetaStore{})
	_, tok := h.mustCreateAdmin(t)

	before := gitBytes(t, h, tok)
	require.Equal(t, http.StatusOK, h.do("GET", "/api/v1/admin/cache/git", tok, nil).Code)
	after := gitBytes(t, h, tok)

	assert.Equal(t, before, after, "browsing the cache UI must not change the byte total")
}

func gitBytes(t *testing.T, h *harness, tok string) int64 {
	t.Helper()
	rr := h.do("GET", "/api/v1/admin/stats", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp StatsResponse
	decodeJSON(t, rr, &resp)
	for _, p := range resp.PerProtocol {
		if p.Protocol == "git" {
			return p.Bytes
		}
	}
	return 0
}

// ─── defect 2: the tier git actually earns ───────────────────────────────────

// TestListGitMirrors_ReportsTofuTierWhenPinned is the RED test: when TOFU pins
// exist for a mirrored repo, force-push detection IS live for it, and the entry
// must say so. tier="" claims no verification verdict at all.
//
// Per PRD §G2 the pin + change-alert mechanism is exactly `tofu`. It is not
// `signed` — that would need the signed tag/commit anchor — and it is emphatically
// not a fifth label like "mirror".
func TestListGitMirrors_ReportsTofuTierWhenPinned(t *testing.T) {
	mirrorDir := newGitMirrorTree(t)
	ms := &fakeMetaStore{}

	// The git handler pinned this repo's refs on the last sync: TOFU is live.
	require.NoError(t, ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:      githandler.RefTOFUKeyFor("github.com/octocat/Hello-World", "refs/heads/master"),
		Protocol: "git",
		Digest:   "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d",
	}))

	h := newGitHarness(t, mirrorDir, ms)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/admin/cache/git", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp CacheEntriesResponse
	decodeJSON(t, rr, &resp)
	require.Len(t, resp.Entries, 1)

	assert.Equal(t, artifact.TierTofu.String(), resp.Entries[0].Tier,
		"the repo has ref→SHA pins recorded, so force-push/history-rewrite detection is "+
			"live for it — that is precisely what PRD §G2 calls the tofu tier. Reporting "+
			"tier=\"\" under-claims a guarantee we actually provide.")
}

// TestListGitMirrors_NoTierWithoutPins pins the honest floor: a mirror with no
// pins recorded has earned nothing, and must not claim tofu.
func TestListGitMirrors_NoTierWithoutPins(t *testing.T) {
	h := newGitHarness(t, newGitMirrorTree(t), &fakeMetaStore{})
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/admin/cache/git", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp CacheEntriesResponse
	decodeJSON(t, rr, &resp)
	require.Len(t, resp.Entries, 1)

	assert.Empty(t, resp.Entries[0].Tier,
		"no pins recorded → no verdict earned → no tier claimed")
}
