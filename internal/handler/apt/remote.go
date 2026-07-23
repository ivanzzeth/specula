package apt

import (
	"strings"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// RepositoryMap maps an archive name (e.g. "ubuntu") to its upstream root.
type RepositoryMap map[string]upstream.Upstream

// RepositorySpec is the config-facing form of one allowlisted archive.
type RepositorySpec struct {
	Name    string
	BaseURL string
}

// WithRepositories configures the multi-archive allowlist.
// Empty / nil keeps legacy behavior (any path prefix is cache-scoped only;
// fetch always uses the default upstreams chain).
func WithRepositories(repos RepositoryMap) Option {
	return func(h *Handler) { h.repos = repos }
}

// RepositoriesFromSpecs builds the allowlist map from config-like specs.
func RepositoriesFromSpecs(specs []RepositorySpec) RepositoryMap {
	out := make(RepositoryMap, len(specs))
	for _, s := range specs {
		name := strings.ToLower(strings.TrimSpace(s.Name))
		base := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
		if name == "" || base == "" {
			continue
		}
		out[name] = upstream.Upstream{
			Name:     "apt:" + name,
			BaseURL:  base,
			Priority: 1,
			Official: true,
		}
	}
	return out
}

// selectUpstreams returns the upstream chain for an archive prefix.
//
//   - no repositories configured → default upstreams (legacy)
//   - empty repo prefix → default upstreams
//   - allowlisted name → that archive's BaseURL
//   - unknown name with repositories set → ok=false (404)
func (h *Handler) selectUpstreams(repo string) (ups []upstream.Upstream, ok bool) {
	repo = strings.ToLower(strings.Trim(repo, "/"))
	if len(h.repos) == 0 {
		if len(h.upstreams) == 0 {
			return nil, false
		}
		return h.upstreams, true
	}
	if repo == "" {
		if len(h.upstreams) == 0 {
			return nil, false
		}
		return h.upstreams, true
	}
	up, hit := h.repos[repo]
	if !hit || up.BaseURL == "" {
		return nil, false
	}
	return []upstream.Upstream{up}, true
}

// poolCacheName scopes pool CAS keys by archive when a repo prefix is present.
func poolCacheName(repo, name string) string {
	repo = strings.Trim(repo, "/")
	if repo == "" {
		return name
	}
	return repo + "/" + name
}

// poolFetchRef builds the upstream Fetch ref (unscoped pool directory).
func poolFetchRef(name, file string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     name,
		Version:  file,
		Mutable:  false,
	}
}
