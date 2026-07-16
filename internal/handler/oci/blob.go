package oci

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// serveBlob handles GET/HEAD /v2/<name>/blobs/<digest>.
// GET supports the Range header for resumable downloads (fix M2).
// Only verified CAS content is served (verify-on-write guarantee).
func (h *Handler) serveBlob(w http.ResponseWriter, r *http.Request, imageName, digest string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if !isDigestRef(digest) {
		writeOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest format")
		return
	}

	ctx := r.Context()

	ref := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     imageName,
		Digest:   digest,
		Mutable:  false,
	}

	// Look up first to get the full size (needed for Content-Length and Range
	// validation before we open the streaming reader).
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil || entry == nil {
		writeOCIError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry")
		return
	}

	size := entry.Size

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.Header().Set("Accept-Ranges", "bytes")

	// HEAD: return headers only.
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}

	// Parse Range header.
	offset, length, partial, rangeErr := parseRange(r.Header.Get("Range"), size)
	if rangeErr != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	rc, _, err := h.cache.Serve(ctx, ref, offset, length)
	if err != nil || rc == nil {
		writeOCIError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry")
		return
	}
	defer rc.Close()

	if partial {
		end := offset + length - 1
		servedLen := length
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, end, size))
		w.Header().Set("Content-Length", strconv.FormatInt(servedLen, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
	}

	_, _ = io.Copy(w, rc)
}

// parseRange parses a single "bytes=start-end" Range header against the full
// object size. Returns (offset, length, partial, err).
//
//   - No Range header → offset=0, length=-1 (full object), partial=false.
//   - Valid range → offset, length>0, partial=true.
//   - Invalid / unsatisfiable → err != nil.
//
// Only the first range specifier is handled (multi-range is not required by
// the OCI Distribution spec for blob downloads).
func parseRange(rangeHeader string, size int64) (offset, length int64, partial bool, err error) {
	if rangeHeader == "" {
		return 0, -1, false, nil
	}
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, 0, false, errors.New("unsupported range unit")
	}

	spec := strings.TrimPrefix(rangeHeader, "bytes=")
	// Ignore multi-range; take the first specifier only.
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = strings.TrimSpace(spec[:i])
	}

	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false, errors.New("invalid range format")
	}

	hasStart := parts[0] != ""
	hasEnd := parts[1] != ""

	var start, end int64

	if hasStart {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || start < 0 {
			return 0, 0, false, errors.New("invalid range start")
		}
	}
	if hasEnd {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < 0 {
			return 0, 0, false, errors.New("invalid range end")
		}
	}

	switch {
	case !hasStart && !hasEnd:
		return 0, 0, false, errors.New("empty range specifier")

	case !hasStart:
		// Suffix range: last <end> bytes.
		if end > size {
			end = size
		}
		start = size - end
		end = size - 1

	case !hasEnd:
		// Open-ended: from start to EOF.
		if start >= size {
			return 0, 0, false, fmt.Errorf("range start %d beyond object size %d", start, size)
		}
		end = size - 1

	default:
		if start > end {
			return 0, 0, false, errors.New("range start > end")
		}
		if start >= size {
			return 0, 0, false, fmt.Errorf("range start %d beyond object size %d", start, size)
		}
		if end >= size {
			end = size - 1
		}
	}

	return start, end - start + 1, true, nil
}
