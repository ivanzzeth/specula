package registry_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ivanzzeth/specula/internal/acl"
	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/handler/registry"
	"github.com/ivanzzeth/specula/internal/repo"
)

// ── test doubles ─────────────────────────────────────────────────────────────

// allowAuthz is an Authorizer that always allows and returns the given repo.
type allowAuthz struct {
	r *repo.Repo
}

func (a *allowAuthz) Authorize(_ context.Context, _ string, _ string) (*repo.Repo, error) {
	return a.r, nil
}

// denyAuthz is an Authorizer that always returns the provided error.
type denyAuthz struct{ err error }

func (d *denyAuthz) Authorize(_ context.Context, _ string, _ string) (*repo.Repo, error) {
	return nil, d.err
}

// memBlobStore is an in-memory BlobStore for tests.
type memBlobStore struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func newMemBlobStore() *memBlobStore { return &memBlobStore{blobs: make(map[string][]byte)} }

func (m *memBlobStore) Put(_ context.Context, digest string, r io.Reader, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.blobs[digest] = data
	m.mu.Unlock()
	return nil
}
func (m *memBlobStore) Get(_ context.Context, digest string, offset, length int64) (io.ReadCloser, int64, error) {
	m.mu.Lock()
	data, ok := m.blobs[digest]
	m.mu.Unlock()
	if !ok {
		return nil, 0, fmt.Errorf("blob not found: %s", digest)
	}
	if offset > 0 && offset < int64(len(data)) {
		data = data[offset:]
	}
	if length >= 0 && length < int64(len(data)) {
		data = data[:length]
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}
func (m *memBlobStore) Exists(_ context.Context, digest string) (bool, error) {
	m.mu.Lock()
	_, ok := m.blobs[digest]
	m.mu.Unlock()
	return ok, nil
}
func (m *memBlobStore) Delete(_ context.Context, digest string) error {
	m.mu.Lock()
	delete(m.blobs, digest)
	m.mu.Unlock()
	return nil
}
func (m *memBlobStore) UsageBytes(_ context.Context) (int64, error) { return 0, nil }
func (m *memBlobStore) has(digest string) bool {
	m.mu.Lock()
	_, ok := m.blobs[digest]
	m.mu.Unlock()
	return ok
}

// memRepoStore is a minimal in-memory RepoStore.
type memRepoStore struct {
	mu    sync.Mutex
	repos map[string]*repo.Repo // key: orgID+"/"+name
}

func newMemRepoStore() *memRepoStore { return &memRepoStore{repos: make(map[string]*repo.Repo)} }

func (s *memRepoStore) key(orgID, name string) string { return orgID + "/" + name }

func (s *memRepoStore) CreateRepo(_ context.Context, orgID, name, vis, ownerUserID string) (*repo.Repo, error) {
	r := &repo.Repo{
		ID:          "repo_" + orgID + "_" + name,
		OrgID:       orgID,
		Name:        name,
		Visibility:  repo.NormalizeVisibility(vis),
		OwnerUserID: ownerUserID,
	}
	s.mu.Lock()
	s.repos[s.key(orgID, name)] = r
	s.mu.Unlock()
	return r, nil
}
func (s *memRepoStore) GetRepo(_ context.Context, orgID, name string) (*repo.Repo, error) {
	s.mu.Lock()
	r, ok := s.repos[s.key(orgID, name)]
	s.mu.Unlock()
	if !ok {
		return nil, repo.ErrNotFound
	}
	return r, nil
}
func (s *memRepoStore) ListRepos(_ context.Context, orgID string) ([]*repo.Repo, error) {
	return nil, nil
}
func (s *memRepoStore) SetVisibility(_ context.Context, orgID, name, vis string) error { return nil }
func (s *memRepoStore) DeleteRepo(_ context.Context, orgID, name string) error {
	s.mu.Lock()
	delete(s.repos, s.key(orgID, name))
	s.mu.Unlock()
	return nil
}

// memTagStore is a minimal in-memory TagStore.
type memTagStore struct {
	mu   sync.Mutex
	tags map[string]*repo.Tag // key: repoID+":"+tag
}

func newMemTagStore() *memTagStore { return &memTagStore{tags: make(map[string]*repo.Tag)} }

func (s *memTagStore) key(repoID, tag string) string { return repoID + ":" + tag }

func (s *memTagStore) PutTag(_ context.Context, repoID, tag, digest string) error {
	t := &repo.Tag{RepoID: repoID, Tag: tag, Digest: digest}
	s.mu.Lock()
	s.tags[s.key(repoID, tag)] = t
	s.mu.Unlock()
	return nil
}
func (s *memTagStore) GetTag(_ context.Context, repoID, tag string) (*repo.Tag, error) {
	s.mu.Lock()
	t, ok := s.tags[s.key(repoID, tag)]
	s.mu.Unlock()
	if !ok {
		return nil, repo.ErrNotFound
	}
	return t, nil
}

// ListTags returns every tag belonging to repoID. The real store orders by tag
// name; callers that care about order sort explicitly, so the map iteration
// order here is deliberately left unsorted to keep them honest.
func (s *memTagStore) ListTags(_ context.Context, repoID string) ([]*repo.Tag, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*repo.Tag
	for _, t := range s.tags {
		if t.RepoID != repoID {
			continue
		}
		cp := *t
		out = append(out, &cp)
	}
	return out, nil
}
func (s *memTagStore) DeleteTag(_ context.Context, repoID, tag string) error {
	s.mu.Lock()
	delete(s.tags, s.key(repoID, tag))
	s.mu.Unlock()
	return nil
}

// nilCacheManager satisfies cache.CacheManager with no-op implementations.
// Tests that exercise only the push path do not need cache functionality.
type nilCacheManager struct{}

func (n *nilCacheManager) Lookup(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, nil
}
func (n *nilCacheManager) Store(_ context.Context, _ artifact.ArtifactRef, _ *artifact.Artifact) (*artifact.CacheEntry, error) {
	return nil, nil
}
func (n *nilCacheManager) Serve(_ context.Context, _ artifact.ArtifactRef, _, _ int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	return nil, nil, cache.ErrCacheMiss
}

// ── helpers ───────────────────────────────────────────────────────────────────

// sha256Digest computes "sha256:<hex>" for the given data.
func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// distError is the OCI Distribution error envelope returned by the handler.
type distError struct {
	Errors []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// parseDistError deserialises an OCI Distribution error response body.
func parseDistError(body []byte) distError {
	var e distError
	_ = json.Unmarshal(body, &e)
	return e
}

// testRepo returns a fixed *repo.Repo for use in allowAuthz.
func testRepo(orgID, name string) *repo.Repo {
	return &repo.Repo{
		ID:          "repo_test_" + orgID + "_" + strings.ReplaceAll(name, "/", "_"),
		OrgID:       orgID,
		Name:        name,
		Visibility:  repo.VisibilityPrivate,
		OwnerUserID: "user:owner1",
	}
}

// newTestHandler builds a Handler wired with the given stores; the CacheManager
// is a no-op (tests only exercise the push path).
func newTestHandler(blobs *memBlobStore, tags *memTagStore, authz registry.Authorizer) *registry.Handler {
	return registry.NewHandler(
		&nilCacheManager{},
		newMemRepoStore(),
		tags,
		authz,
		registry.WithBlobStore(blobs),
	)
}

// extractUploadUUID parses the upload UUID from a Location header such as
// "/v2/org1/myrepo/blobs/uploads/<uuid>".
func extractUploadUUID(location string) string {
	parts := strings.Split(location, "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestUploadSessionLifecycle exercises the full three-step chunked blob push:
//
//	POST  → open session (202 + Location)
//	PATCH → append chunk  (202 + updated Range)
//	PUT   → finalise      (201 + Docker-Content-Digest)
//
// and verifies the blob lands in the BlobStore.
func TestUploadSessionLifecycle(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()
	repoObj := testRepo("org1", "org1/myrepo")
	h := newTestHandler(blobs, tags, &allowAuthz{r: repoObj})

	// ── Step 1: POST – open session ──────────────────────────────────────────
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v2/org1/myrepo/blobs/uploads/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST: expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatal("POST: missing Location header")
	}
	if rec.Header().Get("Docker-Upload-UUID") == "" {
		t.Fatal("POST: missing Docker-Upload-UUID header")
	}

	uuid := extractUploadUUID(loc)
	if uuid == "" {
		t.Fatalf("POST: could not extract UUID from Location %q", loc)
	}

	// ── Step 2: PATCH – append chunk ─────────────────────────────────────────
	chunk := []byte("the quick brown fox jumps over the lazy dog")
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPatch, loc, bytes.NewReader(chunk))
	h.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusAccepted {
		t.Fatalf("PATCH: expected 202, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if rec2.Header().Get("Range") == "" {
		t.Fatal("PATCH: missing Range header")
	}

	// ── Step 3: PUT – finalise ────────────────────────────────────────────────
	digest := sha256Digest(chunk)
	putURL := fmt.Sprintf("%s?digest=%s", loc, digest)

	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPut, putURL, http.NoBody)
	h.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d: %s", rec3.Code, rec3.Body.String())
	}
	got := rec3.Header().Get("Docker-Content-Digest")
	if got != digest {
		t.Errorf("PUT: Docker-Content-Digest: got %q, want %q", got, digest)
	}
	if !blobs.has(digest) {
		t.Errorf("PUT: blob %s not in store after upload", digest)
	}
}

// TestMonolithicBlobUpload verifies that a single PUT (POST + PUT in one step,
// body in the finalise request) also works — the monolithic push path.
func TestMonolithicBlobUpload(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()
	repoObj := testRepo("org1", "org1/myrepo")
	h := newTestHandler(blobs, tags, &allowAuthz{r: repoObj})

	// Open session.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v2/org1/myrepo/blobs/uploads/", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST: expected 202, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")

	// PUT with full body (no prior PATCH).
	blob := []byte("monolithic blob content")
	digest := sha256Digest(blob)
	putURL := fmt.Sprintf("%s?digest=%s", loc, digest)

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPut, putURL, bytes.NewReader(blob))
	h.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusCreated {
		t.Fatalf("PUT: expected 201, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if !blobs.has(digest) {
		t.Error("blob not in store after monolithic upload")
	}
}

// TestUploadStatusReportsOffset verifies GET on an active session returns 204
// with a Range header reflecting the bytes already uploaded.
func TestUploadStatusReportsOffset(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()
	repoObj := testRepo("org1", "org1/myrepo")
	h := newTestHandler(blobs, tags, &allowAuthz{r: repoObj})

	// Open session.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v2/org1/myrepo/blobs/uploads/", nil))
	loc := rec.Header().Get("Location")

	// PATCH some bytes.
	chunk := bytes.Repeat([]byte("x"), 512)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPatch, loc, bytes.NewReader(chunk)))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("PATCH: expected 202, got %d", rec2.Code)
	}

	// GET upload status.
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, loc, nil))

	if rec3.Code != http.StatusNoContent {
		t.Fatalf("GET status: expected 204, got %d: %s", rec3.Code, rec3.Body.String())
	}
	rangeHdr := rec3.Header().Get("Range")
	if rangeHdr == "" {
		t.Fatal("GET status: missing Range header")
	}
	// Range should reflect 512 bytes: "0-511".
	if rangeHdr != "0-511" {
		t.Errorf("GET status: Range = %q, want %q", rangeHdr, "0-511")
	}
}

// TestDigestMismatchRejectsUpload verifies that PUT with a wrong declared digest
// returns 400 DIGEST_INVALID and does NOT store the blob.
func TestDigestMismatchRejectsUpload(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()
	repoObj := testRepo("org1", "org1/myrepo")
	h := newTestHandler(blobs, tags, &allowAuthz{r: repoObj})

	// Open session.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v2/org1/myrepo/blobs/uploads/", nil))
	loc := rec.Header().Get("Location")

	// PATCH real data.
	chunk := []byte("some real content")
	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPatch, loc, bytes.NewReader(chunk)))

	// PUT with a deliberately wrong digest.
	wrongDigest := "sha256:" + strings.Repeat("0", 64)
	putURL := fmt.Sprintf("%s?digest=%s", loc, wrongDigest)

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPut, putURL, http.NoBody))

	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec2.Code, rec2.Body.String())
	}
	e := parseDistError(rec2.Body.Bytes())
	if len(e.Errors) == 0 || e.Errors[0].Code != "DIGEST_INVALID" {
		t.Errorf("expected DIGEST_INVALID error, got %+v", e.Errors)
	}
	// Blob must NOT have been stored.
	if blobs.has(wrongDigest) {
		t.Error("mismatch: blob should not be in store after digest rejection")
	}
	// Session must have been cleaned up.
	uuid := extractUploadUUID(loc)
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, loc, nil))
	_ = uuid // consumed; check via GET returning 404
	if rec3.Code != http.StatusNotFound {
		t.Errorf("after digest mismatch, GET session expected 404, got %d", rec3.Code)
	}
}

// TestManifestPutCreatesTag verifies that PUT /v2/<name>/manifests/<tag>:
//  1. Returns 201 Created with Docker-Content-Digest.
//  2. Stores the manifest bytes in the BlobStore.
//  3. Writes the tag→digest pointer via TagStore.
func TestManifestPutCreatesTag(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()
	repoObj := testRepo("org1", "org1/myapp")
	h := newTestHandler(blobs, tags, &allowAuthz{r: repoObj})

	cfgDigest := "sha256:" + strings.Repeat("ab", 32)
	_ = blobs.Put(context.Background(), cfgDigest, bytes.NewReader([]byte("{}")), 2)

	manifest := []byte(`{
		"schemaVersion": 2,
		"mediaType": "application/vnd.docker.distribution.manifest.v2+json",
		"config": {"mediaType": "application/vnd.docker.container.image.v1+json","digest": "` + cfgDigest + `","size": 2},
		"layers": []
	}`)
	wantDigest := sha256Digest(manifest)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v2/org1/myapp/manifests/v1.2.3",
		bytes.NewReader(manifest))
	req.Header.Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	gotDigest := rec.Header().Get("Docker-Content-Digest")
	if gotDigest != wantDigest {
		t.Errorf("Docker-Content-Digest: got %q, want %q", gotDigest, wantDigest)
	}

	// Manifest blob must be in the blob store.
	if !blobs.has(wantDigest) {
		t.Errorf("manifest blob %s not found in blob store", wantDigest)
	}

	// Tag pointer must have been written.
	tag, err := tags.GetTag(context.Background(), repoObj.ID, "v1.2.3")
	if err != nil {
		t.Fatalf("tag v1.2.3 not found: %v", err)
	}
	if tag.Digest != wantDigest {
		t.Errorf("tag digest: got %q, want %q", tag.Digest, wantDigest)
	}
}

// TestManifestPutByDigestNoTag verifies that a by-digest push
// (PUT …/manifests/sha256:…) stores the blob but writes no tag.
func TestManifestPutByDigestNoTag(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()
	repoObj := testRepo("org1", "org1/myapp")
	h := newTestHandler(blobs, tags, &allowAuthz{r: repoObj})

	manifest := []byte(`{"schemaVersion":2}`)
	digest := sha256Digest(manifest)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		fmt.Sprintf("/v2/org1/myapp/manifests/%s", digest),
		bytes.NewReader(manifest))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if !blobs.has(digest) {
		t.Errorf("manifest blob not in store")
	}
	// No tag should have been written.
	tagList, _ := tags.ListTags(context.Background(), repoObj.ID)
	if len(tagList) != 0 {
		t.Errorf("expected no tags for by-digest push, got %d", len(tagList))
	}
}

// TestCrossOrgPushDenied verifies that a push to a repo whose Authorizer returns
// acl.ErrForbidden is rejected with 403 DENIED.
func TestCrossOrgPushDenied(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()

	// Authorizer that simulates a cross-org push denial.
	h := newTestHandler(blobs, tags, &denyAuthz{err: acl.ErrForbidden})

	manifest := []byte(`{"schemaVersion":2}`)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/v2/other-org/secret-repo/manifests/latest",
		bytes.NewReader(manifest))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	e := parseDistError(rec.Body.Bytes())
	if len(e.Errors) == 0 || e.Errors[0].Code != "DENIED" {
		t.Errorf("expected DENIED error, got %+v", e.Errors)
	}
}

// TestCrossOrgBlobUploadDenied verifies that the blob upload session endpoints
// also enforce the Authorizer for cross-org requests.
func TestCrossOrgBlobUploadDenied(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()
	h := newTestHandler(blobs, tags, &denyAuthz{err: acl.ErrForbidden})

	// POST – open session
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v2/other-org/repo/blobs/uploads/", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST: expected 403, got %d", rec.Code)
	}

	// PATCH – append chunk (fake uuid; auth check precedes session lookup)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPatch, "/v2/other-org/repo/blobs/uploads/fakeuuid", nil))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("PATCH: expected 403, got %d", rec2.Code)
	}

	// PUT – finalise
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest(http.MethodPut,
		"/v2/other-org/repo/blobs/uploads/fakeuuid?digest=sha256:"+strings.Repeat("a", 64), nil))
	if rec3.Code != http.StatusForbidden {
		t.Fatalf("PUT: expected 403, got %d", rec3.Code)
	}
}

// TestReadOnlyAccessDeniedOnPush verifies that a caller with read-only access
// (acl.ErrReadOnly) receives 403 on a push attempt.
func TestReadOnlyAccessDeniedOnPush(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()
	h := newTestHandler(blobs, tags, &denyAuthz{err: acl.ErrReadOnly})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v2/org1/repo/manifests/latest",
		bytes.NewReader([]byte(`{"schemaVersion":2}`)))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestDeleteManifestTag verifies DELETE /v2/<name>/manifests/<tag> removes the
// tag pointer from the TagStore.
func TestDeleteManifestTag(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()
	repoObj := testRepo("org1", "org1/myapp")
	h := newTestHandler(blobs, tags, &allowAuthz{r: repoObj})

	// Pre-populate a tag.
	_ = tags.PutTag(context.Background(), repoObj.ID, "v1", "sha256:"+strings.Repeat("a", 64))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v2/org1/myapp/manifests/v1", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	_, err := tags.GetTag(context.Background(), repoObj.ID, "v1")
	if err == nil {
		t.Error("expected tag to be deleted but it still exists")
	}
}

// TestDeleteBlobRemovesFromStore verifies DELETE /v2/<name>/blobs/<digest>
// removes the blob from the BlobStore.
func TestDeleteBlobRemovesFromStore(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()
	repoObj := testRepo("org1", "org1/myapp")
	h := newTestHandler(blobs, tags, &allowAuthz{r: repoObj})

	// Pre-populate a blob.
	digest := sha256Digest([]byte("some blob data"))
	_ = blobs.Put(context.Background(), digest, bytes.NewReader([]byte("some blob data")), 14)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/v2/org1/myapp/blobs/%s", digest), nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	if blobs.has(digest) {
		t.Error("blob should have been deleted but is still in store")
	}
}

// TestNoBlobStoreReturns501 verifies that push endpoints return 501 when no
// BlobStore is injected via WithBlobStore.
func TestNoBlobStoreReturns501(t *testing.T) {
	tags := newMemTagStore()
	repoObj := testRepo("org1", "org1/myapp")

	// Build handler WITHOUT WithBlobStore.
	h := registry.NewHandler(
		&nilCacheManager{},
		newMemRepoStore(),
		tags,
		&allowAuthz{r: repoObj},
		// deliberately omit registry.WithBlobStore
	)

	t.Run("manifest_put", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/v2/org1/myapp/manifests/latest",
			bytes.NewReader([]byte(`{"schemaVersion":2}`)))
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("expected 501, got %d", rec.Code)
		}
	})
}
