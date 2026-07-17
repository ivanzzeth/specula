package gomod

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── recordingUpstream — ground truth for "did we really contact upstream" ────
//
// This is deliberately NOT one of Specula's own counters. ARCHITECTURE §7's
// stampede claim is about REAL upstream round trips, and every hit/miss/latency
// counter in the tree is incremented by the very code whose behaviour it
// describes: 10 uncollapsed fetches produce a perfectly self-consistent
// latency_count of 10, so a counter-reading test agrees with the bug. Only an
// independent recorder on the wire can tell 1 from N.
//
// gate is closed until the test releases it, so every concurrent request is
// guaranteed to be in flight simultaneously. Without it a fast serial fetch
// could complete before the next request starts and the collapse would look
// like it worked when nothing overlapped.
type recordingUpstream struct {
	srv *httptest.Server

	hits atomic.Int64 // real requests that reached the wire, per path

	mu       sync.Mutex
	perPath  map[string]int
	gate     chan struct{}
	failWith int // when >0, respond with this status instead of the body
}

func newRecordingUpstream(t *testing.T, module string, bodies map[string][]byte) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{
		perPath: make(map[string]int),
		gate:    make(chan struct{}),
	}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Record BEFORE gating: a request that arrived is a request that
		// arrived, whether or not we ever answer it.
		u.hits.Add(1)
		u.mu.Lock()
		u.perPath[r.URL.Path]++
		fail := u.failWith
		u.mu.Unlock()

		// Hold the response until the test opens the gate, guaranteeing overlap.
		<-u.gate

		if fail > 0 {
			http.Error(w, "upstream sad", fail)
			return
		}

		const atV = "/@v/"
		p := r.URL.Path
		idx := lastIndex(p, atV)
		if idx < 0 {
			http.NotFound(w, r)
			return
		}
		file := p[idx+len(atV):]
		data, ok := bodies[file]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentTypeForFile(file))
		_, _ = w.Write(data)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func lastIndex(s, sub string) int {
	for i := len(s) - len(sub); i >= 0; i-- {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func (u *recordingUpstream) open()        { close(u.gate) }
func (u *recordingUpstream) count() int64 { return u.hits.Load() }
func (u *recordingUpstream) countFor(p string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.perPath[p]
}

// TestGomodStampede_ConcurrentColdFetches_CollapseToOneUpstreamRequest is the
// load-bearing RED for ARCHITECTURE §7.
//
// N concurrent requests for ONE cold artifact must produce exactly ONE real
// upstream fetch. The arbiter is recordingUpstream — independent of every
// counter Specula keeps about itself.
//
// PRD §G5 (CN-first) is why this matters in cash terms: an uncollapsed round
// trip is a slow, expensive one (27 kB/s was measured on a real aliyun link),
// so a popular package going cold while N CI jobs hit at once hammers a slow
// upstream N times.
func TestGomodStampede_ConcurrentColdFetches_CollapseToOneUpstreamRequest(t *testing.T) {
	const (
		module      = "github.com/foo/bar"
		file        = "v1.0.0.mod"
		concurrency = 10
	)
	body := []byte("module github.com/foo/bar\n")
	up := newRecordingUpstream(t, module, map[string][]byte{file: body})

	cm := newGomodTestCache()
	h := newHandlerWithUpstream(cm, up.srv.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	url := fmt.Sprintf("%s/%s/@v/%s", srv.URL, module, file)

	// Fire N concurrent requests for the same cold artifact.
	var wg sync.WaitGroup
	codes := make([]int, concurrency)
	bodies := make([][]byte, concurrency)
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := http.Get(url)
			if err != nil {
				codes[i] = -1
				return
			}
			defer resp.Body.Close()
			codes[i] = resp.StatusCode
			bodies[i], _ = io.ReadAll(resp.Body)
		}(i)
	}
	close(start)

	// Give every request time to reach the upstream (or be collapsed behind the
	// leader), then release the gate. If single-flight works, only ONE request
	// is parked at the gate; the other nine never touch the wire at all.
	time.Sleep(200 * time.Millisecond)
	up.open()
	wg.Wait()

	// Every caller must still get correct bytes — collapsing must not cost
	// anyone their response.
	for i := 0; i < concurrency; i++ {
		assert.Equal(t, http.StatusOK, codes[i], "caller %d must get its response", i)
		assert.Equal(t, body, bodies[i], "caller %d must get the real bytes", i)
	}

	// THE CLAIM. Counted by the wire, not by us.
	assert.Equal(t, int64(1), up.count(),
		"ARCHITECTURE §7: %d concurrent cold requests for ONE artifact must collapse "+
			"to exactly ONE upstream fetch; the coalescer is keyed by digest and only "+
			"used in cache.Store, so the expensive CN round trip is never collapsed",
		concurrency)
}

// failingStampede drives n concurrent requests at an upstream that fails every
// attempt, and returns how many attempts actually reached the wire.
func failingStampede(t *testing.T, n int) int64 {
	t.Helper()
	const (
		module = "github.com/foo/bar"
		file   = "v1.0.0.mod"
	)
	up := newRecordingUpstream(t, module, nil)
	up.mu.Lock()
	up.failWith = http.StatusInternalServerError
	up.mu.Unlock()

	cm := newGomodTestCache()
	h := newHandlerWithUpstream(cm, up.srv.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	url := fmt.Sprintf("%s/%s/@v/%s", srv.URL, module, file)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.Get(url)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	close(start)
	// Let all n requests arrive and collapse behind one leader before the
	// upstream is allowed to answer (and thus to fail).
	time.Sleep(200 * time.Millisecond)
	up.open()
	wg.Wait()
	return up.count()
}

// TestGomodStampede_LeaderFailure_DoesNotStampede pins the failure semantics —
// the decision that actually matters in this fix.
//
// When the leader's fetch FAILS, followers must NOT each re-fetch. That would
// re-create the stampede under exactly the upstream-trouble conditions where it
// hurts most: the upstream is already failing and N callers pile on to fail
// again. Followers share the leader's error and then degrade independently via
// serve-stale (§3), which costs zero further upstream contact.
//
// The bar is CALIBRATED, not hard-coded: it is whatever a single lone request
// costs. The upstream client makes maxAttempts (3) attempts with fallback inside
// one leader call, and that retry budget is the client's business — this test
// asserts only that N concurrent callers cost the same as ONE, whatever one
// costs. Hard-coding 3 here would couple the stampede claim to an unrelated
// retry setting and would start lying the day someone tuned it.
func TestGomodStampede_LeaderFailure_DoesNotStampede(t *testing.T) {
	const concurrency = 10

	baseline := failingStampede(t, 1)
	require.Greater(t, baseline, int64(0),
		"sanity: a single failing request must reach the wire at least once")

	concurrent := failingStampede(t, concurrency)

	require.Equal(t, baseline, concurrent,
		"a FAILING leader must still collapse: %d concurrent callers must cost the "+
			"same %d upstream attempt(s) as one lone caller, not %d× that. The leader's "+
			"single call already performed the configured retry/fallback internally, so "+
			"followers piling on adds load without adding any chance of success",
		concurrency, baseline, concurrency)
}
