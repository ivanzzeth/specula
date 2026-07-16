package upstream

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// chainOf is the configured baseline used by most tests: three mirrors in
// explicit priority order.
func chainOf() []Upstream {
	return []Upstream{
		{Name: "fast", BaseURL: "https://fast.example", Priority: 0},
		{Name: "mid", BaseURL: "https://mid.example", Priority: 1},
		{Name: "origin", BaseURL: "https://origin.example", Priority: 2, Official: true},
	}
}

// stateByName indexes a snapshot for direct assertions.
func stateByName(states []MirrorState) map[string]MirrorState {
	out := make(map[string]MirrorState, len(states))
	for _, s := range states {
		out[s.Name] = s
	}
	return out
}

func TestSnapshot_ReportsConfigBaselineBeforeAnyTraffic(t *testing.T) {
	rt := NewRuntime("oci")

	states := rt.Snapshot(chainOf())
	require.Len(t, states, 3)

	// Order must be fallback order, not config-slice order.
	assert.Equal(t, []string{"fast", "mid", "origin"},
		[]string{states[0].Name, states[1].Name, states[2].Name})

	s := states[0]
	assert.Equal(t, "oci", s.Protocol)
	assert.Equal(t, "https://fast.example", s.BaseURL)
	assert.Equal(t, 0, s.ConfigPriority)
	assert.Equal(t, 0, s.Priority)
	assert.False(t, s.Overridden)
	assert.True(t, s.Enabled, "mirrors are enabled unless overridden")

	// The whole point of HealthUnknown: an unqueried mirror must not be
	// reported as healthy, and its latency must be flagged unmeasured.
	assert.Equal(t, HealthUnknown, s.Health)
	assert.False(t, s.LastLatencyValid)
	assert.Zero(t, s.ServedCount)
	assert.True(t, s.LastServedAt.IsZero())

	assert.True(t, states[2].Official)
}

func TestSnapshot_RecordsServeMeasurements(t *testing.T) {
	rt := NewRuntime("oci")
	rt.RecordServe("mid", 42*time.Millisecond)
	rt.RecordServe("mid", 17*time.Millisecond)

	s := stateByName(rt.Snapshot(chainOf()))["mid"]
	assert.Equal(t, HealthUp, s.Health)
	assert.Equal(t, int64(2), s.ServedCount)
	assert.Equal(t, 17*time.Millisecond, s.LastLatency, "latency is last, not average")
	assert.True(t, s.LastLatencyValid)
	assert.False(t, s.LastServedAt.IsZero())
}

func TestSnapshot_ProbingWhileFailingBelowThreshold(t *testing.T) {
	rt := NewRuntime("oci")
	rt.RecordFailure("mid", errors.New("connection refused"), true)

	s := stateByName(rt.Snapshot(chainOf()))["mid"]
	assert.Equal(t, HealthProbing, s.Health, "failing but not yet blocked = probing")
	assert.False(t, s.Blocked)
	assert.Equal(t, 1, s.ConsecutiveFailures)
	assert.Equal(t, "connection refused", s.LastErr)
}

func TestSnapshot_BlockedWhenCircuitBreakerTrips(t *testing.T) {
	rt := NewRuntime("oci")
	for i := 0; i < defaultMaxFailures; i++ {
		rt.RecordFailure("mid", errors.New("HTTP 503"), true)
	}

	s := stateByName(rt.Snapshot(chainOf()))["mid"]
	assert.Equal(t, HealthBlocked, s.Health)
	assert.True(t, s.Blocked)
	assert.False(t, s.BlockedUntil.IsZero(), "a blocked mirror must report its window")
	assert.Equal(t, "HTTP 503", s.LastErr)
}

func TestSnapshot_SuccessClearsLastErr(t *testing.T) {
	rt := NewRuntime("oci")
	rt.RecordFailure("mid", errors.New("boom"), true)
	rt.RecordServe("mid", time.Millisecond)

	s := stateByName(rt.Snapshot(chainOf()))["mid"]
	assert.Empty(t, s.LastErr, "a stale error must not linger after a success")
	assert.Equal(t, HealthUp, s.Health)
}

func TestUnblock_ReadmitsMirrorImmediately(t *testing.T) {
	rt := NewRuntime("oci")
	for i := 0; i < defaultMaxFailures; i++ {
		rt.blocker.recordFailure("mid")
	}
	require.True(t, rt.blocker.isBlocked("mid"))

	rt.Unblock("mid")

	s := stateByName(rt.Snapshot(chainOf()))["mid"]
	assert.False(t, s.Blocked)
	assert.Zero(t, s.ConsecutiveFailures, "unblock must also reset the streak")
	assert.Empty(t, s.LastErr)
}

func TestSetEnabled_RemovesMirrorFromChainButKeepsItVisible(t *testing.T) {
	rt := NewRuntime("oci")
	rt.SetEnabled("fast", false)

	// effective() drives the fetch path: the disabled mirror must be gone.
	eff := rt.effective(chainOf())
	require.Len(t, eff, 2)
	assert.Equal(t, []string{"mid", "origin"}, []string{eff[0].Name, eff[1].Name})

	// Snapshot() drives the operator view: it must still be listed, or an
	// operator could never re-enable what they just disabled.
	states := rt.Snapshot(chainOf())
	require.Len(t, states, 3)
	assert.False(t, stateByName(states)["fast"].Enabled)

	rt.SetEnabled("fast", true)
	assert.Len(t, rt.effective(chainOf()), 3)
}

func TestReorder_ChangesEffectiveFallbackOrder(t *testing.T) {
	rt := NewRuntime("oci")
	require.NoError(t, rt.Reorder([]string{"origin", "fast", "mid"}))

	eff := rt.effective(chainOf())
	assert.Equal(t, []string{"origin", "fast", "mid"},
		[]string{eff[0].Name, eff[1].Name, eff[2].Name})

	states := rt.Snapshot(chainOf())
	assert.Equal(t, []string{"origin", "fast", "mid"},
		[]string{states[0].Name, states[1].Name, states[2].Name})

	origin := stateByName(states)["origin"]
	assert.Equal(t, 0, origin.Priority, "effective priority reflects the reorder")
	assert.Equal(t, 2, origin.ConfigPriority, "config baseline is still reported")
	assert.True(t, origin.Overridden, "drift from the YAML baseline must be visible")
}

func TestReorder_RejectsDuplicateNames(t *testing.T) {
	rt := NewRuntime("oci")
	err := rt.Reorder([]string{"fast", "fast", "mid"})

	require.Error(t, err, "an ambiguous order must not silently apply")
	assert.Contains(t, err.Error(), "duplicate")

	// The rejected request must not have partially applied.
	eff := rt.effective(chainOf())
	assert.Equal(t, []string{"fast", "mid", "origin"},
		[]string{eff[0].Name, eff[1].Name, eff[2].Name})
}

func TestEffective_DoesNotMutateConfigSlice(t *testing.T) {
	rt := NewRuntime("oci")
	require.NoError(t, rt.Reorder([]string{"origin", "fast", "mid"}))

	cfg := chainOf()
	_ = rt.effective(cfg)

	// Config is shared, immutable state; an override must never leak into it.
	assert.Equal(t, 2, cfg[2].Priority, "config baseline must be untouched")
	assert.Equal(t, "origin", cfg[2].Name)
}

func TestClearOverrides_ReturnsToConfigBaseline(t *testing.T) {
	rt := NewRuntime("oci")
	rt.SetEnabled("fast", false)
	require.NoError(t, rt.Reorder([]string{"origin", "mid", "fast"}))

	rt.ClearOverrides()

	eff := rt.effective(chainOf())
	require.Len(t, eff, 3)
	assert.Equal(t, []string{"fast", "mid", "origin"},
		[]string{eff[0].Name, eff[1].Name, eff[2].Name})
}

func TestRegistry_RuntimeIsPerProtocolAndStable(t *testing.T) {
	reg := NewRegistry()

	oci := reg.Runtime("oci")
	pypi := reg.Runtime("pypi")

	assert.Same(t, oci, reg.Runtime("oci"), "get-or-create must be stable")
	assert.NotSame(t, oci, pypi, "protocols must not share state")
	assert.Equal(t, "oci", oci.Protocol())

	// State must not bleed across protocols even for an identical mirror name.
	oci.RecordServe("shared", time.Millisecond)
	assert.Equal(t, HealthUp, stateByName(oci.Snapshot([]Upstream{{Name: "shared"}}))["shared"].Health)
	assert.Equal(t, HealthUnknown, stateByName(pypi.Snapshot([]Upstream{{Name: "shared"}}))["shared"].Health)

	assert.Equal(t, []string{"oci", "pypi"}, reg.Protocols())
}

func TestRegistry_LookupDistinguishesUnknownProtocol(t *testing.T) {
	reg := NewRegistry()
	reg.Runtime("oci")

	_, ok := reg.Lookup("oci")
	assert.True(t, ok)

	_, ok = reg.Lookup("npm")
	assert.False(t, ok, "Lookup must not create, so unknown protocols stay distinguishable")
}

// ── client ↔ runtime integration ─────────────────────────────────────────────

// A client bound to a Runtime must record what actually happened on the wire.
func TestClientWithRuntime_RecordsRealServeAndLatency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(15 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	rt := NewRuntime("oci")
	c := newFallbackClientWithRuntime(rt)

	ups := []Upstream{{Name: "only", BaseURL: srv.URL, Priority: 0}}
	body, _, err := c.Fetch(t.Context(), artifact.ArtifactRef{Protocol: "oci", Name: "x", Version: "1"}, ups)
	require.NoError(t, err)
	_ = body.Close()

	s := stateByName(rt.Snapshot(ups))["only"]
	assert.Equal(t, HealthUp, s.Health)
	assert.Equal(t, int64(1), s.ServedCount)
	assert.True(t, s.LastLatencyValid)
	assert.GreaterOrEqual(t, s.LastLatency, 15*time.Millisecond,
		"latency must reflect the real round-trip")
}

// The disable override must actually steer the fetch path, not just the view.
func TestClientWithRuntime_HonoursDisableOverride(t *testing.T) {
	var disabledHits int
	disabled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		disabledHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer disabled.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer fallback.Close()

	rt := NewRuntime("oci")
	c := newFallbackClientWithRuntime(rt)
	ups := []Upstream{
		{Name: "first", BaseURL: disabled.URL, Priority: 0},
		{Name: "second", BaseURL: fallback.URL, Priority: 1},
	}

	rt.SetEnabled("first", false)
	body, meta, err := c.Fetch(t.Context(), artifact.ArtifactRef{Protocol: "oci", Name: "x", Version: "1"}, ups)
	require.NoError(t, err)
	_ = body.Close()

	assert.Zero(t, disabledHits, "a disabled mirror must never be contacted")
	assert.Equal(t, "second", meta.Upstream)
	assert.Equal(t, int64(1), stateByName(rt.Snapshot(ups))["second"].ServedCount)
}

// A failing mirror must surface a real reason to the operator view.
func TestClientWithRuntime_RecordsFailureReason(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()

	rt := NewRuntime("oci")
	c := newFallbackClientWithRuntime(rt)
	c.maxAttempts = 1
	c.backoffBase = time.Millisecond
	ups := []Upstream{
		{Name: "bad", BaseURL: bad.URL, Priority: 0},
		{Name: "good", BaseURL: good.URL, Priority: 1},
	}

	body, _, err := c.Fetch(t.Context(), artifact.ArtifactRef{Protocol: "oci", Name: "x", Version: "1"}, ups)
	require.NoError(t, err)
	_ = body.Close()

	states := stateByName(rt.Snapshot(ups))
	assert.Contains(t, states["bad"].LastErr, "500")
	assert.Equal(t, HealthProbing, states["bad"].Health)
	assert.Equal(t, HealthUp, states["good"].Health)
}

// The Runtime and the fetch path must share one block tracker, not two copies.
func TestClientWithRuntime_SharesBlockStateWithAdminView(t *testing.T) {
	rt := NewRuntime("oci")
	c := newFallbackClientWithRuntime(rt)

	for i := 0; i < defaultMaxFailures; i++ {
		c.blocker.recordFailure("mid")
	}

	assert.True(t, stateByName(rt.Snapshot(chainOf()))["mid"].Blocked,
		"the admin view must observe the fetch path's block state")

	rt.Unblock("mid")
	assert.False(t, c.blocker.isBlocked("mid"),
		"an admin unblock must re-admit the mirror on the fetch path")
}
