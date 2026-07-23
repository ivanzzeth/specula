package helm

import (
	"strings"

	"github.com/ivanzzeth/specula/internal/upstream"
)

// RepositoryMap maps a classic-HTTP repo name to its chart-root upstream.
type RepositoryMap map[string]upstream.Upstream

// RepositorySpec is the config-facing form of one allowlisted Helm repo.
type RepositorySpec struct {
	Name    string
	BaseURL string
}

// WithRepositories configures the multi-repo allowlist.
// Empty / nil keeps legacy behavior (repo segment is a subpath under upstreams).
func WithRepositories(repos RepositoryMap) Option {
	return func(h *Handler) { h.repos = repos }
}

// RepositoriesFromSpecs builds the allowlist map from config-like specs.
func RepositoriesFromSpecs(specs []RepositorySpec) RepositoryMap {
	out := make(RepositoryMap, len(specs))
	for _, s := range specs {
		name := strings.Trim(strings.TrimSpace(s.Name), "/")
		base := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
		if name == "" || base == "" {
			continue
		}
		out[name] = upstream.Upstream{
			Name:     "helm:" + name,
			BaseURL:  base,
			Priority: 1,
			Official: true,
		}
	}
	return out
}

// upstreamForRepo returns the upstream chain and the repository name to use in
// upstream Fetch paths. Cache keys keep the full repo; only the Fetch
// ArtifactRef.Name is stripped when a named source is used.
//
//   - no repositories configured → default upstreams + full repo path
//   - empty repo (flat layout) → default upstreams + empty name
//   - allowlisted name → that repo's BaseURL + empty fetch name (root)
//   - unknown name with repositories set → ok=false (404)
func (h *Handler) upstreamForRepo(repo string) (ups []upstream.Upstream, fetchName string, ok bool) {
	repo = strings.Trim(repo, "/")
	if len(h.repos) == 0 {
		if len(h.upstreams) == 0 {
			return nil, "", false
		}
		return h.upstreams, repo, true
	}
	if repo == "" {
		if len(h.upstreams) == 0 {
			return nil, "", false
		}
		return h.upstreams, "", true
	}
	// Peel first path segment as the allowlist key (supports nested chart paths
	// under a named repo: bitnami/extra/chart.tgz → still keyed as "bitnami").
	key := repo
	rest := ""
	if i := strings.IndexByte(repo, '/'); i > 0 {
		key = repo[:i]
		rest = repo[i+1:]
	}
	up, hit := h.repos[key]
	if !hit || up.BaseURL == "" {
		return nil, "", false
	}
	return []upstream.Upstream{up}, rest, true
}
