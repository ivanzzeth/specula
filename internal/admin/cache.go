package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ivanzzeth/specula/internal/artifact"
	githandler "github.com/ivanzzeth/specula/internal/handler/git"
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

// gitMirrorDir extracts the configured mirror directory for the git protocol.
// Returns empty string when git is unconfigured or MirrorDir is unset.
func (s *Server) gitMirrorDir() string {
	if s.cfg == nil {
		return ""
	}
	pc, ok := s.cfg.Protocols["git"]
	if !ok || pc.Git == nil {
		return ""
	}
	return pc.Git.MirrorDir
}

// registerOpaqueCaches tells the stats collector about every cache whose bytes
// live outside the CAS/MetadataStore and can only be measured by walking a
// directory. Today that is git's bare-mirror root (ARCHITECTURE §10: "git bare
// mirror（不透明）：du -sb 兜底采集").
//
// It is called once, when the control plane is constructed — NOT from a request
// handler.
//
// # Why construction and not first-use
//
// This registration used to happen only inside handleListGitMirrors, i.e. as a
// side effect of a human opening the git tab in the cache browser. On a fresh
// process /admin/stats omitted git entirely; one GET /api/v1/admin/cache/git
// later, the very same stats call reported git bytes=1993200. A headless replica
// — the DaemonSet/HA topology PRD §G3 specifies, where Prometheus is the only
// thing that ever connects — therefore never reported git at all. A measurement
// that exists only when someone happens to look is not a measurement, and a
// gauge that depends on UI traffic is worse than an absent one: it makes the
// bytes look like they appeared when the operator clicked.
//
// AddOpaquePath is idempotent, so this is safe regardless of what else calls it.
func (s *Server) registerOpaqueCaches() {
	if s.stats == nil {
		return
	}
	if dir := s.gitMirrorDir(); dir != "" {
		s.stats.AddOpaquePath(dir, "git")
	}
}

// mirrorDirSize returns the total size in bytes of a bare-mirror repo directory
// (analogous to `du -sb`). Returns 0 on any error (best-effort).
func mirrorDirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, e := d.Info(); e == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// listGitMirrors walks the bare-mirror tree rooted at mirrorDir and returns a
// CacheEntryDTO for each discovered bare-mirror repository. The mirror layout
// follows the git handler's convention:
//
//	<mirrorDir>/<host>/<project-path>.git
//
// where <project-path> may include slashes (e.g., github.com/alice/hello.git).
// The walk uses filepath.WalkDir and stops descending into any directory whose
// name ends in ".git" (those are the bare mirrors, not intermediate path
// components). Size is approximated via mirrorDirSize (analogous to du -sb).
// Returns nil, nil when mirrorDir does not exist.
//
// ms supplies each repo's earned trust tier (see git.RepoTier). A nil ms yields
// entries with no tier, which is the honest reading: with no MetadataStore there
// are no pins, and therefore no guarantee to report.
func listGitMirrors(ctx context.Context, ms meta.MetadataStore, mirrorDir string) ([]CacheEntryDTO, error) {
	var entries []CacheEntryDTO

	walkErr := filepath.WalkDir(mirrorDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Propagate root-not-exist so the caller can distinguish "not
			// configured" from "broken"; skip individual unreadable sub-paths.
			if path == mirrorDir {
				return err
			}
			return nil
		}
		if !d.IsDir() || path == mirrorDir {
			return nil // skip files and the root itself
		}
		if !strings.HasSuffix(d.Name(), ".git") {
			return nil // intermediate directory (host, user/org); keep descending
		}
		// Found a bare-mirror directory (ends in ".git").
		rel, err2 := filepath.Rel(mirrorDir, path)
		if err2 != nil {
			return nil
		}
		// rel is like "github.com/alice/hello.git"; strip the suffix to get the
		// canonical repo name used in Upstream and display.
		name := strings.TrimSuffix(filepath.ToSlash(rel), ".git")
		entries = append(entries, CacheEntryDTO{
			ID:       name,
			Protocol: "git",
			Name:     name,
			// The tier this repo has actually EARNED, derived from real pin state
			// rather than asserted: git.RepoTier answers "tofu" only when ref→SHA
			// pins exist for it, which is exactly when force-push / history-rewrite
			// detection is live (PRD §G2's definition of the tier), and "" when the
			// repo has earned nothing.
			//
			// Not "mirror" — e181e5a rightly deleted that fifth label, which sits
			// outside the four-tier model. But deleting it left git reporting no
			// tier at all while TOFU was being enforced underneath, which
			// under-claims a guarantee we do provide. Both are mis-reporting; the
			// fix is to report what is true, not to round to the nearest silence.
			//
			// Never "signed": that anchor exists in PRD §G2 for git but is not
			// invoked anywhere in this build. See git.RepoTier.
			Tier:     githandler.RepoTier(ctx, ms, mirrorDir, name),
			Size:     mirrorDirSize(path),
			Upstream: "https://" + name,
		})
		return fs.SkipDir // do not descend into the bare-mirror contents
	})

	if walkErr != nil {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil, nil
		}
		return nil, walkErr
	}
	return entries, nil
}

// handleListGitMirrors handles GET /api/v1/admin/cache/git. It walks the
// configured git mirror directory (bare-mirror tree, not the CAS) and returns
// one CacheEntryDTO per discovered repository. Sizes come from the directory
// walk because git objects are never stored in the CAS; the MetadataStore is
// consulted only for each repo's TOFU pins, which is what its tier is derived
// from.
//
// This handler no longer registers the mirror directory with the stats
// collector: that now happens when the Server is constructed
// (registerOpaqueCaches), so git bytes are reported to Prometheus and
// /admin/stats on a headless replica that nobody ever browses.
func (s *Server) handleListGitMirrors(w http.ResponseWriter, r *http.Request) {
	mirrorDir := s.gitMirrorDir()
	if mirrorDir == "" {
		// Git is not configured or MirrorDir is unset; return empty, not an error.
		writeJSON(w, http.StatusOK, CacheEntriesResponse{Entries: []CacheEntryDTO{}})
		return
	}

	entries, err := listGitMirrors(r.Context(), s.meta, mirrorDir)
	if err != nil {
		s.log.Error("admin: list git mirrors", "err", err, "mirror_dir", mirrorDir)
		writeError(w, http.StatusInternalServerError, "failed to list git mirror repos")
		return
	}
	if entries == nil {
		entries = []CacheEntryDTO{}
	}
	writeJSON(w, http.StatusOK, CacheEntriesResponse{
		Entries: entries,
		Total:   int64(len(entries)),
		Limit:   len(entries),
		Offset:  0,
	})
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
	protocol := canonicalProtocol(r.PathValue("protocol"))
	if _, ok := knownProtocols[protocol]; !ok {
		writeError(w, http.StatusNotFound, "unknown protocol")
		return
	}

	// Git is special: objects live in a bare-mirror tree on disk, not in the
	// CAS / MetadataStore. Delegate to the mirror-aware handler.
	if protocol == "git" {
		s.handleListGitMirrors(w, r)
		return
	}

	if s.meta == nil {
		writeError(w, http.StatusNotImplemented, "metadata store not configured")
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
