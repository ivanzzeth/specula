//go:build integration

package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/stretchr/testify/require"

	ocihandler "github.com/ivanzzeth/specula/internal/handler/oci"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// crossUpstreamMidStreamFailoverReady mirrors internal/upstream's gate. Flip
// together when resume.go gains mid-stream fallthrough across the chain.
const crossUpstreamMidStreamFailoverReady = true

func requireCrossUpstreamMidStreamFailover(t *testing.T) {
	t.Helper()
	if !crossUpstreamMidStreamFailoverReady {
		t.Skip("TODO(mid-stream cross-upstream failover): Specula must keep the cold-fill " +
			"alive when the first mirror dies mid-blob and a later mirror is healthy; " +
			"enable after upstream resume fallthrough ships")
	}
}

// ── minimalOCIImage — deterministic image bytes for controllable upstreams ──

type minimalOCIImage struct {
	Name           string // upstream repo path (stripped host), e.g. "pause"
	Tag            string
	Manifest       []byte
	ManifestDigest string
	Config         []byte
	ConfigDigest   string
	Layer          []byte
	LayerDigest    string
	Img            v1.Image
}

func newMinimalOCIImage(t *testing.T, name, tag string, layerSize int64) *minimalOCIImage {
	t.Helper()
	img, err := random.Image(layerSize, 1)
	require.NoError(t, err)

	manifest, err := img.RawManifest()
	require.NoError(t, err)
	md, err := img.Digest()
	require.NoError(t, err)

	config, err := img.RawConfigFile()
	require.NoError(t, err)
	cd, err := img.ConfigName()
	require.NoError(t, err)

	layers, err := img.Layers()
	require.NoError(t, err)
	require.Len(t, layers, 1)
	ld, err := layers[0].Digest()
	require.NoError(t, err)
	lrc, err := layers[0].Compressed()
	require.NoError(t, err)
	defer lrc.Close()
	layerBytes, err := io.ReadAll(lrc)
	require.NoError(t, err)

	return &minimalOCIImage{
		Name:           name,
		Tag:            tag,
		Manifest:       manifest,
		ManifestDigest: md.String(),
		Config:         config,
		ConfigDigest:   cd.String(),
		Layer:          layerBytes,
		LayerDigest:    ld.String(),
		Img:            img,
	}
}

func (m *minimalOCIImage) blob(digest string) ([]byte, bool) {
	switch digest {
	case m.ConfigDigest:
		return m.Config, true
	case m.LayerDigest:
		return m.Layer, true
	default:
		return nil, false
	}
}

// ── controllableOCIRegistry — serves one image with pluggable blob policy ──

type blobPolicy int

const (
	blobOK               blobPolicy = iota
	blobDropOnceThenOK              // mid-stream drop once, then Range/full OK (same-upstream resume)
	blobDropOnceThenDead            // mid-stream drop once, then permanent 503
	blobAlwaysRefuse                // connection-level death (server closed)
)

type controllableOCIRegistry struct {
	img      *minimalOCIImage
	policy   blobPolicy
	blobGets atomic.Int64
	requests atomic.Int64 // all /v2/ hits (manifest+blob+version)

	mu      sync.Mutex
	dropped bool // whether the one-shot mid-stream drop already happened
	srv     *httptest.Server
}

func startControllableOCIRegistry(t *testing.T, img *minimalOCIImage, policy blobPolicy) *controllableOCIRegistry {
	t.Helper()
	r := &controllableOCIRegistry{img: img, policy: policy}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *controllableOCIRegistry) URL() string { return r.srv.URL }

func (r *controllableOCIRegistry) Requests() int64 { return r.requests.Load() }

func (r *controllableOCIRegistry) asUpstream(name string, priority int) upstream.Upstream {
	return upstream.Upstream{Name: name, BaseURL: r.srv.URL, Priority: priority}
}

func (r *controllableOCIRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.requests.Add(1)
	p := req.URL.Path
	switch {
	case p == "/v2/" || p == "/v2":
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		return
	case strings.Contains(p, "/manifests/"):
		r.serveManifest(w, req)
	case strings.Contains(p, "/blobs/"):
		r.serveBlob(w, req)
	default:
		http.NotFound(w, req)
	}
}

func (r *controllableOCIRegistry) serveManifest(w http.ResponseWriter, req *http.Request) {
	ref := req.URL.Path[strings.LastIndexByte(req.URL.Path, '/')+1:]
	if ref != r.img.Tag && ref != r.img.ManifestDigest {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
	w.Header().Set("Docker-Content-Digest", r.img.ManifestDigest)
	w.Header().Set("Content-Length", strconv.Itoa(len(r.img.Manifest)))
	w.WriteHeader(http.StatusOK)
	if req.Method != http.MethodHead {
		_, _ = w.Write(r.img.Manifest)
	}
}

func (r *controllableOCIRegistry) serveBlob(w http.ResponseWriter, req *http.Request) {
	r.blobGets.Add(1)
	digest := req.URL.Path[strings.LastIndexByte(req.URL.Path, '/')+1:]
	data, ok := r.img.blob(digest)
	if !ok {
		http.NotFound(w, req)
		return
	}

	rng := req.Header.Get("Range")
	start := 0
	if strings.HasPrefix(rng, "bytes=") {
		fmt.Sscanf(rng, "bytes=%d-", &start)
		if start < 0 || start > len(data) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}

	switch r.policy {
	case blobAlwaysRefuse:
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	case blobDropOnceThenDead:
		r.mu.Lock()
		already := r.dropped
		if !already && start == 0 {
			r.dropped = true
			r.mu.Unlock()
			writeBlobPrefixThenDrop(w, data, len(data)/4)
			return
		}
		r.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "upstream crashed")
		return
	case blobDropOnceThenOK:
		r.mu.Lock()
		if !r.dropped && start == 0 {
			r.dropped = true
			r.mu.Unlock()
			writeBlobPrefixThenDrop(w, data, len(data)/4)
			return
		}
		r.mu.Unlock()
		// fall through to normal serve (Range or full)
	}

	rest := data[start:]
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Accept-Ranges", "bytes")
	if start > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(rest)))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(rest)))
		w.WriteHeader(http.StatusOK)
	}
	if req.Method != http.MethodHead {
		_, _ = w.Write(rest)
	}
}

func writeBlobPrefixThenDrop(w http.ResponseWriter, full []byte, prefixLen int) {
	if prefixLen <= 0 || prefixLen >= len(full) {
		prefixLen = len(full) / 4
		if prefixLen < 1 {
			prefixLen = 1
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(full)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(full[:prefixLen])
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	time.Sleep(20 * time.Millisecond)
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}

// ── remote registry Specula wiring helpers ──────────────────────────────────

// k8sRemoteChain builds a RemoteRegistryMap for host with mirrors ending at
// originURL (replacing the auto-appended https://host).
func k8sRemoteChain(t *testing.T, host string, mirrors []upstream.Upstream, originURL string) ocihandler.RemoteRegistryMap {
	t.Helper()
	specs := []ocihandler.RemoteUpstreamSpec{}
	for i, m := range mirrors {
		specs = append(specs, ocihandler.RemoteUpstreamSpec{
			Name:     m.Name,
			BaseURL:  m.BaseURL,
			Priority: i + 1,
		})
	}
	regs := ocihandler.RemoteRegistriesFromSpecs([]ocihandler.RemoteRegistrySpec{{
		Host:      host,
		Upstreams: specs,
	}})
	chain := regs[host]
	require.NotEmpty(t, chain)
	// Last entry is official origin — point at test origin.
	chain[len(chain)-1].BaseURL = originURL
	regs[host] = chain
	return regs
}

// newRemoteSpeculaServer wires Specula with Hub upstreams + remote allowlist.
func newRemoteSpeculaServer(
	t *testing.T,
	s *speculaStack,
	hub []upstream.Upstream,
	regs ocihandler.RemoteRegistryMap,
) (regHost string) {
	t.Helper()
	opts := []ocihandler.Option{
		ocihandler.WithQuarantineDir(s.dir),
		ocihandler.WithRemoteRegistries(regs),
	}
	if len(hub) > 0 {
		opts = append(opts, ocihandler.WithUpstream(upstream.NewClient(), hub))
	} else {
		// Hub unused; still need a client for remote fetches.
		opts = append(opts, ocihandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "hub-unused", BaseURL: "http://127.0.0.1:1", Priority: 99},
		}))
	}
	_, host := newSpeculaServer(t, s, opts...)
	return host
}

// sha256Of is a tiny helper for digest assertions in failover helpers.
func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// deadURL returns a URL that immediately connection-refuses.
func deadURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := srv.URL
	srv.Close()
	return url
}

// countingStatusUpstream returns  code for every request and counts hits.
func countingStatusUpstream(t *testing.T, code int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, "denied")
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// Suppress unused import if helpers evolve.
var _ = bytes.Equal
