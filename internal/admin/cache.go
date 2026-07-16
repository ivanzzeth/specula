package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// knownProtocols is the closed set of protocols the cache browser serves. It is
// validated up front so an unknown tab yields 404 rather than a silently empty —
// and therefore misleading — "nothing is cached" page.
var knownProtocols = map[string]struct{}{
	"oci": {}, "pypi": {}, "npm": {}, "go": {}, "gomod": {},
	"apt": {}, "helm": {}, "git": {}, "tarball": {},
}

// protocolAliases maps a user-facing protocol name onto the value the handler
// actually stores in ArtifactRef.Protocol.
//
// The Go module handler stores "gomod" (gomod.Protocol) while the config block,
// the docs, and the UI tab all say "go". Both names are accepted above, but
// without this mapping GET /admin/cache/go passes validation and then queries a
// protocol no row ever carries — producing exactly the silently-empty,
// misleading "nothing is cached" page the validation exists to prevent. Caught
// by a real local deployment: stats reported gomod=10 objects while the Go tab
// showed zero.
var protocolAliases = map[string]string{
	"go": "gomod",
}

// canonicalProtocol resolves a request's protocol name to its stored form.
func canonicalProtocol(p string) string {
	if canon, ok := protocolAliases[p]; ok {
		return canon
	}
	return p
}

// parseTier maps a tier name from the query string onto artifact.Tier.
// Returns ok=false for an unrecognised name so the handler can 400 rather than
// silently ignore the filter (which would show the operator the wrong rows
// under a filter chip that claims otherwise).
func parseTier(s string) (artifact.Tier, bool) {
	switch s {
	case "signed":
		return artifact.TierSigned, true
	case "consensus":
		return artifact.TierConsensus, true
	case "tofu":
		return artifact.TierTofu, true
	case "checksum":
		return artifact.TierChecksum, true
	}
	return 0, false
}

// parseSort maps the sort query value onto a meta.SortField.
func parseSort(s string) (meta.SortField, bool) {
	switch meta.SortField(s) {
	case meta.SortCreatedAt, meta.SortSize, meta.SortName, meta.SortVerifiedAt:
		return meta.SortField(s), true
	}
	return "", false
}

// toCacheEntryDTO projects a store entry onto the wire contract.
func toCacheEntryDTO(e meta.Entry) CacheEntryDTO {
	return CacheEntryDTO{
		ID:              e.ID,
		Protocol:        e.Ref.Protocol,
		Name:            e.Ref.Name,
		Version:         e.Ref.Version,
		Digest:          e.Digest,
		Size:            e.Size,
		Tier:            e.Tier.String(),
		Upstream:        e.Upstream,
		ETag:            e.ETag,
		Mutable:         e.Ref.Mutable,
		Pinned:          e.Pinned,
		VerifiedUnix:    unixOrZero(e.VerifiedAt),
		FirstCachedUnix: unixOrZero(e.CreatedAt),
	}
}

// handleListCache → GET /api/v1/admin/cache/{protocol}. Returns
// CacheEntriesResponse.
//
// Query parameters (all optional):
//
//	name=<substring>   name contains (literal; % and _ are not wildcards)
//	tier=signed|consensus|tofu|checksum
//	upstream=<name>    exact upstream match
//	pinned=true|false  pin state
//	sort=created_at|size|name|verified_at   (default created_at)
//	order=asc|desc                          (default desc)
//	limit=<n>          clamped to meta.MaxLimit
//	offset=<n>
//
// An unparseable filter value is a 400: silently dropping it would render rows
// that contradict the filter chips the operator can see.
func (s *Server) handleListCache(w http.ResponseWriter, r *http.Request) {
	if s.meta == nil {
		writeError(w, http.StatusNotImplemented, "metadata store not configured")
		return
	}
	protocol := canonicalProtocol(r.PathValue("protocol"))
	if _, ok := knownProtocols[protocol]; !ok {
		writeError(w, http.StatusNotFound, "unknown protocol")
		return
	}

	q := r.URL.Query()
	filter := meta.EntryFilter{
		NameContains: q.Get("name"),
		Upstream:     q.Get("upstream"),
	}
	if v := q.Get("tier"); v != "" {
		tier, ok := parseTier(v)
		if !ok {
			writeError(w, http.StatusBadRequest,
				"tier must be one of: signed, consensus, tofu, checksum")
			return
		}
		filter.Tier = &tier
	}
	if v := q.Get("pinned"); v != "" {
		pinned, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "pinned must be true or false")
			return
		}
		filter.Pinned = &pinned
	}

	// Default to newest-first: the operator's first question about a cache is
	// almost always "what just landed here".
	page := meta.Page{Sort: meta.SortCreatedAt, Desc: true}
	if v := q.Get("sort"); v != "" {
		sort, ok := parseSort(v)
		if !ok {
			writeError(w, http.StatusBadRequest,
				"sort must be one of: created_at, size, name, verified_at")
			return
		}
		page.Sort = sort
	}
	if v := q.Get("order"); v != "" {
		switch v {
		case "asc":
			page.Desc = false
		case "desc":
			page.Desc = true
		default:
			writeError(w, http.StatusBadRequest, "order must be asc or desc")
			return
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "limit must be an integer")
			return
		}
		page.Limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "offset must be an integer")
			return
		}
		page.Offset = n
	}

	result, err := s.meta.ListEntries(r.Context(), protocol, filter, page)
	if err != nil {
		s.log.Error("admin: list cache entries", "err", err, "protocol", protocol)
		writeError(w, http.StatusInternalServerError, "failed to list cache entries")
		return
	}

	entries := make([]CacheEntryDTO, 0, len(result.Entries))
	for _, e := range result.Entries {
		entries = append(entries, toCacheEntryDTO(e))
	}
	writeJSON(w, http.StatusOK, CacheEntriesResponse{
		Entries: entries,
		Total:   result.Total,
		Limit:   result.Limit,
		Offset:  result.Offset,
	})
}

// decodeEntryRef resolves the {protocol}/{id} path pair to an artifact ref,
// writing the response and reporting ok=false on failure.
//
// The id encodes its own protocol, which must match the path's: a mismatch means
// the client pasted an id from another protocol's tab, and honouring it would
// let a request addressed to /cache/pypi/... delete an OCI row.
func decodeEntryRef(w http.ResponseWriter, protocol, id string) (artifact.ArtifactRef, bool) {
	ref, err := meta.DecodeEntryID(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, "malformed cache entry id")
		return artifact.ArtifactRef{}, false
	}
	if ref.Protocol != protocol {
		writeError(w, http.StatusBadRequest, "entry id does not belong to this protocol")
		return artifact.ArtifactRef{}, false
	}
	return ref, true
}

// handleDeleteCacheEntry → DELETE /api/v1/admin/cache/{protocol}/{id}.
// Returns 204.
//
// This evicts the metadata row for one cached artifact. The CAS blob itself is
// left to GC: blobs are content-addressed and shared (a pull-through entry and a
// hosted image can be the same bytes), so removing them here could break an
// unrelated artifact that shares the digest.
func (s *Server) handleDeleteCacheEntry(w http.ResponseWriter, r *http.Request) {
	if s.meta == nil {
		writeError(w, http.StatusNotImplemented, "metadata store not configured")
		return
	}
	protocol := canonicalProtocol(r.PathValue("protocol"))
	if _, ok := knownProtocols[protocol]; !ok {
		writeError(w, http.StatusNotFound, "unknown protocol")
		return
	}
	ref, ok := decodeEntryRef(w, protocol, r.PathValue("id"))
	if !ok {
		return
	}

	if err := s.meta.Delete(r.Context(), ref); err != nil {
		if errors.Is(err, meta.ErrBadEntryID) {
			writeError(w, http.StatusBadRequest, "malformed cache entry id")
			return
		}
		s.log.Error("admin: delete cache entry", "err", err,
			"protocol", ref.Protocol, "name", ref.Name, "version", ref.Version)
		writeError(w, http.StatusInternalServerError, "failed to delete cache entry")
		return
	}
	s.log.Info("admin: cache entry evicted",
		"protocol", ref.Protocol, "name", ref.Name, "version", ref.Version)
	w.WriteHeader(http.StatusNoContent)
}

// handlePinCacheEntry → POST /api/v1/admin/cache/{protocol}/{id}/pin.
// Body: PinCacheEntryRequest. Returns 204.
//
// A pinned entry is exempt from GC/eviction.
func (s *Server) handlePinCacheEntry(w http.ResponseWriter, r *http.Request) {
	if s.meta == nil {
		writeError(w, http.StatusNotImplemented, "metadata store not configured")
		return
	}
	protocol := canonicalProtocol(r.PathValue("protocol"))
	if _, ok := knownProtocols[protocol]; !ok {
		writeError(w, http.StatusNotFound, "unknown protocol")
		return
	}
	ref, ok := decodeEntryRef(w, protocol, r.PathValue("id"))
	if !ok {
		return
	}

	var req PinCacheEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if err := s.meta.SetPinned(r.Context(), ref, req.Pinned); err != nil {
		s.log.Error("admin: pin cache entry", "err", err,
			"protocol", ref.Protocol, "name", ref.Name)
		writeError(w, http.StatusInternalServerError, "failed to update pin")
		return
	}
	s.log.Info("admin: cache entry pin changed",
		"protocol", ref.Protocol, "name", ref.Name, "pinned", req.Pinned)
	w.WriteHeader(http.StatusNoContent)
}
