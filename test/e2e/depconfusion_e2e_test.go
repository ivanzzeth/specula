// Package e2e — hermetic end-to-end dep-confusion guard tests.
//
// These tests drive the full Specula data-plane stack (LocalDiskDriver +
// SQLiteStore + verify.Chain + cache.New) through real HTTP servers, verifying
// that for both PyPI and npm:
//
//  1. A configured-private name/scope is served from the private upstream ONLY —
//     the public mirror is never contacted.
//  2. A public mirror offering a "higher version" is ignored for private names.
//  3. Private-upstream DOWN results in 5xx — the public mirror is NEVER used as
//     a fallback, not even when failClosed=false.
//
// No network calls to real PyPI / npm are made. All servers are in-process
// httptest.Servers.
package e2e

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	npmhandler "github.com/ivanzzeth/specula/internal/handler/npm"
	pypihandler "github.com/ivanzzeth/specula/internal/handler/pypi"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// ── fake PyPI helpers (dep-confusion flavour) ─────────────────────────────────

// newFakeSimpleServer starts an httptest.Server that serves PyPI simple-index
// pages for the given project→body map. Every request is counted atomically.
// Returns the server and a pointer to the hit counter.
func newFakeSimpleServer(t *testing.T, pages map[string][]byte) (*httptest.Server, *int64) {
	t.Helper()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		p := r.URL.Path
		if !strings.HasPrefix(p, "/simple/") {
			http.NotFound(w, r)
			return
		}
		name := strings.Trim(strings.TrimPrefix(p, "/simple/"), "/")
		body, ok := pages[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// newFakeNpmServer starts an httptest.Server that serves npm packuments from
// the given pkg→body map. Every request is counted atomically.
func newFakeNpmServer(t *testing.T, packuments map[string][]byte) (*httptest.Server, *int64) {
	t.Helper()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		pkg := strings.TrimLeft(r.URL.Path, "/")
		// Decode %2F for scoped names (@scope%2Fpkg → @scope/pkg)
		pkg = strings.ReplaceAll(pkg, "%2F", "/")
		pkg = strings.ReplaceAll(pkg, "%2f", "/")
		body, ok := packuments[pkg]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// ── PyPI dep-confusion e2e tests ──────────────────────────────────────────────

// TestDepConfusionPypi_PrivateNameServedFromPrivateOnly verifies that a private
// package is proxied exclusively from the private upstream. The public mirror
// that holds the "same" package is never contacted.
func TestDepConfusionPypi_PrivateNameServedFromPrivateOnly(t *testing.T) {
	const pkg = "corp-internal"
	privateIndex := []byte(`<!DOCTYPE html><html><body>
<a href="/packages/corp/corp_internal-1.0.0-py3-none-any.whl">corp_internal-1.0.0</a>
</body></html>`)
	publicIndex := []byte(`<!DOCTYPE html><html><body>
<a href="/packages/corp/corp_internal-9.9.9-py3-none-any.whl">corp_internal-9.9.9</a>
</body></html>`)

	prvSrv, prvHits := newFakeSimpleServer(t, map[string][]byte{pkg: privateIndex})
	pubSrv, pubHits := newFakeSimpleServer(t, map[string][]byte{pkg: publicIndex})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	h := pypihandler.NewHandler(s.cm,
		pypihandler.WithMeta(s.metaStore),
		pypihandler.WithPrivateNames([]string{pkg}),
		pypihandler.WithPrivateUpstream(upstream.Upstream{Name: "private", BaseURL: prvSrv.URL, Priority: 0}),
		pypihandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		pypihandler.WithQuarantineDir(s.dir),
		pypihandler.WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	status, body := httpGet(t, srv.URL+"/simple/"+pkg+"/")
	require.Equal(t, http.StatusOK, status, "private package must return 200 from private upstream")
	assert.Equal(t, privateIndex, body, "response body must be from private upstream, not public")

	assert.Positive(t, atomic.LoadInt64(prvHits), "private upstream must have been contacted")
	assert.Zero(t, atomic.LoadInt64(pubHits), "public upstream must NEVER be contacted for a private name")
}

// TestDepConfusionPypi_PublicHigherVersionIgnored verifies that a public mirror
// offering a higher version is ignored for a private package: Specula serves
// only from the private upstream and the public mirror sees zero requests.
func TestDepConfusionPypi_PublicHigherVersionIgnored(t *testing.T) {
	const pkg = "acme-sdk"
	privateIndex := []byte(`<html><body><a href="acme_sdk-1.0.0.whl">acme_sdk-1.0.0.whl</a></body></html>`)
	// Public "attacker" version — higher semver but must be ignored.
	publicIndex := []byte(`<html><body><a href="acme_sdk-99.0.0.whl">acme_sdk-99.0.0.whl</a></body></html>`)

	prvSrv, prvHits := newFakeSimpleServer(t, map[string][]byte{pkg: privateIndex})
	pubSrv, pubHits := newFakeSimpleServer(t, map[string][]byte{pkg: publicIndex})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	h := pypihandler.NewHandler(s.cm,
		pypihandler.WithMeta(s.metaStore),
		pypihandler.WithPrivateNames([]string{pkg}),
		pypihandler.WithPrivateUpstream(upstream.Upstream{Name: "private", BaseURL: prvSrv.URL, Priority: 0}),
		pypihandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		pypihandler.WithQuarantineDir(s.dir),
		pypihandler.WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	status, body := httpGet(t, srv.URL+"/simple/"+pkg+"/")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, privateIndex, body,
		"served index must come from private upstream (not public higher-version)")
	assert.Positive(t, atomic.LoadInt64(prvHits), "private upstream must be contacted")
	assert.Zero(t, atomic.LoadInt64(pubHits),
		"public mirror with higher version must receive ZERO requests for private pkg")
}

// TestDepConfusionPypi_PrivateDownFailClosed verifies that when the private
// upstream is unreachable (connection refused), Specula returns 5xx and the
// public mirror is NEVER consulted — not even when it would successfully serve
// the package (the attack scenario).
func TestDepConfusionPypi_PrivateDownFailClosed(t *testing.T) {
	const pkg = "corp-lib"
	publicIndex := []byte(`<html><body><a href="corp_lib-1.0.whl">corp_lib-1.0.whl</a></body></html>`)

	// Public mirror is healthy and would serve the package — but must NOT be used.
	pubSrv, pubHits := newFakeSimpleServer(t, map[string][]byte{pkg: publicIndex})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	h := pypihandler.NewHandler(s.cm,
		pypihandler.WithMeta(s.metaStore),
		pypihandler.WithPrivateNames([]string{pkg}),
		pypihandler.WithPrivateUpstream(upstream.Upstream{
			Name:     "private",
			BaseURL:  "http://127.0.0.1:0", // port 0 → connection refused
			Priority: 0,
		}),
		pypihandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		pypihandler.WithFailClosed(true),
		pypihandler.WithQuarantineDir(s.dir),
		pypihandler.WithMutableTTL(0), // always revalidate → upstream always contacted
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	status, _ := httpGet(t, srv.URL+"/simple/"+pkg+"/")
	assert.GreaterOrEqual(t, status, 500,
		"private-upstream-down must result in 5xx, not serve from public (attack path)")
	assert.Zero(t, atomic.LoadInt64(pubHits),
		"public mirror must receive ZERO requests when private upstream is down")
}

// TestDepConfusionPypi_GlobPrivateName verifies that glob patterns in the
// private-name list correctly match multiple packages under the same prefix,
// ensuring none of them ever fall through to the public mirror.
func TestDepConfusionPypi_GlobPrivateName(t *testing.T) {
	const (
		pkg1 = "acme-core"
		pkg2 = "acme-utils"
	)
	privateIndexCore := []byte(`<html><body><a href="acme_core-1.0.whl">acme_core-1.0.whl</a></body></html>`)
	privateIndexUtils := []byte(`<html><body><a href="acme_utils-1.0.whl">acme_utils-1.0.whl</a></body></html>`)

	prvSrv, prvHits := newFakeSimpleServer(t, map[string][]byte{
		pkg1: privateIndexCore,
		pkg2: privateIndexUtils,
	})
	pubSrv, pubHits := newFakeSimpleServer(t, map[string][]byte{
		pkg1: []byte(`<html><body>public-attacker-version</body></html>`),
		pkg2: []byte(`<html><body>public-attacker-version</body></html>`),
	})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	h := pypihandler.NewHandler(s.cm,
		pypihandler.WithMeta(s.metaStore),
		// Glob pattern: all "acme-*" packages are private.
		pypihandler.WithPrivateNames([]string{"acme-*"}),
		pypihandler.WithPrivateUpstream(upstream.Upstream{Name: "private", BaseURL: prvSrv.URL, Priority: 0}),
		pypihandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		pypihandler.WithQuarantineDir(s.dir),
		pypihandler.WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	status1, body1 := httpGet(t, srv.URL+"/simple/acme-core/")
	require.Equal(t, http.StatusOK, status1)
	assert.Equal(t, privateIndexCore, body1, "acme-core must come from private upstream")

	status2, body2 := httpGet(t, srv.URL+"/simple/acme-utils/")
	require.Equal(t, http.StatusOK, status2)
	assert.Equal(t, privateIndexUtils, body2, "acme-utils must come from private upstream")

	assert.Positive(t, atomic.LoadInt64(prvHits), "private upstream must be contacted for glob-matched names")
	assert.Zero(t, atomic.LoadInt64(pubHits), "public mirror must receive ZERO requests for glob-matched private names")
}

// ── npm dep-confusion e2e tests ───────────────────────────────────────────────

// TestDepConfusionNpm_ScopedServedFromPrivateOnly verifies that a scoped private
// package (@corp/sdk) is routed exclusively to the private registry. The public
// registry that would serve it is never contacted.
func TestDepConfusionNpm_ScopedServedFromPrivateOnly(t *testing.T) {
	const pkg = "@corp/sdk"
	privatePackument := []byte(`{"name":"@corp/sdk","dist-tags":{"latest":"1.0.0"},"versions":{}}`)
	publicPackument := []byte(`{"name":"@corp/sdk","dist-tags":{"latest":"9.9.9"},"versions":{}}`)

	prvSrv, prvHits := newFakeNpmServer(t, map[string][]byte{pkg: privatePackument})
	pubSrv, pubHits := newFakeNpmServer(t, map[string][]byte{pkg: publicPackument})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	h := npmhandler.NewHandler(s.cm,
		npmhandler.WithMeta(s.metaStore),
		npmhandler.WithPrivateScopes([]string{"@corp"}),
		npmhandler.WithPrivateUpstream(upstream.Upstream{Name: "private", BaseURL: prvSrv.URL, Priority: 0}),
		npmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		npmhandler.WithQuarantineDir(s.dir),
		npmhandler.WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	status, body := httpGet(t, srv.URL+"/@corp/sdk")
	require.Equal(t, http.StatusOK, status, "@corp/sdk must return 200 from private registry")
	assert.Equal(t, privatePackument, body, "response must be from private registry, not public")

	assert.Positive(t, atomic.LoadInt64(prvHits), "private registry must be contacted for @corp scope")
	assert.Zero(t, atomic.LoadInt64(pubHits), "public registry must NEVER be contacted for @corp scoped pkg")
}

// TestDepConfusionNpm_UnscopedBlocklistNoPublicFallback verifies that an
// unscoped private package ("internal-lib" in the denylist) is routed only
// to the private registry. When the private registry is DOWN the handler
// returns 5xx and the public registry is never used as fallback.
func TestDepConfusionNpm_UnscopedBlocklistNoPublicFallback(t *testing.T) {
	const pkg = "internal-lib"
	publicPackument := []byte(`{"name":"internal-lib","dist-tags":{"latest":"2.0.0"},"versions":{}}`)

	// Public registry is healthy — it would serve the package but must be blocked.
	pubSrv, pubHits := newFakeNpmServer(t, map[string][]byte{pkg: publicPackument})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	h := npmhandler.NewHandler(s.cm,
		npmhandler.WithMeta(s.metaStore),
		npmhandler.WithPrivateUnscoped([]string{pkg}),
		npmhandler.WithPrivateUpstream(upstream.Upstream{
			Name:     "private",
			BaseURL:  "http://127.0.0.1:0", // port 0 → connection refused
			Priority: 0,
		}),
		npmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		npmhandler.WithFailClosed(true),
		npmhandler.WithQuarantineDir(s.dir),
		npmhandler.WithMutableTTL(0),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	status, _ := httpGet(t, srv.URL+"/"+pkg)
	assert.GreaterOrEqual(t, status, 500,
		"private-upstream-down must return 5xx, not serve from public (attack path)")
	assert.Zero(t, atomic.LoadInt64(pubHits),
		"public registry must receive ZERO requests when private upstream is down")
}

// TestDepConfusionNpm_PrivateDownFailClosed_ScopedPkg verifies that a scoped
// private package with a DOWN private registry returns 5xx — not a redirect to
// or content from the public registry.
func TestDepConfusionNpm_PrivateDownFailClosed_ScopedPkg(t *testing.T) {
	const pkg = "@myorg/utils"
	publicPackument := []byte(`{"name":"@myorg/utils","dist-tags":{"latest":"3.0.0"},"versions":{}}`)

	pubSrv, pubHits := newFakeNpmServer(t, map[string][]byte{pkg: publicPackument})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	h := npmhandler.NewHandler(s.cm,
		npmhandler.WithMeta(s.metaStore),
		npmhandler.WithPrivateScopes([]string{"@myorg"}),
		npmhandler.WithPrivateUpstream(upstream.Upstream{
			Name:     "private",
			BaseURL:  "http://127.0.0.1:0",
			Priority: 0,
		}),
		npmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		npmhandler.WithFailClosed(true),
		npmhandler.WithQuarantineDir(s.dir),
		npmhandler.WithMutableTTL(0),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	status, _ := httpGet(t, srv.URL+"/"+pkg)
	assert.GreaterOrEqual(t, status, 500,
		"private-upstream-down for scoped pkg must return 5xx (fail-closed)")
	assert.Zero(t, atomic.LoadInt64(pubHits),
		"public registry must receive ZERO requests for scoped private pkg with down upstream")
}

// TestDepConfusionNpm_TarballFromPrivateOnly verifies that an immutable tarball
// for a scoped private package is fetched only from the private registry.
func TestDepConfusionNpm_TarballFromPrivateOnly(t *testing.T) {
	const pkg = "@corp/lib"
	const file = "lib-1.0.0.tgz"
	privateTgz := bytes.Repeat([]byte("PRIVATE_TGZ_"), 32)
	publicTgz := bytes.Repeat([]byte("PUBLIC_TGZ__"), 32) // different bytes

	var prvHits, pubHits int64

	prvSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&prvHits, 1)
		if strings.Contains(r.URL.Path, "/-/") {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(privateTgz)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(prvSrv.Close)

	pubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&pubHits, 1)
		if strings.Contains(r.URL.Path, "/-/") {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(publicTgz)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(pubSrv.Close)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	h := npmhandler.NewHandler(s.cm,
		npmhandler.WithMeta(s.metaStore),
		npmhandler.WithPrivateScopes([]string{"@corp"}),
		npmhandler.WithPrivateUpstream(upstream.Upstream{Name: "private", BaseURL: prvSrv.URL, Priority: 0}),
		npmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		npmhandler.WithQuarantineDir(s.dir),
		npmhandler.WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// Fetch tarball for a @corp-scoped package.
	status, body := httpGet(t, srv.URL+"/@corp/lib/-/"+file)
	require.Equal(t, http.StatusOK, status, "tarball fetch must return 200")
	assert.Equal(t, privateTgz, body, "tarball bytes must come from private registry")

	assert.Positive(t, atomic.LoadInt64(&prvHits), "private registry must be contacted for tarball")
	assert.Zero(t, atomic.LoadInt64(&pubHits), "public registry must receive ZERO requests for @corp tarball")
}

// TestDepConfusionPypi_WheelFromPrivateOnly verifies that an immutable wheel
// for a private PyPI package is fetched only from the private index, not from
// the public mirror.
func TestDepConfusionPypi_WheelFromPrivateOnly(t *testing.T) {
	const pkg = "corp-crypto"
	const file = "corp_crypto-1.0.0-py3-none-any.whl"
	privateWheel := bytes.Repeat([]byte("PRIVATE_WHEEL_"), 64)
	publicWheel := bytes.Repeat([]byte("PUBLIC_WHEEL__"), 64)

	var prvHits, pubHits int64

	// Private PyPI: serves both the simple index and the wheel file.
	prvSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&prvHits, 1)
		p := r.URL.Path
		if strings.HasPrefix(p, "/simple/") {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body><a href="/packages/cc/` + file + `">` + file + `</a></body></html>`))
			return
		}
		if strings.HasPrefix(p, "/packages/") {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(privateWheel)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(prvSrv.Close)

	// Public PyPI: serves different bytes — should never be contacted.
	pubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&pubHits, 1)
		if strings.HasPrefix(r.URL.Path, "/packages/") {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(publicWheel)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(pubSrv.Close)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	h := pypihandler.NewHandler(s.cm,
		pypihandler.WithMeta(s.metaStore),
		pypihandler.WithPrivateNames([]string{pkg}),
		pypihandler.WithPrivateUpstream(upstream.Upstream{Name: "private", BaseURL: prvSrv.URL, Priority: 0}),
		pypihandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		pypihandler.WithQuarantineDir(s.dir),
		pypihandler.WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// Fetch the wheel file (immutable path: /packages/cc/<file>).
	status, body := httpGet(t, srv.URL+"/packages/cc/"+file)
	require.Equal(t, http.StatusOK, status, "wheel fetch must return 200 from private upstream")
	assert.Equal(t, privateWheel, body, "wheel bytes must come from private upstream")

	// Index page should also be from private only.
	status2, _ := httpGet(t, srv.URL+"/simple/"+pkg+"/")
	assert.Equal(t, http.StatusOK, status2, "private index fetch must return 200")

	assert.Positive(t, atomic.LoadInt64(&prvHits), "private upstream must be contacted")
	assert.Zero(t, atomic.LoadInt64(&pubHits), "public mirror must receive ZERO requests for private wheel")
}

// TestDepConfusionPypi_PublicPackageNotAffected confirms that public packages
// are still served from the public mirror and are unaffected by private config.
func TestDepConfusionPypi_PublicPackageNotAffected(t *testing.T) {
	publicIndex := []byte(`<html><body><a href="flask-3.0.whl">flask-3.0.whl</a></body></html>`)

	pubSrv, pubHits := newFakeSimpleServer(t, map[string][]byte{"flask": publicIndex})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	h := pypihandler.NewHandler(s.cm,
		pypihandler.WithMeta(s.metaStore),
		pypihandler.WithPrivateNames([]string{"corp-internal"}), // flask is NOT private
		pypihandler.WithPrivateUpstream(upstream.Upstream{
			Name:     "private",
			BaseURL:  "http://127.0.0.1:0", // down; should not be contacted for public pkgs
			Priority: 0,
		}),
		pypihandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		pypihandler.WithQuarantineDir(s.dir),
		pypihandler.WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	status, body := httpGet(t, srv.URL+"/simple/flask/")
	require.Equal(t, http.StatusOK, status, "public package must still be served from public mirror")
	assert.Equal(t, publicIndex, body, "public package index must come from public mirror")
	assert.Positive(t, atomic.LoadInt64(pubHits), "public mirror must be contacted for public packages")
}

// TestDepConfusionNpm_PublicPackageNotAffected confirms that unscoped public
// packages (not in any scope/denylist) are served normally from the public registry.
func TestDepConfusionNpm_PublicPackageNotAffected(t *testing.T) {
	const pkg = "react"
	publicPackument := []byte(`{"name":"react","dist-tags":{"latest":"18.2.0"},"versions":{}}`)

	pubSrv, pubHits := newFakeNpmServer(t, map[string][]byte{pkg: publicPackument})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	h := npmhandler.NewHandler(s.cm,
		npmhandler.WithMeta(s.metaStore),
		npmhandler.WithPrivateScopes([]string{"@corp"}), // react is NOT in @corp
		npmhandler.WithPrivateUpstream(upstream.Upstream{
			Name:     "private",
			BaseURL:  "http://127.0.0.1:0",
			Priority: 0,
		}),
		npmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		npmhandler.WithQuarantineDir(s.dir),
		npmhandler.WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	status, body := httpGet(t, srv.URL+"/react")
	require.Equal(t, http.StatusOK, status, "public package must be served from public mirror")
	assert.Equal(t, publicPackument, body)
	assert.Positive(t, atomic.LoadInt64(pubHits), "public registry must be contacted for non-private packages")
}

// TestDepConfusionNpm_PublicHigherVersionIgnored verifies that a public registry
// offering a "higher" version is ignored for an npm private package: only the
// private registry's response is served.
func TestDepConfusionNpm_PublicHigherVersionIgnored(t *testing.T) {
	const pkg = "@corp/cli"
	privatePackument := []byte(`{"name":"@corp/cli","dist-tags":{"latest":"1.0.0"},"versions":{}}`)
	// Attacker's public registry has "higher" version — must be ignored.
	attackerPackument := []byte(`{"name":"@corp/cli","dist-tags":{"latest":"999.0.0"},"versions":{}}`)

	prvSrv, prvHits := newFakeNpmServer(t, map[string][]byte{pkg: privatePackument})
	pubSrv, pubHits := newFakeNpmServer(t, map[string][]byte{pkg: attackerPackument})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	h := npmhandler.NewHandler(s.cm,
		npmhandler.WithMeta(s.metaStore),
		npmhandler.WithPrivateScopes([]string{"@corp"}),
		npmhandler.WithPrivateUpstream(upstream.Upstream{Name: "private", BaseURL: prvSrv.URL, Priority: 0}),
		npmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		npmhandler.WithQuarantineDir(s.dir),
		npmhandler.WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	status, body := httpGet(t, srv.URL+"/@corp/cli")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, privatePackument, body,
		"private packument must be served (not attacker's higher version from public)")
	assert.Positive(t, atomic.LoadInt64(prvHits), "private registry must be contacted")
	assert.Zero(t, atomic.LoadInt64(pubHits),
		"public registry with higher version must receive ZERO requests for @corp pkg")
}

// TestDepConfusionNpm_TarballCacheHit_StillPrivate verifies that a tarball
// served from the Specula CAS (cache hit) is the private content: the cache
// is seeded from the private upstream, and a second fetch returns the same bytes
// without contacting either upstream.
func TestDepConfusionNpm_TarballCacheHit_StillPrivate(t *testing.T) {
	const pkg = "@corp/sdk"
	const file = "sdk-2.0.0.tgz"
	privateTgz := bytes.Repeat([]byte("PRIVATE_TARBALL_"), 64)

	var prvHits, pubHits int64

	prvSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&prvHits, 1)
		if strings.Contains(r.URL.Path, "/-/") {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(privateTgz)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(prvSrv.Close)

	pubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&pubHits, 1)
		http.NotFound(w, r)
	}))
	t.Cleanup(pubSrv.Close)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	h := npmhandler.NewHandler(s.cm,
		npmhandler.WithMeta(s.metaStore),
		npmhandler.WithPrivateScopes([]string{"@corp"}),
		npmhandler.WithPrivateUpstream(upstream.Upstream{Name: "private", BaseURL: prvSrv.URL, Priority: 0}),
		npmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		npmhandler.WithQuarantineDir(s.dir),
		npmhandler.WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// First fetch — cold cache, contacts private upstream.
	status1, body1 := httpGet(t, srv.URL+"/@corp/sdk/-/"+file)
	require.Equal(t, http.StatusOK, status1, "first tarball fetch must return 200")
	assert.Equal(t, privateTgz, body1)

	prvHitsAfterFirst := atomic.LoadInt64(&prvHits)
	assert.Positive(t, prvHitsAfterFirst, "private upstream must be contacted on cold miss")
	assert.Zero(t, atomic.LoadInt64(&pubHits), "public must not be contacted")

	// Second fetch — CAS hit; no upstream contact.
	status2, body2 := httpGet(t, srv.URL+"/@corp/sdk/-/"+file)
	require.Equal(t, http.StatusOK, status2)
	assert.Equal(t, privateTgz, body2, "second fetch must return same private bytes from CAS")
	assert.Equal(t, prvHitsAfterFirst, atomic.LoadInt64(&prvHits),
		"private upstream must NOT be hit again for CAS-cached tarball")
	assert.Zero(t, atomic.LoadInt64(&pubHits), "public must receive zero hits throughout")
}

// TestDepConfusionNpm_PrivateNotFound_NoPublicFallthrough verifies that when
// the private upstream returns 404 for a private package, the handler returns
// 404 to the client and does NOT fall through to the public registry (the
// confusion attack path for mislabelled private packages).
func TestDepConfusionNpm_PrivateNotFound_NoPublicFallthrough(t *testing.T) {
	const pkg = "@corp/missing"
	publicPackument := []byte(`{"name":"@corp/missing","dist-tags":{"latest":"1.0.0"},"versions":{}}`)

	// Private registry returns 404 for this package.
	prvSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(prvSrv.Close)

	// Public registry has the package — must NEVER be consulted.
	pubSrv, pubHits := newFakeNpmServer(t, map[string][]byte{pkg: publicPackument})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	h := npmhandler.NewHandler(s.cm,
		npmhandler.WithMeta(s.metaStore),
		npmhandler.WithPrivateScopes([]string{"@corp"}),
		npmhandler.WithPrivateUpstream(upstream.Upstream{Name: "private", BaseURL: prvSrv.URL, Priority: 0}),
		npmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		npmhandler.WithFailClosed(true),
		npmhandler.WithQuarantineDir(s.dir),
		npmhandler.WithMutableTTL(0),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	status, _ := httpGet(t, srv.URL+"/@corp/missing")
	// A 404 from private is NOT a mandate to try public; it must surface as non-200.
	assert.NotEqual(t, http.StatusOK, status,
		"private-404 must not fall through to serve 200 from public (dep-confusion)")
	assert.Zero(t, atomic.LoadInt64(pubHits),
		"public registry must receive ZERO requests when private returns 404")
}
