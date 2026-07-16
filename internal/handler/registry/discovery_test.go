package registry_test

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ivanzzeth/specula/internal/acl"
	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/handler/registry"
	"github.com/ivanzzeth/specula/internal/repo"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// ── meta store double ─────────────────────────────────────────────────────────

// memMetaStore is an in-memory MetadataStore covering the immutable + mutable
// tiers the registry handler touches.
type memMetaStore struct {
	mu      sync.Mutex
	entries map[string]artifact.CacheEntry
	mutable map[string]artifact.MutableEntry
}

func newMemMetaStore() *memMetaStore {
	return &memMetaStore{
		entries: map[string]artifact.CacheEntry{},
		mutable: map[string]artifact.MutableEntry{},
	}
}

func metaKey(ref artifact.ArtifactRef) string {
	return ref.Protocol + "|" + ref.Name + "|" + ref.Version
}

func (m *memMetaStore) Get(_ context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[metaKey(ref)]
	if !ok {
		return nil, nil
	}
	return &e, nil
}

func (m *memMetaStore) Put(_ context.Context, e artifact.CacheEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[metaKey(e.Ref)] = e
	return nil
}

func (m *memMetaStore) Delete(_ context.Context, ref artifact.ArtifactRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, metaKey(ref))
	return nil
}

func (m *memMetaStore) GetMutable(_ context.Context, key string) (*artifact.MutableEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.mutable[key]
	if !ok {
		return nil, nil
	}
	return &e, nil
}

func (m *memMetaStore) PutMutable(_ context.Context, e artifact.MutableEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mutable[e.Key] = e
	return nil
}

func (m *memMetaStore) DeleteMutable(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mutable, key)
	return nil
}

func (m *memMetaStore) CacheSizeByProtocol(_ context.Context) (map[string]artifact.SizeStat, error) {
	return map[string]artifact.SizeStat{}, nil
}

// ListEntries / SetPinned are part of meta.MetadataStore but unused by the
// registry discovery path; stubbed to satisfy the interface.
func (m *memMetaStore) ListEntries(_ context.Context, _ string, _ meta.EntryFilter, _ meta.Page) (meta.EntryPage, error) {
	return meta.EntryPage{}, nil
}

func (m *memMetaStore) SetPinned(_ context.Context, _ artifact.ArtifactRef, _ bool) error {
	return nil
}

// hasMutable reports whether a mutable key is present.
func (m *memMetaStore) hasMutable(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.mutable[key]
	return ok
}

// newDiscoveryHandler builds a Handler with a MetadataStore wired, which the
// referrers index and the tag-pointer mirror both require.
func newDiscoveryHandler(blobs *memBlobStore, tags *memTagStore, meta *memMetaStore, authz registry.Authorizer) *registry.Handler {
	return registry.NewHandler(
		&nilCacheManager{},
		newMemRepoStore(),
		tags,
		authz,
		registry.WithBlobStore(blobs),
		registry.WithMeta(meta),
	)
}

// ── tag listing ───────────────────────────────────────────────────────────────

// TestListTagsReturnsPushedTagsSorted verifies GET /v2/<name>/tags/list reports
// the tags actually pushed, in lexical order, with the spec's {name, tags} shape.
func TestListTagsReturnsPushedTagsSorted(t *testing.T) {
	blobs, tags, meta := newMemBlobStore(), newMemTagStore(), newMemMetaStore()
	repoObj := testRepo("org1", "org1/myrepo")
	h := newDiscoveryHandler(blobs, tags, meta, &allowAuthz{r: repoObj})

	for _, tag := range []string{"v2", "latest", "v1"} {
		if err := tags.PutTag(context.Background(), repoObj.ID, tag, "sha256:"+strings.Repeat("a", 64)); err != nil {
			t.Fatalf("seed tag %s: %v", tag, err)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/org1/myrepo/tags/list", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var got struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "org1/myrepo" {
		t.Errorf("name = %q, want org1/myrepo", got.Name)
	}
	want := []string{"latest", "v1", "v2"}
	if fmt.Sprint(got.Tags) != fmt.Sprint(want) {
		t.Errorf("tags = %v, want %v (must be lexically sorted)", got.Tags, want)
	}
}

// TestListTagsEmptyRepoReturnsEmptyArray verifies a repo with no tags reports an
// empty JSON array rather than null — clients range over the field directly.
func TestListTagsEmptyRepoReturnsEmptyArray(t *testing.T) {
	repoObj := testRepo("org1", "org1/myrepo")
	h := newDiscoveryHandler(newMemBlobStore(), newMemTagStore(), newMemMetaStore(), &allowAuthz{r: repoObj})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/org1/myrepo/tags/list", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"tags":[]`) {
		t.Errorf("body = %s, want an empty tags array (not null)", rec.Body)
	}
}

// TestListTagsPagination verifies the ?n= / ?last= pagination contract and the
// rel="next" Link header on a truncated page.
func TestListTagsPagination(t *testing.T) {
	blobs, tags, meta := newMemBlobStore(), newMemTagStore(), newMemMetaStore()
	repoObj := testRepo("org1", "org1/myrepo")
	h := newDiscoveryHandler(blobs, tags, meta, &allowAuthz{r: repoObj})

	all := []string{"a", "b", "c", "d"}
	for _, tag := range all {
		if err := tags.PutTag(context.Background(), repoObj.ID, tag, "sha256:"+strings.Repeat("a", 64)); err != nil {
			t.Fatalf("seed tag: %v", err)
		}
	}

	list := func(query string) ([]string, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/org1/myrepo/tags/list"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d for %q, want 200", rec.Code, query)
		}
		var got struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got.Tags, rec.Header().Get("Link")
	}

	// ?n=2 → first page, truncated, advertises the next page.
	page, link := list("?n=2")
	if fmt.Sprint(page) != fmt.Sprint([]string{"a", "b"}) {
		t.Errorf("n=2 page = %v, want [a b]", page)
	}
	if !strings.Contains(link, `rel="next"`) || !strings.Contains(link, "last=b") {
		t.Errorf("Link = %q, want a rel=next link resuming after b", link)
	}

	// ?last=b → strictly after b, and b itself must not reappear.
	page, _ = list("?last=b")
	if fmt.Sprint(page) != fmt.Sprint([]string{"c", "d"}) {
		t.Errorf("last=b page = %v, want [c d]", page)
	}

	// A complete page carries no next link.
	if _, link = list(""); link != "" {
		t.Errorf("Link = %q on a complete listing, want none", link)
	}
}

// TestListTagsUnknownRepo404 verifies an authorized caller naming a repo that
// does not exist gets NAME_UNKNOWN, not a permission error.
func TestListTagsUnknownRepo404(t *testing.T) {
	h := newDiscoveryHandler(newMemBlobStore(), newMemTagStore(), newMemMetaStore(),
		&denyAuthz{err: repo.ErrNotFound})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/org1/nope/tags/list", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// ── referrers ─────────────────────────────────────────────────────────────────

// subjectDigest is a syntactically valid sha256 digest used as a referrers subject.
const subjectDigest = "sha256:" + "cafe" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab"

// putManifestBody pushes a manifest body and returns the response recorder.
func putManifestBody(t *testing.T, h *registry.Handler, ref string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v2/org1/myrepo/manifests/"+ref, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	h.ServeHTTP(rec, req)
	return rec
}

// getReferrers fetches the referrers index for a subject.
func getReferrers(t *testing.T, h *registry.Handler, subject, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/org1/myrepo/referrers/"+subject+query, nil))
	return rec
}

// decodeIndex decodes an OCI image index response body.
func decodeIndex(t *testing.T, rec *httptest.ResponseRecorder) struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Manifests     []struct {
		MediaType    string            `json:"mediaType"`
		Digest       string            `json:"digest"`
		Size         int64             `json:"size"`
		ArtifactType string            `json:"artifactType"`
		Annotations  map[string]string `json:"annotations"`
	} `json:"manifests"`
} {
	t.Helper()
	var idx struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		Manifests     []struct {
			MediaType    string            `json:"mediaType"`
			Digest       string            `json:"digest"`
			Size         int64             `json:"size"`
			ArtifactType string            `json:"artifactType"`
			Annotations  map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &idx); err != nil {
		t.Fatalf("decode index: %v (body %s)", err, rec.Body)
	}
	return idx
}

// TestReferrersEmptyReturnsIndexNot404 pins the spec rule that a registry
// supporting the referrers API MUST NOT answer 404: a subject with no referrers
// — including one in a repo that does not exist yet — is 200 with an empty index.
func TestReferrersEmptyReturnsIndexNot404(t *testing.T) {
	for _, tc := range []struct {
		name  string
		authz registry.Authorizer
	}{
		{"existing repo, no referrers", &allowAuthz{r: testRepo("org1", "org1/myrepo")}},
		{"repo does not exist", &denyAuthz{err: repo.ErrNotFound}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDiscoveryHandler(newMemBlobStore(), newMemTagStore(), newMemMetaStore(), tc.authz)
			rec := getReferrers(t, h, subjectDigest, "")

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (spec: referrers MUST NOT 404)", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.oci.image.index.v1+json" {
				t.Errorf("Content-Type = %q, want the OCI index media type", ct)
			}
			idx := decodeIndex(t, rec)
			if idx.SchemaVersion != 2 || idx.MediaType != "application/vnd.oci.image.index.v1+json" {
				t.Errorf("index = %+v, want a well-formed empty OCI index", idx)
			}
			if len(idx.Manifests) != 0 {
				t.Errorf("manifests = %v, want empty", idx.Manifests)
			}
		})
	}
}

// TestReferrersForbiddenStillDenied guards the security boundary: the
// MUST-NOT-404 rule relaxes existence reporting, never authorization.
func TestReferrersForbiddenStillDenied(t *testing.T) {
	h := newDiscoveryHandler(newMemBlobStore(), newMemTagStore(), newMemMetaStore(),
		&denyAuthz{err: acl.ErrForbidden})

	if rec := getReferrers(t, h, subjectDigest, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a caller without pull scope", rec.Code)
	}
}

// TestReferrersInvalidDigest400 verifies a malformed subject digest is the
// endpoint's documented 400 case.
func TestReferrersInvalidDigest400(t *testing.T) {
	h := newDiscoveryHandler(newMemBlobStore(), newMemTagStore(), newMemMetaStore(),
		&allowAuthz{r: testRepo("org1", "org1/myrepo")})

	if rec := getReferrers(t, h, "not-a-digest", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestReferrersIndexedOnSubjectPush verifies that pushing a manifest with a
// `subject` lists it under that subject, echoes OCI-Subject, and honours the
// artifactType filter with OCI-Filters-Applied.
func TestReferrersIndexedOnSubjectPush(t *testing.T) {
	blobs, tags, meta := newMemBlobStore(), newMemTagStore(), newMemMetaStore()
	h := newDiscoveryHandler(blobs, tags, meta, &allowAuthz{r: testRepo("org1", "org1/myrepo")})

	body := []byte(`{"schemaVersion":2,` +
		`"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"artifactType":"application/vnd.example.sbom.v1",` +
		`"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:` + strings.Repeat("0", 64) + `","size":2},` +
		`"layers":[],` +
		`"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + subjectDigest + `","size":123},` +
		`"annotations":{"org.example.key":"value"}}`)

	rec := putManifestBody(t, h, "sbom", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("push status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("OCI-Subject"); got != subjectDigest {
		t.Errorf("OCI-Subject = %q, want %q — the client relies on this to know the subject was processed", got, subjectDigest)
	}
	manifestDigest := rec.Header().Get("Docker-Content-Digest")

	// The referrer is listed under its subject.
	idx := decodeIndex(t, getReferrers(t, h, subjectDigest, ""))
	if len(idx.Manifests) != 1 {
		t.Fatalf("manifests = %d, want 1", len(idx.Manifests))
	}
	got := idx.Manifests[0]
	if got.Digest != manifestDigest {
		t.Errorf("digest = %q, want %q", got.Digest, manifestDigest)
	}
	if got.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", got.Size, len(body))
	}
	if got.ArtifactType != "application/vnd.example.sbom.v1" {
		t.Errorf("artifactType = %q, want the manifest's artifactType", got.ArtifactType)
	}
	if got.Annotations["org.example.key"] != "value" {
		t.Errorf("annotations = %v, want the manifest's annotations propagated", got.Annotations)
	}

	// A matching artifactType filter keeps it and advertises the filter.
	rec = getReferrers(t, h, subjectDigest, "?artifactType=application/vnd.example.sbom.v1")
	if f := rec.Header().Get("OCI-Filters-Applied"); f != "artifactType" {
		t.Errorf("OCI-Filters-Applied = %q, want artifactType", f)
	}
	if n := len(decodeIndex(t, rec).Manifests); n != 1 {
		t.Errorf("filtered manifests = %d, want 1", n)
	}

	// A non-matching filter excludes it.
	rec = getReferrers(t, h, subjectDigest, "?artifactType=application/vnd.example.other")
	if n := len(decodeIndex(t, rec).Manifests); n != 0 {
		t.Errorf("non-matching filter returned %d manifests, want 0", n)
	}
}

// TestReferrersArtifactTypeFallsBackToConfigMediaType pins the image-spec rule
// that a manifest without artifactType is indexed under its config mediaType.
func TestReferrersArtifactTypeFallsBackToConfigMediaType(t *testing.T) {
	h := newDiscoveryHandler(newMemBlobStore(), newMemTagStore(), newMemMetaStore(),
		&allowAuthz{r: testRepo("org1", "org1/myrepo")})

	body := []byte(`{"schemaVersion":2,` +
		`"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.example.config.v1+json","digest":"sha256:` + strings.Repeat("0", 64) + `","size":2},` +
		`"layers":[],` +
		`"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + subjectDigest + `","size":123}}`)

	if rec := putManifestBody(t, h, "cfg", body); rec.Code != http.StatusCreated {
		t.Fatalf("push status = %d, want 201", rec.Code)
	}

	idx := decodeIndex(t, getReferrers(t, h, subjectDigest, ""))
	if len(idx.Manifests) != 1 {
		t.Fatalf("manifests = %d, want 1", len(idx.Manifests))
	}
	if got := idx.Manifests[0].ArtifactType; got != "application/vnd.example.config.v1+json" {
		t.Errorf("artifactType = %q, want the config mediaType fallback", got)
	}
}

// TestReferrersRePushIsIdempotent verifies re-pushing the same referrer does not
// duplicate its entry (spec: duplicate entries SHOULD NOT be created).
func TestReferrersRePushIsIdempotent(t *testing.T) {
	h := newDiscoveryHandler(newMemBlobStore(), newMemTagStore(), newMemMetaStore(),
		&allowAuthz{r: testRepo("org1", "org1/myrepo")})

	body := []byte(`{"schemaVersion":2,` +
		`"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"artifactType":"application/vnd.example.sbom.v1",` +
		`"layers":[],` +
		`"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + subjectDigest + `","size":123}}`)

	for i := 0; i < 3; i++ {
		if rec := putManifestBody(t, h, "sbom", body); rec.Code != http.StatusCreated {
			t.Fatalf("push %d status = %d, want 201", i, rec.Code)
		}
	}

	if n := len(decodeIndex(t, getReferrers(t, h, subjectDigest, "")).Manifests); n != 1 {
		t.Errorf("manifests after 3 identical pushes = %d, want 1", n)
	}
}

// ── multi-algorithm digests ───────────────────────────────────────────────────

// TestSHA512BlobUploadAccepted verifies a blob pushed under sha512 is accepted
// and addressed by its sha512 digest. The OCI image spec registers sha256,
// sha384 and sha512; the client picks, and the registry must not assume sha256.
func TestSHA512BlobUploadAccepted(t *testing.T) {
	blobs := newMemBlobStore()
	h := newTestHandler(blobs, newMemTagStore(), &allowAuthz{r: testRepo("org1", "org1/myrepo")})

	content := []byte("sha512 addressed content")
	sum := sha512.Sum512(content)
	want := "sha512:" + hex.EncodeToString(sum[:])

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v2/org1/myrepo/blobs/uploads/", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", rec.Code)
	}
	uuid := extractUploadUUID(rec.Header().Get("Location"))

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut,
		"/v2/org1/myrepo/blobs/uploads/"+uuid+"?digest="+want, bytes.NewReader(content)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Docker-Content-Digest"); got != want {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, want)
	}
	if !blobs.has(want) {
		t.Errorf("blob not stored under its sha512 digest %q", want)
	}
}

// TestSHA512DigestMismatchRejected verifies verify-on-write still holds for a
// non-sha256 algorithm: accepting the algorithm must not mean skipping the check.
func TestSHA512DigestMismatchRejected(t *testing.T) {
	blobs := newMemBlobStore()
	h := newTestHandler(blobs, newMemTagStore(), &allowAuthz{r: testRepo("org1", "org1/myrepo")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v2/org1/myrepo/blobs/uploads/", nil))
	uuid := extractUploadUUID(rec.Header().Get("Location"))

	wrong := "sha512:" + strings.Repeat("0", 128)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut,
		"/v2/org1/myrepo/blobs/uploads/"+uuid+"?digest="+wrong, strings.NewReader("some content")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a sha512 digest mismatch", rec.Code)
	}
	if blobs.has(wrong) {
		t.Error("blob was stored despite failing digest verification")
	}
}

// TestUnsupportedDigestAlgorithmRejected verifies an unregistered algorithm is
// still DIGEST_INVALID — accepting sha384/sha512 must not open the door to any
// algorithm a client invents.
func TestUnsupportedDigestAlgorithmRejected(t *testing.T) {
	h := newTestHandler(newMemBlobStore(), newMemTagStore(), &allowAuthz{r: testRepo("org1", "org1/myrepo")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v2/org1/myrepo/blobs/uploads/", nil))
	uuid := extractUploadUUID(rec.Header().Get("Location"))

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut,
		"/v2/org1/myrepo/blobs/uploads/"+uuid+"?digest=md5:"+strings.Repeat("0", 32), strings.NewReader("x")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unsupported digest algorithm", rec.Code)
	}
}

// ── chunked upload ordering ───────────────────────────────────────────────────

// TestOutOfOrderChunkReturns416 verifies a non-contiguous PATCH is rejected with
// 416 and leaves the session untouched, so the client can resume from the echoed
// Range rather than discovering the corruption at finalisation.
func TestOutOfOrderChunkReturns416(t *testing.T) {
	h := newTestHandler(newMemBlobStore(), newMemTagStore(), &allowAuthz{r: testRepo("org1", "org1/myrepo")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v2/org1/myrepo/blobs/uploads/", nil))
	uuid := extractUploadUUID(rec.Header().Get("Location"))

	// Session is at offset 0; a chunk claiming to start at 1024 skips a gap.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v2/org1/myrepo/blobs/uploads/"+uuid, strings.NewReader("late chunk"))
	req.Header.Set("Content-Range", "1024-2047")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416 for a non-contiguous chunk", rec.Code)
	}
	if got := rec.Header().Get("Range"); got != "0-0" {
		t.Errorf("Range = %q, want 0-0 — the session must be untouched so the client can resume", got)
	}

	// The rejected bytes must not have landed: an in-order chunk still starts at 0.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/v2/org1/myrepo/blobs/uploads/"+uuid, strings.NewReader("abcd"))
	req.Header.Set("Content-Range", "0-3")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("in-order chunk status = %d, want 202", rec.Code)
	}
	if got := rec.Header().Get("Range"); got != "0-3" {
		t.Errorf("Range = %q, want 0-3 — the 416 must not have appended anything", got)
	}
}

// TestOutOfOrderFinalPutReturns416 verifies the closing PUT is subject to the
// same contiguity rule as a PATCH: a chunk that skips a gap is 416, not a blind
// append that later surfaces as a confusing DIGEST_INVALID.
func TestOutOfOrderFinalPutReturns416(t *testing.T) {
	h := newTestHandler(newMemBlobStore(), newMemTagStore(), &allowAuthz{r: testRepo("org1", "org1/myrepo")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v2/org1/myrepo/blobs/uploads/", nil))
	uuid := extractUploadUUID(rec.Header().Get("Location"))

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v2/org1/myrepo/blobs/uploads/"+uuid, strings.NewReader("abcd"))
	req.Header.Set("Content-Range", "0-3")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("PATCH status = %d, want 202", rec.Code)
	}

	// Session is at offset 4; a closing PUT claiming to start at 100 skips a gap.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut,
		"/v2/org1/myrepo/blobs/uploads/"+uuid+"?digest=sha256:"+strings.Repeat("0", 64),
		strings.NewReader("tail"))
	req.Header.Set("Content-Range", "100-103")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416 for a non-contiguous closing PUT", rec.Code)
	}
}

// TestChunkWithoutContentRangeIsAppend verifies the streaming form (no
// Content-Range) still appends at the current offset.
func TestChunkWithoutContentRangeIsAppend(t *testing.T) {
	h := newTestHandler(newMemBlobStore(), newMemTagStore(), &allowAuthz{r: testRepo("org1", "org1/myrepo")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v2/org1/myrepo/blobs/uploads/", nil))
	uuid := extractUploadUUID(rec.Header().Get("Location"))

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch,
		"/v2/org1/myrepo/blobs/uploads/"+uuid, strings.NewReader("streamed")))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if got := rec.Header().Get("Range"); got != "0-7" {
		t.Errorf("Range = %q, want 0-7", got)
	}
}

// ── authorization error mapping ───────────────────────────────────────────────

// TestAuthzErrorStatusMapping pins the status a caller sees for each class of
// authorization outcome. The delete path is the reason this matters: an
// authorized caller naming a missing repo must see 404, never 403 — the spec
// allows only 202/404/405 there, and a 403 misleads the client into believing
// its credentials are wrong.
func TestAuthzErrorStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"forbidden is a real permission decision", acl.ErrForbidden, http.StatusForbidden},
		{"read-only is a real permission decision", acl.ErrReadOnly, http.StatusForbidden},
		{"missing repo is not a permission problem", repo.ErrNotFound, http.StatusNotFound},
		{"unexpected store error is not a permission problem", fmt.Errorf("database exploded"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(newMemBlobStore(), newMemTagStore(), &denyAuthz{err: tc.err})

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete,
				"/v2/org1/myrepo/blobs/sha256:"+strings.Repeat("a", 64), nil))

			if rec.Code != tc.want {
				t.Errorf("DELETE blob status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// TestDeleteUsesDeleteAction verifies the write handler re-checks a DELETE
// against the "delete" action — the action the Bearer middleware challenged for
// — rather than "push". A token scoped for delete carries no push grant, so
// checking push would deny every correctly authorized delete.
func TestDeleteUsesDeleteAction(t *testing.T) {
	blobs := newMemBlobStore()
	digest := "sha256:" + strings.Repeat("a", 64)
	if err := blobs.Put(context.Background(), digest, strings.NewReader("x"), 1); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	rec := recordAuthzActions(t, blobs, http.MethodDelete, "/v2/org1/myrepo/blobs/"+digest)
	if want := []string{"delete"}; fmt.Sprint(rec) != fmt.Sprint(want) {
		t.Errorf("actions checked = %v, want %v", rec, want)
	}
}

// TestPushUsesPushAction verifies the push path still checks "push".
func TestPushUsesPushAction(t *testing.T) {
	actions := recordAuthzActions(t, newMemBlobStore(), http.MethodPost, "/v2/org1/myrepo/blobs/uploads/")
	if want := []string{"push"}; fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Errorf("actions checked = %v, want %v", actions, want)
	}
}

// TestReadUsesPullAction verifies discovery reads check "pull".
func TestReadUsesPullAction(t *testing.T) {
	actions := recordAuthzActions(t, newMemBlobStore(), http.MethodGet, "/v2/org1/myrepo/tags/list")
	if want := []string{"pull"}; fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Errorf("actions checked = %v, want %v", actions, want)
	}
}

// recordingAuthz captures the actions the handler asks about.
type recordingAuthz struct {
	mu      sync.Mutex
	actions []string
	r       *repo.Repo
}

func (a *recordingAuthz) Authorize(_ context.Context, _ string, action string) (*repo.Repo, error) {
	a.mu.Lock()
	a.actions = append(a.actions, action)
	a.mu.Unlock()
	return a.r, nil
}

// recordAuthzActions issues one request and returns the sorted set of actions
// the handler asked the Authorizer about.
func recordAuthzActions(t *testing.T, blobs *memBlobStore, method, path string) []string {
	t.Helper()
	authz := &recordingAuthz{r: testRepo("org1", "org1/myrepo")}
	h := newDiscoveryHandler(blobs, newMemTagStore(), newMemMetaStore(), authz)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))

	authz.mu.Lock()
	defer authz.mu.Unlock()
	got := append([]string(nil), authz.actions...)
	sort.Strings(got)
	return got
}

// TestDeleteMissingBlobReturns404 verifies deleting a blob that is not present
// reports 404 rather than a misleading 202 "deleted".
func TestDeleteMissingBlobReturns404(t *testing.T) {
	h := newTestHandler(newMemBlobStore(), newMemTagStore(), &allowAuthz{r: testRepo("org1", "org1/myrepo")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete,
		"/v2/org1/myrepo/blobs/sha256:"+strings.Repeat("b", 64), nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a blob that was never pushed", rec.Code)
	}
}

// ── tag deletion clears the pull-path pointer ─────────────────────────────────

// TestDeleteTagRemovesMutablePointer verifies deleting a tag also clears the
// mutable tag→digest mirror the pull path resolves through. Leaving it behind
// (never-revalidate TTL) would keep serving a deleted tag forever.
func TestDeleteTagRemovesMutablePointer(t *testing.T) {
	blobs, tags, meta := newMemBlobStore(), newMemTagStore(), newMemMetaStore()
	repoObj := testRepo("org1", "org1/myrepo")
	h := newDiscoveryHandler(blobs, tags, meta, &allowAuthz{r: repoObj})

	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)
	if rec := putManifestBody(t, h, "v1", body); rec.Code != http.StatusCreated {
		t.Fatalf("push status = %d, want 201", rec.Code)
	}
	const pointer = "oci:org1/myrepo:v1"
	if !meta.hasMutable(pointer) {
		t.Fatalf("push did not write the mutable tag pointer %q", pointer)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v2/org1/myrepo/manifests/v1", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete status = %d, want 202", rec.Code)
	}

	if meta.hasMutable(pointer) {
		t.Error("mutable tag pointer survived the tag delete — the tag would keep resolving")
	}
	if _, err := tags.GetTag(context.Background(), repoObj.ID, "v1"); err == nil {
		t.Error("tag row survived the tag delete")
	}
}

// TestDeleteManifestByDigestRemovesTags verifies deleting a manifest by digest
// also drops every tag resolving to it — a tag pointing at bytes that no longer
// exist is a dangling reference.
func TestDeleteManifestByDigestRemovesTags(t *testing.T) {
	blobs, tags, meta := newMemBlobStore(), newMemTagStore(), newMemMetaStore()
	repoObj := testRepo("org1", "org1/myrepo")
	h := newDiscoveryHandler(blobs, tags, meta, &allowAuthz{r: repoObj})

	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)
	rec := putManifestBody(t, h, "v1", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("push status = %d, want 201", rec.Code)
	}
	digest := rec.Header().Get("Docker-Content-Digest")

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v2/org1/myrepo/manifests/"+digest, nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete status = %d, want 202 (body %s)", rec.Code, rec.Body)
	}

	if _, err := tags.GetTag(context.Background(), repoObj.ID, "v1"); err == nil {
		t.Error("tag v1 still resolves after its manifest was deleted by digest")
	}
	if meta.hasMutable("oci:org1/myrepo:v1") {
		t.Error("mutable tag pointer survived the by-digest manifest delete")
	}
	if blobs.has(digest) {
		t.Error("manifest bytes survived the by-digest delete")
	}
}

// TestManifestPutByDigestMismatchRejected verifies a by-digest push whose bytes
// do not hash to the declared digest is rejected.
func TestManifestPutByDigestMismatchRejected(t *testing.T) {
	blobs := newMemBlobStore()
	h := newDiscoveryHandler(blobs, newMemTagStore(), newMemMetaStore(),
		&allowAuthz{r: testRepo("org1", "org1/myrepo")})

	wrong := "sha256:" + strings.Repeat("c", 64)
	rec := putManifestBody(t, h, wrong, []byte(`{"schemaVersion":2,"layers":[]}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a by-digest push with mismatching bytes", rec.Code)
	}
	if blobs.has(wrong) {
		t.Error("manifest stored under a digest its bytes do not hash to")
	}
}
