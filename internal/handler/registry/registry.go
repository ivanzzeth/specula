// Package registry implements the WRITE half of the OCI Distribution v2 API for
// Specula's hosted, multi-tenant private registry — the push path that the
// read-only pull-through handler in internal/handler/oci does not cover.
//
// Endpoints (REGISTRY-DESIGN §1):
//
//	POST   /v2/<name>/blobs/uploads/               → open an upload session (202 + Location)
//	PATCH  /v2/<name>/blobs/uploads/<uuid>          → append a chunk (Content-Range)
//	PUT    /v2/<name>/blobs/uploads/<uuid>?digest=  → finalise: verify digest, land in CAS
//	PUT    /v2/<name>/manifests/<ref>               → push a manifest, set tag→digest
//	DELETE /v2/<name>/manifests|blobs/<ref>         → delete (org admin)
//
// # Push → hosted, shared CAS
//
// A push writes blobs and manifests into the SAME content-addressed store the
// pull cache uses (blob dedup by digest is automatic). What makes the result
// "hosted" rather than "cached" is the repo/tag metadata this handler records
// via repo.RepoStore + repo.TagStore: an org-owned repo row and its tag→digest
// pointers. Hosted content is authoritative and is never GC-evicted (see the
// hosted-lifecycle note in oci.HostedResolver).
//
// # Authorization
//
// Every request first passes the registrytoken /v2/ Bearer challenge middleware
// (token scope check). The Authorizer here is the data-plane's own
// defence-in-depth chokepoint: it re-resolves the hosted repo and confirms the
// caller may pull/push it via acl.CanAccess, and it is where a first push
// lazily creates the org-owned repo row.
package registry

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/acl"
	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/digestutil"
	"github.com/ivanzzeth/specula/internal/repo"
	blobstore "github.com/ivanzzeth/specula/internal/store/blob"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// Registry scope actions (OCI Distribution token scope grammar). The write
// handler asks the Authorizer for the action the request actually needs so the
// data-plane chokepoint checks the SAME action the token was scoped for
// (a delete request carries a "delete" grant, not "push").
const (
	actionPull   = "pull"
	actionPush   = "push"
	actionDelete = "delete"
)

// maxManifestBytes bounds a manifest push body. OCI Distribution §"Pushing
// Manifests" recommends registries reject manifests larger than 4 MiB.
const maxManifestBytes = 4 << 20

// Authorizer is the write-path authorization chokepoint. Implementations bind
// repoName ("<org>/<repo>") to its owning org, resolve (or, on a first push,
// create) the hosted repo, and confirm the request principal may perform the
// action via acl.CanAccess. The request carries the verified token claims in its
// context (registrytoken.ClaimsFromContext), so implementations read the subject
// from there rather than re-parsing credentials.
//
// action is the Distribution scope action the request actually needs — "pull",
// "push" or "delete" — NOT a needWrite boolean. The distinction matters: the
// /v2/ Bearer challenge middleware challenges a DELETE with scope
// "repository:<name>:delete", so the client's token carries a delete grant and
// no push grant. Re-checking such a request against "push" would deny a properly
// authorized delete (OCI Distribution §"Deleting" expects 202/404/405, never
// 403 for a correctly scoped caller).
type Authorizer interface {
	// Authorize returns the resolved hosted repo when the principal in ctx may
	// perform action on repoName, else an error. Only a push may lazily create
	// the repo row; a pull or delete against a repo that does not exist yields
	// repo.ErrNotFound (→ 404), never a permission error.
	Authorize(ctx context.Context, repoName, action string) (*repo.Repo, error)
}

// Handler serves the OCI Distribution write endpoints for hosted repos.
type Handler struct {
	cache    cache.CacheManager  // verify-on-write promotion into CAS
	blobs    blobstore.BlobStore // direct CAS access for hosted blobs
	meta     meta.MetadataStore  // immutable/mutable metadata (hosted origin marking)
	repos    repo.RepoStore      // hosted repo ownership + visibility
	tags     repo.TagStore       // tag→digest pointers
	authz    Authorizer          // per-repo push/pull authorization
	sessions UploadSessionStore  // in-progress chunked upload sessions
	read     http.Handler        // optional read fallthrough (oci pull handler)
	log      *slog.Logger
}

// Option configures a Handler.
type Option func(*Handler)

// WithMeta injects the MetadataStore used to mark hosted CacheEntry origin.
func WithMeta(m meta.MetadataStore) Option { return func(h *Handler) { h.meta = m } }

// WithBlobStore injects direct CAS access (for hosted blob existence / delete).
func WithBlobStore(b blobstore.BlobStore) Option { return func(h *Handler) { h.blobs = b } }

// WithSessions injects the upload-session store (defaults to an in-memory one).
func WithSessions(s UploadSessionStore) Option { return func(h *Handler) { h.sessions = s } }

// WithReadHandler sets the handler that non-write (GET/HEAD) requests fall
// through to — the internal/handler/oci pull handler. Without it, reads 404.
func WithReadHandler(next http.Handler) Option { return func(h *Handler) { h.read = next } }

// WithLogger injects a structured logger.
func WithLogger(l *slog.Logger) Option { return func(h *Handler) { h.log = l } }

// NewHandler constructs the write handler. cm (CAS promotion), repos and tags
// (hosted metadata) and authz (per-repo authorization) are required; use the
// options to add BlobStore/MetadataStore/sessions and the read fallthrough.
func NewHandler(cm cache.CacheManager, repos repo.RepoStore, tags repo.TagStore, authz Authorizer, opts ...Option) *Handler {
	h := &Handler{
		cache:    cm,
		repos:    repos,
		tags:     tags,
		authz:    authz,
		sessions: NewMemorySessions(),
		log:      slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Compile-time assertion.
var _ http.Handler = (*Handler)(nil)

// ServeHTTP routes OCI Distribution requests. Write methods are dispatched to
// the push handlers; non-write requests fall through to the read handler (oci
// pull) when configured, preserving existing pull behaviour.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Non-/v2 or the bare version probe: defer to the read handler.
	if !strings.HasPrefix(path, "/v2/") || path == "/v2/" {
		h.serveRead(w, r)
		return
	}
	rest := strings.TrimPrefix(path, "/v2/")

	// ── blob upload session routes: /v2/<name>/blobs/uploads[/<uuid>] ─────────
	if i := strings.Index(rest, "/blobs/uploads"); i >= 0 {
		name := rest[:i]
		tail := strings.TrimPrefix(rest[i+len("/blobs/uploads"):], "/") // "" or "<uuid>"
		if name == "" {
			writeError(w, http.StatusBadRequest, "NAME_INVALID", "invalid repository name")
			return
		}
		switch r.Method {
		case http.MethodPost:
			h.startUpload(w, r, name)
		case http.MethodPatch:
			h.patchUpload(w, r, name, tail)
		case http.MethodPut:
			h.completeUpload(w, r, name, tail)
		case http.MethodGet:
			h.uploadStatus(w, r, name, tail)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	// ── manifests: /v2/<name>/manifests/<ref> ─────────────────────────────────
	if i := strings.LastIndex(rest, "/manifests/"); i >= 0 {
		name := rest[:i]
		ref := rest[i+len("/manifests/"):]
		if name == "" || ref == "" {
			writeError(w, http.StatusBadRequest, "NAME_INVALID", "invalid name or reference")
			return
		}
		switch r.Method {
		case http.MethodPut:
			h.putManifest(w, r, name, ref)
		case http.MethodDelete:
			h.deleteManifest(w, r, name, ref)
		default:
			h.serveRead(w, r) // GET/HEAD manifest → pull path
		}
		return
	}

	// ── blobs: /v2/<name>/blobs/<digest> ──────────────────────────────────────
	if i := strings.LastIndex(rest, "/blobs/"); i >= 0 {
		name := rest[:i]
		digest := rest[i+len("/blobs/"):]
		if name == "" || digest == "" {
			writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid name or digest")
			return
		}
		if r.Method == http.MethodDelete {
			h.deleteBlob(w, r, name, digest)
			return
		}
		h.serveRead(w, r) // GET/HEAD blob → pull path
		return
	}

	// ── tag listing: /v2/<name>/tags/list ─────────────────────────────────────
	if strings.HasSuffix(rest, "/tags/list") {
		name := strings.TrimSuffix(rest, "/tags/list")
		if name == "" {
			writeError(w, http.StatusBadRequest, "NAME_INVALID", "invalid repository name")
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.listTags(w, r, name)
		default:
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	// ── referrers: /v2/<name>/referrers/<digest> ──────────────────────────────
	if i := strings.LastIndex(rest, "/referrers/"); i >= 0 {
		name := rest[:i]
		subject := rest[i+len("/referrers/"):]
		if name == "" || subject == "" {
			writeError(w, http.StatusBadRequest, "NAME_INVALID", "invalid name or digest")
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.listReferrers(w, r, name, subject)
		default:
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}

	// Anything else → read path.
	h.serveRead(w, r)
}

// serveRead forwards to the configured read handler (oci pull), or 404s.
func (h *Handler) serveRead(w http.ResponseWriter, r *http.Request) {
	if h.read != nil {
		h.read.ServeHTTP(w, r)
		return
	}
	writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository not known")
}

// ── write endpoint implementations ───────────────────────────────────────────

// startUpload handles POST /v2/<name>/blobs/uploads/ — opens a chunked upload
// session and replies 202 with Location: /v2/<name>/blobs/uploads/<uuid>.
//
// Authorization is checked first so unauthenticated/unauthorised callers cannot
// consume temp-file resources by creating unlimited upload sessions.
func (h *Handler) startUpload(w http.ResponseWriter, r *http.Request, name string) {
	if _, err := h.authz.Authorize(r.Context(), name, actionPush); err != nil {
		writeAuthzError(w, err)
		return
	}

	sess, err := h.sessions.Create(r.Context(), name)
	if err != nil {
		h.log.Error("registry: create upload session", "name", name, "err", err)
		writeError(w, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "failed to create upload session")
		return
	}

	location := fmt.Sprintf("/v2/%s/blobs/uploads/%s", name, sess.ID)
	w.Header().Set("Location", location)
	w.Header().Set("Docker-Upload-UUID", sess.ID)
	w.Header().Set("Range", "0-0")
	w.Header().Set("Content-Length", "0")
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(http.StatusAccepted)
}

// patchUpload handles PATCH /v2/<name>/blobs/uploads/<uuid> — appends the
// request body to the session's quarantine file and reports the new offset.
func (h *Handler) patchUpload(w http.ResponseWriter, r *http.Request, name, uuid string) {
	if _, err := h.authz.Authorize(r.Context(), name, actionPush); err != nil {
		writeAuthzError(w, err)
		return
	}

	sess, err := h.sessions.Get(r.Context(), uuid)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "session lookup failed")
		return
	}
	if sess.Repo != name {
		// Guard against cross-repo session hijacking.
		writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload session not found")
		return
	}

	if !h.checkChunkContiguous(w, r, name, uuid, sess) {
		return
	}

	newOffset, err := h.sessions.Append(r.Context(), uuid, r.Body)
	if err != nil {
		h.log.Error("registry: append upload chunk", "uuid", uuid, "err", err)
		writeError(w, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "failed to append chunk")
		return
	}

	location := fmt.Sprintf("/v2/%s/blobs/uploads/%s", name, uuid)
	rangeEnd := newOffset - 1
	if rangeEnd < 0 {
		rangeEnd = 0
	}
	w.Header().Set("Location", location)
	w.Header().Set("Range", fmt.Sprintf("0-%d", rangeEnd))
	w.Header().Set("Content-Length", "0")
	w.Header().Set("Docker-Upload-UUID", uuid)
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(http.StatusAccepted)
}

// checkChunkContiguous validates a chunk's Content-Range against what the
// session has already accumulated, writing the error response and returning
// false when the chunk does not start exactly at the current offset.
//
// OCI Distribution §"Pushing a blob in chunks": a chunk that is not contiguous
// with the previous one MUST be rejected with 416 Requested Range Not
// Satisfiable, and the session MUST be left untouched so the client can resume
// from the Range echoed back. Appending a non-contiguous chunk instead would
// silently corrupt the blob and surface much later as a baffling
// DIGEST_INVALID at finalisation rather than an actionable 416 at the chunk
// that was actually wrong.
//
// A chunk without a Content-Range header is an append at the current offset
// (the streaming-upload form) and is always contiguous by definition.
func (h *Handler) checkChunkContiguous(w http.ResponseWriter, r *http.Request, name, uuid string, sess *UploadSession) bool {
	cr := r.Header.Get("Content-Range")
	if cr == "" {
		return true
	}
	start, _, ok := parseContentRange(cr)
	if !ok {
		writeError(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", "malformed Content-Range")
		return false
	}
	if start != sess.Offset {
		writeUploadRange(w, name, uuid, sess.Offset)
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID",
			fmt.Sprintf("chunk starts at %d but upload is at offset %d", start, sess.Offset))
		return false
	}
	return true
}

// completeUpload handles PUT /v2/<name>/blobs/uploads/<uuid>?digest=… —
// finalises a chunked (or monolithic single-request) blob upload:
//
//  1. Appends the request body to the session (monolithic upload: the whole blob
//     arrives here; chunked upload: body is empty).
//  2. Hashes the accumulated quarantine file and compares to the declared digest.
//  3. On match, promotes the bytes into the CAS via BlobStore.Put (dedup by
//     digest is automatic; an identical digest is a no-op).
//  4. Deletes the upload session and responds 201 Created.
func (h *Handler) completeUpload(w http.ResponseWriter, r *http.Request, name, uuid string) {
	if _, err := h.authz.Authorize(r.Context(), name, actionPush); err != nil {
		writeAuthzError(w, err)
		return
	}

	declaredDigest := r.URL.Query().Get("digest")
	if declaredDigest == "" {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "digest query parameter required")
		return
	}
	// Accept any digest algorithm the OCI image spec registers (sha256 canonical,
	// sha384, sha512) — the client, not the registry, chooses. Only a malformed
	// or genuinely unsupported algorithm is DIGEST_INVALID.
	if err := digestutil.Validate(declaredDigest); err != nil {
		_ = h.sessions.Delete(r.Context(), uuid)
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
		return
	}

	sess, err := h.sessions.Get(r.Context(), uuid)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "session lookup failed")
		return
	}
	if sess.Repo != name {
		writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload session not found")
		return
	}

	// A closing PUT may itself carry the last chunk, so it is subject to the same
	// contiguity rule as a PATCH: a Content-Range that skips a gap is 416, not a
	// blind append that would later fail the digest check.
	if !h.checkChunkContiguous(w, r, name, uuid, sess) {
		return
	}

	// Append the final body (monolithic single-PUT upload sends the full blob
	// here; chunked upload has an empty body — Append is a safe no-op for 0 bytes).
	if _, err := h.sessions.Append(r.Context(), uuid, r.Body); err != nil {
		h.log.Error("registry: append final chunk", "uuid", uuid, "err", err)
		writeError(w, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "failed to append final chunk")
		_ = h.sessions.Delete(r.Context(), uuid)
		return
	}

	// Re-fetch session so we have the final on-disk path.
	sess, err = h.sessions.Get(r.Context(), uuid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "session re-read failed")
		_ = h.sessions.Delete(r.Context(), uuid)
		return
	}

	// Open the quarantine file and compute the digest of the full content using
	// the algorithm the client declared.
	f, err := os.Open(sess.Path)
	if err != nil {
		h.log.Error("registry: open upload file", "path", sess.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "failed to open upload file")
		_ = h.sessions.Delete(r.Context(), uuid)
		return
	}
	defer f.Close()

	algo, _, _ := digestutil.Split(declaredDigest) // Validate above guarantees ok
	hw, err := digestutil.NewHasher(algo)
	if err != nil {
		_ = h.sessions.Delete(r.Context(), uuid)
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
		return
	}
	size, err := io.Copy(hw, f)
	if err != nil {
		h.log.Error("registry: hash upload file", "err", err)
		writeError(w, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "failed to hash upload")
		_ = h.sessions.Delete(r.Context(), uuid)
		return
	}

	actualDigest := algo + ":" + hex.EncodeToString(hw.Sum(nil))
	if actualDigest != declaredDigest {
		// Digest mismatch — discard the upload and report the error.
		_ = h.sessions.Delete(r.Context(), uuid)
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID",
			fmt.Sprintf("digest mismatch: declared %s actual %s", declaredDigest, actualDigest))
		return
	}

	// Digest verified. Promote into CAS.
	if h.blobs == nil {
		_ = h.sessions.Delete(r.Context(), uuid)
		writeError(w, http.StatusNotImplemented, "UNSUPPORTED", "blob store not configured")
		return
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		h.log.Error("registry: seek upload file", "err", err)
		writeError(w, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "seek failed")
		_ = h.sessions.Delete(r.Context(), uuid)
		return
	}

	if err := h.blobs.Put(r.Context(), actualDigest, f, size); err != nil {
		h.log.Error("registry: blob put", "digest", actualDigest, "err", err)
		writeError(w, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "failed to store blob")
		_ = h.sessions.Delete(r.Context(), uuid)
		return
	}

	// Record the immutable CacheEntry so the pull-through OCI handler
	// (cache.Lookup → meta.Get) can serve this hosted blob from CAS. Hosted
	// blobs are pushed straight into CAS via BlobStore.Put, bypassing the cache
	// manager, so this metadata row is what makes them discoverable on pull.
	h.recordHostedBlob(r.Context(), name, actualDigest, size)

	_ = h.sessions.Delete(r.Context(), uuid)

	location := fmt.Sprintf("/v2/%s/blobs/%s", name, actualDigest)
	w.Header().Set("Location", location)
	w.Header().Set("Content-Length", "0")
	w.Header().Set("Docker-Content-Digest", actualDigest)
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(http.StatusCreated)
}

// uploadStatus handles GET /v2/<name>/blobs/uploads/<uuid> — reports the
// current upload offset so clients can resume interrupted uploads.
func (h *Handler) uploadStatus(w http.ResponseWriter, r *http.Request, name, uuid string) {
	if _, err := h.authz.Authorize(r.Context(), name, actionPush); err != nil {
		writeAuthzError(w, err)
		return
	}

	sess, err := h.sessions.Get(r.Context(), uuid)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "session lookup failed")
		return
	}
	if sess.Repo != name {
		writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload session not found")
		return
	}

	rangeEnd := sess.Offset - 1
	if rangeEnd < 0 {
		rangeEnd = 0
	}
	w.Header().Set("Range", fmt.Sprintf("0-%d", rangeEnd))
	w.Header().Set("Docker-Upload-UUID", uuid)
	w.Header().Set("Content-Length", "0")
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(http.StatusNoContent)
}

// putManifest handles PUT /v2/<name>/manifests/<ref> — stores the manifest
// blob in the CAS and records the tag→digest pointer (or no-op for a
// by-digest push). The Authorizer lazily creates the repo row on a first push.
//
// TODO(R2+): validate that every blob digest referenced inside the manifest
// body exists in the CAS before accepting the push (OCI Distribution §4.2.2).
func (h *Handler) putManifest(w http.ResponseWriter, r *http.Request, name, ref string) {
	repoRow, err := h.authz.Authorize(r.Context(), name, actionPush)
	if err != nil {
		writeAuthzError(w, err)
		return
	}

	if h.blobs == nil {
		writeError(w, http.StatusNotImplemented, "UNSUPPORTED", "blob store not configured")
		return
	}

	// Bound the read: OCI Distribution recommends registries reject manifests
	// larger than 4 MiB, and an unbounded io.ReadAll on a request body is a
	// trivial memory-exhaustion vector on an authenticated-but-hostile push.
	body, err := readLimited(r.Body, maxManifestBytes)
	if err != nil {
		h.log.Warn("registry: read manifest body", "name", name, "ref", ref, "err", err)
		writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", "manifest body too large or unreadable")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", "empty manifest body")
		return
	}

	// Compute the content digest of the manifest bytes. When the client pushes by
	// digest it has declared the algorithm, so honour it (sha256 | sha384 |
	// sha512) and verify the bytes match; a tag push uses the canonical sha256.
	digestAlgo := "sha256"
	if isDigestRef(ref) {
		if err := digestutil.Validate(ref); err != nil {
			writeError(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
			return
		}
		digestAlgo, _, _ = digestutil.Split(ref)
	}
	hw, err := digestutil.NewHasher(digestAlgo)
	if err != nil {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
		return
	}
	_, _ = hw.Write(body)
	digest := digestAlgo + ":" + hex.EncodeToString(hw.Sum(nil))

	// A by-digest push must match the bytes actually received (OCI Distribution
	// §"Pushing Manifests": the registry MUST reject a mismatching digest).
	if isDigestRef(ref) && ref != digest {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID",
			fmt.Sprintf("digest mismatch: declared %s actual %s", ref, digest))
		return
	}

	// Store the manifest bytes in the shared CAS. BlobStore.Put is idempotent
	// (same digest → same bytes → no-op), so a repeated push of the same
	// manifest is safe.
	if err := h.blobs.Put(r.Context(), digest, bytes.NewReader(body), int64(len(body))); err != nil {
		h.log.Error("registry: store manifest blob", "digest", digest, "err", err)
		writeError(w, http.StatusInternalServerError, "MANIFEST_INVALID", "failed to store manifest")
		return
	}

	// Record the immutable CacheEntry for the manifest blob so the OCI pull
	// path can serve it by digest from CAS (hosted-first pull).
	h.recordHostedBlob(r.Context(), name, digest, int64(len(body)))

	// Record the tag→digest pointer when ref is a tag (not a content digest).
	// A by-digest push (e.g. PUT …/manifests/sha256:abc) skips tag creation.
	if !isDigestRef(ref) {
		if err := h.tags.PutTag(r.Context(), repoRow.ID, ref, digest); err != nil {
			h.log.Error("registry: write tag", "repo", repoRow.ID, "tag", ref, "digest", digest, "err", err)
			writeError(w, http.StatusInternalServerError, "MANIFEST_INVALID", "failed to record tag")
			return
		}
		// Also write the OCI mutable tag→digest pointer (never-revalidate TTL)
		// so serveManifest's resolveManifestDigest resolves the tag from the
		// metadata store without an upstream probe. tags.PutTag above is the
		// authoritative hosted record; this mirror feeds the shared pull path.
		h.recordHostedTag(r.Context(), name, ref, digest)
	}

	// Referrers (OCI 1.1): a manifest carrying a `subject` descriptor is a
	// referrer of that subject. Index it so GET /v2/<name>/referrers/<subject>
	// can report it, and echo OCI-Subject so the client knows the registry
	// understood the link (rather than silently dropping it, which is the signal
	// to fall back to the tag schema).
	if meta := parseManifestMeta(body); meta != nil && meta.Subject != nil && meta.Subject.Digest != "" {
		h.recordReferrer(r.Context(), name, ociDescriptor{
			MediaType:    meta.MediaType,
			Digest:       digest,
			Size:         int64(len(body)),
			ArtifactType: meta.effectiveArtifactType(),
			Annotations:  meta.Annotations,
		}, meta.Subject.Digest)
		w.Header().Set("OCI-Subject", meta.Subject.Digest)
	}

	location := fmt.Sprintf("/v2/%s/manifests/%s", name, digest)
	w.Header().Set("Location", location)
	w.Header().Set("Content-Length", "0")
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(http.StatusCreated)
}

// deleteManifest handles DELETE /v2/<name>/manifests/<ref> (org admin).
// A tag ref deletes the tag→digest pointer; a digest ref removes the manifest
// blob from the CAS (affects all repos sharing that digest — org admin only).
func (h *Handler) deleteManifest(w http.ResponseWriter, r *http.Request, name, ref string) {
	repoRow, err := h.authz.Authorize(r.Context(), name, actionDelete)
	if err != nil {
		writeAuthzError(w, err)
		return
	}

	if isDigestRef(ref) {
		// Delete by content digest: the manifest itself goes away, so every tag
		// that resolves to it must go too — otherwise the tag would keep
		// resolving to a digest whose bytes no longer exist.
		if delErr := h.deleteManifestByDigest(r.Context(), repoRow, name, ref); delErr != nil {
			h.log.Error("registry: delete manifest by digest", "name", name, "digest", ref, "err", delErr)
			writeError(w, http.StatusInternalServerError, "MANIFEST_UNKNOWN", "failed to delete manifest")
			return
		}
	} else {
		// Delete tag: remove the tag→digest pointer. The manifest bytes stay in
		// CAS — they may still be referenced by digest or by another tag.
		if delErr := h.tags.DeleteTag(r.Context(), repoRow.ID, ref); delErr != nil {
			if !errors.Is(delErr, repo.ErrNotFound) {
				h.log.Error("registry: delete tag", "repo", repoRow.ID, "tag", ref, "err", delErr)
				writeError(w, http.StatusInternalServerError, "MANIFEST_UNKNOWN", "failed to delete tag")
				return
			}
			// Tag not found is a no-op (idempotent delete).
		}
		// The tag store row is the authoritative record, but the pull path
		// resolves tags through the mutable metadata mirror written by
		// putManifest. Leaving that mirror behind (TTL -1 = never revalidate)
		// makes a deleted tag keep serving its old manifest forever.
		h.removeHostedTag(r.Context(), name, ref)
	}

	w.Header().Set("Content-Length", "0")
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(http.StatusAccepted)
}

// deleteBlob handles DELETE /v2/<name>/blobs/<digest> (org admin).
// Removes the blob from the shared CAS. Because the CAS is content-addressed
// and shared across all orgs, callers should be org admins (enforced via
// Authorizer + token scope).
func (h *Handler) deleteBlob(w http.ResponseWriter, r *http.Request, name, digest string) {
	if _, err := h.authz.Authorize(r.Context(), name, actionDelete); err != nil {
		writeAuthzError(w, err)
		return
	}

	if h.blobs == nil {
		writeError(w, http.StatusNotImplemented, "UNSUPPORTED", "blob store not configured")
		return
	}

	// A syntactically invalid digest can never name a blob.
	if err := digestutil.Validate(digest); err != nil {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
		return
	}

	// Deleting a blob that is not there is 404 BLOB_UNKNOWN, not 202: the spec
	// distinguishes "I removed it" from "there was nothing to remove", and
	// clients use the difference to detect a no-op.
	exists, err := h.blobs.Exists(r.Context(), digest)
	if err != nil {
		h.log.Error("registry: blob exists check", "digest", digest, "err", err)
		writeError(w, http.StatusInternalServerError, "UNKNOWN", "failed to check blob")
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry")
		return
	}

	// Metadata row first, then bytes: a reader must never find a live pointer to
	// bytes that are already gone.
	h.removeHostedBlob(r.Context(), name, digest)

	if err := h.blobs.Delete(r.Context(), digest); err != nil {
		h.log.Error("registry: delete blob", "digest", digest, "err", err)
		writeError(w, http.StatusInternalServerError, "BLOB_UNKNOWN", "failed to delete blob")
		return
	}

	w.Header().Set("Content-Length", "0")
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(http.StatusAccepted)
}

// ── hosted metadata recording ─────────────────────────────────────────────────

// hostedOCIProtocol is the protocol tag under which hosted OCI content is
// recorded in the MetadataStore, matching the OCI pull handler's ref.Protocol.
const hostedOCIProtocol = "oci"

// recordHostedBlob writes the immutable CacheEntry for a hosted blob/manifest so
// the OCI pull handler (cache.Lookup → meta.Get, keyed on protocol+name+digest)
// can serve it from the shared CAS. A no-op when no MetadataStore is injected
// (unit tests that exercise only the push path). Failures are logged, not fatal:
// the bytes are already durably in CAS and the authoritative repo/tag rows are
// written separately.
func (h *Handler) recordHostedBlob(ctx context.Context, name, digest string, size int64) {
	if h.meta == nil {
		return
	}
	now := time.Now().UTC()
	entry := artifact.CacheEntry{
		Ref: artifact.ArtifactRef{
			Protocol: hostedOCIProtocol,
			Name:     name,
			Version:  digest,
			Digest:   digest,
		},
		Digest:     digest,
		Size:       size,
		Protocol:   hostedOCIProtocol,
		Tier:       artifact.TierChecksum,
		VerifiedAt: now,
		CreatedAt:  now,
	}
	if err := h.meta.Put(ctx, entry); err != nil {
		h.log.Warn("registry: record hosted blob metadata", "name", name, "digest", digest, "err", err)
	}
}

// deleteManifestByDigest removes a manifest from a hosted repo: every tag that
// currently resolves to it, its mutable pointers, its CAS metadata row, and
// finally the bytes themselves.
//
// Order matters: pointers first, bytes last. A reader that races this sees
// either a live pointer to live bytes or no pointer at all — never a pointer to
// bytes that have already been unlinked.
func (h *Handler) deleteManifestByDigest(ctx context.Context, repoRow *repo.Repo, name, digest string) error {
	// 1. Drop every tag resolving to this digest (both the authoritative tag row
	//    and the mutable mirror the pull path reads).
	tags, err := h.tags.ListTags(ctx, repoRow.ID)
	if err != nil {
		return fmt.Errorf("list tags for digest delete: %w", err)
	}
	for _, t := range tags {
		if t.Digest != digest {
			continue
		}
		if delErr := h.tags.DeleteTag(ctx, repoRow.ID, t.Tag); delErr != nil && !errors.Is(delErr, repo.ErrNotFound) {
			return fmt.Errorf("delete tag %q: %w", t.Tag, delErr)
		}
		h.removeHostedTag(ctx, name, t.Tag)
	}

	// 2. Drop the CAS metadata row so the pull path stops advertising the blob
	//    even if the bytes linger (shared CAS: another repo may hold the same
	//    digest and keep the file alive).
	h.removeHostedBlob(ctx, name, digest)

	// 3. Remove the bytes. The CAS is shared and content-addressed, so this is
	//    best-effort: Delete is idempotent and already-absent is not an error.
	if h.blobs != nil {
		if delErr := h.blobs.Delete(ctx, digest); delErr != nil {
			return fmt.Errorf("delete manifest blob: %w", delErr)
		}
	}
	return nil
}

// removeHostedTag deletes the OCI mutable tag→digest pointer written by
// recordHostedTag. Best-effort: the authoritative tag row is removed separately.
func (h *Handler) removeHostedTag(ctx context.Context, name, tag string) {
	if h.meta == nil {
		return
	}
	if err := h.meta.DeleteMutable(ctx, hostedOCIProtocol+":"+name+":"+tag); err != nil {
		h.log.Warn("registry: remove hosted tag pointer", "name", name, "tag", tag, "err", err)
	}
}

// removeHostedBlob deletes the immutable CacheEntry recorded by
// recordHostedBlob, so the pull path no longer resolves the digest in this repo.
func (h *Handler) removeHostedBlob(ctx context.Context, name, digest string) {
	if h.meta == nil {
		return
	}
	ref := artifact.ArtifactRef{
		Protocol: hostedOCIProtocol,
		Name:     name,
		Version:  digest,
		Digest:   digest,
	}
	if err := h.meta.Delete(ctx, ref); err != nil {
		h.log.Warn("registry: remove hosted blob metadata", "name", name, "digest", digest, "err", err)
	}
}

// recordHostedTag mirrors a hosted tag→digest pointer into the OCI mutable tier
// (key "oci:<name>:<tag>", never-revalidate TTL) so serveManifest resolves the
// tag from the metadata store without probing an upstream.
func (h *Handler) recordHostedTag(ctx context.Context, name, tag, digest string) {
	if h.meta == nil {
		return
	}
	me := artifact.MutableEntry{
		Key:        hostedOCIProtocol + ":" + name + ":" + tag,
		Protocol:   hostedOCIProtocol,
		Digest:     digest,
		TTLSeconds: -1, // never revalidate: hosted content is authoritative
		FetchedAt:  time.Now().UTC(),
	}
	if err := h.meta.PutMutable(ctx, me); err != nil {
		h.log.Warn("registry: record hosted tag pointer", "name", name, "tag", tag, "digest", digest, "err", err)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// isDigestRef reports whether ref is a content digest (contains an algorithm
// separator ':'). Tags never contain ':', digest refs are of the form
// "sha256:<hex64>" or similar algorithm:hex patterns.
func isDigestRef(ref string) bool {
	return strings.ContainsRune(ref, ':')
}

// writeAuthzError maps authorization errors to OCI Distribution HTTP responses.
// acl.ErrForbidden and acl.ErrReadOnly are 403 DENIED (a real permission
// decision); repo.ErrNotFound is 404 NAME_UNKNOWN (the caller is allowed but the
// repo does not exist).
//
// An unexpected error is 500, NOT 403: "fail closed" means refusing the
// operation, and a 500 already does that. Reporting a store failure as DENIED
// misleads the client into thinking its credentials are wrong and violates the
// spec's status-code contract (e.g. a delete of a repo whose row is missing must
// surface as 404/405, never 403).
func writeAuthzError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, acl.ErrForbidden), errors.Is(err, acl.ErrReadOnly):
		writeError(w, http.StatusForbidden, "DENIED", "access denied")
	case errors.Is(err, repo.ErrNotFound):
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository not known")
	default:
		writeError(w, http.StatusInternalServerError, "UNKNOWN", "authorization check failed")
	}
}

// ── error helpers ─────────────────────────────────────────────────────────────

// distError is the OCI Distribution error envelope.
type distError struct {
	Errors []distErrorItem `json:"errors"`
}
type distErrorItem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError writes an OCI Distribution spec JSON error.
func writeError(w http.ResponseWriter, status int, code, message string) {
	body, _ := json.Marshal(distError{Errors: []distErrorItem{{Code: code, Message: message}}})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
