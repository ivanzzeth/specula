package conda

import (
	"strings"

	"github.com/ivanzzeth/specula/internal/upstream"
)

// ChannelMap maps a channel name (e.g. "conda-forge") to its channel-root upstream.
type ChannelMap map[string]upstream.Upstream

// ChannelSpec is the config-facing form of one allowlisted channel.
type ChannelSpec struct {
	Name    string
	BaseURL string
}

// WithChannels configures the per-channel root allowlist.
// Empty / nil keeps legacy behavior (full path under cloud-root upstreams).
func WithChannels(channels ChannelMap) Option {
	return func(h *Handler) { h.channels = channels }
}

// ChannelsFromSpecs builds the allowlist map from config-like specs.
func ChannelsFromSpecs(specs []ChannelSpec) ChannelMap {
	out := make(ChannelMap, len(specs))
	for _, s := range specs {
		name := strings.Trim(strings.TrimSpace(s.Name), "/")
		base := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
		if name == "" || base == "" {
			continue
		}
		out[name] = upstream.Upstream{
			Name:     "conda:" + name,
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
//   - no channels configured → default upstreams + full path
//   - allowlisted channel prefix → that channel's BaseURL + stripped remainder
//   - unknown channel with channels set → ok=false (404)
func (h *Handler) upstreamForPath(relPath string) (ups []upstream.Upstream, fetchName string, ok bool) {
	relPath = strings.Trim(relPath, "/")
	if len(h.channels) == 0 {
		if len(h.upstreams) == 0 {
			return nil, "", false
		}
		return h.upstreams, relPath, true
	}
	i := strings.IndexByte(relPath, '/')
	if i <= 0 || i == len(relPath)-1 {
		return nil, "", false
	}
	ch, rest := relPath[:i], relPath[i+1:]
	up, hit := h.channels[ch]
	if !hit || up.BaseURL == "" {
		return nil, "", false
	}
	return []upstream.Upstream{up}, rest, true
}
