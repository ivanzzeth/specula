package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ivanzzeth/specula/internal/config"
)

var errFake = errors.New("synthetic failure")

// ─── localHealthzURL ─────────────────────────────────────────────────────────

// A single-host upgrade self-check probes the daemon it just restarted. The
// listen address is a bind spec, not a dial spec: wildcard binds must become
// loopback, while a host-bound daemon must be dialled on that same host (or the
// probe reports a false failure and triggers a needless rollback).
func TestLocalHealthzURL(t *testing.T) {
	cases := []struct {
		name  string
		addr  string
		tlsOn bool
		want  string
	}{
		{"port only", ":7733", false, "http://127.0.0.1:7733/healthz"},
		{"ipv4 wildcard", "0.0.0.0:7733", false, "http://127.0.0.1:7733/healthz"},
		{"ipv6 wildcard", "[::]:7733", false, "http://127.0.0.1:7733/healthz"},
		{"explicit loopback", "127.0.0.1:9999", false, "http://127.0.0.1:9999/healthz"},
		{"tls", "0.0.0.0:7733", true, "https://127.0.0.1:7733/healthz"},
		{"host bound stays", "specula.internal:7732", false, "http://specula.internal:7732/healthz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := localHealthzURL(tc.addr, tc.tlsOn)
			if err != nil {
				t.Fatalf("localHealthzURL(%q): %v", tc.addr, err)
			}
			if got != tc.want {
				t.Fatalf("localHealthzURL(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

func TestLocalHealthzURLRejectsUnusable(t *testing.T) {
	for _, addr := range []string{"", "   ", "7733", "0.0.0.0:"} {
		if got, err := localHealthzURL(addr, false); err == nil {
			t.Fatalf("localHealthzURL(%q) = %q, want error", addr, got)
		}
	}
}

// ─── replaceBinary / restoreBinary ───────────────────────────────────────────

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The whole point of upgrade-by-rename: Linux refuses to write into a running
// executable (ETXTBSY), so the new bytes land beside the target and rename over
// it. The previous binary must survive as .prev or rollback is impossible.
func TestReplaceBinaryAtomicWithBackup(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new", "specula")
	dst := filepath.Join(dir, "bin", "specula")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, src, "NEW", 0o755)
	writeFile(t, dst, "OLD", 0o755)

	if err := replaceBinary(src, dst); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}
	if got := readFile(t, dst); got != "NEW" {
		t.Fatalf("dst = %q, want NEW", got)
	}
	if got := readFile(t, dst+".prev"); got != "OLD" {
		t.Fatalf("backup = %q, want OLD", got)
	}
	if _, err := os.Stat(dst + ".new"); !os.IsNotExist(err) {
		t.Fatalf("staging file %s.new must not survive (err=%v)", dst, err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("dst mode = %v, want 0755", fi.Mode().Perm())
	}
}

// Upgrading from the installed path itself would nuke the only copy of the
// binary into its own backup. Refuse instead.
func TestReplaceBinaryRejectsSelfTarget(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "specula")
	writeFile(t, p, "SAME", 0o755)
	if err := replaceBinary(p, p); err == nil {
		t.Fatal("replaceBinary(p, p) must fail")
	}
	if got := readFile(t, p); got != "SAME" {
		t.Fatalf("binary clobbered: %q", got)
	}
}

func TestReplaceBinaryRequiresExistingTarget(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new")
	writeFile(t, src, "NEW", 0o755)
	err := replaceBinary(src, filepath.Join(dir, "absent"))
	if err == nil {
		t.Fatal("replaceBinary into a missing target must fail (use install, not upgrade)")
	}
	if !strings.Contains(err.Error(), "install") {
		t.Fatalf("error should point at install: %v", err)
	}
}

func TestRestoreBinary(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "specula")
	writeFile(t, dst, "NEW", 0o755)
	writeFile(t, dst+".prev", "OLD", 0o755)

	if err := restoreBinary(dst); err != nil {
		t.Fatalf("restoreBinary: %v", err)
	}
	if got := readFile(t, dst); got != "OLD" {
		t.Fatalf("dst = %q, want OLD", got)
	}
	// Backup is kept: a second rollback (or a human inspecting it) must still work.
	if got := readFile(t, dst+".prev"); got != "OLD" {
		t.Fatalf("backup = %q, want OLD retained", got)
	}
}

func TestRestoreBinaryWithoutBackup(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "specula")
	writeFile(t, dst, "NEW", 0o755)
	if err := restoreBinary(dst); err == nil {
		t.Fatal("restoreBinary without .prev must fail")
	}
}

// ─── upgradePlan.run ─────────────────────────────────────────────────────────

func newPlan(t *testing.T) (*upgradePlan, string, *int, *int) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "staged")
	dst := filepath.Join(dir, "specula")
	writeFile(t, src, "NEW", 0o755)
	writeFile(t, dst, "OLD", 0o755)

	restarts, healths := 0, 0
	p := &upgradePlan{
		self:   src,
		binary: dst,
		restart: func() error {
			restarts++
			return nil
		},
		health: func() error {
			healths++
			return nil
		},
	}
	return p, dst, &restarts, &healths
}

func TestUpgradePlanHappyPath(t *testing.T) {
	p, dst, restarts, healths := newPlan(t)
	if err := p.run(io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := readFile(t, dst); got != "NEW" {
		t.Fatalf("binary = %q, want NEW", got)
	}
	if *restarts != 1 || *healths != 1 {
		t.Fatalf("restarts=%d healths=%d, want 1/1", *restarts, *healths)
	}
}

// A bad build must not leave the host with a dead daemon: failed health check
// rolls the binary back and restarts the known-good one.
func TestUpgradePlanRollsBackOnUnhealthy(t *testing.T) {
	p, dst, restarts, _ := newPlan(t)
	p.health = func() error { return errFake }

	err := p.run(io.Discard)
	if err == nil {
		t.Fatal("run must fail when health check fails")
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("error must say it rolled back: %v", err)
	}
	if got := readFile(t, dst); got != "OLD" {
		t.Fatalf("binary = %q, want OLD after rollback", got)
	}
	if *restarts != 2 {
		t.Fatalf("restarts=%d, want 2 (new + rollback)", *restarts)
	}
}

func TestUpgradePlanRollsBackWhenRestartFails(t *testing.T) {
	p, dst, _, healths := newPlan(t)
	p.restart = func() error { return errFake }

	if err := p.run(io.Discard); err == nil {
		t.Fatal("run must fail when restart fails")
	}
	if got := readFile(t, dst); got != "OLD" {
		t.Fatalf("binary = %q, want OLD after rollback", got)
	}
	if *healths != 0 {
		t.Fatalf("health must not run after a failed restart (got %d)", *healths)
	}
}

// --no-restart stages the binary for a maintenance window: swap now, restart later.
func TestUpgradePlanNoRestart(t *testing.T) {
	p, dst, restarts, healths := newPlan(t)
	p.noRestart = true
	if err := p.run(io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := readFile(t, dst); got != "NEW" {
		t.Fatalf("binary = %q, want NEW", got)
	}
	if *restarts != 0 || *healths != 0 {
		t.Fatalf("restarts=%d healths=%d, want 0/0", *restarts, *healths)
	}
}

func TestUpgradePlanSkipHealth(t *testing.T) {
	p, _, restarts, healths := newPlan(t)
	p.skipHealth = true
	if err := p.run(io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}
	if *restarts != 1 || *healths != 0 {
		t.Fatalf("restarts=%d healths=%d, want 1/0", *restarts, *healths)
	}
}

// ─── upgradeHealthURL / waitLocalHealthz ─────────────────────────────────────

// The probe target comes from the live config, not a hardcoded port: an operator
// who moved the control plane must still get a working health gate.
func TestUpgradeHealthURLFromConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "specula.yaml")
	if _, err := config.WriteExampleIfMissing(path, func(src string) string {
		return patchConfigForSystemInstall(strings.ReplaceAll(src,
			`control_plane_addr: "0.0.0.0:7733"`, `control_plane_addr: "0.0.0.0:19733"`))
	}); err != nil {
		t.Fatal(err)
	}
	got, err := upgradeHealthURL(path)
	if err != nil {
		t.Fatalf("upgradeHealthURL: %v", err)
	}
	if want := "http://127.0.0.1:19733/healthz"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestUpgradeHealthURLMissingConfig(t *testing.T) {
	if _, err := upgradeHealthURL(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("upgradeHealthURL on a missing config must fail (caller degrades to no health gate)")
	}
}

// A restart is not instant: the gate must poll, not sample once, or every
// upgrade would roll back on a daemon that was merely still booting.
func TestWaitLocalHealthzPollsUntilReady(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := waitLocalHealthz(srv.URL+"/healthz", 30*time.Second); err != nil {
		t.Fatalf("waitLocalHealthz: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got < 2 {
		t.Fatalf("expected a retry, got %d attempts", got)
	}
}

func TestWaitLocalHealthzTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := waitLocalHealthz(srv.URL+"/healthz", 50*time.Millisecond)
	if err == nil {
		t.Fatal("waitLocalHealthz must fail when the endpoint never turns healthy")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("error should carry the last status: %v", err)
	}
}

// TLS verification is deliberately off: the probe dials loopback while the cert
// is minted for the public domain. A self-signed httptest cert must pass.
func TestWaitLocalHealthzIgnoresCertHostname(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := waitLocalHealthz(srv.URL+"/healthz", 5*time.Second); err != nil {
		t.Fatalf("waitLocalHealthz over TLS: %v", err)
	}
}

// The old→new version line is cosmetic; it must never abort an upgrade.
func TestInstalledVersionDegradesToUnknown(t *testing.T) {
	if got := installedVersion(filepath.Join(t.TempDir(), "absent")); got != "unknown" {
		t.Fatalf("installedVersion = %q, want unknown", got)
	}
}

func TestTrimVersionPrefix(t *testing.T) {
	if got := trimVersionPrefix("specula v0.5.0 (commit abc, built x)\n"); got != "v0.5.0 (commit abc, built x)" {
		t.Fatalf("got %q", got)
	}
	if got := trimVersionPrefix("  unknown  "); got != "unknown" {
		t.Fatalf("got %q", got)
	}
}
