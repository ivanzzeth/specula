package stats

import (
	"context"
	"testing"

	"github.com/ivanzzeth/specula/internal/artifact"
)

func TestSeriesRing_WrapsAndOrders(t *testing.T) {
	r := newSeriesRing(3)
	r.push(SeriesPoint{Unix: 1, Bytes: 10})
	r.push(SeriesPoint{Unix: 2, Bytes: 20})
	r.push(SeriesPoint{Unix: 3, Bytes: 30})
	r.push(SeriesPoint{Unix: 4, Bytes: 40}) // drops unix=1
	got := r.snapshot()
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	if got[0].Unix != 2 || got[2].Unix != 4 || got[2].Bytes != 40 {
		t.Fatalf("order=%v want unix 2..4 ending bytes=40", got)
	}
}

func TestCollector_SeriesAfterRefresh(t *testing.T) {
	store := &fakeStore{
		stats: map[string]artifact.SizeStat{
			"oci": {Bytes: 100, Objects: 1},
		},
	}
	c := NewCollectorWithStore(store).(*collector)
	c.Refresh(context.Background())

	pts, err := c.Series(context.Background(), "oci")
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) < 1 || pts[len(pts)-1].Bytes != 100 {
		t.Fatalf("oci series=%v", pts)
	}
	total, err := c.Series(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(total) < 1 || total[len(total)-1].Bytes != 100 {
		t.Fatalf("total series=%v", total)
	}
}
