package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/config"
	"github.com/ivanzzeth/specula/internal/version"
)

// Single-host upgrade path: push a new static binary to the box, run it once,
// and it replaces the installed copy, restarts the unit, and rolls back if the
// daemon does not come up healthy.
//
// Why this exists as a command instead of three shell lines: Linux refuses to
// write into a running executable (ETXTBSY), so a naive `scp` straight onto
// /usr/local/bin/specula fails, and the obvious workaround (stop, copy, start)
// has no rollback when the new build is bad — leaving the host with a dead
// mirror and every node unable to pull.
const defaultUpgradeHealthTimeout = 60 * time.Second

// upgradePlan is the injectable form of the upgrade so the ordering and the
// rollback path are unit-testable without root or systemd.
type upgradePlan struct {
	self   string // new binary (the process running this command)
	binary string // installed path to replace

	restart func() error
	health  func() error

	noRestart  bool
	skipHealth bool
}

func (p *upgradePlan) run(w io.Writer) error {
	if err := replaceBinary(p.self, p.binary); err != nil {
		return err
	}
	fmt.Fprintf(w, "installed %s → %s (rollback copy: %s.prev)\n", p.self, p.binary, p.binary)

	if p.noRestart {
		fmt.Fprintf(w, "not restarting (--no-restart); apply with: systemctl restart specula.service\n")
		return nil
	}
	if err := p.restart(); err != nil {
		return p.rollback(w, fmt.Errorf("restart after upgrade: %w", err))
	}
	if p.skipHealth {
		fmt.Fprintln(w, "specula.service restarted (health gate skipped)")
		return nil
	}
	if err := p.health(); err != nil {
		return p.rollback(w, fmt.Errorf("health check after upgrade: %w", err))
	}
	fmt.Fprintln(w, "upgrade OK — specula.service healthy")
	return nil
}

// rollback restores the previous binary and restarts, then returns an error
// wrapping the original cause. A failed rollback is escalated loudly: that is
// the one state where a human must intervene.
func (p *upgradePlan) rollback(w io.Writer, cause error) error {
	fmt.Fprintf(w, "upgrade failed (%v) — rolling back to %s.prev\n", cause, p.binary)
	if err := restoreBinary(p.binary); err != nil {
		return fmt.Errorf("%w; ROLLBACK FAILED: %v — host is still on the new binary, "+
			"restore %s.prev by hand", cause, err, p.binary)
	}
	if p.restart != nil {
		if err := p.restart(); err != nil {
			return fmt.Errorf("%w; rolled back the binary but the restart failed: %v", cause, err)
		}
	}
	return fmt.Errorf("%w — rolled back to the previous binary", cause)
}

// replaceBinary installs src at dst atomically, keeping dst as dst+".prev".
//
// The backup is a copy rather than a rename: if staging the new bytes fails, the
// live path must still hold a working binary. The swap itself is a rename, which
// is safe against a running process (the old inode stays alive until it exits).
func replaceBinary(src, dst string) error {
	if strings.TrimSpace(src) == "" || strings.TrimSpace(dst) == "" {
		return fmt.Errorf("replace binary: empty source or destination path")
	}
	if samePath(src, dst) {
		return errSameBinary(dst)
	}
	if _, err := os.Stat(dst); err != nil {
		return fmt.Errorf("no binary at %s to upgrade — use `specula install` for a first install: %w", dst, err)
	}
	in, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read new binary %s: %w", src, err)
	}
	if len(in) == 0 {
		return fmt.Errorf("new binary %s is empty — refusing to install", src)
	}
	old, err := os.ReadFile(dst)
	if err != nil {
		return fmt.Errorf("read current binary %s: %w", dst, err)
	}
	if err := writeExecutable(dst+".prev", old); err != nil {
		return fmt.Errorf("write rollback copy: %w", err)
	}
	staged := dst + ".new"
	if err := writeExecutable(staged, in); err != nil {
		return fmt.Errorf("stage new binary: %w", err)
	}
	if err := os.Rename(staged, dst); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("swap %s into place: %w", dst, err)
	}
	return nil
}

// restoreBinary puts dst+".prev" back at dst. The backup is retained so a
// second rollback (or a human diffing the two) still works.
func restoreBinary(dst string) error {
	prev := dst + ".prev"
	body, err := os.ReadFile(prev)
	if err != nil {
		return fmt.Errorf("no rollback copy at %s: %w", prev, err)
	}
	staged := dst + ".rollback"
	if err := writeExecutable(staged, body); err != nil {
		return fmt.Errorf("stage rollback binary: %w", err)
	}
	if err := os.Rename(staged, dst); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("swap %s back into place: %w", dst, err)
	}
	return nil
}

// writeExecutable writes body at path with mode 0755 regardless of umask — a
// restrictive umask silently producing a 0700 binary would break User=specula.
func writeExecutable(path string, body []byte) error {
	if err := os.WriteFile(path, body, 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

func errSameBinary(dst string) error {
	return fmt.Errorf("upgrade source and destination are the same file (%s): "+
		"push the new binary to a staging path first (e.g. /tmp/specula) and run that one", dst)
}

func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// localHealthzURL turns a listen address into a dialable loopback probe URL.
//
// A listen address is a bind spec, not a dial spec: wildcard binds must become
// loopback, while a host-bound daemon has to be dialled on that same host or the
// probe reports a false failure and triggers a needless rollback.
func localHealthzURL(addr string, tlsOn bool) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", fmt.Errorf("empty listen address")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("parse listen address %q: %w", addr, err)
	}
	if strings.TrimSpace(port) == "" {
		return "", fmt.Errorf("listen address %q has no port", addr)
	}
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	scheme := "http"
	if tlsOn {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/healthz", scheme, net.JoinHostPort(host, port)), nil
}

// upgradeHealthURL derives the post-restart probe target from the live config.
// One listener now, so there is one address to probe.
func upgradeHealthURL(configPath string) (string, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return "", err
	}
	return localHealthzURL(cfg.EffectiveListenAddr(), cfg.Server.TLS.Enabled())
}

// waitLocalHealthz polls url until it answers 2xx or the deadline passes.
//
// TLS verification is off by design: this dials 127.0.0.1 while the cert is
// minted for the public/internal domain, so hostname verification would always
// fail. It is a process-alive check on loopback, not a trust decision.
func waitLocalHealthz(url string, timeout time.Duration) error {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // loopback liveness probe
		},
	}
	deadline := time.Now().Add(timeout)
	var last error
	for {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s not healthy within %s: %w", url, timeout, last)
		}
		time.Sleep(time.Second)
	}
}

// ─── commands ────────────────────────────────────────────────────────────────

func serviceUpgrade(args []string) error {
	fs := flag.NewFlagSet("service upgrade", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	binaryPath := fs.String("binary", defaultBinaryPath, "installed binary to replace")
	configPath := fs.String("config", defaultConfigPath, "config file used to locate the health endpoint")
	noRestart := fs.Bool("no-restart", false, "swap the binary but do not restart the service")
	skipHealth := fs.Bool("skip-health", false, "restart without waiting for /healthz")
	timeout := fs.Duration("health-timeout", defaultUpgradeHealthTimeout, "how long to wait for /healthz after restart")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("service upgrade requires root (try: sudo %s upgrade)", os.Args[0])
	}
	self, err := resolveSelf()
	if err != nil {
		return err
	}
	// Checked before anything is printed: "upgrading X: v1 → v1" followed by a
	// refusal reads like a half-applied upgrade.
	if samePath(self, *binaryPath) {
		return errSameBinary(*binaryPath)
	}

	gate := !*skipHealth && !*noRestart
	healthURL, healthErr := upgradeHealthURL(*configPath)
	if gate && healthErr != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot derive a health endpoint from %s (%v) — restarting without the health gate\n",
			*configPath, healthErr)
	}

	fmt.Fprintf(os.Stderr, "upgrading %s: %s → %s\n",
		*binaryPath, installedVersion(*binaryPath), trimVersionPrefix(version.String()))

	plan := &upgradePlan{
		self:       self,
		binary:     *binaryPath,
		noRestart:  *noRestart,
		skipHealth: *skipHealth || healthErr != nil,
		restart:    func() error { return runSystemctl("restart", "specula.service") },
		health:     func() error { return waitLocalHealthz(healthURL, *timeout) },
	}
	if err := plan.run(os.Stderr); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "verify wiring: specula doctor")
	return nil
}

func serviceRollback(args []string) error {
	fs := flag.NewFlagSet("service rollback", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	binaryPath := fs.String("binary", defaultBinaryPath, "installed binary to roll back")
	configPath := fs.String("config", defaultConfigPath, "config file used to locate the health endpoint")
	skipHealth := fs.Bool("skip-health", false, "restart without waiting for /healthz")
	timeout := fs.Duration("health-timeout", defaultUpgradeHealthTimeout, "how long to wait for /healthz after restart")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("service rollback requires root (try: sudo %s rollback)", os.Args[0])
	}
	if err := restoreBinary(*binaryPath); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "restored %s from %s.prev (now %s)\n", *binaryPath, *binaryPath, installedVersion(*binaryPath))
	if err := runSystemctl("restart", "specula.service"); err != nil {
		return err
	}
	if *skipHealth {
		return nil
	}
	healthURL, err := upgradeHealthURL(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot derive a health endpoint from %s (%v) — skipping health gate\n", *configPath, err)
		return nil
	}
	if err := waitLocalHealthz(healthURL, *timeout); err != nil {
		return fmt.Errorf("rolled back but the previous binary is not healthy either: %w", err)
	}
	fmt.Fprintln(os.Stderr, "rollback OK — specula.service healthy")
	return nil
}

func resolveSelf() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate self: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(self)
	if err != nil {
		return "", fmt.Errorf("resolve self: %w", err)
	}
	return resolved, nil
}

// installedVersion best-effort reports the version of the binary at path.
// Used only for the human-facing "old → new" line, so failures degrade to
// "unknown" rather than aborting the upgrade. The redundant "specula " prefix is
// trimmed: both sides of the arrow are Specula by definition.
func installedVersion(path string) string {
	out, err := exec.Command(path, "version").CombinedOutput()
	if err != nil {
		return "unknown"
	}
	return trimVersionPrefix(string(out))
}

func trimVersionPrefix(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "specula ")
}
