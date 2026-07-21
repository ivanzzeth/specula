package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMiddlewareRecordsBytesAndDuration(t *testing.T) {
	body := strings.Repeat("z", 50*1024)
	bytesBefore := testutil.ToFloat64(ResponseBytesTotal.WithLabelValues("npm"))

	h := Middleware("npm", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/x")
	require.NoError(t, err)
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	require.Equal(t, len(body), len(got))

	assert.InDelta(t, bytesBefore+float64(len(body)),
		testutil.ToFloat64(ResponseBytesTotal.WithLabelValues("npm")), 0.1)

	snap := SnapshotTraffic()
	var found bool
	for _, p := range snap.Protocols {
		if p.Protocol == "npm" && p.BytesTotal >= uint64(len(body)) {
			found = true
			assert.Greater(t, p.RequestsTotal, uint64(0))
			assert.Greater(t, p.WindowBytes, uint64(0))
		}
	}
	assert.True(t, found, "npm traffic missing from snapshot: %+v", snap)
	table := FormatTrafficTable(snap)
	assert.Contains(t, table, "PROTO")
	assert.Contains(t, table, "npm")
}

func TestSnapshotTrafficWindowPrune(t *testing.T) {
	// Directly inject an old event and ensure prune drops it.
	trafficMu.Lock()
	trafficEvents = append(trafficEvents, trafficEvent{
		at: time.Now().Add(-2 * trafficWindow), protocol: "apt", bytes: 999, dur: time.Second,
	})
	pruneTrafficLocked(time.Now())
	for _, e := range trafficEvents {
		if e.protocol == "apt" && e.bytes == 999 {
			trafficMu.Unlock()
			t.Fatal("stale event not pruned")
		}
	}
	trafficMu.Unlock()
}
