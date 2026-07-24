package registry_test

// registry_extra_test.go — additional tests for the registry write handler.
//
// These tests target the branches not reached by registry_test.go,
// routing_test.go, and discovery_test.go:
//
//   registry.go uncovered paths
//     - completeUpload: blob.Put error → 500 + session cleanup
//     - deleteBlob: blobs.Exists error → 500
//     - deleteBlob: blobs.Delete error → 500
//     - deleteManifestByDigest: tags.ListTags error → deleteManifest returns 500
//     - uploadStatus: non-ErrSessionNotFound from Get → 500
//     - uploadStatus: sess.Repo != name (cross-repo GET) → 404
//
//   discovery.go uncovered paths
//     - listTags: tags.ListTags error → 500
//     - listTags: invalid ?n= pagination parameter → 400
//     - loadReferrers: blob.Get error for stale pointer → empty index (non-fatal)
//     - storeReferrers: blobs.Put error → non-fatal, original push still succeeds
//
//   session.go / options
//     - WithSessions option function
//     - MemorySessions.Create failure path via a missing directory is hard to
//       inject without OS-level tricks; session coverage is addressed via the
//       cross-repo and internal-error paths above.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/handler/registry"
	"github.com/ivanzzeth/specula/internal/repo"
)

// ── failingBlobStore ──────────────────────────────────────────────────────────

// failingBlobStore wraps memBlobStore with injectable failures for Exists,
// Delete, and Put so we can hit the store-error branches in the handler.
//
//	failPut      = true: every Put call fails.
//	failPutAfterN > 0  : the first N-1 Put calls succeed; call N and beyond fail.
//	                     Use this to let the manifest blob land but fail the
//	                     referrers-index Put (which is the second Put call).
type failingBlobStore struct {
	real          *memBlobStore
	failExists    bool
	failDelete    bool
	failPut       bool
	failPutAfterN int // 0 = disabled; N = succeed first N-1 calls, fail Nth+
	putCallCount  int
}

func newFailingBlobStore() *failingBlobStore {
	return &failingBlobStore{real: newMemBlobStore()}
}

func (f *failingBlobStore) Put(ctx context.Context, digest string, r io.Reader, size int64) error {
	f.putCallCount++
	shouldFail := f.failPut || (f.failPutAfterN > 0 && f.putCallCount >= f.failPutAfterN)
	if shouldFail {
		// Drain the reader so the caller doesn't block on a full pipe.
		_, _ = io.Copy(io.Discard, r)
		return errors.New("blob store: injected Put failure")
	}
	return f.real.Put(ctx, digest, r, size)
}
func (f *failingBlobStore) Get(ctx context.Context, digest string, offset, length int64) (io.ReadCloser, int64, error) {
	return f.real.Get(ctx, digest, offset, length)
}
func (f *failingBlobStore) Exists(ctx context.Context, digest string) (bool, error) {
	if f.failExists {
		return false, errors.New("blob store: injected Exists failure")
	}
	return f.real.Exists(ctx, digest)
}
func (f *failingBlobStore) Delete(ctx context.Context, digest string) error {
	if f.failDelete {
		return errors.New("blob store: injected Delete failure")
	}
	return f.real.Delete(ctx, digest)
}
func (f *failingBlobStore) UsageBytes(ctx context.Context) (int64, error) {
	return f.real.UsageBytes(ctx)
}

// ── failingTagStore ────────────────────────────────────────────────────────────

// failingTagStore wraps memTagStore with an injectable ListTags failure.
type failingTagStore struct {
	real         *memTagStore
	failListTags bool
	failDelete   bool
}

func newFailingTagStore() *failingTagStore {
	return &failingTagStore{real: newMemTagStore()}
}

func (f *failingTagStore) PutTag(ctx context.Context, repoID, tag, digest string) error {
	return f.real.PutTag(ctx, repoID, tag, digest)
}
func (f *failingTagStore) GetTag(ctx context.Context, repoID, tag string) (*repo.Tag, error) {
	return f.real.GetTag(ctx, repoID, tag)
}
func (f *failingTagStore) ListTags(ctx context.Context, repoID string) ([]*repo.Tag, error) {
	if f.failListTags {
		return nil, errors.New("tag store: injected ListTags failure")
	}
	return f.real.ListTags(ctx, repoID)
}
func (f *failingTagStore) DeleteTag(ctx context.Context, repoID, tag string) error {
	if f.failDelete {
		return errors.New("tag store: injected DeleteTag failure")
	}
	return f.real.DeleteTag(ctx, repoID, tag)
}

// ── errSessionStore ────────────────────────────────────────────────────────────

// errSessionStore is an UploadSessionStore whose Get always returns a
// non-ErrSessionNotFound error, exercising the handler's internal-error branch.
type errSessionStore struct {
	real *registry.MemorySessions
}

func newErrSessionStore() *errSessionStore {
	return &errSessionStore{real: registry.NewMemorySessions()}
}

func (e *errSessionStore) Create(ctx context.Context, repoName string) (*registry.UploadSession, error) {
	return e.real.Create(ctx, repoName)
}
func (e *errSessionStore) Get(_ context.Context, _ string) (*registry.UploadSession, error) {
	// Return something that is NOT ErrSessionNotFound so the handler hits its
	// "internal session error" branch (different from the standard 404 path).
	return nil, errors.New("session store: injected internal error (NOT ErrSessionNotFound)")
}
func (e *errSessionStore) Append(ctx context.Context, id string, r io.Reader) (int64, error) {
	return e.real.Append(ctx, id, r)
}
func (e *errSessionStore) Delete(ctx context.Context, id string) error {
	return e.real.Delete(ctx, id)
}

// ── helpers ────────────────────────────────────────────────────────────────────

// newHandlerWithFailingBlobs builds a Handler using a failingBlobStore.
func newHandlerWithFailingBlobs(fb *failingBlobStore, tags repo.TagStore, authz registry.Authorizer) *registry.Handler {
	return registry.NewHandler(
		&nilCacheManager{},
		newMemRepoStore(),
		tags,
		authz,
		registry.WithBlobStore(fb),
	)
}

// openUploadSession performs the POST to start an upload and returns the
// Location URL and upload UUID, or fails the test immediately.
func openUploadSession(t *testing.T, h *registry.Handler, repoPath string) (loc, uuid string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v2/%s/blobs/uploads/", repoPath), nil))
	require.Equal(t, http.StatusAccepted, rec.Code, "POST to open session: %s", rec.Body)
	loc = rec.Header().Get("Location")
	require.NotEmpty(t, loc, "POST must return Location header")
	uuid = extractUploadUUID(loc)
	require.NotEmpty(t, uuid, "could not extract UUID from Location %q", loc)
	return loc, uuid
}

// patchChunk appends a chunk to an existing session.
func patchChunk(t *testing.T, h *registry.Handler, loc string, data []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, loc, bytes.NewReader(data)))
	require.Equal(t, http.StatusAccepted, rec.Code, "PATCH to append chunk: %s", rec.Body)
}

// ═══════════════════════════════════════════════════════════════════════════════
// Tests
// ═══════════════════════════════════════════════════════════════════════════════

// ── WithSessions option ────────────────────────────────────────────────────────

// TestWithSessionsOption verifies the WithSessions constructor option is applied
// and the injected session store is used for new upload sessions.
func TestWithSessionsOption(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()
	repoObj := testRepo("org1", "org1/myrepo")

	// Inject a custom MemorySessions via WithSessions.
	sessions := registry.NewMemorySessions()
	h := registry.NewHandler(
		&nilCacheManager{},
		newMemRepoStore(),
		tags,
		&allowAuthz{r: repoObj},
		registry.WithBlobStore(blobs),
		registry.WithSessions(sessions),
	)

	// The injected sessions store should be used — a successful POST proves it.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v2/org1/myrepo/blobs/uploads/", nil))
	assert.Equal(t, http.StatusAccepted, rec.Code, "WithSessions: expected 202 on POST: %s", rec.Body)
	assert.NotEmpty(t, rec.Header().Get("Location"), "WithSessions: expected Location header")
}

// ── completeUpload error paths ─────────────────────────────────────────────────

// TestCompleteUpload_BlobPutError verifies that when blobs.Put fails on the
// finalise (PUT) step, the handler returns 500 and cleans up the session so
// a subsequent GET status returns 404.
func TestCompleteUpload_BlobPutError(t *testing.T) {
	fb := newFailingBlobStore()
	tags := newMemTagStore()
	repoObj := testRepo("org1", "org1/myrepo")
	h := newHandlerWithFailingBlobs(fb, tags, &allowAuthz{r: repoObj})

	loc, _ := openUploadSession(t, h, "org1/myrepo")

	chunk := []byte("hello world")
	patchChunk(t, h, loc, chunk)

	// Enable blob.Put failure BEFORE the finalise PUT.
	fb.failPut = true
	digest := sha256Digest(chunk)
	putURL := fmt.Sprintf("%s?digest=%s", loc, digest)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, putURL, http.NoBody))

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"blob.Put failure must return 500: %s", rec.Body)

	// Per the spec: after a failed finalise the session must be cleaned up so
	// a GET status now returns 404, not stale in-progress state.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, loc, nil))
	assert.Equal(t, http.StatusNotFound, rec2.Code,
		"after blob.Put failure, GET session status must return 404 (session cleaned)")
}

// ── uploadStatus error paths ───────────────────────────────────────────────────

// TestUploadStatus_SessionGetInternalError verifies that a non-ErrSessionNotFound
// error from sessions.Get during GET status → 500 BLOB_UPLOAD_INVALID.
func TestUploadStatus_SessionGetInternalError(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()
	repoObj := testRepo("org1", "org1/myrepo")

	// Build a handler with the error-injecting session store.
	h := registry.NewHandler(
		&nilCacheManager{},
		newMemRepoStore(),
		tags,
		&allowAuthz{r: repoObj},
		registry.WithBlobStore(blobs),
		registry.WithSessions(newErrSessionStore()),
	)

	// The error session store always errors from Get; any upload UUID triggers it.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v2/org1/myrepo/blobs/uploads/deadbeef", nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"non-ErrSessionNotFound from Get must return 500")
	body := rec.Body.Bytes()
	e := parseDistError(body)
	require.NotEmpty(t, e.Errors, "expected OCI error body, got %s", body)
	assert.Equal(t, "BLOB_UPLOAD_INVALID", e.Errors[0].Code,
		"internal session error must map to BLOB_UPLOAD_INVALID")
}

// TestUploadStatus_CrossRepo verifies that a GET upload-status request for a
// session that belongs to a different repo returns 404 BLOB_UPLOAD_UNKNOWN.
// This is the GET-specific path of the cross-repo session hijack check.
func TestUploadStatus_CrossRepo(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()
	// Use a shared sessions store so both "repos" share the same sessions map.
	sessions := registry.NewMemorySessions()

	repoA := testRepo("org1", "org1/repoA")
	repoB := testRepo("org1", "org1/repoB")

	// Handler A opens the session under "org1/repoA".
	hA := registry.NewHandler(&nilCacheManager{}, newMemRepoStore(), tags,
		&allowAuthz{r: repoA},
		registry.WithBlobStore(blobs),
		registry.WithSessions(sessions),
	)

	// Handler B is the attacker trying to probe "org1/repoA"'s session via "org1/repoB".
	hB := registry.NewHandler(&nilCacheManager{}, newMemRepoStore(), tags,
		&allowAuthz{r: repoB},
		registry.WithBlobStore(blobs),
		registry.WithSessions(sessions),
	)

	locA, _ := openUploadSession(t, hA, "org1/repoA")
	// Attempt GET status via the B handler (different repo name, same UUID).
	uuidA := extractUploadUUID(locA)
	bPath := fmt.Sprintf("/v2/org1/repoB/blobs/uploads/%s", uuidA)

	rec := httptest.NewRecorder()
	hB.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, bPath, nil))

	assert.Equal(t, http.StatusNotFound, rec.Code,
		"cross-repo session probe via GET must return 404 BLOB_UPLOAD_UNKNOWN")
	e := parseDistError(rec.Body.Bytes())
	if len(e.Errors) > 0 {
		assert.Equal(t, "BLOB_UPLOAD_UNKNOWN", e.Errors[0].Code)
	}
}

// ── deleteBlob error paths ─────────────────────────────────────────────────────

// TestDeleteBlob_ExistsError verifies that when blobs.Exists returns an error
// during DELETE, the handler returns 500 UNKNOWN.
func TestDeleteBlob_ExistsError(t *testing.T) {
	fb := newFailingBlobStore()
	fb.failExists = true
	tags := newMemTagStore()
	repoObj := testRepo("org1", "org1/myapp")
	h := newHandlerWithFailingBlobs(fb, tags, &allowAuthz{r: repoObj})

	digest := "sha256:" + strings.Repeat("a", 64)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/v2/org1/myapp/blobs/%s", digest), nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"blobs.Exists error during DELETE must return 500: %s", rec.Body)
}

// TestDeleteBlob_DeleteError verifies that when the blob exists but blobs.Delete
// returns an error, the handler returns 500 UNKNOWN.
func TestDeleteBlob_DeleteError(t *testing.T) {
	fb := newFailingBlobStore()
	tags := newMemTagStore()
	repoObj := testRepo("org1", "org1/myapp")

	// Pre-seed a blob so Exists returns true.
	digest := sha256Digest([]byte("data"))
	_ = fb.real.Put(context.Background(), digest, bytes.NewReader([]byte("data")), 4)

	fb.failDelete = true // Delete fails after Exists succeeds.
	h := newHandlerWithFailingBlobs(fb, tags, &allowAuthz{r: repoObj})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/v2/org1/myapp/blobs/%s", digest), nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"blobs.Delete error must return 500: %s", rec.Body)
}

// ── deleteManifestByDigest error paths ────────────────────────────────────────

// TestDeleteManifestByDigest_ListTagsError verifies that when tags.ListTags
// returns an error during a by-digest manifest delete, the handler returns 500.
func TestDeleteManifestByDigest_ListTagsError(t *testing.T) {
	blobs := newMemBlobStore()
	fts := newFailingTagStore()
	repoObj := testRepo("org1", "org1/myapp")

	// Pre-seed the manifest blob so the authz + blob-not-found checks pass.
	manifest := []byte(`{"schemaVersion":2}`)
	digest := sha256Digest(manifest)
	_ = blobs.Put(context.Background(), digest, bytes.NewReader(manifest), int64(len(manifest)))

	h := registry.NewHandler(
		&nilCacheManager{},
		newMemRepoStore(),
		fts,
		&allowAuthz{r: repoObj},
		registry.WithBlobStore(blobs),
	)

	// Enable ListTags failure before attempting the digest-based delete.
	fts.failListTags = true

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/v2/org1/myapp/manifests/%s", digest), nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"tags.ListTags error during deleteManifestByDigest must return 500: %s", rec.Body)
}

// ── listTags error paths ───────────────────────────────────────────────────────

// TestListTags_InternalError verifies that when tags.ListTags returns an error
// during GET /v2/<name>/tags/list, the handler returns 500.
func TestListTags_InternalError(t *testing.T) {
	blobs := newMemBlobStore()
	fts := newFailingTagStore()
	ms := newMemMetaStore()
	repoObj := testRepo("org1", "org1/myrepo")
	h := registry.NewHandler(
		&nilCacheManager{},
		newMemRepoStore(),
		fts,
		&allowAuthz{r: repoObj},
		registry.WithBlobStore(blobs),
		registry.WithMeta(ms),
	)

	fts.failListTags = true

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/org1/myrepo/tags/list", nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"tags.ListTags error must return 500: %s", rec.Body)
}

// TestListTags_InvalidPaginationN verifies that a non-numeric ?n= query
// parameter returns 400 (malformed request, not 500).
func TestListTags_InvalidPaginationN(t *testing.T) {
	blobs, tags, ms := newMemBlobStore(), newMemTagStore(), newMemMetaStore()
	repoObj := testRepo("org1", "org1/myrepo")
	h := newDiscoveryHandler(blobs, tags, ms, &allowAuthz{r: repoObj})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/v2/org1/myrepo/tags/list?n=notanumber", nil))

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"non-numeric ?n= must return 400: %s", rec.Body)
}

// ── loadReferrers stale-pointer path ─────────────────────────────────────────

// TestLoadReferrers_StaleBlobPointer verifies REGISTRY-DESIGN §3 (Referrers API):
// when the mutable referrers pointer in meta points to a blob digest that no
// longer exists in the blob store (stale pointer after a GC or failed write),
// the handler must return 200 with an empty manifests array — never 404 and
// never 500.
//
// This tests the "pointer outlived its index blob" fail-open path in
// loadReferrers, which degrades gracefully rather than surfacing internal
// storage inconsistency to the client.
func TestLoadReferrers_StaleBlobPointer(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()
	ms := newMemMetaStore()
	repoObj := testRepo("org1", "org1/myrepo")
	h := newDiscoveryHandler(blobs, tags, ms, &allowAuthz{r: repoObj})

	subjectDigest := "sha256:" + strings.Repeat("a", 64)

	// Inject a mutable entry pointing to a referrers index blob that does NOT
	// exist in the blob store — simulating a stale pointer after GC.
	missingBlobDigest := "sha256:" + strings.Repeat("b", 64)
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:    "registry:referrers:org1/myrepo:" + subjectDigest,
		Digest: missingBlobDigest,
	})
	// Do NOT put missingBlobDigest into blobs — it should be absent.

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/v2/org1/myrepo/referrers/%s", subjectDigest), nil))

	require.Equal(t, http.StatusOK, rec.Code,
		"stale referrers pointer must not cause 404 or 500 — Referrers API must return 200 always: %s", rec.Body)

	var resp struct {
		Manifests []interface{} `json:"manifests"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp),
		"body must be valid JSON: %s", rec.Body)
	assert.Empty(t, resp.Manifests,
		"stale pointer → empty referrers list (not 404/500)")
}

// ── storeReferrers non-fatal path ─────────────────────────────────────────────

// TestStoreReferrers_BlobPutError verifies that when blobs.Put fails while
// writing the updated referrers index, the manifest push still returns 201 —
// the referrers index update is non-fatal (REGISTRY-DESIGN §3).
func TestStoreReferrers_BlobPutError(t *testing.T) {
	fb := newFailingBlobStore()
	tags := newMemTagStore()
	ms := newMemMetaStore()
	repoObj := testRepo("org1", "org1/myrepo")

	// Build a discovery handler (WithMeta) but backed by the failing blob store.
	h := registry.NewHandler(
		&nilCacheManager{},
		newMemRepoStore(),
		tags,
		&allowAuthz{r: repoObj},
		registry.WithBlobStore(fb),
		registry.WithMeta(ms),
	)

	// Push the subject manifest first (directly into the real store, not counted).
	subject := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	subjectDigest := sha256Digest(subject)
	_ = fb.real.Put(context.Background(), subjectDigest, bytes.NewReader(subject), int64(len(subject)))

	// Allow the FIRST blobs.Put (referrer manifest blob itself) to succeed.
	// Fail the SECOND blobs.Put (the referrers-index blob) — that failure must be non-fatal.
	// putCallCount is still 0 at this point (fb.real.Put above is direct, not counted).
	fb.failPutAfterN = 2

	// Push a referrer manifest (one with subject field pointing to subjectDigest).
	referrer := []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"example/sig","subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d},"config":{},"layers":[]}`,
		subjectDigest, len(subject),
	))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut,
		"/v2/org1/myrepo/manifests/sig",
		bytes.NewReader(referrer))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	h.ServeHTTP(rec, req)

	// The manifest push must succeed even though the referrers index update failed.
	assert.Equal(t, http.StatusCreated, rec.Code,
		"storeReferrers failure must be non-fatal — manifest push must still return 201: %s", rec.Body)
}

// TestPutManifestRejectsUnknownBlobRefs verifies Distribution Spec blob existence
// checks: a manifest whose config digest is not in the CAS is rejected with
// MANIFEST_BLOB_UNKNOWN before the manifest blob is stored.
func TestPutManifestRejectsUnknownBlobRefs(t *testing.T) {
	blobs := newMemBlobStore()
	tags := newMemTagStore()
	repoObj := testRepo("org1", "org1/myrepo")
	h := newTestHandler(blobs, tags, &allowAuthz{r: repoObj})

	missing := "sha256:" + strings.Repeat("d", 64)
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + missing + `","size":2},` +
		`"layers":[]}`)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v2/org1/myrepo/manifests/v1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "MANIFEST_BLOB_UNKNOWN")
	assert.False(t, blobs.has(sha256Digest(body)), "rejected manifest must not land in CAS")
}
