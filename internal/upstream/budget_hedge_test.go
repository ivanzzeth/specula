package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

func TestAttemptBudget_NonFinalAllowsSoftRetry(t *testing.T) {
	c := testClient(3)
	assert.Equal(t, 2, c.attemptBudget(nil, 1, Upstream{Name: "m"}, false))
	assert.Equal(t, 1, c.attemptBudget(&StatusError{StatusCode: 404}, 0, Upstream{Name: "m"}, false))
	assert.Equal(t, 1, c.attemptBudget(nil, 0, Upstream{Name: "hub", Official: true}, true))
	assert.Equal(t, 3, c.attemptBudget(nil, 0, Upstream{Name: "hub", Official: true}, false))
	assert.Equal(t, 1, testClient(1).attemptBudget(nil, 2, Upstream{Name: "m"}, false),
		"maxAttempts=1 must not invent a soft retry")
}

func TestFetch_NonFinal5xxGetsOneSoftRetryThenFailover(t *testing.T) {
	bad, badHits := countingServer(t, http.StatusInternalServerError, "")
	defer bad.Close()
	good := okServer(t, "ok", nil)
	defer good.Close()

	c := testClient(3)
	body, meta, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{
			{Name: "bad", BaseURL: bad.URL, Priority: 1},
			{Name: "good", BaseURL: good.URL, Priority: 2},
		})
	require.NoError(t, err)
	defer body.Close()
	assert.Equal(t, "good", meta.Upstream)
	assert.Equal(t, int64(2), badHits.Load(),
		"non-final 5xx should soft-retry once (budget=2) then fall through")
}

func TestFetch_NonFinalDialStillOneAttempt(t *testing.T) {
	good := okServer(t, "from-1ms", nil)
	defer good.Close()

	var deadHits atomic.Int32
	c := testClient(3)
	c.http = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Host, "dead-cdn") {
				deadHits.Add(1)
				return nil, context.DeadlineExceeded
			}
			return http.DefaultTransport.RoundTrip(req)
		}),
	}

	body, meta, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{
			{Name: "daocloud-r2", BaseURL: "http://dead-cdn.test", Priority: 1},
			{Name: "1ms", BaseURL: good.URL, Priority: 2},
		})
	require.NoError(t, err)
	defer body.Close()
	assert.Equal(t, "1ms", meta.Upstream)
	assert.Equal(t, int32(1), deadHits.Load(), "dial-class must fail-fast despite budget=2")
}

func TestFetch_OfficialLastHopCompressedAfterTransportFail(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer dead.Close()

	var hubHits atomic.Int64
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer hub.Close()

	c := testClient(3)
	_, _, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{
			{Name: "mirror", BaseURL: dead.URL, Priority: 1},
			{Name: "hub", BaseURL: hub.URL, Priority: 2, Official: true},
		})
	require.Error(t, err)
	assert.Equal(t, int64(1), hubHits.Load(),
		"Official last hop after prior transport fail must use budget=1")
}

func TestFetch_MutableHedgePrefersFasterNext(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, "slow")
	}))
	defer slow.Close()
	fast := okServer(t, "fast", nil)
	defer fast.Close()

	c := testClient(1)
	ref := artifact.ArtifactRef{Protocol: "oci", Name: "lib/x", Version: "latest", Mutable: true}
	start := time.Now()
	body, meta, err := c.Fetch(context.Background(), ref,
		[]Upstream{
			{Name: "slow", BaseURL: slow.URL, Priority: 1},
			{Name: "fast", BaseURL: fast.URL, Priority: 2},
		})
	require.NoError(t, err)
	defer body.Close()
	data, _ := io.ReadAll(body)
	assert.Equal(t, "fast", string(data))
	assert.Equal(t, "fast", meta.Upstream)
	assert.Less(t, time.Since(start), 350*time.Millisecond,
		"hedge should return the fast mirror without waiting for slow")
}

func TestFailoverReason(t *testing.T) {
	assert.Equal(t, "dial_timeout", failoverReason(context.DeadlineExceeded))
	assert.Equal(t, "http_429", failoverReason(asTransient(
		&plainErr{s: "upstream x: HTTP 429 (rate limited)"})))
	assert.Equal(t, "circuit_open", failoverReason(circuitbreaker.ErrOpen))
}

type plainErr struct{ s string }

func (e *plainErr) Error() string { return e.s }
