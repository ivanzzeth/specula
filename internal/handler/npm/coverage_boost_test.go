package npm

// coverage_boost_test.go — targeted tests for remaining uncovered branches
// to push npm package from 79.2% to ≥80%.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// ── errStoreCacheManager — Store always returns an error ──────────────────────

type errStoreCacheManager struct {
	npmTestCache
}

func (e *errStoreCacheManager) Store(_ context.Context, _ artifact.ArtifactRef, _ *artifact.Artifact) (*artifact.CacheEntry, error) {
	return nil, errors.New("simulated disk full on Store")
}

// ── errPutMutableMetaStore — PutMutable always returns an error ───────────────

type errPutMutableMetaStore struct {
	fakeNpmMetaStore
}

func (e *errPutMutableMetaStore) PutMutable(_ context.Context, _ artifact.MutableEntry) error {
	return errors.New("simulated meta write error")
}

// ── Tests: serveTarball dep-confusion error ───────────────────────────────────

func TestNpmServeTarball_DepConfusion_NoPrivateUpstream_502(t *testing.T) {
	// Private unscoped name with no private upstream configured → selectUpstreams
	// returns an error → 502 (dep-confusion protection even for tarballs).
	h := NewHandler(newNpmTestCache(),
		WithPrivateUnscoped([]string{"corp-secret"}),
		// No WithPrivateUpstream → error from selectUpstreams.
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/corp-secret/-/corp-secret-1.0.0.tgz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode,
		"dep-confusion guard: private tarball without private upstream must 502")
}

// ── Tests: speculaBaseURL edge case ──────────────────────────────────────────

func TestSpeculaBaseURL_HostFromURLHost(t *testing.T) {
	// When r.Host is empty, speculaBaseURL must fall back to r.URL.Host.
	r := httptest.NewRequest(http.MethodGet, "http://specula.internal:7732/npm/react", nil)
	r.Host = "" // explicitly clear — simulates some proxy configurations
	got := speculaBaseURL(r)
	assert.Equal(t, "http://specula.internal:7732", got,
		"speculaBaseURL must use r.URL.Host when r.Host is empty")
}

// ── Tests: WithLogger option ──────────────────────────────────────────────────

func TestWithLogger_SetsLogger(t *testing.T) {
	h := NewHandler(newNpmTestCache(), WithLogger(slog.Default()))
	assert.NotNil(t, h)
}

// ── Tests: fetchBodyAndStore — quarantine failure ─────────────────────────────

func TestNpmFetchBodyAndStore_QuarantineError_ReturnsError(t *testing.T) {
	// Pass a quarantineDir that does not exist → os.CreateTemp fails.
	h := NewHandler(newNpmTestCache(),
		WithQuarantineDir("/nonexistent/path/that/definitely/does/not/exist"),
	)
	body := bytes.NewReader([]byte(`{"name":"react","versions":{}}`))
	_, err := h.fetchBodyAndStore(
		context.Background(), packumentRef("react"), body, artifact.UpstreamMeta{},
	)
	require.Error(t, err, "quarantine failure must be returned as an error")
	assert.Contains(t, err.Error(), "quarantine")
}

// ── Tests: fetchBodyAndStore — PutMutable error is non-fatal ─────────────────

func TestNpmFetchBodyAndStore_PutMutableError_NonFatal(t *testing.T) {
	// PutMutable fails → entry still returned (non-fatal; just TTL pointer lost).
	ms := &errPutMutableMetaStore{fakeNpmMetaStore: *newFakeNpmMetaStore()}
	h := NewHandler(newNpmTestCache(),
		WithMeta(ms),
		WithMutableTTL(120),
		WithQuarantineDir(t.TempDir()),
	)
	body := bytes.NewReader([]byte(`{"name":"react","versions":{}}`))
	entry, err := h.fetchBodyAndStore(
		context.Background(), packumentRef("react"), body, artifact.UpstreamMeta{ETag: `"abc"`},
	)
	require.NoError(t, err, "PutMutable error must be non-fatal; entry must still be returned")
	assert.NotNil(t, entry)
}

// ── Tests: serveMutable — upstream down, no stale → 502 ──────────────────────

func TestNpmServeMutable_UpstreamDown_NoStale_502(t *testing.T) {
	// Upstream is unreachable and no stale entry exists → 502.
	h := newNpmHandlerWithUpstream(newNpmTestCache(), "http://127.0.0.1:0") // nothing listening
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/react")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode,
		"unreachable upstream with no stale packument must return 502")
}

// ── Tests: serveMutable — store failure → 502 ────────────────────────────────

func TestNpmServeMutable_StoreError_502(t *testing.T) {
	// Store fails after successful upstream fetch → 502.
	cm := &errStoreCacheManager{npmTestCache: *newNpmTestCache()}
	packumentData := []byte(`{"name":"react","versions":{}}`)
	reg, _, _ := fakeNpmRegistry(t, map[string][]byte{"react": packumentData}, nil)

	h := NewHandler(cm,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "fake-npm", BaseURL: reg.URL}}),
		WithMutableTTL(300),
		// quarantineDir="" means os.TempDir(), which should work
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/react")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode,
		"store failure after successful upstream fetch must return 502")
}
