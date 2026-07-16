package admin

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/ivanzzeth/specula/internal/config"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// unixOrZero renders a time as Unix seconds, mapping the zero time to 0 so the
// UI can distinguish "never happened" from a real timestamp.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// configUpstreams converts a protocol's configured mirror list into the
// upstream package type. Config remains the declarative source of truth for
// which mirrors exist; the Runtime only holds the operator's runtime delta.
func configUpstreams(pc config.ProtocolConfig) []upstream.Upstream {
	out := make([]upstream.Upstream, 0, len(pc.Upstreams))
	for _, u := range pc.Upstreams {
		out = append(out, upstream.Upstream{
			Name:     u.Name,
			BaseURL:  u.BaseURL,
			Priority: u.Priority,
			Official: u.Official,
		})
	}
	return out
}

// toUpstreamHealth projects one measured mirror onto the wire contract. order is
// its 0-based position in the effective chain; totalServed is the protocol's
// denominator for HitShare.
func toUpstreamHealth(m upstream.MirrorState, order int, totalServed int64) UpstreamHealth {
	h := UpstreamHealth{
		Protocol:            m.Protocol,
		Name:                m.Name,
		URL:                 m.BaseURL,
		Official:            m.Official,
		Order:               order,
		Priority:            m.Priority,
		ConfigPriority:      m.ConfigPriority,
		Overridden:          m.Overridden,
		Enabled:             m.Enabled,
		Health:              string(m.Health),
		Blocked:             m.Blocked,
		BlockedUntilUnix:    unixOrZero(m.BlockedUntil),
		ConsecutiveFailures: m.ConsecutiveFailures,
		LastErr:             m.LastErr,
		HasLatency:          m.LastLatencyValid,
		ServedCount:         m.ServedCount,
		LastServedUnix:      unixOrZero(m.LastServedAt),
	}
	if m.LastLatencyValid {
		h.LastLatencyMs = m.LastLatency.Milliseconds()
	}
	// Guard the denominator: with no traffic every share is 0, not NaN.
	if totalServed > 0 {
		h.HitShare = float64(m.ServedCount) / float64(totalServed)
	}
	return h
}

// protocolChain renders one protocol's complete mirror chain. rt may be nil,
// which yields a config-only echo with Live=false — the honest representation of
// "these mirrors are configured but nothing is measuring them".
func protocolChain(protocol string, pc config.ProtocolConfig, rt *upstream.Runtime) ProtocolUpstreams {
	configured := configUpstreams(pc)
	out := ProtocolUpstreams{Protocol: protocol, Live: rt != nil}

	if rt == nil {
		// No instrumentation: report identity + config order only. Every health
		// and measurement field stays at its zero value, and Live=false tells
		// the UI to render them as "—" rather than as facts.
		sort.SliceStable(configured, func(i, j int) bool {
			if configured[i].Priority != configured[j].Priority {
				return configured[i].Priority < configured[j].Priority
			}
			return configured[i].Name < configured[j].Name
		})
		out.Mirrors = make([]UpstreamHealth, 0, len(configured))
		for i, u := range configured {
			out.Mirrors = append(out.Mirrors, UpstreamHealth{
				Protocol:       protocol,
				Name:           u.Name,
				URL:            u.BaseURL,
				Official:       u.Official,
				Order:          i,
				Priority:       u.Priority,
				ConfigPriority: u.Priority,
				Enabled:        true,
				Health:         string(upstream.HealthUnknown),
			})
		}
		return out
	}

	states := rt.Snapshot(configured)

	var latest time.Time
	for _, m := range states {
		out.TotalServed += m.ServedCount
		if m.ServedCount > 0 && m.LastServedAt.After(latest) {
			latest = m.LastServedAt
			out.LastServedBy = m.Name
		}
	}

	out.Mirrors = make([]UpstreamHealth, 0, len(states))
	for i, m := range states {
		out.Mirrors = append(out.Mirrors, toUpstreamHealth(m, i, out.TotalServed))
	}
	return out
}

// runtimeFor returns the Runtime for a configured protocol, or nil when no
// registry is wired in at all.
//
// It uses Runtime (get-or-create), not Lookup: a Runtime is created lazily on
// the protocol's first fetch, so a freshly started instance has none yet. That
// absence means "no traffic", which the per-mirror Health="unknown" already
// says — it does not mean "not instrumented". Reporting Live=false for it would
// tell the UI to hide fields that are in fact being measured, and would flip to
// true on the first request, which is indistinguishable from a config change.
func (s *Server) runtimeFor(protocol string) *upstream.Runtime {
	if s.upstreams == nil {
		return nil
	}
	return s.upstreams.Runtime(protocol)
}

// handleUpstreams → GET /api/v1/admin/upstreams. Returns UpstreamsResponse:
// every configured protocol's ordered mirror chain with live health, latency,
// serve counts and the operator's runtime overrides (REGISTRY-DESIGN §5.3).
//
// Config enumerates the protocols (it is the declarative baseline); the upstream
// Registry supplies what has actually been measured for each.
func (s *Server) handleUpstreams(w http.ResponseWriter, r *http.Request) {
	resp := UpstreamsResponse{Protocols: []ProtocolUpstreams{}}
	if s.cfg == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	protocols := make([]string, 0, len(s.cfg.Protocols))
	for p := range s.cfg.Protocols {
		protocols = append(protocols, p)
	}
	sort.Strings(protocols)

	for _, p := range protocols {
		resp.Protocols = append(resp.Protocols,
			protocolChain(p, s.cfg.Protocols[p], s.runtimeFor(p)))
	}
	writeJSON(w, http.StatusOK, resp)
}

// lookupProtocolConfig resolves the {protocol} path segment against config,
// writing a 404 when it is not a configured protocol.
func (s *Server) lookupProtocolConfig(w http.ResponseWriter, protocol string) (config.ProtocolConfig, bool) {
	if s.cfg == nil {
		writeError(w, http.StatusNotImplemented, "config not available")
		return config.ProtocolConfig{}, false
	}
	pc, ok := s.cfg.Protocols[protocol]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown protocol")
		return config.ProtocolConfig{}, false
	}
	return pc, true
}

// requireUpstreamRuntime resolves the mutable-endpoint preconditions: a wired
// registry and a configured protocol. Mutating a chain requires a live Runtime —
// without one there is nothing for an override to take effect on, and accepting
// the request would be a silent no-op.
func (s *Server) requireUpstreamRuntime(w http.ResponseWriter, protocol string) (*upstream.Runtime, config.ProtocolConfig, bool) {
	if s.upstreams == nil {
		writeError(w, http.StatusNotImplemented, "upstream runtime not configured")
		return nil, config.ProtocolConfig{}, false
	}
	pc, ok := s.lookupProtocolConfig(w, protocol)
	if !ok {
		return nil, config.ProtocolConfig{}, false
	}
	// Runtime (not Lookup): the protocol is configured, so an operator may steer
	// it even before its first fetch has created the Runtime lazily.
	return s.upstreams.Runtime(protocol), pc, true
}

// mirrorNames returns the configured mirror names for a protocol.
func mirrorNames(pc config.ProtocolConfig) map[string]struct{} {
	out := make(map[string]struct{}, len(pc.Upstreams))
	for _, u := range pc.Upstreams {
		out[u.Name] = struct{}{}
	}
	return out
}

// handleReorderUpstreams → POST /api/v1/admin/upstreams/{protocol}/reorder.
// Body: ReorderUpstreamsRequest. Returns the protocol's updated chain
// (ProtocolUpstreams).
//
// The request must name every configured mirror exactly once. A partial list is
// rejected rather than merged: unlisted mirrors would keep their config
// priority and could interleave with the requested positions, producing an
// order the operator never asked for.
func (s *Server) handleReorderUpstreams(w http.ResponseWriter, r *http.Request) {
	protocol := r.PathValue("protocol")
	rt, pc, ok := s.requireUpstreamRuntime(w, protocol)
	if !ok {
		return
	}

	var req ReorderUpstreamsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	known := mirrorNames(pc)
	if len(req.Order) != len(known) {
		writeError(w, http.StatusBadRequest,
			"order must list every configured mirror for this protocol exactly once")
		return
	}
	for _, name := range req.Order {
		if _, found := known[name]; !found {
			writeError(w, http.StatusBadRequest, "unknown mirror: "+name)
			return
		}
	}

	// Runtime.Reorder rejects duplicates; combined with the length and
	// membership checks above, that makes req.Order a true permutation.
	if err := rt.Reorder(req.Order); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("admin: upstream chain reordered", "protocol", protocol, "order", req.Order)
	writeJSON(w, http.StatusOK, protocolChain(protocol, pc, rt))
}

// handlePatchUpstream → PATCH /api/v1/admin/upstreams/{protocol}/{id}.
// Body: PatchUpstreamRequest. {id} is the mirror's config name.
// Returns the protocol's updated chain (ProtocolUpstreams).
func (s *Server) handlePatchUpstream(w http.ResponseWriter, r *http.Request) {
	protocol := r.PathValue("protocol")
	rt, pc, ok := s.requireUpstreamRuntime(w, protocol)
	if !ok {
		return
	}
	name := r.PathValue("id")
	if _, found := mirrorNames(pc)[name]; !found {
		writeError(w, http.StatusNotFound, "unknown mirror")
		return
	}

	var req PatchUpstreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Enabled != nil {
		rt.SetEnabled(name, *req.Enabled)
		s.log.Info("admin: upstream mirror toggled",
			"protocol", protocol, "mirror", name, "enabled", *req.Enabled)
	}
	writeJSON(w, http.StatusOK, protocolChain(protocol, pc, rt))
}

// handleUnblockUpstream → POST /api/v1/admin/upstreams/{protocol}/{id}/unblock.
// Returns the protocol's updated chain (ProtocolUpstreams).
//
// Clears the auto-block circuit breaker and the failure streak, re-admitting the
// mirror to the fallback chain immediately instead of waiting out its block
// window. It is idempotent: unblocking a healthy mirror is a no-op.
func (s *Server) handleUnblockUpstream(w http.ResponseWriter, r *http.Request) {
	protocol := r.PathValue("protocol")
	rt, pc, ok := s.requireUpstreamRuntime(w, protocol)
	if !ok {
		return
	}
	name := r.PathValue("id")
	if _, found := mirrorNames(pc)[name]; !found {
		writeError(w, http.StatusNotFound, "unknown mirror")
		return
	}

	rt.Unblock(name)
	s.log.Info("admin: upstream mirror unblocked", "protocol", protocol, "mirror", name)
	writeJSON(w, http.StatusOK, protocolChain(protocol, pc, rt))
}
