package upstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// crossUpstreamMidStreamFailoverReady gates the mid-stream *cross-upstream*
// contract tests. Today resume.go pins the Fetch body to the first upstream
// that returned headers; a permanent crash of that upstream fails the whole
// Fetch even when a later mirror is healthy.
//
// Flip to true in the implementation session after resumingReader falls through
// the remaining chain (Range from offset when possible) without closing the
// caller's ReadCloser mid-stream.
const crossUpstreamMidStreamFailoverReady = true

func requireCrossUpstreamMidStreamFailover(t *testing.T) {
	t.Helper()
	if !crossUpstreamMidStreamFailoverReady {
		t.Skip("TODO(mid-stream cross-upstream failover): resume.go pins to the first " +
			"upstream; enable after fallthrough to later mirrors without resetting the Fetch body")
	}
}

// flakyThenDeadUpstream delivers a prefix of full on the first GET, then drops
// the connection. Every subsequent request (including Range resumes) returns
// 503 — the upstream is "crashed" for good.
func flakyThenDeadUpstream(t *testing.T, full string, prefixLen int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var gets atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := gets.Add(1)
		if n == 1 && r.Header.Get("Range") == "" {
			writePrefixThenDrop(t, w, full, prefixLen)
			return
		}
		// Subsequent GETs (Range resume or fallthrough) — permanently dead.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "upstream crashed")
	}))
	t.Cleanup(srv.Close)
	return srv, &gets
}

// healthyBlobUpstream serves full for any GET; honours Range with 206.
func healthyBlobUpstream(t *testing.T, full string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var gets atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)))
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, full)
			return
		}
		var start int
		_, err := fmt.Sscanf(rng, "bytes=%d-", &start)
		require.NoError(t, err)
		require.GreaterOrEqual(t, start, 0)
		require.LessOrEqual(t, start, len(full))
		rest := full[start:]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(full)-1, len(full)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(rest)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, rest)
	}))
	t.Cleanup(srv.Close)
	return srv, &gets
}

// TestFetch_MidStreamDeathFallsThroughToNextUpstream is the load-bearing
// contract for CN / multi-mirror OCI layer pulls:
//
//	mirror-a delivers a prefix then dies permanently;
//	mirror-b (or origin) has the full blob and honours Range;
//	a single Fetch body must assemble the full object — the caller must NOT
//	see a hard error that would turn into OCI 502 / ImagePullBackOff.
func TestFetch_MidStreamDeathFallsThroughToNextUpstream(t *testing.T) {
	requireCrossUpstreamMidStreamFailover(t)

	const full = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789k8s-pause-layer!!"
	prefixLen := 16

	dead, deadGets := flakyThenDeadUpstream(t, full, prefixLen)
	good, goodGets := healthyBlobUpstream(t, full)

	c := resumeTestClient()
	c.idleBodyTimeout = 80 * time.Millisecond

	body, meta, err := c.Fetch(context.Background(), ociBlobRef("pause", "sha256:deadbeef"),
		[]Upstream{
			{Name: "k8s-daocloud", BaseURL: dead.URL, Priority: 1},
			{Name: "k8s-1ms", BaseURL: good.URL, Priority: 2},
		})
	require.NoError(t, err, "Fetch headers must succeed from the first upstream")
	defer body.Close()

	got, err := io.ReadAll(body)
	require.NoError(t, err,
		"mid-stream crash of the pinned upstream must fall through to the next "+
			"mirror without failing the Fetch body (caller connection stays open)")
	assert.Equal(t, full, string(got))
	assert.GreaterOrEqual(t, deadGets.Load(), int64(1))
	assert.GreaterOrEqual(t, goodGets.Load(), int64(1),
		"healthy secondary must be contacted after primary mid-stream death")
	// meta.Upstream may still report the first that answered headers; content wins.
	assert.NotEmpty(t, meta.Upstream)
}

// TestFetch_MidStreamDeathFallsThroughWithRangeOnNext asserts the secondary
// receives a Range request from the already-delivered offset (no re-download
// of the prefix when the secondary supports Range).
func TestFetch_MidStreamDeathFallsThroughWithRangeOnNext(t *testing.T) {
	requireCrossUpstreamMidStreamFailover(t)

	const full = "0123456789abcdefghijklmnopqrstuvwxyz"
	prefixLen := 10
	var secondaryRange atomic.Bool

	dead, _ := flakyThenDeadUpstream(t, full, prefixLen)

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng != "" {
			secondaryRange.Store(true)
			require.Equal(t, fmt.Sprintf("bytes=%d-", prefixLen), rng)
			rest := full[prefixLen:]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", prefixLen, len(full)-1, len(full)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, rest)
			return
		}
		// Full GET should not be needed if Range fallthrough works.
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, full)
	}))
	t.Cleanup(good.Close)

	c := resumeTestClient()
	body, _, err := c.Fetch(context.Background(), ociBlobRef("metrics-server/metrics-server", "sha256:abcd"),
		[]Upstream{
			{Name: "primary", BaseURL: dead.URL, Priority: 1},
			{Name: "secondary", BaseURL: good.URL, Priority: 2},
		})
	require.NoError(t, err)
	defer body.Close()

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, full, string(got))
	assert.True(t, secondaryRange.Load(),
		"secondary must see Range from the already-delivered offset")
}

// TestFetch_MidStreamDeathNextIgnoresRangeRestarts: secondary answers 200 full
// body (ignored Range) → ErrRestartFromBeginning once, then full content.
func TestFetch_MidStreamDeathNextIgnoresRangeRestarts(t *testing.T) {
	requireCrossUpstreamMidStreamFailover(t)

	const full = "FULL-BODY-NO-RANGE-ON-SECONDARY-OK!!"
	prefixLen := 8

	dead, _ := flakyThenDeadUpstream(t, full, prefixLen)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always ignore Range — classic broken mirror.
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, full)
	}))
	t.Cleanup(good.Close)

	c := resumeTestClient()
	body, _, err := c.Fetch(context.Background(), ociBlobRef("rancher/mirrored-pause", "sha256:ef"),
		[]Upstream{
			{Name: "dead", BaseURL: dead.URL, Priority: 1},
			{Name: "no-range", BaseURL: good.URL, Priority: 2},
		})
	require.NoError(t, err)
	defer body.Close()

	// Quarantine handles ErrRestartFromBeginning; here we assemble manually.
	var out []byte
	buf := make([]byte, 32)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if rerr == nil {
			continue
		}
		if rerr == ErrRestartFromBeginning {
			out = out[:0]
			continue
		}
		if rerr == io.EOF {
			break
		}
		require.NoError(t, rerr)
	}
	assert.Equal(t, full, string(out))
}

// TestFetch_MidStreamDeathAllUpstreamsDead still fails hard when every
// upstream in the chain is gone — fallthrough must not invent success.
func TestFetch_MidStreamDeathAllUpstreamsDead(t *testing.T) {
	requireCrossUpstreamMidStreamFailover(t)

	const full = "unreachable-payload-bytes"
	prefixLen := 5

	a, _ := flakyThenDeadUpstream(t, full, prefixLen)
	b, _ := flakyThenDeadUpstream(t, full, prefixLen)

	c := resumeTestClient()
	body, _, err := c.Fetch(context.Background(), ociBlobRef("pause", "sha256:zz"),
		[]Upstream{
			{Name: "a", BaseURL: a.URL, Priority: 1},
			{Name: "b", BaseURL: b.URL, Priority: 2},
		})
	require.NoError(t, err, "headers succeed from first upstream")
	defer body.Close()

	_, err = io.ReadAll(body)
	require.Error(t, err, "all upstreams dead mid-stream must surface an error")
}

// TestFetch_RequestLevelFallbackStillWorks documents the already-implemented
// path: primary never answers headers → secondary serves. This must keep
// passing regardless of mid-stream work.
func TestFetch_RequestLevelFallbackStillWorks(t *testing.T) {
	const full = "request-level-fallback-ok"

	deadURL := deadUpstream(t)
	good, _ := healthyBlobUpstream(t, full)

	c := resumeTestClient()
	body, meta, err := c.Fetch(context.Background(), ociBlobRef("pause", "sha256:aa"),
		[]Upstream{
			{Name: "dead", BaseURL: deadURL, Priority: 1},
			{Name: "good", BaseURL: good.URL, Priority: 2},
		})
	require.NoError(t, err)
	defer body.Close()
	got, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, full, string(got))
	assert.Equal(t, "good", meta.Upstream)
}
