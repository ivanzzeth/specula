// Package e2e — hermetic end-to-end tests for the Specula git clone-acceleration
// handler.
//
// Every test runs entirely in-process using:
//   - A real local bare git repository as the fake upstream.
//   - An httptest.Server that serves Smart HTTP (git-upload-pack) from that
//     bare repo via git http-backend CGI — the same mechanism Specula uses
//     internally.
//   - A Specula git.Handler pointed at a temp mirror directory and the fake
//     upstream host.
//   - Real `git clone` commands against the Specula server.
//
// # What is tested
//
//   - TestGitCloneObjectsCached: first clone hits the upstream (bare mirror is
//     created); second clone is served from the mirror with zero upstream
//     contacts.
//   - TestGitForcePushDetection: after a force-push on the upstream, re-sync
//     detects a non-fast-forward ref update and logs a TOFU alert.
//
// # Prerequisites
//
// The `git` binary must be on PATH. Tests are skipped automatically when git
// is not available.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	githandler "github.com/ivanzzeth/specula/internal/handler/git"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
)

// requireGit skips the test if the `git` binary is not on PATH.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not on PATH; skipping hermetic git e2e test")
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// runGitCmd runs a git subcommand and fails the test on error.
func runGitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Specula-Test",
		"GIT_AUTHOR_EMAIL=test@specula.local",
		"GIT_COMMITTER_NAME=Specula-Test",
		"GIT_COMMITTER_EMAIL=test@specula.local",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// headSHA returns the commit SHA of HEAD in the given repository (working copy
// or bare repo).
func headSHA(t *testing.T, repoPath string) string {
	t.Helper()
	return runGitCmd(t, repoPath, "rev-parse", "HEAD")
}

// gitCGIHandler returns an http.Handler that serves a bare git repo located at
// projectRoot via git http-backend CGI. The requests are counted atomically via
// hitCounter.
func gitCGIHandler(t *testing.T, projectRoot string, hitCounter *int64) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(hitCounter, 1)
		if err := gitHTTPBackendCGI(w, r, projectRoot, r.URL.Path); err != nil {
			t.Logf("fake upstream: git http-backend error: %v", err)
			http.Error(w, fmt.Sprintf("git http-backend: %v", err),
				http.StatusInternalServerError)
		}
	})
}

// gitHTTPBackendCGI invokes `git http-backend` as a subprocess and writes the
// CGI response to w. This is a self-contained copy of the handler's internal
// serve.go logic so the e2e test has no import of unexported symbols.
func gitHTTPBackendCGI(w http.ResponseWriter, r *http.Request, projectRoot, pathInfo string) error {
	cmd := exec.Command("git", "http-backend")
	var stdout, stderr bytes.Buffer
	cmd.Env = append(os.Environ(),
		"GIT_HTTP_EXPORT_ALL=1",
		"GIT_PROJECT_ROOT="+projectRoot,
		"PATH_INFO="+pathInfo,
		"QUERY_STRING="+r.URL.RawQuery,
		"REQUEST_METHOD="+r.Method,
		"SERVER_PROTOCOL=HTTP/1.1",
		"CONTENT_TYPE="+r.Header.Get("Content-Type"),
		"CONTENT_LENGTH="+r.Header.Get("Content-Length"),
	)
	cmd.Stdin = r.Body
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("git http-backend: %s", msg)
	}
	return writeCGIResp(w, stdout.Bytes())
}

func writeCGIResp(w http.ResponseWriter, raw []byte) error {
	headerEnd := bytes.Index(raw, []byte("\r\n\r\n"))
	sepLen := 4
	if headerEnd < 0 {
		headerEnd = bytes.Index(raw, []byte("\n\n"))
		sepLen = 2
	}
	if headerEnd < 0 {
		w.WriteHeader(http.StatusOK)
		_, err := w.Write(raw)
		return err
	}
	headerBlock := string(raw[:headerEnd])
	body := raw[headerEnd+sepLen:]

	status := http.StatusOK
	headers := make(http.Header)
	for _, line := range strings.Split(headerBlock, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(line), "status:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				var code int
				if _, err := fmt.Sscanf(fields[1], "%d", &code); err == nil {
					status = code
				}
			}
			continue
		}
		if i := strings.IndexByte(line, ':'); i > 0 {
			headers.Add(strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]))
		}
	}
	for k, vs := range headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	if len(body) > 0 {
		_, err := io.Copy(w, bytes.NewReader(body))
		return err
	}
	return nil
}

// setupFakeRepo creates:
//
//  1. fakeGitRoot/<repoName>.git — bare upstream repo.
//  2. A working clone of that repo with an initial commit on the default branch.
//  3. An httptest.Server serving the bare repo via git http-backend.
//
// Returns (fakeGitRoot, workDir, bareDir, server, hitCounterPtr).
func setupFakeRepo(t *testing.T, repoName string) (
	fakeGitRoot string, workDir string, bareDir string,
	srv *httptest.Server, hits *int64,
) {
	t.Helper()

	// 1. Bare upstream.
	fakeGitRoot = t.TempDir()
	bareDir = filepath.Join(fakeGitRoot, repoName+".git")
	runGitCmd(t, "", "init", "--bare", bareDir)
	// Allow force-push (needed for TestGitForcePushDetection).
	runGitCmd(t, bareDir, "config", "receive.denyNonFastForwards", "false")

	// 2. Working copy with an initial commit.
	workDir = t.TempDir()
	runGitCmd(t, "", "clone", bareDir, workDir)
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, "README.md"), []byte("# "+repoName+"\n"), 0o644))
	runGitCmd(t, workDir, "add", ".")
	runGitCmd(t, workDir, "commit", "-m", "initial commit")

	// Detect the default branch name (varies by git version: main / master).
	branch := runGitCmd(t, workDir, "rev-parse", "--abbrev-ref", "HEAD")
	runGitCmd(t, workDir, "push", "origin", "HEAD:"+branch)

	// 3. Fake HTTP upstream.
	var hitCount int64
	hits = &hitCount
	mux := http.NewServeMux()
	mux.Handle("/", gitCGIHandler(t, fakeGitRoot, hits))
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return
}

// ─── Test 1 — Clone caching ──────────────────────────────────────────────────

// TestGitCloneObjectsCached verifies:
//
//  1. First `git clone` through Specula creates a bare mirror on disk.
//  2. Upstream receives at least one HTTP request (for git clone --mirror).
//  3. Second `git clone` through Specula is served from the mirror.
//  4. Upstream receives NO additional HTTP requests for the second clone.
func TestGitCloneObjectsCached(t *testing.T) {
	requireGit(t)

	fakeGitRoot, workDir, bareDir, fakeSrv, hits := setupFakeRepo(t, "myrepo")
	_ = fakeGitRoot
	_ = workDir
	_ = bareDir

	fakeHost := strings.TrimPrefix(fakeSrv.URL, "http://")
	mirrorDir := t.TempDir()

	h := githandler.NewHandler(
		githandler.WithAllowedUpstreams([]string{fakeHost}),
		githandler.WithMirrorDir(mirrorDir),
		githandler.WithPublicOnly(false),              // no public probe for localhost
		githandler.WithFailClosed(false),              // don't fall back if probe hypothetically fails
		githandler.WithSyncStaleAfter(30*time.Second), // long enough to keep second clone fresh
		githandler.WithUpstreamScheme("http"),         // plain HTTP for tests
	)

	speculaSrv := httptest.NewServer(h)
	t.Cleanup(speculaSrv.Close)

	// Clone URL: http://specula/<fakeHost>/myrepo.git
	cloneURL := speculaSrv.URL + "/" + fakeHost + "/myrepo.git"

	// ── First clone (cold mirror) ─────────────────────────────────────────
	cloneDir1 := t.TempDir()
	runGitCmd(t, "", "clone", cloneURL, cloneDir1)
	hitAfterFirst := atomic.LoadInt64(hits)
	assert.Positive(t, hitAfterFirst,
		"first clone must cause at least one upstream HTTP request (mirror creation)")

	// Mirror directory must exist on disk.
	expectedMirror := filepath.Join(mirrorDir, fakeHost, "myrepo.git")
	info, err := os.Stat(expectedMirror)
	require.NoError(t, err, "mirror directory must exist after first clone")
	assert.True(t, info.IsDir(), "mirror path must be a directory")

	// Cloned working copy must have the initial commit.
	logOut := runGitCmd(t, cloneDir1, "log", "--oneline")
	assert.Contains(t, logOut, "initial commit",
		"cloned repo must contain the initial commit")

	// ── Second clone (warm mirror — within syncStaleAfter window) ─────────
	cloneDir2 := t.TempDir()
	runGitCmd(t, "", "clone", cloneURL, cloneDir2)

	hitAfterSecond := atomic.LoadInt64(hits)
	assert.Equal(t, hitAfterFirst, hitAfterSecond,
		"second clone (within stale window) must NOT contact upstream — served from mirror")

	// Content of second clone must match first.
	log2 := runGitCmd(t, cloneDir2, "log", "--oneline")
	assert.Contains(t, log2, "initial commit",
		"second cloned repo must contain the same commit")
}

// ─── Test 2 — Force-push detection ───────────────────────────────────────────

// TestGitForcePushDetection verifies that after a force-push on the upstream:
//
//  1. Specula re-syncs the mirror (syncStaleAfter=1ms so every request triggers).
//  2. The TOFU pin for the branch ref is updated from the initial SHA to the
//     new SHA.
//  3. The MetadataStore records the updated pin, and the old pin differs from
//     the new pin (proving the force-push was detected).
func TestGitForcePushDetection(t *testing.T) {
	requireGit(t)

	_, workDir, bareDir, fakeSrv, _ := setupFakeRepo(t, "fptest")
	_ = bareDir

	fakeHost := strings.TrimPrefix(fakeSrv.URL, "http://")
	mirrorDir := t.TempDir()

	// SQLite store for inspecting TOFU pins.
	dbPath := filepath.Join(t.TempDir(), "specula.db")
	ms, err := sqlite.NewSQLiteStore(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ms.Close() })

	h := githandler.NewHandler(
		githandler.WithAllowedUpstreams([]string{fakeHost}),
		githandler.WithMirrorDir(mirrorDir),
		githandler.WithMeta(ms),
		githandler.WithPublicOnly(false),
		githandler.WithFailClosed(false),
		githandler.WithSyncStaleAfter(time.Millisecond), // trigger re-sync on every request
		githandler.WithUpstreamScheme("http"),
	)

	speculaSrv := httptest.NewServer(h)
	t.Cleanup(speculaSrv.Close)

	cloneURL := speculaSrv.URL + "/" + fakeHost + "/fptest.git"

	// ── First clone: set up initial TOFU pins. ────────────────────────────
	cloneDir1 := t.TempDir()
	runGitCmd(t, "", "clone", cloneURL, cloneDir1)

	// Detect the default branch so we know which TOFU key to check.
	branch := runGitCmd(t, cloneDir1, "rev-parse", "--abbrev-ref", "HEAD")
	initialSHA := headSHA(t, cloneDir1)
	t.Logf("initial clone: branch=%s SHA=%s", branch, initialSHA)

	// ── Inspect initial TOFU pin ──────────────────────────────────────────
	ctx := context.Background()
	tofuKey := "git:tofu:" + fakeHost + "/fptest:" + "refs/heads/" + branch
	me, err := ms.GetMutable(ctx, tofuKey)
	require.NoError(t, err)
	require.NotNil(t, me, "TOFU pin must exist after first clone")
	require.Equal(t, initialSHA, me.Digest,
		"TOFU pin must equal the initial SHA of %s", branch)
	t.Logf("TOFU pin set: %s → %s", tofuKey, me.Digest)

	// ── Force-push: amend the last commit on the upstream. ────────────────
	// Add a new file and amend the initial commit (changes its SHA).
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, "extra.txt"), []byte("amended\n"), 0o644))
	runGitCmd(t, workDir, "add", ".")
	// Use --no-edit to keep the original commit message.
	runGitCmd(t, workDir, "commit", "--amend", "--no-edit")
	runGitCmd(t, workDir, "push", "--force", "origin", "HEAD:"+branch)
	forcedSHA := headSHA(t, workDir)
	require.NotEqual(t, initialSHA, forcedSHA,
		"amend must produce a different SHA")
	t.Logf("force-pushed: new SHA=%s", forcedSHA)

	// ── Sleep to ensure syncStaleAfter has elapsed. ───────────────────────
	time.Sleep(10 * time.Millisecond)

	// ── Second clone: triggers re-sync which detects the force-push. ──────
	cloneDir2 := t.TempDir()
	runGitCmd(t, "", "clone", cloneURL, cloneDir2)

	// The clone must reflect the new (force-pushed) state.
	gotSHA := headSHA(t, cloneDir2)
	require.Equal(t, forcedSHA, gotSHA,
		"second clone must reflect the force-pushed state")

	// ── Verify TOFU pin was updated to the new SHA. ───────────────────────
	me2, err := ms.GetMutable(ctx, tofuKey)
	require.NoError(t, err)
	require.NotNil(t, me2, "TOFU pin must still exist after re-sync")
	assert.Equal(t, forcedSHA, me2.Digest,
		"TOFU pin must be updated to the force-pushed SHA")
	assert.NotEqual(t, initialSHA, me2.Digest,
		"TOFU pin must differ from initial SHA (force-push was detected and pin updated)")
	t.Logf("TOFU pin updated: %s → %s (was %s)", tofuKey, me2.Digest, initialSHA)
}
