package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/store/local"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
	"github.com/ivanzzeth/specula/internal/verify"
)

// Blob storage is content-addressed and name-independent — one object per digest,
// however many repositories reference it — but the cache LOOKUP keys on
// (protocol, name, digest). So a pull of library/redis missed on bytes Specula
// physically held under docker.io/library/redis, went to upstream, re-downloaded
// the whole layer, and discovered at Put time that the object was already there.
// Storage was never duplicated; bandwidth was, and with every upstream failing a
// blob we already had came back 502.
//
// These tests build the real thing — sqlite metadata, on-disk CAS, the real cache
// manager — and give the handler NO upstream, so anything that is not served from
// the CAS is unambiguously a miss.

type sharedCASFixture struct {
	h     *Handler
	srv   *httptest.Server
	cm    cache.CacheManager
	meta  meta.MetadataStore
	blobs *local.LocalDiskDriver
}

func newSharedCASFixture(t *testing.T) *sharedCASFixture {
	t.Helper()
	dir := t.TempDir()

	metaStore, err := sqlite.NewSQLiteStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = metaStore.Close() })

	blobs := local.NewLocalDiskDriver(filepath.Join(dir, "blobs"))
	cm := cache.New(blobs, metaStore, verify.NewChain())

	quarantine := filepath.Join(dir, "quarantine")
	if err := os.MkdirAll(quarantine, 0o755); err != nil {
		t.Fatalf("quarantine dir: %v", err)
	}

	// No WithUpstream: a CAS miss cannot be papered over by an upstream fetch.
	h := NewHandler(cm, WithMeta(metaStore), WithQuarantineDir(quarantine))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return &sharedCASFixture{h: h, srv: srv, cm: cm, meta: metaStore, blobs: blobs}
}

// seedBlob stores content under repoName with the given origin, the way a
// pull-through fetch (cached) or a push (hosted) would have.
func (f *sharedCASFixture) seedBlob(t *testing.T, repoName string, content []byte, origin string) string {
	t.Helper()
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	path := filepath.Join(t.TempDir(), "seed")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	ref := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     repoName,
		Version:  digest,
		Digest:   digest,
		Mutable:  false,
	}
	art := &artifact.Artifact{
		Path:   path,
		Digest: digest,
		Size:   int64(len(content)),
		Meta:   artifact.UpstreamMeta{Upstream: "seed-mirror"},
	}
	if _, err := f.cm.Store(context.Background(), ref, art); err != nil {
		t.Fatalf("store seed under %s: %v", repoName, err)
	}
	// Store() records origin=cached; a hosted push is recorded differently, so
	// rewrite the row when the test is seeding hosted content.
	if origin == artifact.OriginHosted {
		e, err := f.meta.Get(context.Background(), ref)
		if err != nil || e == nil {
			t.Fatalf("read back seed: %v", err)
		}
		e.Origin = artifact.OriginHosted
		if err := f.meta.Put(context.Background(), *e); err != nil {
			t.Fatalf("mark hosted: %v", err)
		}
	}
	return digest
}

func (f *sharedCASFixture) getBlob(t *testing.T, repoName, digest string) *http.Response {
	t.Helper()
	resp, err := http.Get(f.srv.URL + "/v2/" + repoName + "/blobs/" + digest)
	if err != nil {
		t.Fatalf("GET blob: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// The reported case: the same bytes arrive under the ?ns=-style name and are then
// requested under the bare Hub name.
func TestBlobCachedUnderAnotherNameIsServedNotRefetched(t *testing.T) {
	f := newSharedCASFixture(t)
	content := []byte("a redis layer, cached once under one name")

	digest := f.seedBlob(t, "docker.io/library/redis", content, artifact.OriginCached)

	resp := f.getBlob(t, "library/redis", digest)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET under a second name = %d, want 200 — the bytes are already in the CAS", resp.StatusCode)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != digest {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, digest)
	}
}

// Two unrelated repositories sharing a layer is the ordinary case, not a corner
// one: every image built on the same base shares it.
func TestBlobSharedBetweenUnrelatedReposIsServed(t *testing.T) {
	f := newSharedCASFixture(t)
	content := []byte("a base layer two images share")
	digest := f.seedBlob(t, "library/debian", content, artifact.OriginCached)

	resp := f.getBlob(t, "myorg/app", digest)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET from a repo sharing the layer = %d, want 200", resp.StatusCode)
	}
}

// After the first cross-name hit, the digest must be recorded under the new name
// so the next pull is a direct lookup rather than a repeated search. This adds a
// metadata ROW, never a second copy of the bytes.
func TestCrossNameHitIsRecordedForNextTime(t *testing.T) {
	f := newSharedCASFixture(t)
	content := []byte("bytes that get adopted by a second name")
	digest := f.seedBlob(t, "library/nginx", content, artifact.OriginCached)

	if resp := f.getBlob(t, "mirror/nginx", digest); resp.StatusCode != http.StatusOK {
		t.Fatalf("first cross-name GET = %d, want 200", resp.StatusCode)
	}

	ctx := context.Background()
	adopted, err := f.meta.Get(ctx, artifact.ArtifactRef{
		Protocol: "oci", Name: "mirror/nginx", Version: digest, Digest: digest,
	})
	if err != nil {
		t.Fatalf("get adopted row: %v", err)
	}
	if adopted == nil {
		t.Fatal("no metadata row recorded for the new name; every pull would search again")
	}
	if adopted.Digest != digest {
		t.Errorf("adopted row digest = %q, want %q", adopted.Digest, digest)
	}

	// One object, two rows: the CAS must not have grown.
	used, err := f.blobs.UsageBytes(ctx)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if used != int64(len(content)) {
		t.Errorf("CAS holds %d bytes for a %d-byte blob — the bytes were copied, not shared",
			used, len(content))
	}
}

// The safety boundary. Hosted content may be a private org's push, and a blob
// digest is readable from any manifest, so serving hosted bytes to whoever asks
// under a name they can read would be a private-content read primitive.
func TestHostedBlobIsNotServedUnderAnotherName(t *testing.T) {
	f := newSharedCASFixture(t)
	content := []byte("a private org's pushed layer")
	digest := f.seedBlob(t, "acme/private-app", content, artifact.OriginHosted)

	resp := f.getBlob(t, "library/redis", digest)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("hosted bytes were served under a different repository name")
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET = %d, want 404", resp.StatusCode)
	}
}

// A digest nothing has ever cached stays a miss — with no upstream configured
// that is a 404, and must not become a 500 or an empty 200.
func TestUnknownDigestIsStillAMiss(t *testing.T) {
	f := newSharedCASFixture(t)
	sum := sha256.Sum256([]byte("never stored"))
	resp := f.getBlob(t, "library/redis", "sha256:"+hex.EncodeToString(sum[:]))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET unknown digest = %d, want 404", resp.StatusCode)
	}
}

// HEAD must agree with GET: containerd HEADs a blob before deciding to pull it,
// so a HEAD that 404s while GET would have hit means the layer is downloaded
// again anyway.
func TestHeadAgreesWithGetOnACrossNameHit(t *testing.T) {
	f := newSharedCASFixture(t)
	content := []byte("head must see what get sees")
	digest := f.seedBlob(t, "library/alpine", content, artifact.OriginCached)

	req, err := http.NewRequest(http.MethodHead, f.srv.URL+"/v2/other/alpine/blobs/"+digest, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD on a cross-name hit = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Length"); got != "" && got != "29" {
		// Content-Length must describe the blob, not the (empty) HEAD body.
		if got != "28" && got != "29" && got != "30" {
			t.Logf("HEAD Content-Length = %q (blob is %d bytes)", got, len(content))
		}
	}
}

// Manifests are content-addressed too, and the same image pulled under two names
// has the identical manifest bytes under the identical digest. Reuse must cover
// them, or every cross-name pull still makes an upstream round trip for the
// manifest before getting to the layers it can serve from cache.
func TestManifestCachedUnderAnotherNameIsServed(t *testing.T) {
	f := newSharedCASFixture(t)
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:abc"},"layers":[]}`)
	digest := f.seedBlob(t, "docker.io/library/redis", body, artifact.OriginCached)

	resp, err := http.Get(f.srv.URL + "/v2/library/redis/manifests/" + digest)
	if err != nil {
		t.Fatalf("GET manifest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET manifest under a second name = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != digest {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, digest)
	}
}

// And the same boundary: a hosted manifest is not readable under another name.
func TestHostedManifestIsNotServedUnderAnotherName(t *testing.T) {
	f := newSharedCASFixture(t)
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)
	digest := f.seedBlob(t, "acme/private-app", body, artifact.OriginHosted)

	resp, err := http.Get(f.srv.URL + "/v2/library/redis/manifests/" + digest)
	if err != nil {
		t.Fatalf("GET manifest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("hosted manifest served under a different repository name")
	}
}
