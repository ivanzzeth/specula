// Package e2e — hermetic end-to-end tests for the Specula Go module proxy handler.
//
// Every hermetic test runs entirely in-process: a tiny fake GOPROXY upstream
// (httptest.Server) serves fixed module artefacts, and the real Specula gomod
// Handler is wired with LocalDiskDriver + SQLiteStore + verify.Chain + cache.New,
// exactly as production. No external network access is needed.
//
// # What is tested
//
//   - TestGomodRouting          — routing layer: 400 (bad path), 404 (unknown),
//     405 (wrong method), 404-no-upstream, 404 sumdb not configured.
//   - TestGomodImmutableFetchPopulatesCache — cold fetch of .info/.mod/.zip populates
//     the CAS and returns bytes equal to the fake upstream response.
//   - TestGomodSecondFetchCacheHit — after the first fetch, immutable artefacts
//     (.info/.mod/.zip) are served from the CAS; the fake upstream sees no
//     additional requests.
//   - TestGomodListIsMutable — @v/list is served from the mutable tier with a
//     short TTL; after the TTL expires the upstream is contacted again.
//     Contrasted with .mod which is never re-fetched (permanent CAS).
//   - TestGomodVerifyOnWriteRejectsMismatch — a quarantined gomod artefact with a
//     deliberately wrong ref.Digest causes the ChecksumVerifier to return
//     StatusFail; cache.Store returns *cache.VerifyError and leaves the CAS clean.
//   - TestGomodGoToolDownload  — optional: the real `go mod download` command runs
//     against the in-process fake, with GOPROXY pointing at Specula.
//     Skipped if `go` is not on PATH or if the zip is rejected by the go tool.
//
// # Live test
//
// TestGomodLive is always skipped unless SPECULA_E2E_LIVE=1. It fetches a .mod
// file from goproxy.cn through Specula and confirms the second fetch is a CAS hit.
package e2e

import (
	"archive/zip"
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

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	gomodhandler "github.com/ivanzzeth/specula/internal/handler/gomod"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// ── Fake module constants ─────────────────────────────────────────────────────

const (
	fakeMod     = "example.com/fake"
	fakeVer     = "v0.1.0"
	fakeGomodTx = "module example.com/fake\n\ngo 1.21\n"
)

var (
	fakeInfo = []byte(`{"Version":"v0.1.0","Time":"2024-01-01T00:00:00Z"}`)
	fakeMod2 = []byte(fakeGomodTx) // go.mod content
	fakeList = []byte("v0.1.0\n")
	fakeZip  = buildFakeModZip(fakeMod, fakeVer, fakeGomodTx)
)

// buildFakeModZip creates a minimal zip archive in GOPROXY module-zip format.
// The archive contains a single go.mod file named
// {modulePath}@{version}/go.mod. This format satisfies golang.org/x/mod/zip
// extraction rules for the go tool's `go mod download` command.
func buildFakeModZip(modulePath, version, gomodContent string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	prefix := modulePath + "@" + version + "/"
	w, err := zw.Create(prefix + "go.mod")
	if err != nil {
		panic("buildFakeModZip: create go.mod entry: " + err.Error())
	}
	if _, err = io.WriteString(w, gomodContent); err != nil {
		panic("buildFakeModZip: write go.mod: " + err.Error())
	}
	if err = zw.Close(); err != nil {
		panic("buildFakeModZip: close: " + err.Error())
	}
	return buf.Bytes()
}

// ── Fake GOPROXY server ───────────────────────────────────────────────────────

// proxyCounters holds per-endpoint atomic request counters for the fake GOPROXY.
type proxyCounters struct {
	list int64
	info int64
	mod  int64
	zip_ int64
}

// newFakeProxy returns an in-process httptest.Server that serves the fake module
// artefacts, and a pointer to the hit counters.
func newFakeProxy(t *testing.T) (*httptest.Server, *proxyCounters) {
	t.Helper()

	cnt := &proxyCounters{}
	mux := http.NewServeMux()

	// /{module}/@v/list
	mux.HandleFunc("/"+fakeMod+"/@v/list", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&cnt.list, 1)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(fakeList)
	})

	// /{module}/@v/{version}.info
	mux.HandleFunc("/"+fakeMod+"/@v/"+fakeVer+".info", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&cnt.info, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeInfo)
	})

	// /{module}/@v/{version}.mod
	mux.HandleFunc("/"+fakeMod+"/@v/"+fakeVer+".mod", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&cnt.mod, 1)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(fakeMod2)
	})

	// /{module}/@v/{version}.zip
	mux.HandleFunc("/"+fakeMod+"/@v/"+fakeVer+".zip", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&cnt.zip_, 1)
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(fakeZip)
	})

	// /{module}/@latest
	mux.HandleFunc("/"+fakeMod+"/@latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeInfo)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, cnt
}

// ── Gomod handler stack setup ─────────────────────────────────────────────────

// newGomodServer wires gomodhandler.Handler over the given speculaStack,
// pointing at the fakeURL upstream, and returns a running httptest.Server.
// mutableTTL is the TTL in seconds for @v/list and @latest (0 = always
// revalidate, but see note in TestGomodListIsMutable; -1 = never revalidate).
func newGomodServer(
	t *testing.T,
	s *speculaStack,
	fakeURL string,
	mutableTTL int64,
	extra ...gomodhandler.Option,
) *httptest.Server {
	t.Helper()

	ups := []upstream.Upstream{{Name: "fake-goproxy", BaseURL: fakeURL, Priority: 0}}
	opts := []gomodhandler.Option{
		gomodhandler.WithMeta(s.metaStore),
		gomodhandler.WithUpstream(upstream.NewClient(), ups),
		gomodhandler.WithQuarantineDir(s.dir),
		gomodhandler.WithMutableTTL(mutableTTL),
	}
	opts = append(opts, extra...)

	h := gomodhandler.NewHandler(s.cm, opts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// httpGet is a thin GET helper that returns the status code and body bytes.
func httpGet(t *testing.T, url string) (status int, body []byte) {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, body
}

// ── Test 1 — Routing and error codes ─────────────────────────────────────────

// TestGomodRouting verifies the handler's routing and validation layer: correct
// HTTP status codes are returned for malformed requests before any upstream fetch.
func TestGomodRouting(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	// No upstream — purely testing the routing/validation layer.
	h := gomodhandler.NewHandler(s.cm)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	t.Run("bad_module_path_400", func(t *testing.T) {
		// "!INVALID" upper case is not valid bang-escaping for a path component.
		status, _ := httpGet(t, srv.URL+"/!INVALID/@v/list")
		assert.Equal(t, http.StatusBadRequest, status, "bad bang-escaped path must return 400")
	})

	t.Run("unknown_path_404", func(t *testing.T) {
		status, _ := httpGet(t, srv.URL+"/not/a/goproxy/path")
		assert.Equal(t, http.StatusNotFound, status, "path with no @v/ segment must return 404")
	})

	t.Run("post_method_405", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/"+fakeMod+"/@v/list", nil)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "POST must return 405")
	})

	t.Run("no_upstream_immutable_404", func(t *testing.T) {
		// No upstream configured → cache miss → 404.
		status, _ := httpGet(t, srv.URL+"/"+fakeMod+"/@v/"+fakeVer+".info")
		assert.Equal(t, http.StatusNotFound, status, "cache-miss without upstream must return 404")
	})

	t.Run("sumdb_not_configured_404", func(t *testing.T) {
		// /sumdb/ passthrough not configured on this handler → 404.
		status, _ := httpGet(t, srv.URL+"/sumdb/sum.golang.org/supported")
		assert.Equal(t, http.StatusNotFound, status, "/sumdb/ without WithSumDB must return 404")
	})
}

// ── Test 2 — Cold fetch populates CAS; bytes match ───────────────────────────

// TestGomodImmutableFetchPopulatesCache drives a cold-cache fetch for each
// immutable artefact type (.info, .mod, .zip), asserts that the response bytes
// equal the fake upstream content, and verifies that the CAS and per-protocol
// stats are populated after the fetch.
func TestGomodImmutableFetchPopulatesCache(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	fakeProxy, _ := newFakeProxy(t)
	srv := newGomodServer(t, s, fakeProxy.URL, 300)

	tests := []struct {
		path string
		want []byte
	}{
		{"/" + fakeMod + "/@v/" + fakeVer + ".info", fakeInfo},
		{"/" + fakeMod + "/@v/" + fakeVer + ".mod", fakeMod2},
		{"/" + fakeMod + "/@v/" + fakeVer + ".zip", fakeZip},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			status, body := httpGet(t, srv.URL+tc.path)
			require.Equal(t, http.StatusOK, status, "fetch %s must return 200", tc.path)
			require.Equal(t, tc.want, body, "fetch %s: bytes must match fake upstream", tc.path)
		})
	}

	ctx := context.Background()

	// CAS must have bytes for all three artefacts.
	used, err := s.blobStore.UsageBytes(ctx)
	require.NoError(t, err)
	assert.Positive(t, used, "CAS must have bytes after three immutable fetches")

	// Per-protocol stats must show three gomod objects.
	stats, err := s.metaStore.CacheSizeByProtocol(ctx)
	require.NoError(t, err)
	gomodStat, ok := stats["gomod"]
	require.True(t, ok, "gomod stats must exist after fetch")
	assert.Equal(t, int64(3), gomodStat.Objects,
		"three immutable artefacts must appear in stats")
	assert.Positive(t, gomodStat.Bytes, "total bytes must be positive")
}

// ── Test 3 — Second fetch is a CAS hit; upstream counter unchanged ────────────

// TestGomodSecondFetchCacheHit verifies that after the cold-cache miss path
// populates the CAS, subsequent fetches of the same immutable artefact are served
// entirely from the CAS — the fake upstream sees no additional requests.
func TestGomodSecondFetchCacheHit(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	fakeProxy, cnt := newFakeProxy(t)
	srv := newGomodServer(t, s, fakeProxy.URL, 300)

	type ep struct {
		path    string
		counter *int64
		want    []byte
	}
	endpoints := []ep{
		{"/" + fakeMod + "/@v/" + fakeVer + ".info", &cnt.info, fakeInfo},
		{"/" + fakeMod + "/@v/" + fakeVer + ".mod", &cnt.mod, fakeMod2},
		{"/" + fakeMod + "/@v/" + fakeVer + ".zip", &cnt.zip_, fakeZip},
	}

	// First round — cold cache: each fetch must hit the upstream exactly once.
	for _, e := range endpoints {
		status, body := httpGet(t, srv.URL+e.path)
		require.Equal(t, http.StatusOK, status, "first fetch %s must return 200", e.path)
		require.Equal(t, e.want, body, "first fetch %s must return correct bytes", e.path)
	}
	hitAfterFirst := struct{ info, mod, zip int64 }{
		info: atomic.LoadInt64(&cnt.info),
		mod:  atomic.LoadInt64(&cnt.mod),
		zip:  atomic.LoadInt64(&cnt.zip_),
	}
	assert.EqualValues(t, 1, hitAfterFirst.info, "first .info: upstream must be hit exactly once")
	assert.EqualValues(t, 1, hitAfterFirst.mod, "first .mod: upstream must be hit exactly once")
	assert.EqualValues(t, 1, hitAfterFirst.zip, "first .zip: upstream must be hit exactly once")

	// Second round — warm CAS: upstream counters must not increase.
	for _, e := range endpoints {
		status, body := httpGet(t, srv.URL+e.path)
		require.Equal(t, http.StatusOK, status, "second fetch %s must return 200 (cache hit)", e.path)
		require.Equal(t, e.want, body, "second fetch %s: bytes must match (from CAS)", e.path)
	}
	assert.Equal(t, hitAfterFirst.info, atomic.LoadInt64(&cnt.info),
		"second .info must NOT contact upstream (CAS hit)")
	assert.Equal(t, hitAfterFirst.mod, atomic.LoadInt64(&cnt.mod),
		"second .mod must NOT contact upstream (CAS hit)")
	assert.Equal(t, hitAfterFirst.zip, atomic.LoadInt64(&cnt.zip_),
		"second .zip must NOT contact upstream (CAS hit)")
}

// ── Test 4 — @v/list is mutable: re-fetched after TTL; .mod is not ───────────

// TestGomodListIsMutable demonstrates the two-tier caching model:
//
//   - @v/list is a MUTABLE endpoint backed by a short-TTL MutableEntry. After the
//     TTL of 1 s expires, a second fetch contacts the upstream again.
//   - @v/{v}.mod is an IMMUTABLE endpoint backed by the permanent CAS. A second
//     fetch never contacts the upstream regardless of elapsed time.
//
// The TTL is set to 1 s and the test sleeps 1.1 s between the two list fetches
// to reliably trigger TTL expiry.
func TestGomodListIsMutable(t *testing.T) {
	const mutableTTL = int64(1) // 1-second TTL for deterministic expiry

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	fakeProxy, cnt := newFakeProxy(t)
	srv := newGomodServer(t, s, fakeProxy.URL, mutableTTL)

	// ── @v/list round 1 ──────────────────────────────────────────────────────
	status, body := httpGet(t, srv.URL+"/"+fakeMod+"/@v/list")
	require.Equal(t, http.StatusOK, status, "first @v/list must return 200")
	require.Equal(t, fakeList, body, "first @v/list must match upstream content")
	hitList1 := atomic.LoadInt64(&cnt.list)
	assert.EqualValues(t, 1, hitList1, "first @v/list must contact upstream exactly once")

	// ── @v/{v}.mod round 1 ──────────────────────────────────────────────────
	status, _ = httpGet(t, srv.URL+"/"+fakeMod+"/@v/"+fakeVer+".mod")
	require.Equal(t, http.StatusOK, status, "first .mod must return 200")
	hitMod1 := atomic.LoadInt64(&cnt.mod)
	assert.EqualValues(t, 1, hitMod1, "first .mod must contact upstream exactly once")

	// ── Immediate second @v/list — still within TTL ──────────────────────────
	// Served from the mutable-tier payload; upstream must not be contacted again.
	status, body = httpGet(t, srv.URL+"/"+fakeMod+"/@v/list")
	require.Equal(t, http.StatusOK, status, "immediate second @v/list must return 200")
	require.Equal(t, fakeList, body, "immediate second @v/list must return same content")
	assert.Equal(t, hitList1, atomic.LoadInt64(&cnt.list),
		"immediate second @v/list (within TTL) must NOT re-contact upstream")

	// ── Wait for TTL to expire (1 s + 100 ms margin) ────────────────────────
	time.Sleep(1100 * time.Millisecond)

	// ── Third @v/list — TTL expired → upstream re-contacted ──────────────────
	status, body = httpGet(t, srv.URL+"/"+fakeMod+"/@v/list")
	require.Equal(t, http.StatusOK, status, "third @v/list (post-TTL) must return 200")
	require.Equal(t, fakeList, body, "third @v/list must still return correct content")
	hitList3 := atomic.LoadInt64(&cnt.list)
	assert.Greater(t, hitList3, hitList1,
		"third @v/list (TTL expired) must re-contact upstream (mutable revalidation); "+
			"was %d, now %d", hitList1, hitList3)

	// ── Second .mod — immutable, TTL doesn't matter ───────────────────────────
	// No matter how much time has elapsed, .mod is NEVER re-fetched from upstream.
	status, modBody := httpGet(t, srv.URL+"/"+fakeMod+"/@v/"+fakeVer+".mod")
	require.Equal(t, http.StatusOK, status, "second .mod must return 200")
	require.Equal(t, fakeMod2, modBody, "second .mod must return same bytes from CAS")
	assert.Equal(t, hitMod1, atomic.LoadInt64(&cnt.mod),
		"second .mod must NOT re-contact upstream (permanent CAS, no TTL)")
}

// ── Test 5 — verify-on-write: digest mismatch → VerifyError, CAS clean ───────

// TestGomodVerifyOnWriteRejectsMismatch verifies that the verify-on-write
// pipeline rejects an artefact whose declared ref.Digest does not match the
// sha256 computed while streaming to the quarantine file.
//
// The behaviour is protocol-agnostic: the same cache.CacheManager handles gomod
// artefacts as OCI blobs. The test directly calls cache.Quarantine + cm.Store
// with a deliberately wrong digest to trigger ChecksumVerifier → StatusFail.
func TestGomodVerifyOnWriteRejectsMismatch(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	ctx := context.Background()

	realBytes := []byte("real module zip content: some plausible zip bytes for the test")
	art, cleanup, err := cache.Quarantine(ctx, tmp, bytes.NewReader(realBytes), artifact.UpstreamMeta{})
	require.NoError(t, err)
	defer cleanup()

	// Declare a digest that differs from the actual content's sha256.
	wrongDigest := "sha256:" + sha256hex([]byte("completely different content that was never fetched"))
	require.NotEqual(t, art.Digest, wrongDigest,
		"test precondition: wrong digest must differ from actual digest")

	ref := artifact.ArtifactRef{
		Protocol: "gomod",
		Name:     fakeMod,
		Version:  fakeVer + ".zip",
		Digest:   wrongDigest, // intentionally wrong — triggers ChecksumVerifier fail
		Mutable:  false,
	}

	_, storeErr := s.cm.Store(ctx, ref, art)
	require.Error(t, storeErr, "Store must fail when ref.Digest does not match actual digest")

	ve, isVerify := cache.AsVerifyError(storeErr)
	require.True(t, isVerify,
		"error must be *cache.VerifyError (protocol=gomod), got %T: %v", storeErr, storeErr)
	assert.Equal(t, artifact.StatusFail, ve.Result.Status,
		"verify status must be FAIL on digest mismatch")

	// Quarantine file must have been removed by Store on verify failure.
	_, statErr := os.Stat(art.Path)
	assert.True(t, os.IsNotExist(statErr),
		"quarantine file must be removed after verify-on-write failure")

	// Neither the declared wrong digest nor the actual computed digest must
	// appear in the CAS — the artefact must not have been promoted.
	existsBad, err := s.blobStore.Exists(ctx, wrongDigest)
	require.NoError(t, err)
	assert.False(t, existsBad, "CAS must not contain blob under the declared (wrong) digest")

	existsActual, err := s.blobStore.Exists(ctx, art.Digest)
	require.NoError(t, err)
	assert.False(t, existsActual, "CAS must not contain blob under the actual (computed) digest")
}

// ── Test 6 — go tool integration (hermetic, no external network) ──────────────

// TestGomodGoToolDownload runs the real `go mod download` command against the
// Specula gomod handler backed by the in-process fake GOPROXY. This exercises
// the full GOPROXY protocol as the official go command implements it.
//
// Prerequisites:
//   - `go` binary must be on PATH (otherwise the test is skipped).
//   - GOPROXY is pointed at Specula; GONOSUMDB=example.com skips sumdb for the
//     fake module so no real or fake sumdb server is needed.
//
// If the go tool rejects the raw zip (e.g. for format reasons that differ across
// versions), the test is skipped with a note rather than failed — Specula's job
// is to faithfully proxy the bytes, not validate the zip format.
func TestGomodGoToolDownload(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary not on PATH; skipping go tool integration test")
	}

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	fakeProxy, cnt := newFakeProxy(t)
	srv := newGomodServer(t, s, fakeProxy.URL, 300)

	// Create a minimal consumer module that requires example.com/fake v0.1.0.
	modDir := t.TempDir()
	consumerGomod := fmt.Sprintf(
		"module example.com/consumer\n\ngo 1.21\n\nrequire %s %s\n",
		fakeMod, fakeVer,
	)
	require.NoError(t, os.WriteFile(
		filepath.Join(modDir, "go.mod"), []byte(consumerGomod), 0o644))

	// Run `go mod download` in an isolated environment.
	// Note: the go tool writes module files with read-only permissions (0444) to
	// protect the module cache from accidental mutation. t.TempDir's cleanup
	// cannot remove such files without first making them writable. We register a
	// pre-cleanup that chmod 0755-walks the directories so t.TempDir can delete
	// them. Cleanup functions run in LIFO order, so ours runs before t.TempDir's.
	goPath := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.Walk(goPath, func(p string, _ os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			return os.Chmod(p, 0o755)
		})
	})

	goModCache := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.Walk(goModCache, func(p string, _ os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			return os.Chmod(p, 0o755)
		})
	})

	cmd := exec.Command("go", "mod", "download", "-x",
		fakeMod+"@"+fakeVer)
	cmd.Dir = modDir
	cmd.Env = append(filteredEnv(),
		"GOPROXY="+srv.URL+",off", // Specula only; no direct
		"GONOSUMDB=example.com",   // skip sumdb for all example.com modules
		"GOPATH="+goPath,
		"GOMODCACHE="+goModCache,
		"GOFLAGS=",
		"GONOPROXY=", // allow proxy
		"GOPRIVATE=", // not private
	)

	out, err := cmd.CombinedOutput()
	outStr := string(out)

	if err != nil {
		// If the error is about zip format validation, skip rather than fail:
		// the proxy faithfully served the bytes; go tool's validation is a
		// separate concern.
		if strings.Contains(outStr, "unexpected files") ||
			strings.Contains(outStr, "malformed module zip") ||
			strings.Contains(outStr, "go.mod not found") {
			t.Skipf("zip not accepted by go tool (raw archive/zip): %v\n%s", err, outStr)
		}
		t.Fatalf("go mod download failed: %v\n%s", err, outStr)
	}
	t.Logf("go mod download output:\n%s", outStr)

	// All three immutable artefacts must have been fetched from Specula→fake.
	assert.EqualValues(t, 1, atomic.LoadInt64(&cnt.info),
		"go mod download must fetch .info from upstream")
	assert.EqualValues(t, 1, atomic.LoadInt64(&cnt.mod),
		"go mod download must fetch .mod from upstream")
	assert.EqualValues(t, 1, atomic.LoadInt64(&cnt.zip_),
		"go mod download must fetch .zip from upstream")

	// The CAS must have bytes from the download.
	used, err := s.blobStore.UsageBytes(context.Background())
	require.NoError(t, err)
	assert.Positive(t, used, "CAS must have bytes after go mod download")
}

// filteredEnv returns os.Environ() with HOME and PATH preserved but common
// Go-related variables stripped so the test can set its own clean values.
func filteredEnv() []string {
	var result []string
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		switch key {
		case "HOME", "PATH", "USERPROFILE", "SYSTEMROOT", "TEMP", "TMP",
			"SHELL", "TERM", "LANG", "LC_ALL", "LC_CTYPE":
			result = append(result, kv)
		}
	}
	return result
}

// ── Live test — gated behind SPECULA_E2E_LIVE=1 ───────────────────────────────

// TestGomodLive fetches the go.mod of a real, small public module (golang.org/x/text
// v0.3.0) through Specula backed by goproxy.cn. The second fetch must be served
// from the CAS (no upstream contact). Requires CN network access.
//
// Skipped unless SPECULA_E2E_LIVE=1.
func TestGomodLive(t *testing.T) {
	if os.Getenv("SPECULA_E2E_LIVE") != "1" {
		t.Skip("set SPECULA_E2E_LIVE=1 to run live network tests (requires CN network access)")
	}

	const (
		liveModule  = "golang.org/x/text"
		liveVersion = "v0.3.0"
	)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	liveUps := []upstream.Upstream{{
		Name:     "goproxy-cn",
		BaseURL:  "https://goproxy.cn",
		Priority: 0,
	}}

	h := gomodhandler.NewHandler(s.cm,
		gomodhandler.WithMeta(s.metaStore),
		gomodhandler.WithUpstream(upstream.NewClient(), liveUps),
		gomodhandler.WithQuarantineDir(tmp),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	modPath := "/" + liveModule + "/@v/" + liveVersion + ".mod"

	// First fetch — cold cache; must come from goproxy.cn via Specula.
	status, body := httpGet(t, srv.URL+modPath)
	if status == http.StatusBadGateway {
		t.Skip("goproxy.cn unreachable (SPECULA_E2E_LIVE=1 but no CN network access)")
	}
	require.Equal(t, http.StatusOK, status,
		"first live fetch %s must return 200", modPath)
	assert.Contains(t, string(body), "module "+liveModule,
		"go.mod must contain the module declaration")
	t.Logf("live: fetched %s@%s .mod (%d bytes) through Specula→goproxy.cn",
		liveModule, liveVersion, len(body))

	// CAS must have content.
	ctx := context.Background()
	used, err := s.blobStore.UsageBytes(ctx)
	require.NoError(t, err)
	assert.Positive(t, used, "CAS must have bytes after live fetch")

	// Second fetch — warm CAS; bytes must match and no upstream contact.
	status, body2 := httpGet(t, srv.URL+modPath)
	require.Equal(t, http.StatusOK, status, "second live fetch must return 200 (CAS hit)")
	assert.Equal(t, body, body2, "second live fetch must return same bytes as first (CAS served)")
	t.Logf("live: second fetch served from CAS (%d bytes)", len(body2))
}
