package main

import (
	"embed"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ivanzzeth/specula/internal/config"
)

//go:embed systemd/specula.service
var systemdUnitFS embed.FS

const (
	defaultBinaryPath = "/usr/local/bin/specula"
	defaultConfigPath = "/etc/specula/specula.yaml"
	defaultDataDir    = "/var/lib/specula"
	defaultUnitPath   = "/etc/systemd/system/specula.service"
	defaultUnitUser   = "specula"
)

// runService implements: specula service install|uninstall|status|enable|disable|start|stop
func runService(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: specula service <install|uninstall|status|enable|disable|start|stop>")
	}
	cmd := args[0]
	rest := args[1:]
	switch cmd {
	case "install":
		return serviceInstall(rest)
	case "uninstall":
		return serviceUninstall(rest)
	case "status":
		return runSystemctl("status", "specula.service")
	case "enable":
		return runSystemctl("enable", "--now", "specula.service")
	case "disable":
		return runSystemctl("disable", "--now", "specula.service")
	case "start":
		return runSystemctl("start", "specula.service")
	case "stop":
		return runSystemctl("stop", "specula.service")
	case "help", "-h", "--help":
		fmt.Print(serviceUsage)
		return nil
	default:
		return fmt.Errorf("unknown service command %q\n%s", cmd, serviceUsage)
	}
}

const serviceUsage = `Usage:
  specula install | specula service install [--config PATH] [--binary PATH] [--user NAME] [--no-start]
  specula uninstall | specula service uninstall [--purge]
  specula service status|enable|disable|start|stop

Installs a systemd unit so Specula starts on boot (WantedBy=multi-user.target).
Requires root. Creates system user, /etc/specula, /var/lib/specula if missing.
Config is written from the embedded example when missing (no external YAML required).
`

func serviceInstall(args []string) error {
	fs := flag.NewFlagSet("service install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", defaultConfigPath, "config file path written into the unit")
	binaryPath := fs.String("binary", defaultBinaryPath, "destination for the specula binary")
	unitUser := fs.String("user", defaultUnitUser, "system user to run as")
	noStart := fs.Bool("no-start", false, "install and enable, but do not start yet")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("service install requires root (try: sudo specula service install)")
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return fmt.Errorf("resolve self: %w", err)
	}

	if err := ensureSystemUser(*unitUser); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Dir(*configPath), defaultDataDir, filepath.Dir(*binaryPath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := chownPath(defaultDataDir, *unitUser); err != nil {
		return err
	}

	if filepath.Clean(self) != filepath.Clean(*binaryPath) {
		in, err := os.ReadFile(self)
		if err != nil {
			return err
		}
		tmp := *binaryPath + ".new"
		if err := os.WriteFile(tmp, in, 0o755); err != nil {
			return err
		}
		if err := os.Rename(tmp, *binaryPath); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		fmt.Fprintf(os.Stderr, "installed binary → %s\n", *binaryPath)
	}

	created, err := config.WriteExampleIfMissing(*configPath, patchConfigForSystemInstall)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(os.Stderr, "wrote config → %s (embedded example; storage under %s)\n", *configPath, defaultDataDir)
	} else {
		fmt.Fprintf(os.Stderr, "keeping existing config %s\n", *configPath)
	}

	unitBody, err := renderUnit(*binaryPath, *configPath, *unitUser)
	if err != nil {
		return err
	}
	if err := os.WriteFile(defaultUnitPath, []byte(unitBody), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote unit → %s\n", defaultUnitPath)

	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", "specula.service"); err != nil {
		return err
	}
	if !*noStart {
		if err := runSystemctl("restart", "specula.service"); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "specula.service enabled and started (boots with multi-user.target)")
	} else {
		fmt.Fprintln(os.Stderr, "specula.service enabled (not started; --no-start)")
	}
	return nil
}

func serviceUninstall(args []string) error {
	fs := flag.NewFlagSet("service uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also remove binary, config, and data dir")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("service uninstall requires root")
	}
	_ = runSystemctl("disable", "--now", "specula.service")
	_ = os.Remove(defaultUnitPath)
	_ = runSystemctl("daemon-reload")
	if *purge {
		_ = os.Remove(defaultBinaryPath)
		_ = os.RemoveAll(filepath.Dir(defaultConfigPath))
		_ = os.RemoveAll(defaultDataDir)
		fmt.Fprintln(os.Stderr, "purged binary, /etc/specula, /var/lib/specula")
	}
	fmt.Fprintln(os.Stderr, "specula.service removed")
	return nil
}

func renderUnit(binary, config, unitUser string) (string, error) {
	raw, err := systemdUnitFS.ReadFile("systemd/specula.service")
	if err != nil {
		return "", fmt.Errorf("embed unit: %w", err)
	}
	body := string(raw)
	body = strings.ReplaceAll(body, "User=specula", "User="+unitUser)
	body = strings.ReplaceAll(body, "Group=specula", "Group="+unitUser)
	body = strings.ReplaceAll(body,
		"ExecStart=/usr/local/bin/specula --config /etc/specula/specula.yaml",
		fmt.Sprintf("ExecStart=%s --config %s", binary, config))
	return body, nil
}

func ensureSystemUser(name string) error {
	if _, err := user.Lookup(name); err == nil {
		return nil
	}
	cmd := exec.Command("useradd", "-r", "-s", "/usr/sbin/nologin", "-d", defaultDataDir, "-M", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("useradd %s: %v (%s)", name, err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(os.Stderr, "created system user %s\n", name)
	return nil
}

func chownPath(path, name string) error {
	u, err := user.Lookup(name)
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	return os.Chown(path, uid, gid)
}

func runSystemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func patchConfigForSystemInstall(src string) string {
	repl := []struct{ old, new string }{
		{"~/.specula/blobs", defaultDataDir + "/blobs"},
		{"~/.specula/meta.db", defaultDataDir + "/meta.db"},
		{"~/.specula/git", defaultDataDir + "/git"},
		{"./data/blobs", defaultDataDir + "/blobs"},
		{"./data/meta.db", defaultDataDir + "/meta.db"},
		{"./data/git", defaultDataDir + "/git"},
		{"/tmp/specula-blobs", defaultDataDir + "/blobs"},
		{"/tmp/specula-meta.db", defaultDataDir + "/meta.db"},
		{"/tmp/specula-git", defaultDataDir + "/git"},
	}
	out := src
	for _, r := range repl {
		out = strings.ReplaceAll(out, r.old, r.new)
	}
	return out
}
