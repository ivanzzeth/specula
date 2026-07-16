package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	godigest "github.com/opencontainers/go-digest"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/repo"
)

// This file implements the OCI Distribution "content discovery" endpoints for
// hosted repos:
//
//	GET /v2/<name>/tags/list                — tag listing (n / last pagination)
//	GET /v2/<name>/referrers/<digest>       — referrers API (OCI 1.1)
//
// Both are read endpoints, so they authorize with the "pull" action — the same
// action the /v2/ Bearer middleware challenges a GET for.

// mediaTypeOCIIndex is the media type of an OCI image index. The referrers API
// response is always an index of this exact type (OCI Distribution §"Listing
// Referrers": "the response MUST be an image index ... Content-Type MUST be
// application/vnd.oci.image.index.v1+json").
const mediaTypeOCIIndex = "application/vnd.oci.image.index.v1+json"

// defaultTagPageSize caps an unpaginated tag listing so a repo with a very large
// tag set cannot force an unbounded response. A client that wants a specific
// page size passes ?n=.
const defaultTagPageSize = 1000

// ── tag listing ───────────────────────────────────────────────────────────────

// tagListResponse is the GET /v2/<name>/tags/list body.
type tagListResponse struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// listTags handles GET /v2/<name>/tags/list.
//
// Tags are returned in lexical order (the spec requires the listing to be
// ordered so that `last` is well-defined). Pagination follows §"Listing Tags":
// ?n=<count> caps the page, ?last=<tag> resumes strictly after that tag, and a
// truncated response advertises the next page via a Link header with rel="next".
func (h *Handler) listTags(w http.ResponseWriter, r *http.Request, name string) {
	repoRow, err := h.authz.Authorize(r.Context(), name, actionPull)
	if err != nil {
		writeAuthzError(w, err)
		return
	}

	rows, err := h.tags.ListTags(r.Context(), repoRow.ID)
	if err != nil {
		h.log.Error("registry: list tags", "name", name, "err", err)
		writeError(w, http.StatusInternalServerError, "UNKNOWN", "failed to list tags")
		return
	}

	// Always a non-nil slice: the spec's schema types `tags` as an array, and a
	// JSON null breaks clients that range over it without a nil check.
	names := make([]string, 0, len(rows))
	for _, t := range rows {
		names = append(names, t.Tag)
	}
	sort.Strings(names)

	page, truncated, err := paginateTags(names, r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, "PAGINATION_NUMBER_INVALID", err.Error())
		return
	}

	if truncated && len(page) > 0 {
		w.Header().Set("Link", nextTagPageLink(name, r.URL.Query().Get("n"), page[len(page)-1]))
	}
	writeJSON(w, http.StatusOK, tagListResponse{Name: name, Tags: page})
}

// paginateTags applies the ?n= and ?last= query parameters to a sorted tag list,
// returning the page and whether more tags remain after it.
func paginateTags(sorted []string, q url.Values) (page []string, truncated bool, err error) {
	if last := q.Get("last"); last != "" {
		// Resume strictly after `last`. sort.SearchStrings finds the first index
		// >= last; skip that element too when it is an exact match.
		i := sort.SearchStrings(sorted, last)
		if i < len(sorted) && sorted[i] == last {
			i++
		}
		sorted = sorted[i:]
	}

	limit := defaultTagPageSize
	if raw := q.Get("n"); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 0 {
			return nil, false, fmt.Errorf("invalid n parameter %q: must be a non-negative integer", raw)
		}
		limit = n
	}

	if len(sorted) > limit {
		return sorted[:limit], true, nil
	}
	return sorted, false, nil
}

// nextTagPageLink builds the RFC 5988 Link header value pointing at the next
// page of a truncated tag listing.
func nextTagPageLink(name, n, last string) string {
	q := url.Values{}
	if n != "" {
		q.Set("n", n)
	}
	q.Set("last", last)
	return fmt.Sprintf(`</v2/%s/tags/list?%s>; rel="next"`, name, q.Encode())
}

// ── referrers API ─────────────────────────────────────────────────────────────

// ociDescriptor is an OCI content descriptor as it appears in an image index.
type ociDescriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       string            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// ociIndex is an OCI image index — the referrers API response shape.
type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []ociDescriptor `json:"manifests"`
}

// newReferrersIndex returns an empty, well-formed referrers index. The spec
// requires schemaVersion 2 and the index media type even when there are no
// referrers, so an empty result is a 200 with this body — not a 404.
func newReferrersIndex() ociIndex {
	return ociIndex{
		SchemaVersion: 2,
		MediaType:     mediaTypeOCIIndex,
		Manifests:     []ociDescriptor{},
	}
}

// manifestMeta is the subset of a manifest body the push path needs to maintain
// the referrers index: its own media/artifact type, its annotations, and the
// subject it refers to (if any).
type manifestMeta struct {
	MediaType    string `json:"mediaType"`
	ArtifactType string `json:"artifactType"`
	Config       struct {
		MediaType string `json:"mediaType"`
	} `json:"config"`
	Subject     *ociDescriptor    `json:"subject"`
	Annotations map[string]string `json:"annotations"`
}

// effectiveArtifactType implements the OCI image-spec fallback: a manifest's
// artifactType is its own `artifactType` field, or — when that is absent — its
// config descriptor's mediaType.
func (m *manifestMeta) effectiveArtifactType() string {
	if m.ArtifactType != "" {
		return m.ArtifactType
	}
	return m.Config.MediaType
}

// parseManifestMeta extracts the referrers-relevant fields from a manifest body.
// Returns nil when the body is not a JSON object — putManifest has already
// stored the bytes by then, and an unparseable manifest simply has no subject to
// index rather than being a push failure.
func parseManifestMeta(body []byte) *manifestMeta {
	var m manifestMeta
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	return &m
}

// referrersKey is the MetadataStore mutable-tier key under which a repo's
// referrers index for one subject digest is recorded.
//
// The index is kept in the mutable tier (key → index-blob digest) rather than in
// the tag store on purpose: the referrers set is a derived, per-subject index,
// not a user-visible tag. Storing it as a tag would leak `<alg>-<hex>` entries
// into GET /tags/list, which the tag-listing endpoint is meant to report as the
// tags a user actually pushed.
func referrersKey(name, subjectDigest string) string {
	return "oci-referrers:" + name + ":" + subjectDigest
}

// listReferrers handles GET /v2/<name>/referrers/<digest>.
//
// Per OCI Distribution §"Listing Referrers" the response is ALWAYS an image
// index with Content-Type application/vnd.oci.image.index.v1+json: a subject
// with no referrers — or one that was never pushed — yields 200 with an empty
// manifests array, not a 404. Only a malformed digest is an error (400).
func (h *Handler) listReferrers(w http.ResponseWriter, r *http.Request, name, subjectDigest string) {
	// A malformed digest is the one documented error for this endpoint
	// (end-12a: 200 on success, 404/400 on failure). Check it before authz so a
	// nonsense request is not reported as a permission problem.
	if err := godigest.Digest(subjectDigest).Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
		return
	}

	index := newReferrersIndex()
	switch _, err := h.authz.Authorize(r.Context(), name, actionPull); {
	case err == nil:
		loaded, loadErr := h.loadReferrers(r.Context(), name, subjectDigest)
		if loadErr != nil {
			h.log.Error("registry: load referrers", "name", name, "subject", subjectDigest, "err", loadErr)
			writeError(w, http.StatusInternalServerError, "UNKNOWN", "failed to load referrers")
			return
		}
		index = loaded

	case errors.Is(err, repo.ErrNotFound):
		// The caller is permitted but the repo holds nothing. Spec §"Listing
		// Referrers": a registry that supports the referrers API MUST NOT answer
		// 404, and "if a query results in no matching referrers, an empty
		// manifest list MUST be returned". "Repo has no referrers" and "repo does
		// not exist yet" are the same answer to the question being asked — and
		// answering identically leaks no existence information either.
		// Authorization is still enforced: only repo.ErrNotFound lands here, so a
		// caller without pull scope still gets the 403 below.

	default:
		writeAuthzError(w, err)
		return
	}

	// Optional artifactType filter. When applied, the spec requires the response
	// to advertise it via OCI-Filters-Applied so the client knows the list is
	// narrowed rather than complete.
	if at := r.URL.Query().Get("artifactType"); at != "" {
		kept := make([]ociDescriptor, 0, len(index.Manifests))
		for _, d := range index.Manifests {
			if d.ArtifactType == at {
				kept = append(kept, d)
			}
		}
		index.Manifests = kept
		w.Header().Set("OCI-Filters-Applied", "artifactType")
	}

	w.Header().Set("Content-Type", mediaTypeOCIIndex)
	body, err := json.Marshal(index)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "UNKNOWN", "failed to encode referrers index")
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

// loadReferrers reads the stored referrers index for (name, subjectDigest),
// returning an empty index when none has been recorded yet.
func (h *Handler) loadReferrers(ctx context.Context, name, subjectDigest string) (ociIndex, error) {
	if h.meta == nil || h.blobs == nil {
		return newReferrersIndex(), nil
	}
	entry, err := h.meta.GetMutable(ctx, referrersKey(name, subjectDigest))
	if err != nil {
		return newReferrersIndex(), fmt.Errorf("registry: get referrers pointer: %w", err)
	}
	if entry == nil || entry.Digest == "" {
		return newReferrersIndex(), nil
	}

	rc, _, err := h.blobs.Get(ctx, entry.Digest, 0, -1)
	if err != nil {
		// The pointer outlived its index blob. Report an empty list rather than
		// failing the request: an empty index is the spec's "no referrers"
		// answer and is strictly better than a 500 for a read-only discovery
		// endpoint.
		h.log.Warn("registry: referrers index blob missing", "name", name, "subject", subjectDigest, "digest", entry.Digest)
		return newReferrersIndex(), nil
	}
	defer rc.Close()

	var index ociIndex
	if err := json.NewDecoder(rc).Decode(&index); err != nil {
		return newReferrersIndex(), fmt.Errorf("registry: decode referrers index: %w", err)
	}
	if index.Manifests == nil {
		index.Manifests = []ociDescriptor{}
	}
	index.SchemaVersion = 2
	index.MediaType = mediaTypeOCIIndex
	return index, nil
}

// recordReferrer adds the just-pushed manifest to its subject's referrers index.
// The updated index is stored as a CAS blob and the mutable pointer is repointed
// at it, so readers always observe a complete index (never a half-written one).
//
// Failures are logged, not fatal: the manifest itself is already durably stored
// and tagged. Losing an index entry degrades discovery, but failing the push
// would discard a valid manifest the client already considers accepted.
func (h *Handler) recordReferrer(ctx context.Context, name string, desc ociDescriptor, subjectDigest string) {
	if h.meta == nil || h.blobs == nil {
		return
	}

	index, err := h.loadReferrers(ctx, name, subjectDigest)
	if err != nil {
		h.log.Warn("registry: read referrers index for update", "name", name, "subject", subjectDigest, "err", err)
		// Fall through with the empty index loadReferrers returned rather than
		// dropping the new referrer entirely.
	}

	// Idempotent: re-pushing the same manifest must not duplicate its entry.
	for i, existing := range index.Manifests {
		if existing.Digest == desc.Digest {
			index.Manifests[i] = desc
			h.storeReferrers(ctx, name, subjectDigest, index)
			return
		}
	}
	index.Manifests = append(index.Manifests, desc)
	h.storeReferrers(ctx, name, subjectDigest, index)
}

// storeReferrers marshals the index into CAS and repoints the mutable pointer.
func (h *Handler) storeReferrers(ctx context.Context, name, subjectDigest string, index ociIndex) {
	body, err := json.Marshal(index)
	if err != nil {
		h.log.Warn("registry: encode referrers index", "name", name, "subject", subjectDigest, "err", err)
		return
	}
	indexDigest, err := digestOf("sha256", body)
	if err != nil {
		h.log.Warn("registry: digest referrers index", "err", err)
		return
	}
	if err := h.blobs.Put(ctx, indexDigest, bytes.NewReader(body), int64(len(body))); err != nil {
		h.log.Warn("registry: store referrers index", "name", name, "subject", subjectDigest, "err", err)
		return
	}
	// Pin the index blob as hosted content so cache GC never evicts it out from
	// under its pointer.
	h.recordHostedBlob(ctx, name, indexDigest, int64(len(body)))

	me := artifact.MutableEntry{
		Key:        referrersKey(name, subjectDigest),
		Protocol:   hostedOCIProtocol,
		Digest:     indexDigest,
		TTLSeconds: -1, // derived local data: never revalidate
		FetchedAt:  time.Now().UTC(),
	}
	if err := h.meta.PutMutable(ctx, me); err != nil {
		h.log.Warn("registry: write referrers pointer", "name", name, "subject", subjectDigest, "err", err)
	}
}

// ── shared helpers ────────────────────────────────────────────────────────────

// writeJSON writes a JSON body with an accurate Content-Length.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "UNKNOWN", "failed to encode response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// digestOf computes "<algo>:<hex>" over b.
func digestOf(algo string, b []byte) (string, error) {
	a := godigest.Algorithm(algo)
	if !a.Available() {
		return "", fmt.Errorf("unsupported digest algorithm %q", algo)
	}
	return a.FromBytes(b).String(), nil
}

// parseContentRange parses an upload chunk's "Content-Range: <start>-<end>"
// value. The blob-upload PATCH form is the bare inclusive byte range with no
// "bytes " unit prefix and no "/total" suffix (OCI Distribution §"Pushing a
// blob in chunks"), but tolerate both since some clients send the HTTP form.
func parseContentRange(v string) (start, end int64, ok bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "bytes ")
	if i := strings.IndexByte(v, '/'); i >= 0 {
		v = v[:i]
	}
	i := strings.IndexByte(v, '-')
	if i <= 0 {
		return 0, 0, false
	}
	var err error
	if start, err = strconv.ParseInt(strings.TrimSpace(v[:i]), 10, 64); err != nil || start < 0 {
		return 0, 0, false
	}
	if end, err = strconv.ParseInt(strings.TrimSpace(v[i+1:]), 10, 64); err != nil || end < start {
		return 0, 0, false
	}
	return start, end, true
}

// writeUploadRange sets the upload-progress headers (Location / Range /
// Docker-Upload-UUID) describing a session that currently holds offset bytes.
// It only sets headers; the caller writes the status line.
func writeUploadRange(w http.ResponseWriter, name, uuid string, offset int64) {
	rangeEnd := offset - 1
	if rangeEnd < 0 {
		rangeEnd = 0
	}
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", name, uuid))
	w.Header().Set("Range", fmt.Sprintf("0-%d", rangeEnd))
	w.Header().Set("Docker-Upload-UUID", uuid)
}

// readLimited reads at most max+1 bytes from r, erroring when the body exceeds
// max. Manifests are bounded by the spec's 4 MiB recommendation; an unbounded
// io.ReadAll on a request body is a trivial memory-exhaustion vector.
func readLimited(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("body exceeds %d byte limit", max)
	}
	return b, nil
}
