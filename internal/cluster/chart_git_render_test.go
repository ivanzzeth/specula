package cluster

// Render tests for the git-clone opt-in in the specula-bootstrap chart.
//
// WHY THIS EXISTS
// ---------------
// git is the one protocol the binary's DEFAULT table drops under HA
// (internal/config/protocol_defaults.go gitIsHAUnsafe): the git handler keeps
// node-local bare mirrors, so it is opt-in. The chart's escape hatch is an
// explicit `git.enabled` value that renders a `protocols.git` block into the
// ConfigMap — which survives the HA default-drop because it is explicit config,
// not a default injection. This test pins that render so the opt-in cannot
// silently regress to "git handler mounted but configured=false" (the exact
// state that made a real cluster answer 502 on the git path because the fixed
// code path was never reachable).
//
// Hermetic: shells the real `helm` binary against the chart dir. Skipped when
// helm is absent (matching the repo's other real-binary tests).

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// chartDir returns the absolute path to deploy/helm/specula-bootstrap.
func chartDir(t *testing.T) string {
	t.Helper()
	// this file lives at internal/cluster/ ; chart is ../../deploy/helm/...
	p, err := filepath.Abs(filepath.Join("..", "..", "deploy", "helm", "specula-bootstrap"))
	if err != nil {
		t.Fatalf("abs chart dir: %v", err)
	}
	return p
}

func helmTemplate(t *testing.T, extraArgs ...string) string {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed; skipping chart render test")
	}
	args := append([]string{"template", "t", chartDir(t)}, extraArgs...)
	out, err := exec.Command(helm, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// Default render must NOT emit a git protocol block: git stays opt-in.
func TestChart_GitDisabledByDefault(t *testing.T) {
	out := helmTemplate(t)
	if strings.Contains(out, "allowed_upstreams:") || strings.Contains(out, "mirror_dir:") {
		t.Fatalf("default render leaked a git protocol block (git must be opt-in):\n%s", out)
	}
}

// git.enabled=true must render a complete, valid-shape git protocol block:
// a generic `upstreams:` list (satisfies per-protocol validation) AND the
// nested `git:` block the handler reads (allowed_upstreams, mirror_dir, …).
func TestChart_GitEnabledRendersProtocolBlock(t *testing.T) {
	out := helmTemplate(t, "--set", "git.enabled=true")

	// The whole point: an explicit protocols.git block appears.
	for _, want := range []string{
		"protocols:",
		"allowed_upstreams:",
		"- github.com",
		`mirror_dir: "/var/lib/specula/git"`,
		"public_only: true",
		"fail_closed: true",
		"base_url: https://github.com",
		"official: true",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("git.enabled render missing %q:\n%s", want, out)
		}
	}
}

// Regression for the `helm upgrade --reuse-values` footgun: --reuse-values
// reuses the stored release values + --set overrides ONLY, and does NOT re-read
// the chart's values.yaml defaults. So a release created before the git: value
// block existed, upgraded with just `--set git.enabled=true`, arrives at the
// template with NULL git sub-values — which previously rendered zero upstreams
// and crashlooped the pod on `protocols.git: at least one upstream is required`.
// The template must default EVERY sub-value itself so the minimal opt-in is
// valid no matter how the values arrive. Simulate that by nulling the
// sub-values explicitly and asserting the secure defaults still render.
func TestChart_GitEnabledNullSubValuesFallsBackToDefaults(t *testing.T) {
	out := helmTemplate(t,
		"--set", "git.enabled=true",
		"--set", "git.allowedUpstreams=null",
		"--set", "git.mirrorDir=null",
		"--set", "git.syncStaleAfter=null",
	)
	for _, want := range []string{
		"- github.com",
		"base_url: https://github.com",
		`mirror_dir: "/var/lib/specula/git"`,
		`sync_stale_after: "30s"`,
		"public_only: true",
		"fail_closed: true",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("null-subvalue render missing secure default %q:\n%s", want, out)
		}
	}
}

// The allowlist drives BOTH the generic upstreams and the nested allowed_upstreams,
// so a multi-host list renders both entries without hand-editing two places.
func TestChart_GitAllowedUpstreamsDrivesUpstreams(t *testing.T) {
	out := helmTemplate(t,
		"--set", "git.enabled=true",
		"--set", "git.allowedUpstreams={github.com,gitee.com}",
	)
	for _, want := range []string{
		`name: "github.com"`,
		`name: "gitee.com"`,
		"base_url: https://gitee.com",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("multi-upstream render missing %q:\n%s", want, out)
		}
	}
}
