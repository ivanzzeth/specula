package integrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Reported from a live node: hosts.toml written, CRI and transfer config_path both
// /etc/containerd/certs.d, Specula /v2/ answering 401 — doctor green on every
// check. And a consumer still failed to pull, because it called
// containerd.Client.Pull directly. That path builds its own resolver and never
// reads hosts.toml, so it dialled registry-1.docker.io and timed out.
//
// Doctor green therefore did not mean the node's pulls go through Specula. It
// meant the two paths doctor knows about are wired. The gap is not the wiring —
// it is that nothing told the operator which consumers hosts.toml governs and
// which it cannot.

// healthyNode writes a node whose CRI and transfer config_path are both correct
// and whose certs.d points at Specula — the exact state that was reported green.
func healthyNode(t *testing.T) DoctorOptions {
	t.Helper()
	dir := t.TempDir()

	certs := filepath.Join(dir, "certs.d")
	for _, reg := range []string{"docker.io", "registry.k8s.io"} {
		regDir := filepath.Join(certs, reg)
		if err := os.MkdirAll(regDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// No server= line: a public fallback is itself a risk doctor reports, and
		// integrate does not write one on a CN node.
		body := "[host.\"http://172.25.180.149:7733\"]\n  capabilities = [\"pull\", \"resolve\"]\n"
		if reg != "docker.io" {
			body += "  override_path = true\n"
		}
		if err := os.WriteFile(filepath.Join(regDir, "hosts.toml"), []byte(body), 0o644); err != nil {
			t.Fatalf("write hosts.toml: %v", err)
		}
	}

	toml := filepath.Join(dir, "config.toml")
	cfg := `version = 3
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '/etc/containerd/certs.d'
[plugins.'io.containerd.transfer.v1.local']
  config_path = '/etc/containerd/certs.d'
`
	if err := os.WriteFile(toml, []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}

	return DoctorOptions{
		Home:        dir,
		Addr:        "http://172.25.180.149:7733",
		ConfigTOMLs: []string{toml},
		CertsDirs:   []string{certs},
		AptListPath: filepath.Join(dir, "apt.list"),
		AptCAPath:   filepath.Join(dir, "apt-ca.crt"),
		DumpConfig: func(context.Context) (string, error) {
			return cfg, nil
		},
		Probe: func(context.Context, string) error { return nil }, // /v2/ answered
	}
}

func detailsOf(rep Report) string {
	var b strings.Builder
	for _, r := range rep.Results {
		b.WriteString(r.Action)
		b.WriteString(": ")
		b.WriteString(r.Detail)
		b.WriteString("\n")
	}
	return b.String()
}

// A green node must still be told which consumers hosts.toml does not cover.
func TestDoctorNamesTheBarePullBypassOnAnOtherwiseGreenNode(t *testing.T) {
	rep, err := Doctor(healthyNode(t))
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	all := detailsOf(rep)

	// Premise: this node is otherwise clean, or the test is not about the gap.
	for _, r := range rep.Results {
		if r.Action == "risk" {
			t.Fatalf("test node is not green; unexpected risk: %s", r.Detail)
		}
	}

	if !strings.Contains(all, "Client.Pull") {
		t.Errorf("doctor never mentions containerd.Client.Pull, the path that bypasses hosts.toml:\n%s", all)
	}
	for _, want := range []string{"hosts.toml", "bypass"} {
		if !strings.Contains(strings.ToLower(all), strings.ToLower(want)) {
			t.Errorf("doctor output lacks %q:\n%s", want, all)
		}
	}
}

// The advisory has to say what to DO, not merely that a hazard exists.
func TestBarePullAdvisoryCarriesTheRemediation(t *testing.T) {
	rep, err := Doctor(healthyNode(t))
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	all := detailsOf(rep)
	for _, want := range []string{"WithHostsDir", "transfer"} {
		if !strings.Contains(all, want) {
			t.Errorf("remediation missing %q:\n%s", want, all)
		}
	}
}

// It must not be a RISK on a correctly wired node: crying wolf on every green run
// trains operators to ignore doctor, and this is a property of the containerd API
// rather than a misconfiguration they can fix on the node.
func TestBarePullAdvisoryIsNotARiskWhenTheNodeIsWired(t *testing.T) {
	rep, err := Doctor(healthyNode(t))
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	for _, r := range rep.Results {
		if strings.Contains(r.Detail, "Client.Pull") && r.Action == "risk" {
			t.Errorf("bare-Pull advisory reported as risk on a wired node: %s", r.Detail)
		}
	}
}

// When the transfer config_path is NOT set, the same class of consumer bypasses
// Specula for a reason the operator CAN fix — that stays a risk.
func TestEmptyTransferConfigPathIsStillARisk(t *testing.T) {
	opts := healthyNode(t)
	broken := `version = 3
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = '/etc/containerd/certs.d'
[plugins.'io.containerd.transfer.v1.local']
  config_path = ''
`
	opts.DumpConfig = func(context.Context) (string, error) { return broken, nil }

	rep, err := Doctor(opts)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	var sawRisk bool
	for _, r := range rep.Results {
		if r.Action == "risk" && strings.Contains(r.Detail, "transfer") {
			sawRisk = true
		}
	}
	if !sawRisk {
		t.Errorf("empty transfer config_path did not raise a risk:\n%s", detailsOf(rep))
	}
}
