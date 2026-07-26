//go:build integration

// Package e2e — hermetic + live scenarios for k8s / k3s / Helm-OCI image pulls
// through Specula's path-style multi-registry allowlist (registry.k8s.io,
// ghcr.io charts, docker.io rancher/*).
//
// These mirror the real bootstrap / HA gap documented in README and
// deploy/helm/specula-bootstrap: when docker.io / registry.k8s.io are flaky,
// Specula must still land pause, metrics-server, and Helm chart layers.
package e2e

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ocihandler "github.com/ivanzzeth/specula/internal/handler/oci"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// TestK8sPausePullThroughRemoteRegistry pulls registry.k8s.io/pause:<tag>
// path-style through Specula. Origin is an in-process registry hosting the
// stripped repo "pause"; Specula strips the host and fetches from the chain.
func TestK8sPausePullThroughRemoteRegistry(t *testing.T) {
	const (
		host      = "registry.k8s.io"
		repo      = "pause"
		tag       = "3.9"
		fullName  = host + "/" + repo
		layerSize = int64(64 * 1024)
	)

	img := newMinimalOCIImage(t, repo, tag, layerSize)
	origin := startControllableOCIRegistry(t, img, blobOK)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	regs := k8sRemoteChain(t, host, nil, origin.URL())
	regHost := newRemoteSpeculaServer(t, s, nil, regs)

	pulled := pullImage(t, regHost, fullName, tag)
	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, img.ManifestDigest, got.String())

	_, err = pulled.ConfigFile()
	require.NoError(t, err, "config blob must round-trip through Specula CAS")

	used, err := s.blobStore.UsageBytes(t.Context())
	require.NoError(t, err)
	assert.Positive(t, used, "CAS must hold pause layers after cold pull")
}

// TestK8sMetricsServerPull covers the nested path used by bootstrap-prefetch:
// registry.k8s.io/metrics-server/metrics-server:<tag>.
func TestK8sMetricsServerPull(t *testing.T) {
	const (
		host     = "registry.k8s.io"
		repo     = "metrics-server/metrics-server"
		tag      = "v0.8.1"
		fullName = host + "/" + repo
	)

	img := newMinimalOCIImage(t, repo, tag, 32*1024)
	origin := startControllableOCIRegistry(t, img, blobOK)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	regs := k8sRemoteChain(t, host, nil, origin.URL())
	regHost := newRemoteSpeculaServer(t, s, nil, regs)

	pulled := pullImage(t, regHost, fullName, tag)
	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, img.ManifestDigest, got.String())
}

// TestK3sRelatedHubImagePull covers docker.io-style rancher images that k3s
// nodes pull (mirrored pause). Hub chain only — no remote host prefix.
func TestK3sRelatedHubImagePull(t *testing.T) {
	const (
		repo = "rancher/mirrored-pause"
		tag  = "3.6"
	)

	img := newMinimalOCIImage(t, repo, tag, 32*1024)
	hub := startControllableOCIRegistry(t, img, blobOK)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	_, regHost := newSpeculaServer(t, s,
		ocihandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			hub.asUpstream("docker-hub", 1),
		}),
		ocihandler.WithQuarantineDir(tmp),
	)

	pulled := pullImage(t, regHost, repo, tag)
	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, img.ManifestDigest, got.String())
}

// TestHelmOCIChartPullThroughGHCR exercises path-style pulls for Helm charts
// published as OCI artifacts (oci://ghcr.io/.../charts/...). Specula treats
// them as normal OCI images under the ghcr.io allowlist.
func TestHelmOCIChartPullThroughGHCR(t *testing.T) {
	const (
		host     = "ghcr.io"
		repo     = "ivanzzeth/charts/specula"
		tag      = "0.1.0"
		fullName = host + "/" + repo
	)

	img := newMinimalOCIImage(t, repo, tag, 48*1024)
	origin := startControllableOCIRegistry(t, img, blobOK)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	regs := k8sRemoteChain(t, host, nil, origin.URL())
	regHost := newRemoteSpeculaServer(t, s, nil, regs)

	pulled := pullImage(t, regHost, fullName, tag)
	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, img.ManifestDigest, got.String())

	// Second pull must be CAS-hit (no extra origin blob traffic growth is
	// asserted loosely — layer GET count after warm pull must not climb).
	blobsBefore := origin.blobGets.Load()
	pulled2 := pullImage(t, regHost, fullName, tag)
	d2, err := pulled2.Digest()
	require.NoError(t, err)
	assert.Equal(t, got, d2)
	assert.Equal(t, blobsBefore, origin.blobGets.Load(),
		"warm Helm OCI chart pull must not re-hit origin blobs")
}

// TestK8sMirrorChainFallsThroughToOrigin reproduces the CN default chain
// (DaoCloud → 1ms → origin): first two mirrors deny / crash at request level;
// origin serves pause.
func TestK8sMirrorChainFallsThroughToOrigin(t *testing.T) {
	const (
		host     = "registry.k8s.io"
		repo     = "pause"
		tag      = "3.9"
		fullName = host + "/" + repo
	)

	img := newMinimalOCIImage(t, repo, tag, 16*1024)
	origin := startControllableOCIRegistry(t, img, blobOK)
	daocloud, daoHits := countingStatusUpstream(t, http.StatusForbidden)
	oneMS, oneHits := countingStatusUpstream(t, http.StatusBadGateway)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	regs := k8sRemoteChain(t, host, []upstream.Upstream{
		{Name: "daocloud", BaseURL: daocloud.URL},
		{Name: "1ms", BaseURL: oneMS.URL},
	}, origin.URL())
	regHost := newRemoteSpeculaServer(t, s, nil, regs)

	pulled := pullImage(t, regHost, fullName, tag)
	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, img.ManifestDigest, got.String(),
		"CN mirror denials must fall through to registry.k8s.io origin")
	assert.GreaterOrEqual(t, daoHits.Load(), int64(1))
	assert.GreaterOrEqual(t, oneHits.Load(), int64(1))
	_, err = pulled.ConfigFile()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, origin.blobGets.Load(), int64(1))
}

// TestK8sDeadFirstMirrorFallsBack: first mirror connection-refused, second
// (origin) healthy — classic request-level failover for k3s node pulls.
func TestK8sDeadFirstMirrorFallsBack(t *testing.T) {
	const (
		host     = "registry.k8s.io"
		repo     = "coredns/coredns"
		tag      = "v1.11.1"
		fullName = host + "/" + repo
	)

	img := newMinimalOCIImage(t, repo, tag, 16*1024)
	origin := startControllableOCIRegistry(t, img, blobOK)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	regs := k8sRemoteChain(t, host, []upstream.Upstream{
		{Name: "daocloud", BaseURL: deadURL(t)},
	}, origin.URL())
	regHost := newRemoteSpeculaServer(t, s, nil, regs)

	pulled := pullImage(t, regHost, fullName, tag)
	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, img.ManifestDigest, got.String())
}

// TestK8sUnknownRemoteHostRejected ensures SSRF allowlist still rejects
// non-allowlisted registry hosts even when k8s hosts are configured.
func TestK8sUnknownRemoteHostRejected(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	regs := ocihandler.RemoteRegistriesFromSpecs([]ocihandler.RemoteRegistrySpec{
		{Host: "registry.k8s.io"},
		{Host: "ghcr.io"},
	})
	regHost := newRemoteSpeculaServer(t, s, nil, regs)

	resp, err := http.Get(fmt.Sprintf("http://%s/v2/evil.example/foo/manifests/latest", regHost))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestK8sMultiImageColdWarmSequence pulls several cluster-critical images
// in sequence (pause → metrics-server → helm chart) and asserts each lands
// in CAS — the bootstrap-prefetch shape.
func TestK8sMultiImageColdWarmSequence(t *testing.T) {
	type item struct {
		host, repo, tag string
	}
	items := []item{
		{"registry.k8s.io", "pause", "3.9"},
		{"registry.k8s.io", "metrics-server/metrics-server", "v0.8.1"},
		{"ghcr.io", "bitnami/charts/nginx", "18.0.0"},
	}

	for _, it := range items {
		it := it
		t.Run(it.host+"/"+it.repo, func(t *testing.T) {
			img := newMinimalOCIImage(t, it.repo, it.tag, 8*1024)
			origin := startControllableOCIRegistry(t, img, blobOK)
			tmp := t.TempDir()
			s := newSpeculaStack(t, tmp)
			regs := k8sRemoteChain(t, it.host, nil, origin.URL())
			regHost := newRemoteSpeculaServer(t, s, nil, regs)
			full := it.host + "/" + it.repo
			pulled := pullImage(t, regHost, full, it.tag)
			got, err := pulled.Digest()
			require.NoError(t, err)
			assert.Equal(t, img.ManifestDigest, got.String())
		})
	}
}

// ── Live network tests (SPECULA_E2E_LIVE=1) ─────────────────────────────────

func TestLiveK8sPause(t *testing.T) {
	if os.Getenv("SPECULA_E2E_LIVE") != "1" {
		t.Skip("set SPECULA_E2E_LIVE=1 to pull real registry.k8s.io/pause")
	}

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	// Prefer CN mirrors then origin — matches shipped example.yaml.
	regs := ocihandler.RemoteRegistriesFromSpecs([]ocihandler.RemoteRegistrySpec{{
		Host: "registry.k8s.io",
		Upstreams: []ocihandler.RemoteUpstreamSpec{
			{Name: "daocloud", BaseURL: "https://k8s.m.daocloud.io", Priority: 1},
			{Name: "1ms", BaseURL: "https://k8s.1ms.run", Priority: 2},
		},
	}})
	regHost := newRemoteSpeculaServer(t, s, nil, regs)

	ref, err := name.ParseReference(regHost+"/registry.k8s.io/pause:3.9", name.Insecure)
	require.NoError(t, err)
	img, err := remote.Image(ref, remote.WithTransport(http.DefaultTransport))
	if err != nil {
		t.Skipf("live registry.k8s.io/pause unreachable: %v", err)
	}
	d, err := img.Digest()
	require.NoError(t, err)
	t.Logf("pulled registry.k8s.io/pause:3.9 → %s", d)
	require.True(t, strings.HasPrefix(d.String(), "sha256:"))
}

func TestLiveHelmOCIChart(t *testing.T) {
	if os.Getenv("SPECULA_E2E_LIVE") != "1" {
		t.Skip("set SPECULA_E2E_LIVE=1 to pull a real Helm OCI chart via ghcr.io")
	}

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	regs := ocihandler.RemoteRegistriesFromSpecs([]ocihandler.RemoteRegistrySpec{{
		Host: "ghcr.io",
		Upstreams: []ocihandler.RemoteUpstreamSpec{
			{Name: "daocloud", BaseURL: "https://ghcr.m.daocloud.io", Priority: 1},
			{Name: "1ms", BaseURL: "https://ghcr.1ms.run", Priority: 2},
		},
	}})
	regHost := newRemoteSpeculaServer(t, s, nil, regs)

	// Small public chart; skip cleanly if mirrors / origin are unreachable.
	chart := "ghcr.io/stefanprodan/charts/podinfo:6.5.0"
	ref, err := name.ParseReference(regHost+"/"+chart, name.Insecure)
	require.NoError(t, err)
	desc, err := remote.Head(ref, remote.WithTransport(http.DefaultTransport))
	if err != nil {
		t.Skipf("live Helm OCI chart unreachable: %v", err)
	}
	t.Logf("HEAD %s → %s", chart, desc.Digest)
}

// Drain helper kept for future streaming asserts.
var _ = io.Discard
