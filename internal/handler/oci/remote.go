package oci

import (
	"sort"
	"strings"

	"github.com/ivanzzeth/specula/internal/upstream"
)

// RemoteRegistryMap maps a hostname (lowercase) to the ordered upstream chain
// used for path-style multi-registry pulls (/v2/<host>/<repo>/…).
//
// When CN mirrors are configured, the chain is mirror(s) → official origin so
// DaoCloud allowlist 403s (and similar) fall through — same precedence as Hub's
// daocloud→1ms→docker-hub chain.
type RemoteRegistryMap map[string][]upstream.Upstream

// remoteRegistry is the unexported alias used inside the handler.
type remoteRegistry = RemoteRegistryMap

// WithRemoteRegistries configures the SSRF allowlist of non-Hub registries.
// Empty / nil disables multi-registry path-style pulls (unknown host prefixes
// that look like registry hosts are rejected with 404).
func WithRemoteRegistries(regs remoteRegistry) Option {
	return func(h *Handler) { h.remoteRegs = regs }
}

// RemoteUpstreamSpec is one mirror in a remote-registry chain (config→handler
// wiring without importing internal/config).
type RemoteUpstreamSpec struct {
	Name     string
	BaseURL  string
	Priority int
}

// RemoteRegistrySpec is a host + optional mirror chain used when wiring the
// handler from config.
type RemoteRegistrySpec struct {
	Host      string
	BaseURL   string // legacy single-mirror shorthand
	Upstreams []RemoteUpstreamSpec
}

// RemoteRegistriesFromSpecs builds the allowlist map from config-like specs.
//
// Chain construction:
//  1. BaseURL (if set and not already origin) as an early mirror
//  2. Upstreams sorted by Priority ascending (stable; unset Priority → after
//     named priorities, in declaration order among themselves)
//  3. Always append https://<host> as official origin (unless the only source
//     is already origin)
//
// Duplicate BaseURLs are dropped (first wins). Empty BaseURL + empty Upstreams
// → origin-only chain.
func RemoteRegistriesFromSpecs(specs []RemoteRegistrySpec) RemoteRegistryMap {
	out := make(RemoteRegistryMap, len(specs))
	for _, s := range specs {
		host := strings.ToLower(strings.TrimSpace(s.Host))
		if host == "" {
			continue
		}
		origin := "https://" + host
		type cand struct {
			name string
			url  string
			pri  int
			ord  int // declaration order for stable sort
		}
		cands := make([]cand, 0, 1+len(s.Upstreams))
		ord := 0
		if base := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/"); base != "" && base != origin {
			cands = append(cands, cand{
				name: "mirror",
				url:  base,
				pri:  0, // before explicit upstreams with priority≥1
				ord:  ord,
			})
			ord++
		}
		for _, u := range s.Upstreams {
			url := strings.TrimRight(strings.TrimSpace(u.BaseURL), "/")
			if url == "" || url == origin {
				continue
			}
			name := strings.TrimSpace(u.Name)
			if name == "" {
				name = "mirror"
			}
			pri := u.Priority
			if pri == 0 {
				// Unset priority: after BaseURL shorthand (0) and after any
				// explicitly prioritised entries; keep declaration order.
				pri = 1000 + ord
			}
			cands = append(cands, cand{name: name, url: url, pri: pri, ord: ord})
			ord++
		}
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].pri != cands[j].pri {
				return cands[i].pri < cands[j].pri
			}
			return cands[i].ord < cands[j].ord
		})

		seen := make(map[string]struct{}, len(cands)+1)
		chain := make([]upstream.Upstream, 0, len(cands)+1)
		pri := 1
		for _, c := range cands {
			if _, dup := seen[c.url]; dup {
				continue
			}
			seen[c.url] = struct{}{}
			chain = append(chain, upstream.Upstream{
				Name:     "remote:" + host + ":" + c.name,
				BaseURL:  c.url,
				Priority: pri,
				Official: false,
			})
			pri++
		}
		if _, hasOrigin := seen[origin]; !hasOrigin {
			chain = append(chain, upstream.Upstream{
				Name:     "remote:" + host,
				BaseURL:  origin,
				Priority: pri,
				Official: true,
			})
		} else if len(chain) == 0 {
			// BaseURL/Upstreams pointed only at origin — still need a chain.
			chain = []upstream.Upstream{{
				Name:     "remote:" + host,
				BaseURL:  origin,
				Priority: 1,
				Official: true,
			}}
		} else {
			// Origin already listed as a "mirror"; mark the last matching entry official.
			for i := range chain {
				if chain[i].BaseURL == origin {
					chain[i].Official = true
					chain[i].Name = "remote:" + host
				}
			}
		}
		// If somehow empty (shouldn't happen), origin-only.
		if len(chain) == 0 {
			chain = []upstream.Upstream{{
				Name:     "remote:" + host,
				BaseURL:  origin,
				Priority: 1,
				Official: true,
			}}
		}
		out[host] = chain
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
	chain, allowed := regs[first]
	if !allowed || len(chain) == 0 || chain[0].BaseURL == "" {
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
//   - allowlisted host prefix → remote chain (mirrors→origin) + stripped repo
//   - host-looking but not allowlisted → ok=false (caller should 404)
//   - otherwise → Hub chain + full name
func (h *Handler) upstreamForName(imageName string) (ups []upstream.Upstream, fetchName string, ok bool) {
	if host, repo, hit := parseRemoteName(imageName, h.remoteRegs); hit {
		return h.remoteRegs[host], repo, true
	}
	if looksLikeRegistryHost(imageName) {
		return nil, "", false
	}
	if len(h.upstreams) == 0 {
		return nil, "", false
	}
	return h.upstreams, imageName, true
}
