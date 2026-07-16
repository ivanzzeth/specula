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
	"crypto/sha256"
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
	"github.com/ivanzzeth/specula/internal/repo"
	blobstore "github.com/ivanzzeth/specula/internal/store/blob"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// Authorizer is the write-path authorization chokepoint. Implementations bind
// repoName ("<org>/<repo>") to its owning org, resolve (or, on a first push,
// create) the hosted repo, and confirm the request principal may perform the
// action via acl.CanAccess. The request carries the verified token claims in its
// context (registrytoken.ClaimsFromContext), so implementations read the subject
// from there rather than re-parsing credentials.
type Authorizer interface {
	// Authorize returns the resolved hosted repo when the principal in ctx may
	// perform the action (needWrite=true for push/delete) on repoName, else an
	// error. On a first push it may create and return the new repo row.
	Authorize(ctx context.Context, repoName string, needWrite bool) (*repo.Repo, error)
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

	// Anything else (e.g. /v2/<name>/tags/list) → read path.
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
	if _, err := h.authz.Authorize(r.Context(), name, true); err != nil {
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
	if _, err := h.authz.Authorize(r.Context(), name, true); err != nil {
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
	if _, err := h.authz.Authorize(r.Context(), name, true); err != nil {
		writeAuthzError(w, err)
		return
	}

	declaredDigest := r.URL.Query().Get("digest")
	if declaredDigest == "" {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "digest query parameter required")
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

	// Open the quarantine file and compute the sha256 digest of the full content.
	f, err := os.Open(sess.Path)
	if err != nil {
		h.log.Error("registry: open upload file", "path", sess.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "failed to open upload file")
		_ = h.sessions.Delete(r.Context(), uuid)
		return
	}
	defer f.Close()

	hw := sha256.New()
	size, err := io.Copy(hw, f)
	if err != nil {
		h.log.Error("registry: hash upload file", "err", err)
		writeError(w, http.StatusInternalServerError, "BLOB_UPLOAD_INVALID", "failed to hash upload")
		_ = h.sessions.Delete(r.Context(), uuid)
		return
	}

	actualDigest := "sha256:" + hex.EncodeToString(hw.Sum(nil))
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
	if _, err := h.authz.Authorize(r.Context(), name, true); err != nil {
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
	repoRow, err := h.authz.Authorize(r.Context(), name, true)
	if err != nil {
		writeAuthzError(w, err)
		return
	}

	if h.blobs == nil {
		writeError(w, http.StatusNotImplemented, "UNSUPPORTED", "blob store not configured")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.log.Error("registry: read manifest body", "name", name, "ref", ref, "err", err)
		writeError(w, http.StatusInternalServerError, "MANIFEST_INVALID", "failed to read manifest body")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", "empty manifest body")
		return
	}

	// Compute the content digest of the manifest bytes.
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])

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
	repoRow, err := h.authz.Authorize(r.Context(), name, true)
	if err != nil {
		writeAuthzError(w, err)
		return
	}

	if isDigestRef(ref) {
		// Delete by content digest: remove the blob from CAS.
		if h.blobs != nil {
			if delErr := h.blobs.Delete(r.Context(), ref); delErr != nil {
				h.log.Warn("registry: delete manifest blob", "name", name, "digest", ref, "err", delErr)
				writeError(w, http.StatusInternalServerError, "MANIFEST_UNKNOWN", "failed to delete manifest blob")
				return
			}
		}
	} else {
		// Delete tag: remove the tag→digest pointer.
		if delErr := h.tags.DeleteTag(r.Context(), repoRow.ID, ref); delErr != nil {
			if !errors.Is(delErr, repo.ErrNotFound) {
				h.log.Error("registry: delete tag", "repo", repoRow.ID, "tag", ref, "err", delErr)
				writeError(w, http.StatusInternalServerError, "MANIFEST_UNKNOWN", "failed to delete tag")
				return
			}
			// Tag not found is a no-op (idempotent delete).
		}
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
	if _, err := h.authz.Authorize(r.Context(), name, true); err != nil {
		writeAuthzError(w, err)
		return
	}

	if h.blobs == nil {
		writeError(w, http.StatusNotImplemented, "UNSUPPORTED", "blob store not configured")
		return
	}

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
// acl.ErrForbidden and acl.ErrReadOnly are 403 DENIED; repo.ErrNotFound is 404;
// unexpected errors default to 403 (fail-closed).
func writeAuthzError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, acl.ErrForbidden), errors.Is(err, acl.ErrReadOnly):
		writeError(w, http.StatusForbidden, "DENIED", "access denied")
	case errors.Is(err, repo.ErrNotFound):
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository not known")
	default:
		writeError(w, http.StatusForbidden, "DENIED", "access denied")
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

// notImplemented is the uniform 501 for a not-yet-built write endpoint.
func notImplemented(w http.ResponseWriter, what string) {
	writeError(w, http.StatusNotImplemented, "UNSUPPORTED", what+" not implemented")
}
