package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deadUpstream starts an httptest.Server and immediately closes it, returning
// its (now dead) URL. A fetch against it produces a genuine TRANSPORT failure
// (connection refused) — the fast, deterministic analogue of the CN condition
// where proxy.golang.org times out. Crucially it carries NO *StatusError.
func deadUpstream(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := srv.URL
	srv.Close()
	return url
}

// TestFetch_DefinitiveNotFound_BeatsLaterTransportFailure is the load-bearing
// regression for the multi-upstream error-precedence bug.
//
// Chain: upstream A returns a clean 404 (definitive: "does not exist"); a LATER
// upstream B fails on transport (dead port, the CN-blocked proxy.golang.org
// analogue). The caller MUST see the definitive 404 (a *StatusError), NOT the
// transport failure — otherwise the gomod handler flattens it to 502 and the go
// client's module-path-boundary walk breaks on the shipped default CN chain.
//
// A single-upstream test cannot reproduce this (that is exactly why 4c330c9's
// tests missed it): the defect only appears when a definitive answer is
// FOLLOWED by a transport failure that overwrites lastErr.
func TestFetch_DefinitiveNotFound_BeatsLaterTransportFailure(t *testing.T) {
	notFound := statusServer(t, http.StatusNotFound)
	defer notFound.Close()
	deadURL := deadUpstream(t)

	c := testClient(1)
	_, _, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{
			{Name: "mirror-a", BaseURL: notFound.URL, Priority: 1}, // clean 404
			{Name: "dead-origin", BaseURL: deadURL, Priority: 2},   // transport failure
		})
	require.Error(t, err)

	var se *StatusError
	require.Truef(t, errors.As(err, &se),
		"a definitive 404 from an earlier upstream must survive a later upstream's "+
			"transport failure as a typed *StatusError (got %v)", err)
	assert.Equal(t, http.StatusNotFound, se.StatusCode)
}

// TestFetch_TransportThenDefinitiveNotFound_Is404 proves order does not matter:
// a transport failure FIRST must not suppress a later upstream's definitive 404.
func TestFetch_TransportThenDefinitiveNotFound_Is404(t *testing.T) {
	deadURL := deadUpstream(t)
	notFound := statusServer(t, http.StatusGone) // 410 is also definitive
	defer notFound.Close()

	c := testClient(1)
	_, _, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{
			{Name: "dead-a", BaseURL: deadURL, Priority: 1},        // transport failure
			{Name: "mirror-b", BaseURL: notFound.URL, Priority: 2}, // clean 410
		})
	require.Error(t, err)

	var se *StatusError
	require.Truef(t, errors.As(err, &se),
		"a definitive 410 must surface even when an earlier upstream failed on transport (got %v)", err)
	assert.Equal(t, http.StatusGone, se.StatusCode)
}

// TestFetch_DefinitiveNotFound_ThenServed_Serves proves the precedence ceiling:
// a 200 from any upstream wins outright over an earlier definitive 404. A 404
// from one mirror must NEVER short-circuit a later mirror that actually has the
// artifact.
func TestFetch_DefinitiveNotFound_ThenServed_Serves(t *testing.T) {
	notFound := statusServer(t, http.StatusNotFound)
	defer notFound.Close()
	good := okServer(t, "found", nil)
	defer good.Close()

	c := testClient(1)
	body, meta, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{
			{Name: "mirror-a", BaseURL: notFound.URL, Priority: 1}, // 404
			{Name: "mirror-b", BaseURL: good.URL, Priority: 2},     // 200 wins
		})
	require.NoError(t, err)
	defer body.Close()

	data, _ := io.ReadAll(body)
	assert.Equal(t, "found", string(data))
	assert.Equal(t, "mirror-b", meta.Upstream)
}

// TestFetch_AllTransport_NoTypedStatus proves the airtight distinction: when
// EVERY upstream fails on transport (a real outage), the error must NOT carry a
// StatusError, so the handler keeps mapping it to 502 rather than fabricating a
// "does not exist" the go client would cache.
func TestFetch_AllTransport_NoTypedStatus(t *testing.T) {
	deadA := deadUpstream(t)
	deadB := deadUpstream(t)

	c := testClient(1)
	_, _, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{
			{Name: "dead-a", BaseURL: deadA, Priority: 1},
			{Name: "dead-b", BaseURL: deadB, Priority: 2},
		})
	require.Error(t, err)

	var se *StatusError
	assert.Falsef(t, errors.As(err, &se),
		"an all-transport outage must NOT carry a StatusError (got %v)", err)
}

// TestFetch_DefinitiveNotCountedTowardBlock_TransportIs confirms §7.4 is
// preserved through the precedence change: the definitive 404 must not tick the
// auto-block streak, while the transport failure must. (The block gauge decides
// on the transient flag, which is per-upstream and untouched by the fix.)
func TestFetch_DefinitiveNotCountedTowardBlock_TransportIs(t *testing.T) {
	notFound := statusServer(t, http.StatusNotFound)
	defer notFound.Close()
	deadURL := deadUpstream(t)

	c := testClient(1)
	_, _, _ = c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{
			{Name: "mirror-a", BaseURL: notFound.URL, Priority: 1},
			{Name: "dead-origin", BaseURL: deadURL, Priority: 2},
		})

	assert.Equal(t, 0, c.blocker.failureCount("mirror-a"),
		"a definitive 404 must not count toward auto-block (§7.4)")
	assert.Greater(t, c.blocker.failureCount("dead-origin"), 0,
		"a transport failure must count toward auto-block")
}

// TestFetch_DefinitiveHeld_LaterUpstreamNotRetried is the promptness guarantee:
// once a definitive 404 is held from an earlier upstream, a LATER upstream that
// keeps failing transiently must be probed exactly ONCE, not retried maxAttempts
// times. This is what stops the CN default chain from hanging ~30s (3×~10s GFW
// resets on the unreachable proxy.golang.org) on every not-found probe before
// auto-block kicks in — the retries only multiply a dead origin's latency once we
// already hold an authoritative answer.
func TestFetch_DefinitiveHeld_LaterUpstreamNotRetried(t *testing.T) {
	notFound := statusServer(t, http.StatusNotFound)
	defer notFound.Close()
	// A 500 server is a TRANSIENT failure that would normally be retried
	// maxAttempts times; count the hits to prove retries were suppressed.
	flaky, hits := countingServer(t, http.StatusInternalServerError, "")
	defer flaky.Close()

	c := testClient(3) // full budget = 3 attempts
	_, _, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{
			{Name: "mirror-a", BaseURL: notFound.URL, Priority: 1}, // definitive 404
			{Name: "flaky-origin", BaseURL: flaky.URL, Priority: 2}, // 500, transient
		})
	require.Error(t, err)

	var se *StatusError
	require.Truef(t, errors.As(err, &se),
		"the held definitive 404 must be returned (got %v)", err)
	assert.Equal(t, http.StatusNotFound, se.StatusCode)
	assert.Equalf(t, int64(1), hits.Load(),
		"a later upstream must be probed ONCE (not retried) when a definitive answer is already held; got %d hits", hits.Load())
}

// TestFetch_NoDefinitiveHeld_LaterUpstreamStillRetried is the other side: with
// NO definitive answer in hand, a transient failure on the only remaining path
// must still be retried the full budget — the retry-suppression must be scoped
// strictly to the "we already hold an authoritative answer" case.
func TestFetch_NoDefinitiveHeld_LaterUpstreamStillRetried(t *testing.T) {
	flaky, hits := countingServer(t, http.StatusInternalServerError, "")
	defer flaky.Close()

	c := testClient(3) // full budget = 3 attempts
	_, _, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{{Name: "only", BaseURL: flaky.URL, Priority: 1}})
	require.Error(t, err)
	assert.Equalf(t, int64(3), hits.Load(),
		"with no definitive answer held, a transient 5xx must be retried the full budget; got %d hits", hits.Load())
}

// TestRevalidate_DefinitiveNotFound_BeatsLaterTransportFailure applies the same
// precedence guarantee to the conditional-GET path, which shares the loop.
func TestRevalidate_DefinitiveNotFound_BeatsLaterTransportFailure(t *testing.T) {
	notFound := statusServer(t, http.StatusNotFound)
	defer notFound.Close()
	deadURL := deadUpstream(t)

	c := testClient(1)
	_, _, _, err := c.Revalidate(context.Background(), tarballRef("pkg", "v1.0.0"),
		artifact.UpstreamMeta{},
		[]Upstream{
			{Name: "mirror-a", BaseURL: notFound.URL, Priority: 1},
			{Name: "dead-origin", BaseURL: deadURL, Priority: 2},
		})
	require.Error(t, err)

	var se *StatusError
	require.Truef(t, errors.As(err, &se),
		"Revalidate must preserve a definitive 404 over a later transport failure (got %v)", err)
	assert.Equal(t, http.StatusNotFound, se.StatusCode)
}
