package integrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ivanzzeth/specula/internal/bootstrap"
)

const systemContainerdCerts = "/etc/containerd/certs.d"

// integrateContainerdCerts writes containerd hosts.toml drop-ins so non-Hub
// registries (ghcr, quay, codeberg, …) reach Specula with override_path.
// docker.io stays a plain mirror host (Hub-relative paths).
func integrateContainerdCerts(home, addr string, dryRun, skipRoot bool) Result {
	endpoint := strings.TrimRight(addr, "/")
	skipVerify := strings.HasPrefix(strings.ToLower(endpoint), "http://")
	regs := append([]string(nil), bootstrap.DefaultOCIRegistries...)

	systemDir := systemContainerdCerts
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
			r.Detail += "; copy to " + systemDir + " or re-run: sudo specula integrate --protocols oci"
		}
		return r
	}

	// Prefer system certs.d (what containerd reads).
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

	r := writeTo(systemDir)
	if !dryRun && (r.Action == "added" || r.Action == "already") {
		// Best-effort user copy for visibility.
		_ = writeTo(userDir)
	}
	if r.Action == "error" && strings.Contains(r.Err, "permission") {
		ur := writeTo(userDir)
		if ur.Action == "added" || ur.Action == "already" {
			ur.Detail += "; WARNING: live containerd uses " + systemDir + " — run: sudo specula integrate --protocols oci"
			return ur
		}
	}
	return r
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
