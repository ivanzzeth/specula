package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/config"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// multiMirrorConfig is a two-protocol baseline with a real fallback chain, so
// ordering and per-protocol isolation are actually observable.
func multiMirrorConfig() *config.Config {
	cfg := testConfig()
	cfg.Protocols = map[string]config.ProtocolConfig{
		"oci": {
			Upstreams: []config.UpstreamConfig{
				{Name: "daocloud", BaseURL: "https://docker.m.daocloud.io", Priority: 0},
				{Name: "dockerhub", BaseURL: "https://registry-1.docker.io", Priority: 1, Official: true},
			},
		},
		"pypi": {
			Upstreams: []config.UpstreamConfig{
				{Name: "tuna", BaseURL: "https://pypi.tuna.tsinghua.edu.cn", Priority: 0},
				{Name: "pypi", BaseURL: "https://pypi.org", Priority: 1, Official: true},
			},
		},
	}
	return cfg
}

// newUpstreamHarness builds an admin server with a live upstream Registry and
// returns it alongside the registry so tests can drive real mirror state.
func newUpstreamHarness(t *testing.T) (*harness, *upstream.Registry) {
	t.Helper()
	store := newFakeUserStore()
	verifier := auth.NewHS256Verifier([]byte("test-secret-32-bytes-minimum!!!"))
	hasher := &fakeHasher{}
	reg := upstream.NewRegistry()

	srv := New(Deps{
		Stats:     newFakeStatsCollector(),
		Meta:      &fakeMetaStore{},
		Users:     store,
		Auth:      auth.NewService(store, hasher, verifier, false, nil),
		Tokens:    verifier,
		Config:    multiMirrorConfig(),
		Blobs:     &fakeBlobReporter{usedBytes: 999},
		Upstreams: reg,
	})
	srv.hasher = hasher

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return &harness{srv: srv, mux: mux, store: store, verifier: verifier, hasher: hasher}, reg
}

// protoByName indexes an upstreams response by protocol.
func protoByName(t *testing.T, rr *httptest.ResponseRecorder) map[string]ProtocolUpstreams {
	t.Helper()
	var resp UpstreamsResponse
	decodeJSON(t, rr, &resp)
	out := make(map[string]ProtocolUpstreams, len(resp.Protocols))
	for _, p := range resp.Protocols {
		out[p.Protocol] = p
	}
	return out
}

// mirrorByName indexes one protocol's chain by mirror name.
func mirrorByName(p ProtocolUpstreams) map[string]UpstreamHealth {
	out := make(map[string]UpstreamHealth, len(p.Mirrors))
	for _, m := range p.Mirrors {
		out[m.Name] = m
	}
	return out
}

func TestHandleUpstreams_ListsEveryProtocolInFallbackOrder(t *testing.T) {
	h, _ := newUpstreamHarness(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/admin/upstreams", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	protos := protoByName(t, rr)
	require.Len(t, protos, 2)

	oci := protos["oci"]
	assert.True(t, oci.Live, "a wired Registry makes the chain live")
	require.Len(t, oci.Mirrors, 2)
	assert.Equal(t, "daocloud", oci.Mirrors[0].Name)
	assert.Equal(t, 0, oci.Mirrors[0].Order)
	assert.Equal(t, "dockerhub", oci.Mirrors[1].Name)
	assert.Equal(t, 1, oci.Mirrors[1].Order)
	assert.True(t, oci.Mirrors[1].Official)
	assert.True(t, oci.Mirrors[0].Enabled)

	// Nothing has been fetched yet: every measurement must read as absent.
	assert.Equal(t, "unknown", oci.Mirrors[0].Health)
	assert.False(t, oci.Mirrors[0].HasLatency)
	assert.Zero(t, oci.Mirrors[0].ServedCount)
	assert.Zero(t, oci.Mirrors[0].LastServedUnix)
	assert.Empty(t, oci.LastServedBy)
	assert.Zero(t, oci.TotalServed)
}

func TestHandleUpstreams_ReportsHitShareAndLastServedBy(t *testing.T) {
	h, reg := newUpstreamHarness(t)
	_, tok := h.mustCreateAdmin(t)

	// Drive real state through the Runtime the fetch path would use.
	rt := reg.Runtime("oci")
	for i := 0; i < 3; i++ {
		rt.RecordServe("daocloud", 20*time.Millisecond)
	}
	rt.RecordServe("dockerhub", 100*time.Millisecond) // most recent

	rr := h.do("GET", "/api/v1/admin/upstreams", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	oci := protoByName(t, rr)["oci"]
	assert.Equal(t, int64(4), oci.TotalServed)
	assert.Equal(t, "dockerhub", oci.LastServedBy, "most recent serve wins")

	mirrors := mirrorByName(oci)
	assert.Equal(t, int64(3), mirrors["daocloud"].ServedCount)
	assert.InDelta(t, 0.75, mirrors["daocloud"].HitShare, 0.001)
	assert.InDelta(t, 0.25, mirrors["dockerhub"].HitShare, 0.001)
	assert.Equal(t, "up", mirrors["daocloud"].Health)
	assert.True(t, mirrors["daocloud"].HasLatency)
	assert.Equal(t, int64(20), mirrors["daocloud"].LastLatencyMs)
	assert.NotZero(t, mirrors["daocloud"].LastServedUnix)
}

// Measurements must not leak between protocols that share a mirror name.
func TestHandleUpstreams_ProtocolsAreIsolated(t *testing.T) {
	h, reg := newUpstreamHarness(t)
	_, tok := h.mustCreateAdmin(t)
	reg.Runtime("oci").RecordServe("daocloud", 5*time.Millisecond)

	protos := protoByName(t, h.do("GET", "/api/v1/admin/upstreams", tok, nil))
	assert.Equal(t, int64(1), protos["oci"].TotalServed)
	assert.Zero(t, protos["pypi"].TotalServed, "pypi must be unaffected by oci traffic")
}

func TestPatchUpstream_DisableAndReenable(t *testing.T) {
	h, _ := newUpstreamHarness(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("PATCH", "/api/v1/admin/upstreams/oci/daocloud", tok,
		jsonBody(PatchUpstreamRequest{Enabled: boolPtr(false)}))
	require.Equal(t, http.StatusOK, rr.Code)

	var p ProtocolUpstreams
	decodeJSON(t, rr, &p)
	// A disabled mirror must remain listed, or it could never be re-enabled.
	require.Len(t, p.Mirrors, 2)
	assert.False(t, mirrorByName(p)["daocloud"].Enabled)

	rr = h.do("PATCH", "/api/v1/admin/upstreams/oci/daocloud", tok,
		jsonBody(PatchUpstreamRequest{Enabled: boolPtr(true)}))
	require.Equal(t, http.StatusOK, rr.Code)
	decodeJSON(t, rr, &p)
	assert.True(t, mirrorByName(p)["daocloud"].Enabled)
}

func TestPatchUpstream_UnknownMirrorIs404(t *testing.T) {
	h, _ := newUpstreamHarness(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("PATCH", "/api/v1/admin/upstreams/oci/nope", tok,
		jsonBody(PatchUpstreamRequest{Enabled: boolPtr(false)}))
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestPatchUpstream_UnknownProtocolIs404(t *testing.T) {
	h, _ := newUpstreamHarness(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("PATCH", "/api/v1/admin/upstreams/npm/x", tok,
		jsonBody(PatchUpstreamRequest{Enabled: boolPtr(false)}))
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestReorderUpstreams_AppliesNewFallbackOrder(t *testing.T) {
	h, reg := newUpstreamHarness(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("POST", "/api/v1/admin/upstreams/oci/reorder", tok,
		jsonBody(ReorderUpstreamsRequest{Order: []string{"dockerhub", "daocloud"}}))
	require.Equal(t, http.StatusOK, rr.Code)

	var p ProtocolUpstreams
	decodeJSON(t, rr, &p)
	assert.Equal(t, "dockerhub", p.Mirrors[0].Name)
	assert.Equal(t, 0, p.Mirrors[0].Order)
	assert.True(t, p.Mirrors[0].Overridden, "drift from the YAML baseline must be visible")
	assert.Equal(t, 1, p.Mirrors[0].ConfigPriority, "config baseline is still reported")

	// The override must reach the fetch path, not just the response body.
	eff := reg.Runtime("oci").Effective([]upstream.Upstream{
		{Name: "daocloud", Priority: 0},
		{Name: "dockerhub", Priority: 1},
	})
	assert.Equal(t, "dockerhub", eff[0].Name)
}

// A partial order would leave unlisted mirrors at their config priority,
// interleaving them unpredictably. Reject rather than half-apply.
func TestReorderUpstreams_RejectsPartialList(t *testing.T) {
	h, _ := newUpstreamHarness(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("POST", "/api/v1/admin/upstreams/oci/reorder", tok,
		jsonBody(ReorderUpstreamsRequest{Order: []string{"dockerhub"}}))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestReorderUpstreams_RejectsUnknownMirror(t *testing.T) {
	h, _ := newUpstreamHarness(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("POST", "/api/v1/admin/upstreams/oci/reorder", tok,
		jsonBody(ReorderUpstreamsRequest{Order: []string{"dockerhub", "ghost"}}))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestReorderUpstreams_RejectsDuplicates(t *testing.T) {
	h, _ := newUpstreamHarness(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("POST", "/api/v1/admin/upstreams/oci/reorder", tok,
		jsonBody(ReorderUpstreamsRequest{Order: []string{"daocloud", "daocloud"}}))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestUnblockUpstream_ClearsBlockedState(t *testing.T) {
	h, reg := newUpstreamHarness(t)
	_, tok := h.mustCreateAdmin(t)

	// Trip the real circuit breaker the way the fetch path would: consecutive
	// transient failures, until the Runtime reports it blocked.
	rt := reg.Runtime("oci")
	blockMirror(t, rt, "daocloud")

	// Precondition: the mirror really is blocked in the live view.
	protos := protoByName(t, h.do("GET", "/api/v1/admin/upstreams", tok, nil))
	blocked := mirrorByName(protos["oci"])["daocloud"]
	require.True(t, blocked.Blocked)
	assert.Equal(t, "blocked", blocked.Health)
	assert.NotZero(t, blocked.BlockedUntilUnix)

	rr := h.do("POST", "/api/v1/admin/upstreams/oci/daocloud/unblock", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var p ProtocolUpstreams
	decodeJSON(t, rr, &p)
	m := mirrorByName(p)["daocloud"]
	assert.False(t, m.Blocked)
	assert.Zero(t, m.ConsecutiveFailures)
	assert.Zero(t, m.BlockedUntilUnix)
}

// Unblocking a mirror that is not blocked must not error.
func TestUnblockUpstream_IsIdempotent(t *testing.T) {
	h, _ := newUpstreamHarness(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("POST", "/api/v1/admin/upstreams/oci/daocloud/unblock", tok, nil)
	assert.Equal(t, http.StatusOK, rr.Code)
}

// Every mutating upstream endpoint changes what all tenants' misses hit, so
// each must be system-admin only.
func TestUpstreamMutations_RequireSystemAdmin(t *testing.T) {
	h, _ := newUpstreamHarness(t)
	_, tok := h.mustCreateUser(t, "plain@example.com")

	for name, call := range map[string]func() *httptest.ResponseRecorder{
		"reorder": func() *httptest.ResponseRecorder {
			return h.do("POST", "/api/v1/admin/upstreams/oci/reorder", tok,
				jsonBody(ReorderUpstreamsRequest{Order: []string{"dockerhub", "daocloud"}}))
		},
		"patch": func() *httptest.ResponseRecorder {
			return h.do("PATCH", "/api/v1/admin/upstreams/oci/daocloud", tok,
				jsonBody(PatchUpstreamRequest{Enabled: boolPtr(false)}))
		},
		"unblock": func() *httptest.ResponseRecorder {
			return h.do("POST", "/api/v1/admin/upstreams/oci/daocloud/unblock", tok, nil)
		},
		"list": func() *httptest.ResponseRecorder {
			return h.do("GET", "/api/v1/admin/upstreams", tok, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, http.StatusForbidden, call().Code)
		})
	}
}

// Without a Registry there is nothing for an override to act on, so accepting
// the request would be a silent no-op. It must say so instead.
func TestUpstreamMutations_501WithoutRuntime(t *testing.T) {
	h := newHarness(t) // no Upstreams dep
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("PATCH", "/api/v1/admin/upstreams/oci/dockerhub", tok,
		jsonBody(PatchUpstreamRequest{Enabled: boolPtr(false)}))
	assert.Equal(t, http.StatusNotImplemented, rr.Code)
}

func boolPtr(b bool) *bool { return &b }

// blockMirror trips a mirror's auto-block circuit breaker through the same
// exported path the fetch loop uses, rather than reaching into its internals.
// The failure threshold is the upstream package's own business, so this loops
// until the breaker actually trips instead of hard-coding the count.
func blockMirror(t *testing.T, rt *upstream.Runtime, name string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if rt.RecordFailure(name, errors.New("HTTP 503"), true) {
			return
		}
	}
	t.Fatalf("mirror %q did not trip the circuit breaker", name)
}
