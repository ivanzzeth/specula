package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/ivanzzeth/specula/internal/sbom"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/version"
)

// sbomProtocols is the closed set accepted by GET /admin/sbom. Broader than the
// cache-browser tabs so cargo/conda/hf inventory can be exported before UI tabs
// catch up; git is excluded (opaque bare mirrors, not CAS rows).
var sbomProtocols = map[string]struct{}{
	"oci": {}, "pypi": {}, "npm": {}, "go": {}, "gomod": {},
	"apt": {}, "helm": {}, "tarball": {}, "cargo": {}, "conda": {}, "hf": {},
}

// handleSBOM → GET /api/v1/admin/sbom
//
// Emits a CycloneDX 1.5 JSON BOM of immutable cache inventory (what Specula has
// verified and stored). Query:
//
//	protocol=<name>   optional; empty = all CAS protocols
//	format=cyclonedx-json   (default; only supported value)
//	limit=<n>         max components (clamped to sbom.MaxComponents)
//
// This is an audit export, not recursive package-content analysis. Mutable
// entries (indexes, packuments) are omitted by the sbom builder.
func (s *Server) handleSBOM(w http.ResponseWriter, r *http.Request) {
	if s.meta == nil {
		writeError(w, http.StatusNotImplemented, "metadata store not configured")
		return
	}

	q := r.URL.Query()
	format := q.Get("format")
	if format == "" {
		format = sbom.FormatCycloneDXJSON
	}
	if format != sbom.FormatCycloneDXJSON {
		writeError(w, http.StatusBadRequest, "format must be cyclonedx-json")
		return
	}

	protocol := canonicalProtocol(q.Get("protocol"))
	if protocol != "" {
		if _, ok := sbomProtocols[protocol]; !ok {
			writeError(w, http.StatusBadRequest, "unknown or unsupported protocol for SBOM")
			return
		}
	}

	wantLimit := sbom.MaxComponents
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if n < wantLimit {
			wantLimit = n
		}
	}

	entries, err := collectImmutableEntries(r.Context(), s.meta, protocol, wantLimit)
	if err != nil {
		s.log.Error("admin: sbom list entries", "err", err, "protocol", protocol)
		writeError(w, http.StatusInternalServerError, "failed to list cache entries for SBOM")
		return
	}

	doc := sbom.FromEntries(entries, sbom.Options{
		SpeculaVersion: version.Short(),
	})
	// If we filled the requested limit, more rows may exist in the store.
	if wantLimit < sbom.MaxComponents && len(doc.Components) >= wantLimit {
		doc.Truncated = true
	}
	if wantLimit >= sbom.MaxComponents && len(doc.Components) >= sbom.MaxComponents {
		doc.Truncated = true
	}

	w.Header().Set("Content-Type", "application/vnd.cyclonedx+json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="specula-cache.cdx.json"`)
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		s.log.Error("admin: sbom encode", "err", err)
	}
}

// collectImmutableEntries pages ListEntries until wantLimit immutable rows are
// gathered or the store is exhausted. Pages may include mutables; the caller
// (sbom.FromEntries) filters them. We stop once we have enough immutables so a
// large mutable tier cannot force a full-table scan beyond need.
func collectImmutableEntries(ctx context.Context, ms meta.MetadataStore, protocol string, wantLimit int) ([]meta.Entry, error) {
	out := make([]meta.Entry, 0, wantLimit)
	offset := 0
	for len(out) < wantLimit {
		pageLimit := meta.MaxLimit
		remaining := wantLimit - len(out)
		// Over-fetch a bit to absorb mutables in the page.
		fetch := remaining * 2
		if fetch < 50 {
			fetch = 50
		}
		if fetch > pageLimit {
			fetch = pageLimit
		}
		result, err := ms.ListEntries(ctx, protocol, meta.EntryFilter{}, meta.Page{
			Limit:  fetch,
			Offset: offset,
			Sort:   meta.SortCreatedAt,
			Desc:   true,
		})
		if err != nil {
			return nil, err
		}
		if len(result.Entries) == 0 {
			break
		}
		for _, e := range result.Entries {
			if e.Ref.Mutable {
				continue
			}
			out = append(out, e)
			if len(out) >= wantLimit {
				break
			}
		}
		offset += len(result.Entries)
		if offset >= int(result.Total) || len(result.Entries) < fetch {
			break
		}
	}
	return out, nil
}
