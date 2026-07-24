package admin

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/sbom"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

func TestHandleSBOM_CycloneDXInventory(t *testing.T) {
	h, m := newCacheHarness(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	m.seed(
		entry("npm", "ms", "2.1.3", 10, artifact.TierConsensus, "npmmirror", base),
		meta.Entry{CacheEntry: artifact.CacheEntry{
			Ref:      artifact.ArtifactRef{Protocol: "npm", Name: "ms", Version: "packument", Mutable: true},
			Digest:   "sha256:packument",
			Protocol: "npm",
			Tier:     artifact.TierChecksum,
		}},
		entry("pypi", "requests", "2.31.0", 50, artifact.TierTofu, "tuna", base.Add(time.Hour)),
	)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/admin/sbom", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Header().Get("Content-Type"), "cyclonedx")

	var doc sbom.Document
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &doc))
	assert.Equal(t, "CycloneDX", doc.BOMFormat)
	assert.Equal(t, sbom.SpecVersion, doc.SpecVersion)
	require.Len(t, doc.Components, 2, "mutable packument must be omitted")

	names := map[string]bool{}
	for _, c := range doc.Components {
		names[c.Name] = true
		assert.NotEmpty(t, c.PURL)
	}
	assert.True(t, names["ms"])
	assert.True(t, names["requests"])
}

func TestHandleSBOM_ProtocolFilter(t *testing.T) {
	h, m := newCacheHarness(t)
	seedCache(m)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/admin/sbom?protocol=pypi", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var doc sbom.Document
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &doc))
	require.Len(t, doc.Components, 1)
	assert.Equal(t, "requests", doc.Components[0].Name)
}

func TestHandleSBOM_RejectsBadFormatAndGit(t *testing.T) {
	h, _ := newCacheHarness(t)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/admin/sbom?format=spdx", tok, nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	rr = h.do("GET", "/api/v1/admin/sbom?protocol=git", tok, nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestHandleSBOM_RequiresMeta(t *testing.T) {
	h := newHarness(t)
	h.srv.meta = nil
	_, tok := h.mustCreateAdmin(t)
	rr := h.do("GET", "/api/v1/admin/sbom", tok, nil)
	assert.Equal(t, http.StatusNotImplemented, rr.Code)
}
