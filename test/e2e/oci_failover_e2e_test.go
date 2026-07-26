//go:build integration

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ocihandler "github.com/ivanzzeth/specula/internal/handler/oci"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// TestOCIFirstMirrorDownFallsBack: connection-refused on primary → origin serves.
func TestOCIFirstMirrorDownFallsBack(t *testing.T) {
	const (
		host     = "registry.k8s.io"
		repo     = "pause"
		tag      = "3.9"
		fullName = host + "/" + repo
	)
	img := newMinimalOCIImage(t, repo, tag, 16*1024)
	origin := startControllableOCIRegistry(t, img, blobOK)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	regs := k8sRemoteChain(t, host, []upstream.Upstream{
		{Name: "dead", BaseURL: deadURL(t)},
	}, origin.URL())
	regHost := newRemoteSpeculaServer(t, s, nil, regs)

	pulled := pullImage(t, regHost, fullName, tag)
	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, img.ManifestDigest, got.String())
}

// TestOCIFirstMirror5xxFallsBack: primary always 503 → secondary serves.
func TestOCIFirstMirror5xxFallsBack(t *testing.T) {
	const (
		host     = "registry.k8s.io"
		repo     = "pause"
		tag      = "3.9"
		fullName = host + "/" + repo
	)
	img := newMinimalOCIImage(t, repo, tag, 16*1024)
	origin := startControllableOCIRegistry(t, img, blobOK)
	bad, hits := countingStatusUpstream(t, http.StatusServiceUnavailable)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	regs := k8sRemoteChain(t, host, []upstream.Upstream{
		{Name: "flaky", BaseURL: bad.URL},
	}, origin.URL())
	regHost := newRemoteSpeculaServer(t, s, nil, regs)

	pulled := pullImage(t, regHost, fullName, tag)
	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, img.ManifestDigest, got.String())
	assert.GreaterOrEqual(t, hits.Load(), int64(1))
}

// TestOCIRateLimit429ThenSucceeds: primary returns 429 twice then 200 via
// failsafe Retry; pull must succeed without falling to a second mirror.
func TestOCIRateLimit429ThenSucceeds(t *testing.T) {
	const (
		repo = "rancher/mirrored-pause"
		tag  = "3.6"
	)
	img := newMinimalOCIImage(t, repo, tag, 8*1024)
	var hits atomic.Int64
	inner := startControllableOCIRegistry(t, img, blobOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		// Proxy to healthy registry after 429 budget.
		resp, err := http.DefaultClient.Get(inner.URL() + r.URL.Path)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	_, regHost := newSpeculaServer(t, s,
		ocihandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "rate-limited", BaseURL: srv.URL, Priority: 1},
		}),
		ocihandler.WithQuarantineDir(tmp),
	)

	pulled := pullImage(t, regHost, repo, tag)
	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, img.ManifestDigest, got.String())
	assert.GreaterOrEqual(t, hits.Load(), int64(3), "must retry through 429s")
}

// TestOCIMidStreamSameUpstreamResume: primary drops once then Range-resumes.
func TestOCIMidStreamSameUpstreamResume(t *testing.T) {
	const (
		host     = "registry.k8s.io"
		repo     = "pause"
		tag      = "3.9"
		fullName = host + "/" + repo
	)
	img := newMinimalOCIImage(t, repo, tag, 64*1024)
	origin := startControllableOCIRegistry(t, img, blobDropOnceThenOK)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	regs := k8sRemoteChain(t, host, nil, origin.URL())
	regHost := newRemoteSpeculaServer(t, s, nil, regs)

	pulled := pullImage(t, regHost, fullName, tag)
	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, img.ManifestDigest, got.String())
	_, err = pulled.ConfigFile()
	require.NoError(t, err, "config blob forces layer/blob path through resume")
	assert.GreaterOrEqual(t, origin.blobGets.Load(), int64(2),
		"mid-stream drop must Range-resume on the same upstream")
}

// TestOCIMidStreamCrossUpstreamFailover: primary dies mid-blob permanently;
// secondary has the blob — Specula must finish cold-fill without 502.
func TestOCIMidStreamCrossUpstreamFailover(t *testing.T) {
	requireCrossUpstreamMidStreamFailover(t)

	const (
		host     = "registry.k8s.io"
		repo     = "pause"
		tag      = "3.9"
		fullName = host + "/" + repo
	)
	img := newMinimalOCIImage(t, repo, tag, 64*1024)
	primary := startControllableOCIRegistry(t, img, blobDropOnceThenDead)
	secondary := startControllableOCIRegistry(t, img, blobOK)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	regs := k8sRemoteChain(t, host, []upstream.Upstream{
		{Name: "daocloud", BaseURL: primary.URL()},
	}, secondary.URL())
	regHost := newRemoteSpeculaServer(t, s, nil, regs)

	pulled := pullImage(t, regHost, fullName, tag)
	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, img.ManifestDigest, got.String(),
		"mid-stream primary death must fall through without cutting the pull")
	_, err = pulled.ConfigFile()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, secondary.blobGets.Load(), int64(1))
}

// TestOCICircuitBreakerSkipsOpenUpstream: after enough transient failures the
// primary is open; subsequent pulls must skip it and hit origin.
func TestOCICircuitBreakerSkipsOpenUpstream(t *testing.T) {
	const (
		host     = "registry.k8s.io"
		repo     = "coredns/coredns"
		tag      = "v1.11.1"
		fullName = host + "/" + repo
	)
	img := newMinimalOCIImage(t, repo, tag, 8*1024)
	origin := startControllableOCIRegistry(t, img, blobOK)
	bad, badHits := countingStatusUpstream(t, http.StatusBadGateway)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	// Shared client so the CB state persists across pulls.
	cl := upstream.NewClient()
	regs := ocihandler.RemoteRegistryMap{
		host: {
			{Name: "remote:" + host + ":bad", BaseURL: bad.URL, Priority: 1},
			{Name: "remote:" + host, BaseURL: origin.URL(), Priority: 2, Official: true},
		},
	}
	_, regHost := newSpeculaServer(t, s,
		ocihandler.WithUpstream(cl, []upstream.Upstream{
			{Name: "hub-unused", BaseURL: "http://127.0.0.1:1", Priority: 99},
		}),
		ocihandler.WithRemoteRegistries(regs),
		ocihandler.WithQuarantineDir(tmp),
	)

	// Trip the breaker: several cold misses against the bad primary.
	for i := 0; i < 6; i++ {
		resp, err := http.Get(fmt.Sprintf("http://%s/v2/%s/manifests/missing-%d", regHost, fullName, i))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	hitsAfterTrip := badHits.Load()

	pulled := pullImage(t, regHost, fullName, tag)
	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, img.ManifestDigest, got.String())

	// While open, primary should not keep receiving every probe.
	assert.LessOrEqual(t, badHits.Load()-hitsAfterTrip, int64(2),
		"open circuit should largely skip the failing mirror")
}

// TestOCIIdleBodyTimeoutResumes: mid-stream connection drop then Range resume.
func TestOCIIdleBodyTimeoutResumes(t *testing.T) {
	const (
		repo = "library/idle"
		tag  = "1"
	)
	img := newMinimalOCIImage(t, repo, tag, 32*1024)
	origin := startControllableOCIRegistry(t, img, blobDropOnceThenOK)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	_, regHost := newSpeculaServer(t, s,
		ocihandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			origin.asUpstream("idle", 1),
		}),
		ocihandler.WithQuarantineDir(tmp),
	)

	pulled := pullImage(t, regHost, repo, tag)
	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, img.ManifestDigest, got.String())
	_, err = pulled.ConfigFile()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, origin.blobGets.Load(), int64(2))
}
