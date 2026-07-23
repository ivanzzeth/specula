package cargo

import (
	"strings"

	"github.com/ivanzzeth/specula/internal/upstream"
)

// RegistryMap maps a registry name (e.g. "crates-io") to its sparse-index upstream root.
type RegistryMap map[string]upstream.Upstream

// RegistrySpec is the config-facing form of one allowlisted registry.
type RegistrySpec struct {
	Name    string
	BaseURL string
}

// WithRegistries configures the multi-registry allowlist.
// Empty / nil keeps legacy behavior (full path under default upstreams).
func WithRegistries(regs RegistryMap) Option {
	return func(h *Handler) { h.registries = regs }
}

// RegistriesFromSpecs builds the allowlist map from config-like specs.
func RegistriesFromSpecs(specs []RegistrySpec) RegistryMap {
	out := make(RegistryMap, len(specs))
	for _, s := range specs {
		name := strings.Trim(strings.TrimSpace(s.Name), "/")
		base := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
		if name == "" || base == "" {
			continue
		}
		out[name] = upstream.Upstream{
			Name:     "cargo:" + name,
			BaseURL:  base,
			Priority: 1,
			Official: true,
		}
	}
	return out
}

// upstreamForPath returns the upstream chain and the path to use in upstream
// Fetch. Cache keys keep the full relative path; only the Fetch Name is stripped.
//
//   - no registries configured → default upstreams + full path
//   - allowlisted registry prefix → that registry's BaseURL + stripped remainder
//   - unknown registry with registries set → ok=false (404)
func (h *Handler) upstreamForPath(relPath string) (ups []upstream.Upstream, fetchName string, ok bool) {
	relPath = strings.Trim(relPath, "/")
	if len(h.registries) == 0 {
		if len(h.upstreams) == 0 {
			return nil, "", false
		}
		return h.upstreams, relPath, true
	}
	i := strings.IndexByte(relPath, '/')
	if i <= 0 || i == len(relPath)-1 {
		return nil, "", false
	}
	reg, rest := relPath[:i], relPath[i+1:]
	up, hit := h.registries[reg]
	if !hit || up.BaseURL == "" {
		return nil, "", false
	}
	return []upstream.Upstream{up}, rest, true
}
