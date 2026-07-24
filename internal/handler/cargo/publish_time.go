package cargo

import (
	"context"
	"io"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/publishmeta"
	"github.com/ivanzzeth/specula/internal/upstream"
)

const maxMetaBody = 1 << 20 // 1 MiB — crates.io version JSON is tiny

// defaultAPIBase is the crates.io API root used for publish-time lookups.
// Overridable in tests.
var defaultAPIBase = "https://crates.io"

// enrichPublishTime sets umeta.PublishedAt from crates.io API
// GET /api/v1/crates/<name>/<version> created_at. Sparse index has no
// publish time. Best-effort; never fails the crate download.
func (h *Handler) enrichPublishTime(ctx context.Context, ref artifact.ArtifactRef, umeta *artifact.UpstreamMeta) {
	if umeta == nil || !umeta.PublishedAt.IsZero() {
		return
	}
	if ref.Name == "" || ref.Version == "" || h.upstreamClt == nil {
		return
	}
	body, ok := h.crateVersionAPIBody(ctx, ref.Name, ref.Version)
	if !ok {
		return
	}
	if t, ok := publishmeta.FromCargoCrateAPI(body); ok {
		umeta.PublishedAt = t
	}
}

func (h *Handler) crateVersionAPIBody(ctx context.Context, name, version string) ([]byte, bool) {
	apiRef := artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     "api/v1/crates/" + name + "/" + version,
		Version:  "api",
		Mutable:  true,
	}
	ups := []upstream.Upstream{{
		Name: "crates-io-api", BaseURL: defaultAPIBase, Priority: 1, Official: true,
	}}
	rc, _, err := h.upstreamClt.Fetch(ctx, apiRef, ups)
	if err != nil || rc == nil {
		return nil, false
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, maxMetaBody+1))
	if err != nil || len(body) == 0 || len(body) > maxMetaBody {
		return nil, false
	}
	return body, true
}
