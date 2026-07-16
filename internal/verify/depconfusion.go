package verify

import (
	"path"
	"strings"
)

// DependencyConfusionGuard is the per-ecosystem private-name guard the pypi and
// npm handlers consult before resolving a package (DESIGN-REVIEW §4). It answers
// two questions and nothing else:
//
//  1. IsPrivate(name)   — is this an org-owned name that must NEVER be resolved
//     from a public mirror?
//  2. ResolvePrivate(o) — given the private upstream's outcome, what must the
//     handler do? (the fail-open/closed decision table)
//
// The guard is a MANIFEST of owned names, NOT a trust-prefix convention: a
// public prefix rule is security theatre because an attacker can register the
// same prefix on the public index (DESIGN-REVIEW H3). Names are matched exactly
// (or by explicit glob), never by "starts-with-our-org".
//
// Namespace models:
//   - npm:  two-layer — scoped `@scope/pkg` (structurally confusion-resistant:
//     an attacker cannot publish under your scope) and unscoped `pkg` (needs an
//     explicit denylist).
//   - pypi: FLAT — no scopes; private names are matched against an exact/glob
//     manifest. Specula must be the SOLE index (only --index-url).
type DependencyConfusionGuard struct {
	cfg DepConfusionConfig
}

// DepConfusionConfig configures a DependencyConfusionGuard (mapped from
// config.DependencyConfusionConfig by the wiring layer).
type DepConfusionConfig struct {
	// Protocol selects the namespace model: "npm" or "pypi".
	Protocol string
	// PrivateNames is the pypi flat-namespace manifest: exact names or globs
	// (e.g. "mycompany-*"). PEP 503 normalisation is applied on both sides.
	PrivateNames []string
	// PrivateScopes are the npm scopes bound to the private registry (e.g.
	// "@myorg"). A name in a listed scope is private.
	PrivateScopes []string
	// PrivateUnscoped is the npm explicit denylist of unscoped private names
	// (exact or glob) that must never be queried upstream.
	PrivateUnscoped []string
	// FailClosed selects the behaviour for a private name whose private upstream
	// is DOWN: true = fail closed (5xx, never public); false = serve from local
	// cache only ("serve_stale"), still never public.
	FailClosed bool
}

// NewDependencyConfusionGuard builds a guard from its config.
func NewDependencyConfusionGuard(cfg DepConfusionConfig) *DependencyConfusionGuard {
	return &DependencyConfusionGuard{cfg: cfg}
}

// IsPrivate reports whether pkg is an org-owned private name that must resolve
// ONLY from the private upstream and never from a public mirror.
//
//   - npm:  true when pkg is in a configured private scope, OR matches the
//     unscoped denylist.
//   - pypi: true when the PEP 503-normalised pkg matches the private-name
//     manifest (exact or glob).
func (g *DependencyConfusionGuard) IsPrivate(pkg string) bool {
	pkg = strings.TrimSpace(pkg)
	if pkg == "" {
		return false
	}
	switch g.cfg.Protocol {
	case "npm":
		if scope, ok := npmScope(pkg); ok {
			for _, s := range g.cfg.PrivateScopes {
				if strings.EqualFold(scope, normalizeScope(s)) {
					return true
				}
			}
			return false
		}
		return matchAny(pkg, g.cfg.PrivateUnscoped, false)
	case "pypi":
		return matchAny(normalizePEP503(pkg), g.cfg.PrivateNames, true)
	default:
		// Unknown ecosystem: match against every configured manifest, treating
		// scoped names conservatively (a positive match wins).
		if matchAny(pkg, g.cfg.PrivateNames, false) || matchAny(pkg, g.cfg.PrivateUnscoped, false) {
			return true
		}
		if scope, ok := npmScope(pkg); ok {
			for _, s := range g.cfg.PrivateScopes {
				if strings.EqualFold(scope, normalizeScope(s)) {
					return true
				}
			}
		}
		return false
	}
}

// UpstreamOutcome is what the handler observed when it consulted the PRIVATE
// upstream for a private name.
type UpstreamOutcome int

const (
	// OutcomeOK: the private upstream served the name.
	OutcomeOK UpstreamOutcome = iota
	// OutcomeNotFound: the private upstream returned a genuine 404/410.
	OutcomeNotFound
	// OutcomeDown: the private upstream errored / timed out / returned 5xx.
	OutcomeDown
)

// Action is what the handler MUST do for a private name given the private
// upstream's outcome. Public fallback is never an available action.
type Action int

const (
	// ActionServe: serve the private upstream's response.
	ActionServe Action = iota
	// ActionNotFound: return 404 and DO NOT try public — a real 404 for a
	// private name is a publish/config error that must be exposed, not masked by
	// the public index (which is the confusion path).
	ActionNotFound
	// ActionFailClosed: return 5xx and NEVER fall back to public — the down
	// window is exactly when an attacker's public copy would win.
	ActionFailClosed
	// ActionServeStale: serve from LOCAL CACHE only (still never public); if
	// there is no cached copy the handler must fail closed.
	ActionServeStale
)

// ResolvePrivate maps a private name's upstream outcome to the mandated action
// (DESIGN-REVIEW §4 fail-open/closed table). It is only meaningful once
// IsPrivate(name) is true.
//
//	OK        → ActionServe
//	NotFound  → ActionNotFound      (never public)
//	Down      → ActionFailClosed    when FailClosed, else ActionServeStale (never public)
func (g *DependencyConfusionGuard) ResolvePrivate(outcome UpstreamOutcome) Action {
	switch outcome {
	case OutcomeOK:
		return ActionServe
	case OutcomeNotFound:
		return ActionNotFound
	case OutcomeDown:
		if g.cfg.FailClosed {
			return ActionFailClosed
		}
		return ActionServeStale
	default:
		return ActionFailClosed
	}
}

// --------------------------------------------------------------------------
// Matching helpers
// --------------------------------------------------------------------------

// npmScope extracts the scope ("@scope") from a scoped npm name ("@scope/pkg").
// Returns ok=false for unscoped names.
func npmScope(pkg string) (scope string, ok bool) {
	if !strings.HasPrefix(pkg, "@") {
		return "", false
	}
	if i := strings.IndexByte(pkg, '/'); i > 0 {
		return pkg[:i], true
	}
	// "@scope" with no package part: treat the whole thing as the scope.
	return pkg, true
}

// normalizeScope canonicalises a configured scope so "@myorg", "myorg" and
// "@MyOrg" all compare equal (npm scopes are lowercased).
func normalizeScope(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !strings.HasPrefix(s, "@") {
		s = "@" + s
	}
	return s
}

// matchAny reports whether name matches any pattern in patterns. Patterns may be
// exact names or shell-style globs (path.Match, e.g. "mycompany-*"). When
// normalize is true both sides are PEP 503-normalised before comparison.
func matchAny(name string, patterns []string, normalize bool) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if normalize {
			p = normalizePEP503(p)
		}
		if p == name {
			return true
		}
		if strings.ContainsAny(p, "*?[") {
			if ok, err := path.Match(p, name); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// normalizePEP503 applies PEP 503 name normalisation (lowercase; collapse runs
// of -, _ or . into a single '-') so private-name matching is canonical. Glob
// metacharacters (*, ?, [, ]) are preserved so patterns still match.
func normalizePEP503(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevSep := false
	for _, r := range name {
		if r == '-' || r == '_' || r == '.' {
			if !prevSep {
				b.WriteByte('-')
				prevSep = true
			}
			continue
		}
		b.WriteRune(r)
		prevSep = false
	}
	return strings.Trim(b.String(), "-")
}
