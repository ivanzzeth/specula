package integrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ivanzzeth/specula/internal/bootstrap"
)

const systemContainerdCerts = "/etc/containerd/certs.d"

// k3s ships its own containerd; the live certs.d root is under the agent tree
// (server nodes use the same agent path). Writing only /etc/containerd/certs.d
// is a no-op for k3s — live incident: integrate reported success while k3s
// kept Hub-style hosts.toml generated from registries.yaml.
const k3sAgentContainerdCerts = "/var/lib/rancher/k3s/agent/etc/containerd/certs.d"

// resolveContainerdCertsDirs returns the certs.d roots Specula integrate must
// write. When a k3s install is detected, ONLY the k3s agent path is used
// (that is what containerd reads). Otherwise the stock /etc/containerd path.
func resolveContainerdCertsDirs() []string {
	if isK3sNode() {
		return []string{k3sAgentContainerdCerts}
	}
	return []string{systemContainerdCerts}
}

func isK3sNode() bool {
	if fileExists(k3sAgentContainerdCerts) || dirExists(k3sAgentContainerdCerts) {
		return true
	}
	// Fresh node: certs.d may not exist yet, but the k3s tree does.
	if dirExists("/var/lib/rancher/k3s") || fileExists("/usr/local/bin/k3s") || fileExists("/usr/bin/k3s") {
		return true
	}
	return false
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// integrateContainerdCerts writes containerd hosts.toml drop-ins so non-Hub
// registries (ghcr, quay, codeberg, …) reach Specula with override_path.
// docker.io stays a plain mirror host (Hub-relative paths).
func integrateContainerdCerts(home, addr string, dryRun, skipRoot bool) Result {
	endpoint := strings.TrimRight(addr, "/")
	skipVerify := strings.HasPrefix(strings.ToLower(endpoint), "http://")
	regs := append([]string(nil), bootstrap.DefaultOCIRegistries...)

	systemDirs := resolveContainerdCertsDirs()
	primaryDir := systemDirs[0]
	userDir := filepath.Join(home, ".config", "specula", "certs.d")

	alreadyOK := func(dir string) bool {
		// Spot-check one override_path registry + docker.io.
		codeberg := filepath.Join(dir, "codeberg.org", "hosts.toml")
		docker := filepath.Join(dir, "docker.io", "hosts.toml")
		b1, err1 := os.ReadFile(codeberg)
		b2, err2 := os.ReadFile(docker)
		if err1 != nil || err2 != nil {
			return false
		}
		return strings.Contains(string(b1), "override_path") &&
			strings.Contains(string(b1), endpoint+"/v2/codeberg.org") &&
			strings.Contains(string(b2), endpoint)
	}

	writeTo := func(dir string) Result {
		if alreadyOK(dir) {
			return Result{
				Action: "already",
				Detail: "containerd certs.d already points at Specula (" + strings.Join(regs, ",") + ")",
				Path:   dir,
			}
		}
		detail := fmt.Sprintf("containerd certs.d ← %s (%d registries; non-docker.io use override_path)",
			endpoint, len(regs))
		if dryRun {
			return Result{Action: "added", Detail: "would write: " + detail, Path: dir}
		}
		if err := bootstrap.WriteContainerdHosts(bootstrap.MirrorOptions{
			CertsDir:   dir,
			Endpoint:   endpoint,
			Registries: regs,
			SkipVerify: skipVerify,
		}); err != nil {
			return Result{Action: "error", Err: err.Error(), Path: dir}
		}
		return Result{Action: "added", Detail: detail, Path: dir}
	}

	if skipRoot {
		r := writeTo(userDir)
		if r.Action == "added" {
			r.Detail += "; copy to " + primaryDir + " or re-run: sudo specula integrate --protocols oci"
		}
		return r
	}

	// Prefer the live containerd certs.d (k3s agent path or /etc/containerd).
	var last Result
	for _, systemDir := range systemDirs {
		if err := os.MkdirAll(systemDir, 0o755); err != nil {
			if os.IsPermission(err) {
				r := writeTo(userDir)
				if r.Action == "added" || r.Action == "already" {
					r.Detail += "; WARNING: live containerd uses " + systemDir + " — run: sudo specula integrate --protocols oci"
				}
				return r
			}
			return Result{Action: "error", Err: err.Error(), Path: systemDir}
		}
		last = writeTo(systemDir)
		if last.Action == "error" {
			if strings.Contains(last.Err, "permission") {
				ur := writeTo(userDir)
				if ur.Action == "added" || ur.Action == "already" {
					ur.Detail += "; WARNING: live containerd uses " + systemDir + " — run: sudo specula integrate --protocols oci"
					return ur
				}
			}
			return last
		}
	}
	if !dryRun && (last.Action == "added" || last.Action == "already") {
		// Best-effort user copy for visibility.
		_ = writeTo(userDir)
	}
	return last
}

// mergeOCIResults combines dockerd daemon.json + containerd certs.d outcomes
// into one Result for the oci protocol.
func mergeOCIResults(docker, certs Result) Result {
	parts := make([]string, 0, 2)
	if docker.Detail != "" {
		parts = append(parts, docker.Detail)
	}
	if certs.Detail != "" {
		parts = append(parts, certs.Detail)
	}
	if docker.Err != "" {
		parts = append(parts, "docker: "+docker.Err)
	}
	if certs.Err != "" {
		parts = append(parts, "containerd: "+certs.Err)
	}
	action := "added"
	switch {
	case docker.Action == "error" || certs.Action == "error":
		action = "error"
	case docker.Action == "skipped" && certs.Action == "skipped":
		action = "skipped"
	case docker.Action == "already" && (certs.Action == "already" || certs.Action == "skipped"):
		action = "already"
	case docker.Action == "already" && certs.Action == "added":
		action = "added"
	case docker.Action == "added":
		action = "added"
	default:
		action = certs.Action
		if action == "" {
			action = docker.Action
		}
	}
	path := docker.Path
	if path == "" {
		path = certs.Path
	}
	errParts := make([]string, 0, 2)
	if docker.Err != "" {
		errParts = append(errParts, docker.Err)
	}
	if certs.Err != "" {
		errParts = append(errParts, certs.Err)
	}
	return Result{
		Protocol: "oci",
		Action:   action,
		Detail:   strings.Join(parts, "; "),
		Path:     path,
		Err:      strings.Join(errParts, "; "),
	}
}
