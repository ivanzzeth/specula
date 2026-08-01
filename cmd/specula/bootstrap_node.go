package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ivanzzeth/specula/internal/bootstrap"
	"github.com/ivanzzeth/specula/internal/integrate"
)

// runBootstrapNode implements distroless-friendly node wiring for DaemonSets:
//
//	specula bootstrap-node --endpoint … --certs-dir … [--hold] [--reconcile-interval 5m]
//
// Writes containerd hosts.toml, runs integrate (OCI / CRI config_path), optionally
// restarts containerd once per config hash after Specula is healthy, then holds
// and periodically reconciles.
func runBootstrapNode(args []string) error {
	fs := flag.NewFlagSet("bootstrap-node", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	endpoint := fs.String("endpoint", "http://127.0.0.1:30732", "Specula data-plane URL the node dials")
	certsDir := fs.String("certs-dir", "/etc/containerd/certs.d", "containerd certs.d root")
	k3sCertsDir := fs.String("k3s-certs-dir", "/var/lib/rancher/k3s/agent/etc/containerd/certs.d",
		"also write hosts.toml here on real k3s nodes")
	registries := fs.String("registries", strings.Join(bootstrap.DefaultOCIRegistries, ","), "comma-separated registries")
	skipVerify := fs.Bool("skip-verify", true, "set skip_verify on mirror host entries (HTTP NodePort)")
	caFile := fs.String("ca-file", "", "PEM CA on the node for HTTPS Specula")
	// Default MUST be integrate.DefaultProtocols (all). An "oci"-only default
	// silently narrowed every DaemonSet-wired node to OCI client config and
	// violated the Specula delivery contract (chorei iron law #23: never pass
	// a protocols subset — omit or use DefaultProtocols).
	protocols := fs.String("protocols", strings.Join(integrate.DefaultProtocols, ","),
		"comma-separated integrate protocols (default: all DefaultProtocols)")
	configPath := fs.String("config", "/etc/specula/specula.yaml", "specula.yaml for multi-source wiring")
	skipSchemeProbe := fs.Bool("skip-scheme-probe", true, "do not auto-upgrade http→https")
	restartMode := fs.String("restart-containerd", "once",
		`containerd reload: "false" | "true" (always) | "once" (hash stamp + healthz gate)`)
	stampDir := fs.String("stamp-dir", "/var/lib/specula", "host dir for .cri-reload-hash stamp")
	runDoctor := fs.Bool("doctor", true, "run specula doctor after integrate")
	hold := fs.Bool("hold", false, "sleep forever after setup (DaemonSet)")
	reconcileEvery := fs.Duration("reconcile-interval", 0, "when --hold, re-run wiring on this interval (0=no loop beyond first pass)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage:
  specula bootstrap-node [flags]

Distroless node setup for the bootstrap integrate DaemonSet: write hosts.toml,
run integrate (OCI), optional doctor, optional containerd reload, hold+reconcile.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	mode := strings.ToLower(strings.TrimSpace(*restartMode))
	switch mode {
	case "", "false", "0", "no", "off":
		mode = "false"
	case "true", "1", "yes", "always":
		mode = "true"
	case "once":
		mode = "once"
	default:
		return fmt.Errorf("bootstrap-node: invalid --restart-containerd %q (want false|true|once)", *restartMode)
	}

	pass := func(round int) error {
		return bootstrapNodePass(bootstrapNodePassOpts{
			Endpoint:        *endpoint,
			CertsDir:        *certsDir,
			K3sCertsDir:     *k3sCertsDir,
			Registries:      *registries,
			SkipVerify:      *skipVerify,
			CAFile:          strings.TrimSpace(*caFile),
			Protocols:       *protocols,
			ConfigPath:      *configPath,
			SkipSchemeProbe: *skipSchemeProbe,
			RestartMode:     mode,
			StampDir:        strings.TrimSpace(*stampDir),
			RunDoctor:       *runDoctor,
			Round:           round,
		})
	}

	if err := pass(0); err != nil {
		return err
	}
	if !*hold {
		return nil
	}

	interval := *reconcileEvery
	fmt.Fprintln(os.Stdout, "bootstrap-node: holding (SIGINT/SIGTERM to exit)")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if interval <= 0 {
		<-ctx.Done()
		return nil
	}
	fmt.Fprintf(os.Stdout, "bootstrap-node: reconcile every %s\n", interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	round := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			round++
			if err := pass(round); err != nil {
				fmt.Fprintf(os.Stderr, "bootstrap-node: reconcile warn: %v\n", err)
			}
		}
	}
}

type bootstrapNodePassOpts struct {
	Endpoint, CertsDir, K3sCertsDir, Registries string
	SkipVerify                                  bool
	CAFile, Protocols, ConfigPath               string
	SkipSchemeProbe                             bool
	RestartMode, StampDir                       string
	RunDoctor                                   bool
	Round                                       int
}

func bootstrapNodePass(o bootstrapNodePassOpts) error {
	var regs []string
	for _, r := range strings.Split(o.Registries, ",") {
		r = strings.TrimSpace(r)
		if r != "" {
			regs = append(regs, r)
		}
	}
	mopts := bootstrap.MirrorOptions{
		CertsDir:   o.CertsDir,
		Endpoint:   o.Endpoint,
		Registries: regs,
		SkipVerify: o.SkipVerify,
		CaFile:     o.CAFile,
	}
	if err := bootstrap.WriteContainerdHosts(mopts); err != nil {
		return fmt.Errorf("primary certs.d: %w", err)
	}
	if o.Round == 0 {
		fmt.Fprintf(os.Stdout, "bootstrap-node: wrote hosts under %s → %s\n", o.CertsDir, o.Endpoint)
	}

	k3s := strings.TrimSpace(o.K3sCertsDir)
	if k3s != "" && !hostLooksLikeK3s() {
		k3s = ""
	}
	if k3s != "" {
		mopts.CertsDir = k3s
		if err := bootstrap.WriteContainerdHosts(mopts); err != nil {
			fmt.Fprintf(os.Stderr, "bootstrap-node: k3s certs.d warn: %v\n", err)
		} else if o.Round == 0 {
			fmt.Fprintf(os.Stdout, "bootstrap-node: wrote hosts under %s\n", k3s)
		}
	}

	var protos []string
	for _, p := range strings.Split(o.Protocols, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			protos = append(protos, p)
		}
	}
	if len(protos) == 0 {
		// Empty --protocols means "all", matching integrate.Run's own default
		// when Options.Protocols is nil/empty — never silently fall back to oci.
		protos = append([]string(nil), integrate.DefaultProtocols...)
	}
	cfgPath := strings.TrimSpace(o.ConfigPath)
	if cfgPath != "" {
		if _, err := os.Stat(cfgPath); err != nil {
			cfgPath = ""
		}
	}
	rep, err := integrate.Run(integrate.Options{
		Addr:            o.Endpoint,
		Protocols:       protos,
		ConfigPath:      cfgPath,
		CAFile:          o.CAFile,
		SkipSchemeProbe: o.SkipSchemeProbe,
	})
	if err != nil {
		return fmt.Errorf("integrate: %w", err)
	}
	if o.Round == 0 {
		fmt.Print(integrate.PrintReport(rep))
	}

	if err := maybeRestartContainerd(o.RestartMode, o.StampDir, o.CertsDir, o.Endpoint); err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap-node: containerd reload warn: %v\n", err)
	}

	if o.RunDoctor && o.Round == 0 {
		drep, derr := integrate.Doctor(integrate.DoctorOptions{Addr: o.Endpoint})
		if derr != nil {
			fmt.Fprintf(os.Stderr, "bootstrap-node: doctor warn: %v\n", derr)
		} else {
			fmt.Print(integrate.PrintReport(drep))
		}
	}
	return nil
}

func maybeRestartContainerd(mode, stampDir, certsDir, endpoint string) error {
	switch mode {
	case "false":
		return nil
	case "true":
		if err := restartHostContainerd(); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "bootstrap-node: restarted containerd (always)")
		return nil
	case "once":
		hash := bootstrap.CRIReloadHash(certsDir)
		if stampDir == "" {
			stampDir = "/var/lib/specula"
		}
		if !bootstrap.NeedsCRIReload(stampDir, hash) {
			return nil
		}
		if err := waitSpeculaHealthz(endpoint, 90*time.Second); err != nil {
			return fmt.Errorf("defer reload until Specula healthy: %w", err)
		}
		// Stamp BEFORE restart: the process is killed when containerd bounces,
		// so a post-restart write never lands and would death-loop.
		if err := bootstrap.WriteCRIReloadStamp(stampDir, hash); err != nil {
			return fmt.Errorf("stamp: %w", err)
		}
		if err := restartHostContainerd(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "bootstrap-node: restarted containerd once (hash=%s)\n", hash[:8])
		return nil
	default:
		return fmt.Errorf("unknown restart mode %q", mode)
	}
}

func waitSpeculaHealthz(endpoint string, timeout time.Duration) error {
	url := strings.TrimRight(endpoint, "/") + "/healthz"
	deadline := time.Now().Add(timeout)
	var last error
	client := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
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
		time.Sleep(2 * time.Second)
	}
	if last == nil {
		last = fmt.Errorf("timeout")
	}
	return last
}

// hostLooksLikeK3s mirrors internal/integrate detectK3sNode — binary or real
// server/run paths only (not DirectoryOrCreate stub certs.d).
func hostLooksLikeK3s() bool {
	for _, p := range []string{"/usr/local/bin/k3s", "/usr/bin/k3s"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	for _, p := range []string{"/run/k3s", "/var/lib/rancher/k3s/server"} {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

// restartHostContainerd best-effort restarts the host containerd via PID 1's
// rootfs (privileged DaemonSet + hostPID). Falls back to k3s restart.
func restartHostContainerd() error {
	root := "/proc/1/root"
	if _, err := os.Stat(root); err != nil {
		return fmt.Errorf("host PID 1 root not available (need privileged+hostPID): %w", err)
	}
	try := func(bin string, args ...string) error {
		path := filepath.Join(root, strings.TrimPrefix(bin, "/"))
		if _, err := os.Stat(path); err != nil {
			cmd := exec.Command(bin, args...)
			cmd.Env = os.Environ()
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s %v: %w (%s)", bin, args, err, truncateBytes(string(out), 200))
			}
			return nil
		}
		cmd := exec.Command(path, args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
			"SYSTEMD_IGNORE_CHROOT=1",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w (%s)", path, err, truncateBytes(string(out), 200))
		}
		return nil
	}
	nsenter := filepath.Join(root, "usr/bin/nsenter")
	if _, err := os.Stat(nsenter); err == nil {
		for _, unit := range []string{"containerd", "k3s", "k3s-agent"} {
			cmd := exec.Command(nsenter, "-t", "1", "-m", "-u", "-i", "-n", "--",
				"systemctl", "restart", unit)
			if err := cmd.Run(); err == nil {
				fmt.Fprintf(os.Stdout, "bootstrap-node: systemctl restart %s ok\n", unit)
				return nil
			}
		}
	}
	if err := try("/usr/bin/systemctl", "restart", "containerd"); err == nil {
		return nil
	}
	_ = try("/usr/bin/systemctl", "restart", "k3s")
	time.Sleep(2 * time.Second)
	return nil
}

func truncateBytes(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
