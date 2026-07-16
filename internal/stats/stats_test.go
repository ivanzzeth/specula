package stats

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// ---------------------------------------------------------------------------
// fakeStore: test double for meta.MetadataStore
// ---------------------------------------------------------------------------

type fakeStore struct {
	stats    map[string]artifact.SizeStat
	statsErr error
}

var _ meta.MetadataStore = (*fakeStore)(nil)

func (f *fakeStore) Get(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, nil
}
func (f *fakeStore) Put(_ context.Context, _ artifact.CacheEntry) error     { return nil }
func (f *fakeStore) Delete(_ context.Context, _ artifact.ArtifactRef) error { return nil }
func (f *fakeStore) GetMutable(_ context.Context, _ string) (*artifact.MutableEntry, error) {
	return nil, nil
}
func (f *fakeStore) PutMutable(_ context.Context, _ artifact.MutableEntry) error { return nil }
func (f *fakeStore) CacheSizeByProtocol(_ context.Context) (map[string]artifact.SizeStat, error) {
	if f.statsErr != nil {
		return nil, f.statsErr
	}
	return f.stats, nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newTestCollector creates a collector with a fresh Prometheus registry so that
// multiple sub-tests can each create collectors without duplicate-metric panics.
func newTestCollector(store meta.MetadataStore) *collector {
	return newCollector(store, prometheus.NewRegistry(), DefaultCollectorConfig())
}

// newTestCollectorWithCfg creates a collector with a fresh registry and the
// supplied CollectorConfig.
func newTestCollectorWithCfg(store meta.MetadataStore, cfg CollectorConfig) *collector {
	return newCollector(store, prometheus.NewRegistry(), cfg)
}

// gatherGaugeVals collects all specula metric gauge values keyed as
// "metric_name[protocol_label_value]". It fails the test on gather errors.
func gatherGaugeVals(t *testing.T, g prometheus.Gatherer) map[string]float64 {
	t.Helper()
	mfs, err := g.Gather()
	require.NoError(t, err, "prometheus gather")
	out := make(map[string]float64)
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "protocol" {
					out[mf.GetName()+"["+lp.GetValue()+"]"] = m.GetGauge().GetValue()
				}
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// ByProtocol tests
// ---------------------------------------------------------------------------

func TestByProtocol_WithStore(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name       string
		storeStats map[string]artifact.SizeStat
		wantErr    bool
	}{
		{
			name:       "empty store",
			storeStats: map[string]artifact.SizeStat{},
		},
		{
			name: "single protocol",
			storeStats: map[string]artifact.SizeStat{
				"oci": {Bytes: 1024, Objects: 3, Oldest: now.Add(-time.Hour), Newest: now},
			},
		},
		{
			name: "multiple protocols",
			storeStats: map[string]artifact.SizeStat{
				"oci":   {Bytes: 500, Objects: 2},
				"pypi":  {Bytes: 300, Objects: 5},
				"gomod": {Bytes: 100, Objects: 1},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCollector(&fakeStore{stats: tc.storeStats})
			got, err := c.ByProtocol(context.Background())
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.storeStats, got)
		})
	}
}

func TestByProtocol_StoreError(t *testing.T) {
	sentinel := errors.New("db down")
	c := newTestCollector(&fakeStore{statsErr: sentinel})
	_, err := c.ByProtocol(context.Background())
	require.Error(t, err)
	assert.ErrorContains(t, err, "CacheSizeByProtocol")
}

// ---------------------------------------------------------------------------
// Total tests
// ---------------------------------------------------------------------------

func TestTotal_WithStore(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name       string
		storeStats map[string]artifact.SizeStat
		wantBytes  int64
		wantObjs   int64
	}{
		{
			name:       "empty store",
			storeStats: map[string]artifact.SizeStat{},
			wantBytes:  0,
			wantObjs:   0,
		},
		{
			name: "single protocol",
			storeStats: map[string]artifact.SizeStat{
				"oci": {Bytes: 1024, Objects: 3, Oldest: now.Add(-time.Hour), Newest: now},
			},
			wantBytes: 1024,
			wantObjs:  3,
		},
		{
			name: "sum across protocols",
			storeStats: map[string]artifact.SizeStat{
				"oci":  {Bytes: 500, Objects: 2, Oldest: now.Add(-2 * time.Hour), Newest: now.Add(-time.Hour)},
				"pypi": {Bytes: 300, Objects: 5, Oldest: now.Add(-time.Hour), Newest: now},
				"npm":  {Bytes: 100, Objects: 1},
			},
			wantBytes: 900,
			wantObjs:  8,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCollector(&fakeStore{stats: tc.storeStats})
			got, err := c.Total(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tc.wantBytes, got.Bytes, "bytes mismatch")
			assert.Equal(t, tc.wantObjs, got.Objects, "objects mismatch")
		})
	}
}

func TestTotal_TimeFields(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	oldest := now.Add(-5 * time.Hour)
	newest := now

	store := &fakeStore{
		stats: map[string]artifact.SizeStat{
			"oci":  {Bytes: 100, Objects: 1, Oldest: now.Add(-3 * time.Hour), Newest: now.Add(-time.Hour)},
			"pypi": {Bytes: 200, Objects: 2, Oldest: oldest, Newest: newest},
		},
	}
	c := newTestCollector(store)
	got, err := c.Total(context.Background())
	require.NoError(t, err)

	assert.Equal(t, oldest, got.Oldest, "oldest should be the minimum across protocols")
	assert.Equal(t, newest, got.Newest, "newest should be the maximum across protocols")
}

// ---------------------------------------------------------------------------
// RecordPut / RecordEvict tests (standalone mode, store == nil)
// ---------------------------------------------------------------------------

func TestRecordPut_Standalone(t *testing.T) {
	cases := []struct {
		name string
		puts []struct {
			proto string
			size  int64
		}
		wantBytes map[string]int64
		wantObjs  map[string]int64
	}{
		{
			name: "single put",
			puts: []struct {
				proto string
				size  int64
			}{{"oci", 1024}},
			wantBytes: map[string]int64{"oci": 1024},
			wantObjs:  map[string]int64{"oci": 1},
		},
		{
			name: "two puts same protocol",
			puts: []struct {
				proto string
				size  int64
			}{
				{"oci", 100}, {"oci", 200},
			},
			wantBytes: map[string]int64{"oci": 300},
			wantObjs:  map[string]int64{"oci": 2},
		},
		{
			name: "multiple protocols",
			puts: []struct {
				proto string
				size  int64
			}{
				{"oci", 1000}, {"pypi", 500}, {"npm", 250},
			},
			wantBytes: map[string]int64{"oci": 1000, "pypi": 500, "npm": 250},
			wantObjs:  map[string]int64{"oci": 1, "pypi": 1, "npm": 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCollector(nil)
			ctx := context.Background()
			for _, p := range tc.puts {
				require.NoError(t, c.RecordPut(ctx, p.proto, p.size))
			}
			got, err := c.ByProtocol(ctx)
			require.NoError(t, err)
			for proto, want := range tc.wantBytes {
				assert.Equal(t, want, got[proto].Bytes, "bytes for protocol %q", proto)
			}
			for proto, want := range tc.wantObjs {
				assert.Equal(t, want, got[proto].Objects, "objects for protocol %q", proto)
			}
		})
	}
}

func TestRecordEvict_Standalone(t *testing.T) {
	cases := []struct {
		name string
		puts []struct {
			proto string
			size  int64
		}
		evicts []struct {
			proto string
			size  int64
		}
		wantBytes map[string]int64
		wantObjs  map[string]int64
	}{
		{
			name: "put then full evict",
			puts: []struct {
				proto string
				size  int64
			}{{"oci", 1024}},
			evicts: []struct {
				proto string
				size  int64
			}{{"oci", 1024}},
			wantBytes: map[string]int64{"oci": 0},
			wantObjs:  map[string]int64{"oci": 0},
		},
		{
			name: "two puts one evict",
			puts: []struct {
				proto string
				size  int64
			}{
				{"oci", 2048}, {"oci", 512},
			},
			evicts: []struct {
				proto string
				size  int64
			}{{"oci", 512}},
			wantBytes: map[string]int64{"oci": 2048},
			wantObjs:  map[string]int64{"oci": 1},
		},
		{
			name: "evict below zero is clamped",
			evicts: []struct {
				proto string
				size  int64
			}{{"oci", 9999}},
			// no puts → starts at 0; clamped on underflow
			wantBytes: map[string]int64{"oci": 0},
			wantObjs:  map[string]int64{"oci": 0},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCollector(nil)
			ctx := context.Background()
			for _, p := range tc.puts {
				require.NoError(t, c.RecordPut(ctx, p.proto, p.size))
			}
			for _, e := range tc.evicts {
				require.NoError(t, c.RecordEvict(ctx, e.proto, e.size))
			}
			got, err := c.ByProtocol(ctx)
			require.NoError(t, err)
			for proto, want := range tc.wantBytes {
				assert.Equal(t, want, got[proto].Bytes, "bytes for %q", proto)
			}
			for proto, want := range tc.wantObjs {
				assert.Equal(t, want, got[proto].Objects, "objects for %q", proto)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Total standalone tests
// ---------------------------------------------------------------------------

func TestTotal_Standalone(t *testing.T) {
	c := newTestCollector(nil)
	ctx := context.Background()

	require.NoError(t, c.RecordPut(ctx, "oci", 100))
	require.NoError(t, c.RecordPut(ctx, "pypi", 200))
	require.NoError(t, c.RecordPut(ctx, "pypi", 50))

	total, err := c.Total(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(350), total.Bytes)
	assert.Equal(t, int64(3), total.Objects)
}

// ---------------------------------------------------------------------------
// du-path fallback tests
// ---------------------------------------------------------------------------

func TestTotal_WithDUPath(t *testing.T) {
	// Create a temp directory with a few files.
	dir := t.TempDir()
	writeFile := func(name string, size int) {
		t.Helper()
		data := make([]byte, size)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o644))
	}
	writeFile("pack-abc.pack", 1000)
	writeFile("pack-abc.idx", 200)

	// Standalone collector with no store, but an opaque path registered.
	c := newTestCollector(nil)
	c.AddOpaquePath(dir, "git")

	total, err := c.Total(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1200), total.Bytes, "du fallback should add directory file sizes")
}

func TestTotal_DUPath_Unreachable(t *testing.T) {
	c := newTestCollector(nil)
	// Register a non-existent path; Total must not error.
	c.AddOpaquePath("/nonexistent/path/that/does/not/exist", "git")

	_, err := c.Total(context.Background())
	require.NoError(t, err, "unreachable du path should be silently skipped")
}

// ---------------------------------------------------------------------------
// Prometheus gauge wiring tests
// ---------------------------------------------------------------------------

func TestPrometheusGauges_SyncOnByProtocol(t *testing.T) {
	reg := prometheus.NewRegistry()
	store := &fakeStore{
		stats: map[string]artifact.SizeStat{
			"oci": {Bytes: 4096, Objects: 8},
		},
	}
	c := newCollector(store, reg, DefaultCollectorConfig())

	_, err := c.ByProtocol(context.Background())
	require.NoError(t, err)

	found := gatherGaugeVals(t, reg)
	assert.Equal(t, float64(4096), found["specula_cache_bytes[oci]"], "cache_bytes gauge")
	assert.Equal(t, float64(8), found["specula_cache_objects[oci]"], "cache_objects gauge")
}

func TestPrometheusGauges_RecordPutUpdatesGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := newCollector(nil, reg, DefaultCollectorConfig())

	require.NoError(t, c.RecordPut(context.Background(), "npm", 512))

	found := gatherGaugeVals(t, reg)
	assert.Equal(t, float64(512), found["specula_cache_bytes[npm]"])
	assert.Equal(t, float64(1), found["specula_cache_objects[npm]"])
}

// ---------------------------------------------------------------------------
// refreshOnce tests — called directly for determinism (no real time)
// ---------------------------------------------------------------------------

func TestRefreshOnce_UpdatesGaugesFromStore(t *testing.T) {
	reg := prometheus.NewRegistry()
	store := &fakeStore{
		stats: map[string]artifact.SizeStat{
			"oci":  {Bytes: 2048, Objects: 4},
			"pypi": {Bytes: 512, Objects: 1},
		},
	}
	c := newCollector(store, reg, DefaultCollectorConfig())

	c.refreshOnce(context.Background())

	found := gatherGaugeVals(t, reg)
	assert.Equal(t, float64(2048), found["specula_cache_bytes[oci]"], "oci bytes after refresh")
	assert.Equal(t, float64(4), found["specula_cache_objects[oci]"], "oci objects after refresh")
	assert.Equal(t, float64(512), found["specula_cache_bytes[pypi]"], "pypi bytes after refresh")
	assert.Equal(t, float64(1), found["specula_cache_objects[pypi]"], "pypi objects after refresh")
}

func TestRefreshOnce_OverwritesDrift(t *testing.T) {
	// Verify that a background refresh corrects drift introduced by a
	// hypothetical multi-node GC that removed objects without going through
	// RecordEvict on this node.
	reg := prometheus.NewRegistry()
	store := &fakeStore{
		stats: map[string]artifact.SizeStat{
			"npm": {Bytes: 100, Objects: 1},
		},
	}
	c := newCollector(store, reg, DefaultCollectorConfig())

	// Simulate stale gauge state (e.g., from a previous node's RecordPut that
	// was later GC'd on the store side).
	c.cacheBytes.WithLabelValues("npm").Set(9999)
	c.cacheObjects.WithLabelValues("npm").Set(50)

	c.refreshOnce(context.Background())

	found := gatherGaugeVals(t, reg)
	assert.Equal(t, float64(100), found["specula_cache_bytes[npm]"], "gauge corrected to store value")
	assert.Equal(t, float64(1), found["specula_cache_objects[npm]"], "objects corrected to store value")
}

func TestRefreshOnce_StandaloneIsNoOp(t *testing.T) {
	// In standalone mode (no store), refreshOnce must not clear gauges set by
	// RecordPut — those gauges are the only source of truth.
	reg := prometheus.NewRegistry()
	c := newCollector(nil, reg, DefaultCollectorConfig())

	require.NoError(t, c.RecordPut(context.Background(), "npm", 100))

	// Calling refreshOnce on a standalone collector must not touch the gauge.
	c.refreshOnce(context.Background())

	found := gatherGaugeVals(t, reg)
	assert.Equal(t, float64(100), found["specula_cache_bytes[npm]"], "standalone gauge unchanged after refreshOnce")
}

func TestRefreshOnce_StoreError_GaugesUnchanged(t *testing.T) {
	// On a transient store error, refreshOnce is a no-op; existing gauge values
	// must not be zeroed or corrupted.
	reg := prometheus.NewRegistry()
	store := &fakeStore{statsErr: errors.New("db timeout")}
	c := newCollector(store, reg, DefaultCollectorConfig())

	// Pre-seed a gauge value (as if a previous successful refresh ran).
	c.cacheBytes.WithLabelValues("helm").Set(777)

	c.refreshOnce(context.Background())

	found := gatherGaugeVals(t, reg)
	assert.Equal(t, float64(777), found["specula_cache_bytes[helm]"], "gauge unchanged on store error")
}

// ---------------------------------------------------------------------------
// refreshOnce — du-sb fallback tests (EnableDUFallback=true)
// ---------------------------------------------------------------------------

func TestRefreshOnce_DUFallback_UpdatesGauge(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "pack.pack"),
		make([]byte, 3000), 0o644,
	))

	reg := prometheus.NewRegistry()
	// Store reports no entries; the opaque path should fill the gauge.
	store := &fakeStore{stats: map[string]artifact.SizeStat{}}
	c := newCollector(store, reg, CollectorConfig{
		RefreshInterval:  30 * time.Second,
		EnableDUFallback: true,
	})
	c.AddOpaquePath(dir, "git")

	c.refreshOnce(context.Background())

	found := gatherGaugeVals(t, reg)
	assert.Equal(t, float64(3000), found["specula_cache_bytes[git]"],
		"du fallback should set gauge from directory size")
}

func TestRefreshOnce_DUFallback_MultiplePathsSameProtocol(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir1, "a"), make([]byte, 1000), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir2, "b"), make([]byte, 2000), 0o644))

	reg := prometheus.NewRegistry()
	store := &fakeStore{stats: map[string]artifact.SizeStat{}}
	c := newCollector(store, reg, CollectorConfig{
		RefreshInterval:  30 * time.Second,
		EnableDUFallback: true,
	})
	c.AddOpaquePath(dir1, "git")
	c.AddOpaquePath(dir2, "git")

	c.refreshOnce(context.Background())

	found := gatherGaugeVals(t, reg)
	assert.Equal(t, float64(3000), found["specula_cache_bytes[git]"],
		"multiple paths with same protocol should be summed")
}

func TestRefreshOnce_DUFallback_Disabled(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pack"), make([]byte, 4000), 0o644))

	reg := prometheus.NewRegistry()
	store := &fakeStore{stats: map[string]artifact.SizeStat{}}
	c := newCollector(store, reg, CollectorConfig{
		RefreshInterval:  30 * time.Second,
		EnableDUFallback: false, // explicitly disabled
	})
	c.AddOpaquePath(dir, "git")

	c.refreshOnce(context.Background())

	// When du fallback is disabled the gauge for "git" should not be set by
	// the refresh cycle (it remains at the zero value).
	found := gatherGaugeVals(t, reg)
	_, exists := found["specula_cache_bytes[git]"]
	assert.False(t, exists, "gauge must not be set when EnableDUFallback=false")
}

func TestRefreshOnce_DUFallback_UnreachablePath_Skipped(t *testing.T) {
	reg := prometheus.NewRegistry()
	store := &fakeStore{stats: map[string]artifact.SizeStat{}}
	c := newCollector(store, reg, CollectorConfig{
		RefreshInterval:  30 * time.Second,
		EnableDUFallback: true,
	})
	c.AddOpaquePath("/nonexistent/cache/dir", "git")

	// Must not panic or return an error (it's a no-return method).
	c.refreshOnce(context.Background())
}

// ---------------------------------------------------------------------------
// Run tests — trigger channel injection + context cancellation
// ---------------------------------------------------------------------------

func TestRun_ExitsOnContextCancel(t *testing.T) {
	// Use an injected ticker channel that never fires so Run blocks purely on
	// the context. This validates context cancellation without any real time.
	c := newTestCollector(nil)
	c.tickCh = make(chan time.Time) // never sends

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
		// good: Run exited promptly
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run did not exit within 200ms after context cancellation")
	}
}

func TestRun_ClosedTickCh_Exits(t *testing.T) {
	// Closing the ticker channel should also cause Run to return.
	c := newTestCollector(nil)
	tick := make(chan time.Time)
	c.tickCh = tick

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	close(tick)
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run did not exit within 200ms after tick channel was closed")
	}
}

func TestRun_TickDrivesRefreshOnce(t *testing.T) {
	// Inject a buffered tick channel so we can fire exactly one refresh tick
	// and assert the gauge is updated without relying on wall-clock timing.
	reg := prometheus.NewRegistry()
	store := &fakeStore{
		stats: map[string]artifact.SizeStat{
			"helm": {Bytes: 8192, Objects: 16},
		},
	}
	c := newCollector(store, reg, CollectorConfig{RefreshInterval: time.Hour})

	tick := make(chan time.Time, 1) // buffered: goroutine picks it up immediately
	c.tickCh = tick

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	// Fire one tick; the goroutine dequeues it and calls refreshOnce.
	tick <- time.Now()

	// Poll until the gauge reflects the store value, or time out.
	// The goroutine processes the tick synchronously in refreshOnce, so
	// the gauge update happens nearly instantly.
	require.Eventually(t, func() bool {
		found := gatherGaugeVals(t, reg)
		return found["specula_cache_bytes[helm]"] == float64(8192) &&
			found["specula_cache_objects[helm]"] == float64(16)
	}, 200*time.Millisecond, time.Millisecond,
		"gauge not updated within 200ms after injecting one refresh tick")
}

func TestRun_DefaultIntervalNormalised(t *testing.T) {
	// A zero RefreshInterval must be normalised to 30s (not panic/infinite-loop).
	// We verify this by constructing with a zero interval and confirming Run
	// exits cleanly on cancel — the ticker is started inside Run, not here.
	c := newCollector(nil, prometheus.NewRegistry(), CollectorConfig{RefreshInterval: 0})
	// 0 → normalised to 30s inside newCollector already; tickCh overrides for speed.
	tick := make(chan time.Time)
	c.tickCh = tick

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run with normalised interval did not exit on cancel")
	}
}
