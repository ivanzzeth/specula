package verify

// Tests for DependencyConfusionGuard (depconfusion.go).
//
// Each test is traceable to a documented requirement:
//   - DESIGN-REVIEW §4 fail-open/closed decision table
//   - DESIGN-REVIEW H3 (PyPI prefix-convention is security theatre)
//   - DESIGN-REVIEW H4 (private upstream DOWN → fail-closed, not fallback to public)
//   - PRD §G2 dependency confusion guard spec

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func npmGuard(scopes, unscoped []string, failClosed bool) *DependencyConfusionGuard {
	return NewDependencyConfusionGuard(DepConfusionConfig{
		Protocol:        "npm",
		PrivateScopes:   scopes,
		PrivateUnscoped: unscoped,
		FailClosed:      failClosed,
	})
}

func pypiGuard(names []string, failClosed bool) *DependencyConfusionGuard {
	return NewDependencyConfusionGuard(DepConfusionConfig{
		Protocol:     "pypi",
		PrivateNames: names,
		FailClosed:   failClosed,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// npm – scope-bound private names
// ─────────────────────────────────────────────────────────────────────────────

// TestDepConfusion_NPM_ScopedPrivate verifies that scoped packages under a
// configured private scope are recognised as private. An attacker cannot publish
// to a scope they don't own, so scope binding is structurally sound (DESIGN-REVIEW §4).
func TestDepConfusion_NPM_ScopedPrivate(t *testing.T) {
	g := npmGuard([]string{"@myorg"}, nil, true)

	assert.True(t, g.IsPrivate("@myorg/api-client"), "package in configured scope must be private")
	assert.True(t, g.IsPrivate("@myorg/internal-utils"), "another package in same scope")
	assert.False(t, g.IsPrivate("@otherapg/api-client"), "unrelated scope must not be private")
	assert.False(t, g.IsPrivate("public-pkg"), "unscoped public name must not be private")
}

// TestDepConfusion_NPM_ScopedCaseInsensitive verifies that scope comparison is
// case-insensitive (npm scopes are lowercased by convention).
func TestDepConfusion_NPM_ScopedCaseInsensitive(t *testing.T) {
	g := npmGuard([]string{"@MyOrg"}, nil, true)
	assert.True(t, g.IsPrivate("@myorg/pkg"), "case-insensitive scope match must succeed")
	assert.True(t, g.IsPrivate("@MYORG/pkg"), "all-caps scope must match")
}

// TestDepConfusion_NPM_ScopeWithoutAtPrefix verifies that a configured scope
// without the "@" prefix is normalised and still matched correctly.
func TestDepConfusion_NPM_ScopeWithoutAtPrefix(t *testing.T) {
	g := npmGuard([]string{"myorg"}, nil, true) // no "@" prefix in config
	assert.True(t, g.IsPrivate("@myorg/pkg"), "scope without @ in config must still match @myorg/pkg")
}

// TestDepConfusion_NPM_UnscopedDenylist verifies that explicitly listed unscoped
// names are classified as private (DESIGN-REVIEW §4: unscoped private names need
// an explicit denylist since they have no structural protection).
func TestDepConfusion_NPM_UnscopedDenylist(t *testing.T) {
	g := npmGuard(nil, []string{"internal-cli", "build-tool-*"}, true)

	assert.True(t, g.IsPrivate("internal-cli"), "exact unscoped name match")
	assert.True(t, g.IsPrivate("build-tool-webpack"), "glob match for unscoped name")
	assert.True(t, g.IsPrivate("build-tool-rollup"), "glob match for another unscoped name")
	assert.False(t, g.IsPrivate("other-cli"), "non-listed unscoped name must be public")
	assert.False(t, g.IsPrivate("some-package"), "unrelated package must not be private")
}

// TestDepConfusion_NPM_ScopeNameOnly verifies that a bare "@scope" string
// (without a "/package" part) is treated as having the scope.
func TestDepConfusion_NPM_ScopeNameOnly(t *testing.T) {
	g := npmGuard([]string{"@myorg"}, nil, true)
	// "@myorg" without a slash: the whole thing is treated as the scope.
	assert.True(t, g.IsPrivate("@myorg"), "bare scope name must be treated as in-scope")
}

// ─────────────────────────────────────────────────────────────────────────────
// PyPI – flat namespace with PEP 503 normalization
// ─────────────────────────────────────────────────────────────────────────────

// TestDepConfusion_PyPI_ExactMatch verifies exact-name matching for the PyPI
// flat namespace.
func TestDepConfusion_PyPI_ExactMatch(t *testing.T) {
	g := pypiGuard([]string{"my-internal-library", "secret-utils"}, false)

	assert.True(t, g.IsPrivate("my-internal-library"), "exact private name")
	assert.True(t, g.IsPrivate("secret-utils"), "another exact private name")
	assert.False(t, g.IsPrivate("requests"), "public name must not be private")
}

// TestDepConfusion_PyPI_PEP503Normalization verifies that PEP 503 name
// normalization is applied on both sides: "My_Internal_Library" and
// "my-internal-library" and "my.internal.library" all map to the same key.
// This prevents an attacker from bypassing the guard with a variant spelling.
func TestDepConfusion_PyPI_PEP503Normalization(t *testing.T) {
	g := pypiGuard([]string{"my-internal-library"}, false)

	// PEP 503: runs of [-_.] collapse to '-', all lowercase.
	assert.True(t, g.IsPrivate("My_Internal_Library"), "underscore variant must match via PEP 503 normalization")
	assert.True(t, g.IsPrivate("MY.INTERNAL.LIBRARY"), "dot+uppercase variant must match")
	assert.True(t, g.IsPrivate("my---internal---library"), "repeated separators must collapse")
}

// TestDepConfusion_PyPI_GlobMatch verifies glob patterns in the private-name
// manifest (DESIGN-REVIEW H3 note: the real protection is keeping Specula as the
// SOLE index, but globs help with naming conventions).
func TestDepConfusion_PyPI_GlobMatch(t *testing.T) {
	g := pypiGuard([]string{"mycompany-*"}, false)

	assert.True(t, g.IsPrivate("mycompany-api"), "glob match")
	assert.True(t, g.IsPrivate("mycompany-auth"), "another glob match")
	assert.False(t, g.IsPrivate("othercompany-api"), "non-matching prefix must not be private")
}

// TestDepConfusion_PyPI_EmptyName returns false for empty/whitespace input
// so the guard doesn't wrongly classify an absent package name as private.
func TestDepConfusion_PyPI_EmptyName(t *testing.T) {
	g := pypiGuard([]string{"*"}, false)
	assert.False(t, g.IsPrivate(""), "empty name must never be private")
	assert.False(t, g.IsPrivate("   "), "whitespace-only name must never be private")
}

// ─────────────────────────────────────────────────────────────────────────────
// Default / unknown protocol
// ─────────────────────────────────────────────────────────────────────────────

// TestDepConfusion_DefaultProtocol_MatchesPrivateNames verifies that the
// unknown-protocol fallback checks PrivateNames and PrivateUnscoped conservatively.
func TestDepConfusion_DefaultProtocol_MatchesPrivateNames(t *testing.T) {
	g := NewDependencyConfusionGuard(DepConfusionConfig{
		Protocol:     "helm",
		PrivateNames: []string{"mycompany-chart"},
	})
	assert.True(t, g.IsPrivate("mycompany-chart"), "PrivateNames matches for unknown protocol")
	assert.False(t, g.IsPrivate("public-chart"), "public name stays public")
}

func TestDepConfusion_DefaultProtocol_MatchesPrivateScopes(t *testing.T) {
	g := NewDependencyConfusionGuard(DepConfusionConfig{
		Protocol:      "helm",
		PrivateScopes: []string{"@myorg"},
	})
	assert.True(t, g.IsPrivate("@myorg/chart"), "PrivateScopes matches for unknown protocol")
}

// ─────────────────────────────────────────────────────────────────────────────
// DESIGN-REVIEW §4 fail-open/closed decision table (the load-bearing requirement)
// ─────────────────────────────────────────────────────────────────────────────

// TestDepConfusion_ResolvePrivate_OK verifies that a successful private upstream
// response → ActionServe (serve the private upstream's result; never go public).
func TestDepConfusion_ResolvePrivate_OK(t *testing.T) {
	g := npmGuard([]string{"@myorg"}, nil, true)
	assert.Equal(t, ActionServe, g.ResolvePrivate(OutcomeOK), "UP private upstream → ActionServe")
}

// TestDepConfusion_ResolvePrivate_NotFound verifies that a genuine 404 from the
// private upstream → ActionNotFound (never try the public index — that is the
// confusion path).
//
// PRD §G2 "私有名，私有源真 404: 返回 404，不试公网"
func TestDepConfusion_ResolvePrivate_NotFound(t *testing.T) {
	for _, fc := range []bool{true, false} {
		g := npmGuard([]string{"@myorg"}, nil, fc)
		assert.Equal(t, ActionNotFound, g.ResolvePrivate(OutcomeNotFound),
			"404 from private upstream → ActionNotFound (never public), FailClosed=%v", fc)
	}
}

// TestDepConfusion_ResolvePrivate_Down_FailClosed verifies the critical
// DESIGN-REVIEW §4 requirement: a CONFIGURED-PRIVATE name whose private upstream
// is DOWN (error/timeout/5xx) must FAIL CLOSED — the outage is exactly the
// attacker's window to win with the public copy.
//
// "私有名，私有源 DOWN/errored → FAIL CLOSED 硬错误（或仅从本地缓存服务）——宕机正是攻击者公网副本获胜的窗口"
func TestDepConfusion_ResolvePrivate_Down_FailClosed(t *testing.T) {
	g := npmGuard([]string{"@myorg"}, nil, true /* FailClosed=true */)
	action := g.ResolvePrivate(OutcomeDown)
	assert.Equal(t, ActionFailClosed, action,
		"private upstream DOWN with FailClosed=true MUST return ActionFailClosed, never ActionServe or ActionNotFound")
	// Crucially: this must never be ActionServe (which would go to public).
	assert.NotEqual(t, ActionServe, action, "DOWN must never result in public fallback")
}

// TestDepConfusion_ResolvePrivate_Down_ServeStale verifies that when FailClosed
// is false, a DOWN private upstream → ActionServeStale (serve from LOCAL CACHE
// only, still never public).
func TestDepConfusion_ResolvePrivate_Down_ServeStale(t *testing.T) {
	g := npmGuard([]string{"@myorg"}, nil, false /* FailClosed=false */)
	action := g.ResolvePrivate(OutcomeDown)
	assert.Equal(t, ActionServeStale, action, "DOWN private upstream with FailClosed=false → ActionServeStale (still never public)")
	assert.NotEqual(t, ActionServe, action, "stale must never resolve to public-serve")
}

// TestDepConfusion_ResolvePrivate_UnknownOutcome verifies that an unrecognised
// outcome value fails closed (conservative default, fail-safe).
func TestDepConfusion_ResolvePrivate_UnknownOutcome(t *testing.T) {
	g := npmGuard([]string{"@myorg"}, nil, true)
	// Use an integer value not defined in the iota.
	assert.Equal(t, ActionFailClosed, g.ResolvePrivate(UpstreamOutcome(99)),
		"unknown outcome must fail closed")
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

func TestNormalizePEP503(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"requests", "requests"},
		{"Django", "django"},
		{"my_package", "my-package"},
		{"my.package", "my-package"},
		{"My-Package", "my-package"},
		{"some---thing", "some-thing"}, // multiple dashes collapse
		{"some___thing", "some-thing"}, // multiple underscores collapse
		{"some...thing", "some-thing"}, // multiple dots collapse
		{"-leading", "leading"},        // leading dash trimmed
		{"trailing-", "trailing"},      // trailing dash trimmed
		{"a-b_c.d", "a-b-c-d"},         // mixed separators
		{"PyPI", "pypi"},
		// Glob metacharacters preserved.
		{"mycompany-*", "mycompany-*"},
		{"my?org", "my?org"},
		{"my[org]", "my[org]"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := normalizePEP503(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMatchAny(t *testing.T) {
	t.Run("exact match", func(t *testing.T) {
		assert.True(t, matchAny("requests", []string{"requests"}, false))
		assert.False(t, matchAny("requests", []string{"django"}, false))
	})
	t.Run("glob match", func(t *testing.T) {
		assert.True(t, matchAny("build-tool-webpack", []string{"build-tool-*"}, false))
		assert.False(t, matchAny("other-thing", []string{"build-tool-*"}, false))
	})
	t.Run("empty pattern skipped", func(t *testing.T) {
		assert.False(t, matchAny("anything", []string{"", "  "}, false))
	})
	t.Run("normalize=true applies PEP503 to pattern", func(t *testing.T) {
		// matchAny normalizes the PATTERN, not the name. IsPrivate pre-normalizes
		// the name before calling matchAny; here we mimic that: name is already
		// PEP 503 canonical ("my-package"), pattern "My_Package" gets normalized.
		assert.True(t, matchAny("my-package", []string{"My_Package"}, true), "pattern-normalized match")
	})
	t.Run("empty patterns list never matches", func(t *testing.T) {
		assert.False(t, matchAny("requests", nil, false))
	})
}

func TestNpmScope(t *testing.T) {
	scope, ok := npmScope("@myorg/pkg")
	assert.True(t, ok)
	assert.Equal(t, "@myorg", scope)

	scope, ok = npmScope("@myorg")
	assert.True(t, ok)
	assert.Equal(t, "@myorg", scope)

	_, ok = npmScope("public-pkg")
	assert.False(t, ok, "unscoped package has no scope")
}

func TestNormalizeScope(t *testing.T) {
	assert.Equal(t, "@myorg", normalizeScope("@myorg"))
	assert.Equal(t, "@myorg", normalizeScope("myorg"))      // adds "@"
	assert.Equal(t, "@myorg", normalizeScope("@MyOrg"))     // lowercased
	assert.Equal(t, "@myorg", normalizeScope("  @MyOrg  ")) // trimmed
}

// TestDepConfusion_NoPrivateConfig verifies that a guard with no configured
// private names never blocks any package (DESIGN-REVIEW: without explicit config,
// no package is private — fail-safe default is open to public for unconfigured names).
func TestDepConfusion_NoPrivateConfig(t *testing.T) {
	g := NewDependencyConfusionGuard(DepConfusionConfig{Protocol: "npm"})
	assert.False(t, g.IsPrivate("@myorg/pkg"), "no config → never private")
	assert.False(t, g.IsPrivate("some-package"), "no config → never private")
}
