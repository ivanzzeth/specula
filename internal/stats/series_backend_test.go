package stats

import (
	"context"
	"sync"
	"testing"

	"github.com/ivanzzeth/specula/internal/artifact"
)

type fakeSeriesBackend struct {
	mu    sync.Mutex
	byKey map[string][]SeriesPoint
}

func (f *fakeSeriesBackend) Record(_ context.Context, protocol string, bytes int64, unix int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byKey == nil {
		f.byKey = make(map[string][]SeriesPoint)
	}
	f.byKey[protocol] = append(f.byKey[protocol], SeriesPoint{Unix: unix, Bytes: bytes})
	return nil
}

func (f *fakeSeriesBackend) Snapshot(_ context.Context, protocol string, limit int) ([]SeriesPoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pts := f.byKey[protocol]
	if limit <= 0 || len(pts) <= limit {
		out := make([]SeriesPoint, len(pts))
		copy(out, pts)
		return out, nil
	}
	out := make([]SeriesPoint, limit)
	copy(out, pts[len(pts)-limit:])
	return out, nil
}

func TestCollector_SeriesUsesBackendWhenSet(t *testing.T) {
	backend := &fakeSeriesBackend{}
	store := &fakeStore{
		stats: map[string]artifact.SizeStat{
			"oci": {Bytes: 500, Objects: 2},
		},
	}
	c := NewCollectorWithStoreAndSeries(store, backend).(*collector)
	c.Refresh(context.Background())

	pts, err := c.Series(context.Background(), "oci")
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].Bytes != 500 {
		t.Fatalf("oci series=%v", pts)
	}

	backend.mu.Lock()
	if len(backend.byKey["oci"]) != 1 {
		t.Fatalf("backend record count=%d want 1", len(backend.byKey["oci"]))
	}
	backend.mu.Unlock()
}

func TestCollector_SeriesBackendGrandTotal(t *testing.T) {
	backend := &fakeSeriesBackend{}
	store := &fakeStore{
		stats: map[string]artifact.SizeStat{
			"oci":  {Bytes: 100, Objects: 1},
			"npm":  {Bytes: 200, Objects: 1},
		},
	}
	c := NewCollectorWithStoreAndSeries(store, backend)
	c.Refresh(context.Background())

	total, err := c.Series(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(total) != 1 || total[0].Bytes != 300 {
		t.Fatalf("total series=%v want bytes=300", total)
	}
}
