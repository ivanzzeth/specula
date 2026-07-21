package integrate

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// integrateDocker additively wires Specula as a Docker/OCI pull-through mirror:
//
//   - registry-mirrors: Specula first, existing mirrors kept
//   - insecure-registries: host:port when Specula is http:// (required for local
//     plain-HTTP registries)
//
// Prefer /etc/docker/daemon.json (what dockerd actually reads). Without root,
// write a user-visible copy + a merge snippet under ~/.config/specula/ and tell
// the operator to re-run with sudo.
func integrateDocker(home, addr string, dryRun, skipRoot bool) Result {
	mirror := strings.TrimRight(addr, "/")
	insecureHost := dockerInsecureHost(addr) // empty when https

	userPath := filepath.Join(home, ".config", "docker", "daemon.json")
	systemPath := "/etc/docker/daemon.json"
	snippetPath := filepath.Join(home, ".config", "specula", "docker-daemon.snippet.json")

	mergeOne := func(p string, needRoot bool) Result {
		cfg, err := readDockerDaemon(p)
		if err != nil && !os.IsNotExist(err) {
			if needRoot && os.IsPermission(err) {
				return Result{Action: "skipped", Detail: "need root to edit " + p, Path: p}
			}
			return Result{Action: "error", Err: err.Error(), Path: p}
		}
		if cfg == nil {
			cfg = map[string]any{}
		}

		mirrors := dockerMirrors(cfg)
		insecs := dockerInsecures(cfg)
		changed := false

		if !containsURL(mirrors, mirror) {
			mirrors = append([]string{mirror}, mirrors...)
			changed = true
		}
		if insecureHost != "" && !containsFold(insecs, insecureHost) {
			insecs = append(insecs, insecureHost)
			changed = true
		}

		if !changed {
			detail := "registry-mirrors already contains Specula"
			if insecureHost != "" {
				detail += "; insecure-registries ok"
			}
			return Result{Action: "already", Detail: detail, Path: p}
		}

		detailParts := []string{"registry-mirrors ← " + mirror}
		if insecureHost != "" {
			detailParts = append(detailParts, "insecure-registries ← "+insecureHost)
		}
		detail := strings.Join(detailParts, "; ") + " (restart docker to apply)"

		if dryRun {
			return Result{Action: "added", Detail: "would update: " + detail, Path: p}
		}

		cfg["registry-mirrors"] = mirrors
		if len(insecs) > 0 {
			cfg["insecure-registries"] = insecs
		}
		if err := writeDockerDaemon(p, cfg); err != nil {
			if needRoot && os.IsPermission(err) {
				return Result{Action: "skipped", Detail: "need root to edit " + p, Path: p}
			}
			return Result{Action: "error", Err: err.Error(), Path: p}
		}
		return Result{Action: "added", Detail: detail, Path: p}
	}

	writeSnippet := func() {
		snip := map[string]any{
			"registry-mirrors": []string{mirror},
		}
		if insecureHost != "" {
			snip["insecure-registries"] = []string{insecureHost}
		}
		_ = os.MkdirAll(filepath.Dir(snippetPath), 0o755)
		_ = writeDockerDaemon(snippetPath, snip)
	}

	if skipRoot {
		r := mergeOne(userPath, false)
		if !dryRun && (r.Action == "added" || r.Action == "already") {
			writeSnippet()
			if r.Action == "added" {
				r.Detail += "; note: dockerd reads /etc/docker/daemon.json — re-run: sudo specula integrate --protocols oci"
			}
		}
		return r
	}

	// Default: system path first (live dockerd), then user copy for visibility.
	sys := mergeOne(systemPath, true)
	if !dryRun {
		writeSnippet()
		_ = os.MkdirAll(filepath.Dir(userPath), 0o755)
		_ = mergeOne(userPath, false) // best-effort mirror of settings
	}

	if sys.Action == "skipped" {
		// Fall back to user file so something is written, but be honest.
		ur := mergeOne(userPath, false)
		if ur.Action == "added" || ur.Action == "already" {
			ur.Detail += "; WARNING: live dockerd uses " + systemPath + " — run: sudo specula integrate --protocols oci"
			if !dryRun {
				writeSnippet()
			}
			return ur
		}
		sys.Detail += " — run: sudo specula integrate --protocols oci"
		return sys
	}
	return sys
}

// dockerInsecureHost returns "host:port" for http Specula URLs (dockerd
// insecure-registries form). Empty for https.
func dockerInsecureHost(addr string) string {
	u, err := url.Parse(strings.TrimSpace(addr))
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return ""
	}
	return u.Host // already host:port when port present
}

func readDockerDaemon(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg, nil
}

func writeDockerDaemon(path string, cfg map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func dockerMirrors(cfg map[string]any) []string {
	return dockerStringList(cfg, "registry-mirrors")
}

func dockerInsecures(cfg map[string]any) []string {
	return dockerStringList(cfg, "insecure-registries")
}

func dockerStringList(cfg map[string]any, key string) []string {
	raw, ok := cfg[key]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return append([]string(nil), v...)
	default:
		return nil
	}
}
