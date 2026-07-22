package upstream

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Health is the operator-facing state of one upstream mirror.
type Health string

const (
	// HealthUp — the mirror last answered successfully and is not blocked.
	HealthUp Health = "up"
	// HealthBlocked — the auto-block circuit breaker has tripped; the mirror is
	// skipped entirely until its block window elapses.
	HealthBlocked Health = "blocked"
	// HealthProbing — the mirror has accumulated consecutive failures but has
	// not yet crossed the auto-block threshold, so it is still being tried.
	HealthProbing Health = "probing"
	// HealthUnknown — the mirror has never been contacted since this process
	// started, so there is nothing to report. This is deliberately distinct
	// from "up": reporting an unqueried mirror as healthy would be a guess.
	HealthUnknown Health = "unknown"
)

// MirrorState is the full observable state of one upstream mirror within a
// protocol's fallback chain: its configured identity, the operator's runtime
// overrides, and what has actually been measured.
//
// Fields that have not been measured are reported via their *Valid companion
// rather than a zero value, so a caller can render "—" instead of inventing
// "0 ms" or "never".
type MirrorState struct {
	// Protocol is the owning protocol ("oci", "pypi", …).
	Protocol string
	// Name is the mirror's logical name from config ("daocloud", "npmmirror").
	Name string
	// BaseURL is the mirror's configured base URL.
	BaseURL string
	// Official reports whether config marks this as the authoritative origin.
	Official bool

	// ConfigPriority is the priority declared in the YAML baseline.
	ConfigPriority int
	// Priority is the effective priority after any runtime reorder. It equals
	// ConfigPriority when no override is set.
	Priority int
	// Overridden is true when Priority came from a runtime reorder rather than
	// config, so the UI can flag "drifted from the declarative baseline".
	Overridden bool
	// Enabled is false when an operator has disabled the mirror at runtime; a
	// disabled mirror is skipped by the fallback chain.
	Enabled bool

	// Health summarises Blocked / failure state. See the Health constants.
	Health Health
	// Blocked mirrors Health == HealthBlocked.
	Blocked bool
	// BlockedUntil is when the auto-block window expires. Zero when not blocked.
	BlockedUntil time.Time
	// ConsecutiveFailures is the current transient-failure streak (reset on any
	// success).
	ConsecutiveFailures int
	// LastErr is the most recent failure message; empty when none has occurred
	// since the last success.
	LastErr string

	// LastLatency is how long the most recent successful request to this mirror
	// took to return response headers (NOT the full body transfer: bodies are
	// streamed to the client, so total transfer time is the client's download
	// speed, not the mirror's responsiveness).
	LastLatency time.Duration
	// LastLatencyValid is false when no successful request has been measured
	// yet; LastLatency is meaningless in that case.
	LastLatencyValid bool

	// ServedCount is how many cache-miss fetches this mirror has successfully
	// served since process start. It is an in-memory counter: it resets on
	// restart and is per-replica, not cluster-wide.
	ServedCount int64
	// LastServedAt is when this mirror last served a fetch. Zero when never.
	LastServedAt time.Time
}

// mirrorStat is the mutable measurement record for one mirror.
type mirrorStat struct {
	servedCount  int64
	lastServedAt time.Time
	lastLatency  time.Duration
	hasLatency   bool
	lastErr      string
}

// override is an operator's runtime deviation from the YAML baseline.
type override struct {
	disabled    bool
	priority    int
	hasPriority bool
}

// Runtime holds one protocol's live upstream state: the auto-block circuit
// breaker, per-mirror measurements, and the operator's runtime overrides
// (enable/disable, reorder).
//
// # Why per-protocol
//
// A Runtime is scoped to a single protocol because mirror names are only unique
// within one — every protocol has its own fallback chain, and each protocol
// handler already constructs its own Client. Sharing one Runtime across
// protocols would let two chains collide on a common name (e.g. a mirror called
// "official" in both) and cross-contaminate their block state.
//
// # Config is the baseline, overrides are the delta
//
// Runtime never stores the mirror list itself; the YAML config remains the
// declarative source of truth and is passed in on each call. Runtime only holds
// the delta an operator applied at runtime, so a config reload cannot be
// silently overwritten by stale in-memory state, and a restart returns to the
// declared baseline.
//
// All methods are safe for concurrent use.
type Runtime struct {
	protocol string
	blocker  *blockTracker

	mu        sync.Mutex
	stats     map[string]*mirrorStat
	overrides map[string]*override
}

// NewRuntime constructs an empty Runtime for one protocol.
func NewRuntime(protocol string) *Runtime {
	return &Runtime{
		protocol:  protocol,
		blocker:   newBlockTracker(),
		stats:     make(map[string]*mirrorStat),
		overrides: make(map[string]*override),
	}
}

// NewRuntimeWithBlockPersister constructs a Runtime whose auto-block state is
// persisted via persister (shared across HA replicas when backed by Postgres).
func NewRuntimeWithBlockPersister(protocol string, persister BlockPersister) *Runtime {
	return &Runtime{
		protocol:  protocol,
		blocker:   newBlockTrackerWithPersister(persister, defaultMaxFailures, defaultBlockDuration),
		stats:     make(map[string]*mirrorStat),
		overrides: make(map[string]*override),
	}
}

// Protocol returns the protocol this Runtime is scoped to.
func (r *Runtime) Protocol() string { return r.protocol }

// statLocked returns (creating if needed) the stat record for name.
// Caller must hold r.mu.
func (r *Runtime) statLocked(name string) *mirrorStat {
	s, ok := r.stats[name]
	if !ok {
		s = &mirrorStat{}
		r.stats[name] = s
	}
	return s
}

// overrideLocked returns (creating if needed) the override record for name.
// Caller must hold r.mu.
func (r *Runtime) overrideLocked(name string) *override {
	o, ok := r.overrides[name]
	if !ok {
		o = &override{}
		r.overrides[name] = o
	}
	return o
}

// RecordServe records a successful fetch from a mirror: its latency, its serve
// counter, and the clearing of any prior error and failure streak.
//
// A Client built with NewClientWithRuntime calls this for you. It is exported so
// that a protocol which does not use the generic fallback Client — git, which
// reverse-proxies its allowed upstreams directly — can still report into the
// same operator view rather than being a blind spot in it.
//
// latency should measure time-to-response-headers (upstream responsiveness),
// not body transfer.
func (r *Runtime) RecordServe(name string, latency time.Duration) {
	r.blocker.recordSuccess(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.statLocked(name)
	s.servedCount++
	s.lastServedAt = time.Now()
	s.lastLatency = latency
	s.hasLatency = true
	s.lastErr = ""
}

// RecordFailure records a failed fetch from a mirror: its reason, and — when
// transient (5xx / 429 / network error, as opposed to a 4xx that says the
// request itself was wrong) — a tick of the consecutive-failure streak that
// drives auto-blocking.
//
// Exported for the same reason as RecordServe. Returns true when this failure
// tripped the circuit breaker.
func (r *Runtime) RecordFailure(name string, err error, transient bool) bool {
	if err != nil {
		r.mu.Lock()
		r.statLocked(name).lastErr = err.Error()
		r.mu.Unlock()
	}
	if !transient {
		return false
	}
	return r.blocker.recordFailure(name)
}

// Effective returns the mirrors that will actually be tried for the given config
// baseline, in fallback order, with the operator's runtime overrides applied
// (disabled mirrors removed, reordered priorities substituted).
//
// This is the chain the fetch path uses. It is exported so an operator view can
// answer "what will the next miss actually hit", which is not always what config
// says once overrides are in play.
//
// The input slice is never mutated.
func (r *Runtime) Effective(configured []Upstream) []Upstream {
	return r.effective(configured)
}

// SetEnabled enables or disables a mirror at runtime. A disabled mirror is
// skipped by the fallback chain entirely — it is not tried, so it also never
// recovers a "blocked" state on its own.
func (r *Runtime) SetEnabled(name string, enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overrideLocked(name).disabled = !enabled
}

// Unblock clears the auto-block state and failure streak for a mirror,
// re-admitting it to the fallback chain immediately rather than waiting out the
// block window.
func (r *Runtime) Unblock(name string) {
	r.blocker.recordSuccess(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statLocked(name).lastErr = ""
}

// Reorder sets the fallback priority of the named mirrors to their position in
// the given slice (index 0 tried first). Names not listed keep their config
// priority, which is why the list must be complete for the resulting order to
// be fully determined.
//
// It returns an error when order contains a duplicate name — an ambiguous
// request that would otherwise silently apply a last-write-wins priority.
// Validating names against config is the caller's job (Runtime intentionally
// does not hold the mirror list).
func (r *Runtime) Reorder(order []string) error {
	seen := make(map[string]struct{}, len(order))
	for _, name := range order {
		if _, dup := seen[name]; dup {
			return fmt.Errorf("upstream: duplicate mirror %q in reorder request", name)
		}
		seen[name] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, name := range order {
		o := r.overrideLocked(name)
		o.priority = i
		o.hasPriority = true
	}
	return nil
}

// ClearOverrides drops every runtime override for this protocol, returning the
// chain to its declarative config baseline.
func (r *Runtime) ClearOverrides() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overrides = make(map[string]*override)
}

// effective returns the mirrors that should actually be tried, with runtime
// overrides applied: disabled mirrors removed, overridden priorities
// substituted, and the result sorted into fallback order.
//
// The input slice is never mutated (config is shared, immutable state).
func (r *Runtime) effective(ups []Upstream) []Upstream {
	r.mu.Lock()
	out := make([]Upstream, 0, len(ups))
	for _, u := range ups {
		o, ok := r.overrides[u.Name]
		if ok && o.disabled {
			continue
		}
		if ok && o.hasPriority {
			u.Priority = o.priority
		}
		out = append(out, u)
	}
	r.mu.Unlock()
	return sortedUpstreams(out)
}

// Snapshot renders the operator view of a protocol's whole fallback chain:
// configured mirrors joined with their overrides and measurements, in effective
// fallback order.
//
// configured is the current config baseline (the caller passes it in; see the
// type doc for why Runtime does not hold it). Mirrors that are disabled at
// runtime still appear in the snapshot — an operator must be able to see and
// re-enable them — even though effective() omits them from the chain.
func (r *Runtime) Snapshot(configured []Upstream) []MirrorState {
	r.mu.Lock()
	out := make([]MirrorState, 0, len(configured))
	for _, u := range configured {
		st := MirrorState{
			Protocol:       r.protocol,
			Name:           u.Name,
			BaseURL:        u.BaseURL,
			Official:       u.Official,
			ConfigPriority: u.Priority,
			Priority:       u.Priority,
			Enabled:        true,
		}
		if o, ok := r.overrides[u.Name]; ok {
			st.Enabled = !o.disabled
			if o.hasPriority {
				st.Priority = o.priority
				st.Overridden = true
			}
		}
		if s, ok := r.stats[u.Name]; ok {
			st.ServedCount = s.servedCount
			st.LastServedAt = s.lastServedAt
			st.LastLatency = s.lastLatency
			st.LastLatencyValid = s.hasLatency
			st.LastErr = s.lastErr
		}
		out = append(out, st)
	}
	r.mu.Unlock()

	// Block state is read outside r.mu: blockTracker has its own lock, and
	// isBlocked mutates (auto-unblock on expiry), so it must not be called
	// under a lock it does not own.
	for i := range out {
		name := out[i].Name
		out[i].Blocked = r.blocker.isBlocked(name)
		out[i].BlockedUntil = r.blocker.blockedUntilTime(name)
		out[i].ConsecutiveFailures = r.blocker.failureCount(name)
		out[i].Health = deriveHealth(out[i])
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// deriveHealth collapses the measured state into a single operator-facing
// health value. The ordering of the checks matters: blocked dominates, and a
// never-contacted mirror is reported as unknown rather than up.
func deriveHealth(s MirrorState) Health {
	switch {
	case s.Blocked:
		return HealthBlocked
	case s.ConsecutiveFailures > 0:
		return HealthProbing
	case s.ServedCount > 0:
		return HealthUp
	default:
		return HealthUnknown
	}
}

// Registry maps protocol → *Runtime so a single admin surface can report on
// every protocol's fallback chain while each protocol handler keeps its own
// isolated Runtime.
//
// All methods are safe for concurrent use.
type Registry struct {
	mu              sync.Mutex
	byProto         map[string]*Runtime
	blockPersister  func(protocol string) BlockPersister // nil = in-memory per Runtime
}

// NewRegistry constructs an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byProto: make(map[string]*Runtime)}
}

// NewRegistryWithBlockPersister constructs a Registry whose Runtimes share
// persisted auto-block state via persisterForProtocol.
func NewRegistryWithBlockPersister(persisterForProtocol func(protocol string) BlockPersister) *Registry {
	return &Registry{
		byProto:        make(map[string]*Runtime),
		blockPersister: persisterForProtocol,
	}
}

// Runtime returns the Runtime for protocol, creating it on first use so that
// wiring order (handler construction vs. admin construction) does not matter.
func (reg *Registry) Runtime(protocol string) *Runtime {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	rt, ok := reg.byProto[protocol]
	if !ok {
		if reg.blockPersister != nil {
			rt = NewRuntimeWithBlockPersister(protocol, reg.blockPersister(protocol))
		} else {
			rt = NewRuntime(protocol)
		}
		reg.byProto[protocol] = rt
	}
	return rt
}

// Lookup returns the Runtime for protocol without creating one, so a caller can
// distinguish "no such protocol" from "a protocol with no activity yet".
func (reg *Registry) Lookup(protocol string) (*Runtime, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	rt, ok := reg.byProto[protocol]
	return rt, ok
}

// Protocols returns the registered protocol names, sorted.
func (reg *Registry) Protocols() []string {
	reg.mu.Lock()
	out := make([]string, 0, len(reg.byProto))
	for p := range reg.byProto {
		out = append(out, p)
	}
	reg.mu.Unlock()

	sort.Strings(out)
	return out
}
