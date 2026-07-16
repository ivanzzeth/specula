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

// newTestCollector creates a collector with a fresh Prometheus registry so that
// multiple sub-tests can each create collectors without duplicate-metric panics.
func newTestCollector(store meta.MetadataStore) *collector {
	return newCollector(store, prometheus.NewRegistry())
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

	// Standalone collector with no store, but a du path registered.
	c := newTestCollector(nil)
	c.AddDUPath(dir)

	total, err := c.Total(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1200), total.Bytes, "du fallback should add directory file sizes")
}

func TestTotal_DUPath_Unreachable(t *testing.T) {
	c := newTestCollector(nil)
	// Register a non-existent path; Total must not error.
	c.AddDUPath("/nonexistent/path/that/does/not/exist")

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
	c := newCollector(store, reg)

	_, err := c.ByProtocol(context.Background())
	require.NoError(t, err)

	// Gather all metrics from the fresh registry and verify the gauge values.
	mfs, err := reg.Gather()
	require.NoError(t, err)

	found := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "protocol" {
					found[mf.GetName()+"["+lp.GetValue()+"]"] = m.GetGauge().GetValue()
				}
			}
		}
	}

	assert.Equal(t, float64(4096), found["specula_cache_bytes[oci]"], "cache_bytes gauge")
	assert.Equal(t, float64(8), found["specula_cache_objects[oci]"], "cache_objects gauge")
}

func TestPrometheusGauges_RecordPutUpdatesGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := newCollector(nil, reg)

	require.NoError(t, c.RecordPut(context.Background(), "npm", 512))

	mfs, err := reg.Gather()
	require.NoError(t, err)

	found := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "protocol" {
					found[mf.GetName()+"["+lp.GetValue()+"]"] = m.GetGauge().GetValue()
				}
			}
		}
	}

	assert.Equal(t, float64(512), found["specula_cache_bytes[npm]"])
	assert.Equal(t, float64(1), found["specula_cache_objects[npm]"])
}
