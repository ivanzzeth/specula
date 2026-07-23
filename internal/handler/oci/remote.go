package oci

import (
	"strings"

	"github.com/ivanzzeth/specula/internal/upstream"
)

// RemoteRegistryMap maps a hostname (lowercase) to the upstream used for
// path-style multi-registry pulls (/v2/<host>/<repo>/…).
type RemoteRegistryMap map[string]upstream.Upstream

// remoteRegistry is the unexported alias used inside the handler.
type remoteRegistry = RemoteRegistryMap

// WithRemoteRegistries configures the SSRF allowlist of non-Hub registries.
// Empty / nil disables multi-registry path-style pulls (unknown host prefixes
// that look like registry hosts are rejected with 404).
func WithRemoteRegistries(regs remoteRegistry) Option {
	return func(h *Handler) { h.remoteRegs = regs }
}

// RemoteRegistrySpec is a host + optional base URL used when wiring the handler
// from config (avoids importing internal/config into the handler package).
type RemoteRegistrySpec struct {
	Host    string
	BaseURL string
}

// RemoteRegistriesFromSpecs builds the allowlist map from config-like specs.
func RemoteRegistriesFromSpecs(specs []RemoteRegistrySpec) RemoteRegistryMap {
	out := make(RemoteRegistryMap, len(specs))
	for _, s := range specs {
		host := strings.ToLower(strings.TrimSpace(s.Host))
		if host == "" {
			continue
		}
		base := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
		if base == "" {
			base = "https://" + host
		}
		out[host] = upstream.Upstream{
			Name:     "remote:" + host,
			BaseURL:  base,
			Priority: 1,
			Official: true,
		}
	}
	return out
}

// parseRemoteName splits imageName into (host, repo) when the first path
// segment is an allowlisted registry host. ok is false when the name is not
// a remote-prefixed pull (use the Hub upstream chain instead).
func parseRemoteName(imageName string, regs remoteRegistry) (host, repo string, ok bool) {
	if len(regs) == 0 {
		return "", "", false
	}
	name := strings.Trim(imageName, "/")
	i := strings.IndexByte(name, '/')
	if i <= 0 || i == len(name)-1 {
		return "", "", false
	}
	first := strings.ToLower(name[:i])
	up, allowed := regs[first]
	if !allowed || up.BaseURL == "" {
		return "", "", false
	}
	return first, name[i+1:], true
}

// looksLikeRegistryHost reports whether the first path segment appears to be a
// registry hostname (contains a dot). Used to reject non-allowlisted remote
// prefixes instead of forwarding them to the Hub chain (SSRF / wrong-upstream).
func looksLikeRegistryHost(imageName string) bool {
	name := strings.Trim(imageName, "/")
	i := strings.IndexByte(name, '/')
	if i <= 0 {
		return false
	}
	first := name[:i]
	return strings.Contains(first, ".")
}

// upstreamForName returns the upstream chain and the repository name to use in
// upstream Fetch paths. Cache keys keep the full imageName; only the Fetch
// ArtifactRef.Name is stripped.
//
//   - allowlisted host prefix → single remote upstream + stripped repo
//   - host-looking but not allowlisted → ok=false (caller should 404)
//   - otherwise → Hub chain + full name
func (h *Handler) upstreamForName(imageName string) (ups []upstream.Upstream, fetchName string, ok bool) {
	if host, repo, hit := parseRemoteName(imageName, h.remoteRegs); hit {
		return []upstream.Upstream{h.remoteRegs[host]}, repo, true
	}
	if looksLikeRegistryHost(imageName) {
		return nil, "", false
	}
	if len(h.upstreams) == 0 {
		return nil, "", false
	}
	return h.upstreams, imageName, true
}
