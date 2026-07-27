package registry_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ivanzzeth/specula/internal/handler/registry"
	"github.com/ivanzzeth/specula/internal/store/local"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
)

// A blob push is a stateful three-request protocol, and behind a Service or an
// Ingress with replicas > 1 those three requests land wherever the load balancer
// puts them. With per-process session state the PATCH/PUT hit a replica that
// never saw the POST and the push dies with BLOB_UPLOAD_UNKNOWN — reported from
// ACK: crane push over a public Ingress failed at the blob stage, and scaling the
// Deployment to one replica made the identical push succeed. Cookie affinity does
// not help, because crane and docker do not carry the Ingress cookie.
//
// These tests build SEVERAL handlers over ONE shared substrate — a real sqlite
// metadata store and a real on-disk blob store, the same topology as postgres +
// S3 in HA — and route each request of a push to a different one.

// replicas builds n handlers that share one session store, the way n Pods share
// postgres and S3.
func replicas(t *testing.T, n int) []*registry.Handler {
	t.Helper()
	h, _ := replicasAndBlobs(t, n)
	return h
}

// replicasAndBlobs is replicas plus the shared blob store, for assertions about
// what is actually in the CAS after a push.
func replicasAndBlobs(t *testing.T, n int) ([]*registry.Handler, *local.LocalDiskDriver) {
	t.Helper()
	dir := t.TempDir()

	metaStore, err := sqlite.NewSQLiteStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = metaStore.Close() })

	// One blob root for every replica: in HA this is the same S3 bucket.
	blobs := local.NewLocalDiskDriver(filepath.Join(dir, "blobs"))

	out := make([]*registry.Handler, 0, n)
	for i := range n {
		scratch := filepath.Join(dir, fmt.Sprintf("replica%d", i))
		// Per-replica scratch, deliberately NOT shared: if the implementation
		// leaks a dependency on local disk between requests, these tests fail.
		if err := os.MkdirAll(scratch, 0o755); err != nil {
			t.Fatalf("scratch dir: %v", err)
		}
		sessions := registry.NewSharedSessions(metaStore, blobs, scratch, nil)
		out = append(out, registry.NewHandler(
			&nilCacheManager{},
			newMemRepoStore(),
			newMemTagStore(),
			&allowAuthz{r: testRepo("org1", "myrepo")},
			registry.WithBlobStore(blobs),
			registry.WithMeta(metaStore),
			registry.WithSessions(sessions),
		))
	}
	return out, blobs
}

func do(h *registry.Handler, method, target string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// The exact reported failure: POST on one replica, PATCH on a second, PUT on a
// third.
func TestChunkedPushSurvivesADifferentReplicaPerRequest(t *testing.T) {
	r := replicas(t, 3)
	const name = "org1/myrepo"
	blob := []byte("specula multi-replica blob payload, pushed across three Pods")
	digest := sha256Of(blob)

	post := do(r[0], http.MethodPost, "/v2/"+name+"/blobs/uploads/", nil, nil)
	if post.Code != http.StatusAccepted {
		t.Fatalf("POST = %d, want 202", post.Code)
	}
	uuid := extractUploadUUID(post.Header().Get("Location"))
	if uuid == "" {
		t.Fatal("POST returned no upload UUID")
	}

	half := len(blob) / 2
	patch := do(r[1], http.MethodPatch, "/v2/"+name+"/blobs/uploads/"+uuid,
		bytes.NewReader(blob[:half]),
		map[string]string{"Content-Range": fmt.Sprintf("0-%d", half-1)})
	if patch.Code != http.StatusAccepted {
		t.Fatalf("PATCH on a second replica = %d, want 202 (body %s)", patch.Code, patch.Body.String())
	}
	if got := patch.Header().Get("Range"); got != fmt.Sprintf("0-%d", half-1) {
		t.Errorf("PATCH Range = %q, want 0-%d", got, half-1)
	}

	put := do(r[2], http.MethodPut, "/v2/"+name+"/blobs/uploads/"+uuid+"?digest="+digest,
		bytes.NewReader(blob[half:]),
		map[string]string{"Content-Range": fmt.Sprintf("%d-%d", half, len(blob)-1)})
	if put.Code != http.StatusCreated {
		t.Fatalf("PUT on a third replica = %d, want 201 (body %s)", put.Code, put.Body.String())
	}
	if got := put.Header().Get("Docker-Content-Digest"); got != digest {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, digest)
	}
}

// crane's usual shape: POST for a location, then one PUT carrying the whole blob.
func TestMonolithicPushSurvivesADifferentReplicaPerRequest(t *testing.T) {
	r := replicas(t, 2)
	const name = "org1/myrepo"
	blob := []byte("monolithic single-PUT blob")
	digest := sha256Of(blob)

	post := do(r[0], http.MethodPost, "/v2/"+name+"/blobs/uploads/", nil, nil)
	if post.Code != http.StatusAccepted {
		t.Fatalf("POST = %d", post.Code)
	}
	uuid := extractUploadUUID(post.Header().Get("Location"))

	put := do(r[1], http.MethodPut, "/v2/"+name+"/blobs/uploads/"+uuid+"?digest="+digest,
		bytes.NewReader(blob), nil)
	if put.Code != http.StatusCreated {
		t.Fatalf("PUT on the other replica = %d, want 201 (body %s)", put.Code, put.Body.String())
	}
}

// Many small chunks, each on a different replica, must reassemble in order: a
// concatenation bug would surface as DIGEST_INVALID rather than corruption.
func TestManyChunksRoundRobinAcrossReplicasReassembleInOrder(t *testing.T) {
	r := replicas(t, 3)
	const name = "org1/myrepo"

	var full []byte
	chunks := [][]byte{}
	for i := range 7 {
		c := []byte(fmt.Sprintf("chunk-%d-%s|", i, bytes.Repeat([]byte("x"), i*13)))
		chunks = append(chunks, c)
		full = append(full, c...)
	}
	digest := sha256Of(full)

	post := do(r[0], http.MethodPost, "/v2/"+name+"/blobs/uploads/", nil, nil)
	uuid := extractUploadUUID(post.Header().Get("Location"))

	var offset int
	for i, c := range chunks {
		h := r[(i+1)%len(r)] // never the replica that opened the session
		rec := do(h, http.MethodPatch, "/v2/"+name+"/blobs/uploads/"+uuid,
			bytes.NewReader(c),
			map[string]string{"Content-Range": fmt.Sprintf("%d-%d", offset, offset+len(c)-1)})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("chunk %d on replica %d = %d (body %s)", i, (i+1)%len(r), rec.Code, rec.Body.String())
		}
		offset += len(c)
		if want := fmt.Sprintf("0-%d", offset-1); rec.Header().Get("Range") != want {
			t.Fatalf("chunk %d Range = %q, want %q", i, rec.Header().Get("Range"), want)
		}
	}

	put := do(r[2], http.MethodPut, "/v2/"+name+"/blobs/uploads/"+uuid+"?digest="+digest, nil, nil)
	if put.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201 (body %s)", put.Code, put.Body.String())
	}
}

// The premise: with the in-memory store these very requests fail. Without this,
// the tests above could pass for reasons unrelated to sharing.
func TestMemorySessionsStillFailAcrossProcesses(t *testing.T) {
	const name = "org1/myrepo"
	blob := []byte("payload")

	// Two handlers, each with its own MemorySessions — two Pods, no shared state.
	a := newTestHandler(newMemBlobStore(), newMemTagStore(), &allowAuthz{r: testRepo("org1", "myrepo")})
	b := newTestHandler(newMemBlobStore(), newMemTagStore(), &allowAuthz{r: testRepo("org1", "myrepo")})

	post := do(a, http.MethodPost, "/v2/"+name+"/blobs/uploads/", nil, nil)
	uuid := extractUploadUUID(post.Header().Get("Location"))

	put := do(b, http.MethodPut, "/v2/"+name+"/blobs/uploads/"+uuid+"?digest="+sha256Of(blob),
		bytes.NewReader(blob), nil)
	if put.Code != http.StatusNotFound {
		t.Fatalf("PUT on a replica with process-local sessions = %d, want 404", put.Code)
	}
	if !bytes.Contains(put.Body.Bytes(), []byte("BLOB_UPLOAD_UNKNOWN")) {
		t.Errorf("expected BLOB_UPLOAD_UNKNOWN, got %s", put.Body.String())
	}
}

// A digest that does not match the reassembled content must still be rejected —
// sharing the session must not weaken verification.
func TestDigestMismatchIsStillRejectedAcrossReplicas(t *testing.T) {
	r := replicas(t, 2)
	const name = "org1/myrepo"

	post := do(r[0], http.MethodPost, "/v2/"+name+"/blobs/uploads/", nil, nil)
	uuid := extractUploadUUID(post.Header().Get("Location"))

	put := do(r[1], http.MethodPut, "/v2/"+name+"/blobs/uploads/"+uuid+"?digest="+sha256Of([]byte("something else")),
		bytes.NewReader([]byte("actual content")), nil)
	if put.Code != http.StatusBadRequest {
		t.Fatalf("PUT with a wrong digest = %d, want 400 (body %s)", put.Code, put.Body.String())
	}
	if !bytes.Contains(put.Body.Bytes(), []byte("DIGEST_INVALID")) {
		t.Errorf("want DIGEST_INVALID, got %s", put.Body.String())
	}
}

// Cross-repo session reuse must stay refused however the requests are routed.
func TestSessionCannotBeHijackedByAnotherRepoOnAnotherReplica(t *testing.T) {
	r := replicas(t, 2)

	post := do(r[0], http.MethodPost, "/v2/org1/myrepo/blobs/uploads/", nil, nil)
	uuid := extractUploadUUID(post.Header().Get("Location"))

	blob := []byte("x")
	put := do(r[1], http.MethodPut, "/v2/org2/otherrepo/blobs/uploads/"+uuid+"?digest="+sha256Of(blob),
		bytes.NewReader(blob), nil)
	if put.Code != http.StatusNotFound {
		t.Fatalf("cross-repo finalise = %d, want 404", put.Code)
	}
}

// An unknown UUID is a 404 on every replica, not a 500.
func TestUnknownUploadIDIs404OnTheSharedStore(t *testing.T) {
	r := replicas(t, 1)
	rec := do(r[0], http.MethodPatch, "/v2/org1/myrepo/blobs/uploads/deadbeefdeadbeef",
		bytes.NewReader([]byte("x")), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PATCH unknown uuid = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

// Delete must remove the staged chunks, or an aborted push would pin bytes in
// the blob store forever.
func TestDeleteRemovesStagedChunks(t *testing.T) {
	dir := t.TempDir()
	metaStore, err := sqlite.NewSQLiteStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = metaStore.Close() })
	blobs := local.NewLocalDiskDriver(filepath.Join(dir, "blobs"))
	s := registry.NewSharedSessions(metaStore, blobs, dir, nil)

	ctx := context.Background()
	sess, err := s.Create(ctx, "org1/myrepo")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Append(ctx, sess.ID, bytes.NewReader([]byte("staged bytes"))); err != nil {
		t.Fatalf("append: %v", err)
	}

	before, err := blobs.UsageBytes(ctx)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if before == 0 {
		t.Fatal("staged chunk did not reach the blob store")
	}

	if err := s.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	after, err := blobs.UsageBytes(ctx)
	if err != nil {
		t.Fatalf("usage after delete: %v", err)
	}
	if after != 0 {
		t.Errorf("staged bytes still present after Delete: %d", after)
	}
	if _, err := s.Get(ctx, sess.ID); err == nil {
		t.Error("session still readable after Delete")
	}
}

// Reading back what several Appends staged must return them concatenated.
func TestOpenConcatenatesChunksInOrder(t *testing.T) {
	dir := t.TempDir()
	metaStore, err := sqlite.NewSQLiteStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = metaStore.Close() })
	s := registry.NewSharedSessions(metaStore, local.NewLocalDiskDriver(filepath.Join(dir, "blobs")), dir, nil)

	ctx := context.Background()
	sess, err := s.Create(ctx, "org1/myrepo")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	parts := []string{"alpha", "-beta", "-gamma"}
	for _, p := range parts {
		if _, err := s.Append(ctx, sess.ID, bytes.NewReader([]byte(p))); err != nil {
			t.Fatalf("append %q: %v", p, err)
		}
	}

	rc, err := s.Open(ctx, sess.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := "alpha-beta-gamma"; string(got) != want {
		t.Errorf("Open = %q, want %q", got, want)
	}

	// Offset must agree with the bytes actually staged.
	after, err := s.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Offset != int64(len("alpha-beta-gamma")) {
		t.Errorf("offset = %d, want %d", after.Offset, len("alpha-beta-gamma"))
	}
}

// An empty body must not create a staging object: the closing PUT of a chunked
// push has none, and an empty chunk is one more object to read and delete.
func TestAppendOfAnEmptyBodyStagesNothing(t *testing.T) {
	dir := t.TempDir()
	metaStore, err := sqlite.NewSQLiteStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = metaStore.Close() })
	blobs := local.NewLocalDiskDriver(filepath.Join(dir, "blobs"))
	s := registry.NewSharedSessions(metaStore, blobs, dir, nil)

	ctx := context.Background()
	sess, _ := s.Create(ctx, "org1/myrepo")
	off, err := s.Append(ctx, sess.ID, bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("append empty: %v", err)
	}
	if off != 0 {
		t.Errorf("offset after empty append = %d, want 0", off)
	}
	if used, _ := blobs.UsageBytes(ctx); used != 0 {
		t.Errorf("empty append staged %d bytes", used)
	}
}

// The subtle case the Complete/Delete split exists for. Staged chunks are
// content-addressed in the CAS, so a monolithic push's single staged chunk has
// the SAME digest as the finished blob. If finishing the session removed staged
// chunks blindly, the push would return 201 and then the blob would be gone —
// the pull that followed would 404 with nothing in the logs tying it to cleanup.
func TestPromotedBlobSurvivesSessionCleanup(t *testing.T) {
	r, blobs := replicasAndBlobs(t, 2)
	const name = "org1/myrepo"
	blob := []byte("this blob is staged and promoted under one and the same digest")
	digest := sha256Of(blob)

	post := do(r[0], http.MethodPost, "/v2/"+name+"/blobs/uploads/", nil, nil)
	uuid := extractUploadUUID(post.Header().Get("Location"))
	put := do(r[1], http.MethodPut, "/v2/"+name+"/blobs/uploads/"+uuid+"?digest="+digest,
		bytes.NewReader(blob), nil)
	if put.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (body %s)", put.Code, put.Body.String())
	}

	// Assert against the CAS itself. The read path in this harness goes through a
	// no-op CacheManager, so a 404 there would prove nothing either way; what
	// matters is whether cleanup removed the object.
	ctx := context.Background()
	ok, err := blobs.Exists(ctx, digest)
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if !ok {
		t.Fatal("session cleanup evicted the blob that was just pushed")
	}
	rc, size, err := blobs.Get(ctx, digest, 0, -1)
	if err != nil {
		t.Fatalf("get promoted blob: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read promoted blob: %v", err)
	}
	if size != int64(len(blob)) || !bytes.Equal(got, blob) {
		t.Errorf("promoted blob = %q (size %d), want %q", got, size, blob)
	}
}

// A chunked push's intermediate chunks are NOT the final blob, so they must be
// reclaimed — otherwise every chunked push would leave a second copy of the layer
// in the store forever.
func TestChunkedPushDoesNotLeaveStagedCopiesBehind(t *testing.T) {
	dir := t.TempDir()
	metaStore, err := sqlite.NewSQLiteStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = metaStore.Close() })
	blobs := local.NewLocalDiskDriver(filepath.Join(dir, "blobs"))
	s := registry.NewSharedSessions(metaStore, blobs, dir, nil)

	ctx := context.Background()
	sess, _ := s.Create(ctx, "org1/myrepo")
	for _, part := range []string{"first-half-of-the-layer", "second-half-of-the-layer"} {
		if _, err := s.Append(ctx, sess.ID, bytes.NewReader([]byte(part))); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	staged, _ := blobs.UsageBytes(ctx)
	if staged == 0 {
		t.Fatal("chunks were not staged")
	}

	// Promote under the digest of the whole content, as the handler does...
	full := []byte("first-half-of-the-layersecond-half-of-the-layer")
	if err := blobs.Put(ctx, sha256Of(full), bytes.NewReader(full), int64(len(full))); err != nil {
		t.Fatalf("promote: %v", err)
	}
	// ...then complete, naming what was promoted.
	if err := s.Complete(ctx, sess.ID, sha256Of(full)); err != nil {
		t.Fatalf("complete: %v", err)
	}

	after, _ := blobs.UsageBytes(ctx)
	if after != int64(len(full)) {
		t.Errorf("after cleanup the store holds %d bytes, want just the %d-byte blob — staged chunks leaked",
			after, len(full))
	}
	if ok, _ := blobs.Exists(ctx, sha256Of(full)); !ok {
		t.Error("promoted blob was removed by cleanup")
	}
}

// A chunk whose bytes were already in the store belongs to whoever put them
// there; cleanup must not evict it.
func TestCleanupKeepsAChunkThatAlreadyExisted(t *testing.T) {
	dir := t.TempDir()
	metaStore, err := sqlite.NewSQLiteStore(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = metaStore.Close() })
	blobs := local.NewLocalDiskDriver(filepath.Join(dir, "blobs"))

	ctx := context.Background()
	// Someone else's blob, already cached.
	shared := []byte("bytes that were already in the cache")
	if err := blobs.Put(ctx, sha256Of(shared), bytes.NewReader(shared), int64(len(shared))); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := registry.NewSharedSessions(metaStore, blobs, dir, nil)
	sess, _ := s.Create(ctx, "org1/myrepo")
	if _, err := s.Append(ctx, sess.ID, bytes.NewReader(shared)); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Abandon the upload entirely.
	if err := s.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if ok, _ := blobs.Exists(ctx, sha256Of(shared)); !ok {
		t.Error("abandoning an upload evicted a blob that existed before it")
	}
}
