package integrate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// containerd 2.2.x ships a default CRI registry config_path of
//
//	/etc/containerd/certs.d:/etc/docker/certs.d
//
// The CRI docker-resolver path splits on ':' correctly, but the transfer-service
// pull path passes the raw string to registry.WithHostDir, which treats it as
// ONE literal directory. hosts.toml is never found, so crictl / kubelet ignore
// Specula mirrors and dial public origins (registry.k8s.io → *.pkg.dev).
// ctr --hosts-dir <single-dir> still works — exactly the CP failure mode on
// aliyun-cd-b. See containerd#12808 / #12636.
//
// Specula integrate must force a single-root config_path to the certs.d we write.

var (
	reConfigPathAssign = regexp.MustCompile(`(?m)^([ \t]*config_path[ \t]*=[ \t]*)(['"])([^'"]*)(['"])([ \t]*)$`)
	reDisabledCRI      = regexp.MustCompile(`(?i)disabled_plugins\s*=\s*\[[^\]]*['"]cri['"]`)
)

// containerdConfigTOMLs returns candidate config.toml paths for the live runtime.
func containerdConfigTOMLs() []string {
	if isK3sNode() {
		return []string{
			"/var/lib/rancher/k3s/agent/etc/containerd/config.toml",
			"/etc/containerd/config.toml",
		}
	}
	return []string{"/etc/containerd/config.toml"}
}

// isColonSeparatedCertsPath reports a multi-root certs.d config_path that
// containerd 2.2 transfer mishandles.
func isColonSeparatedCertsPath(val string) bool {
	val = strings.TrimSpace(val)
	if val == "" || !strings.Contains(val, "certs.d") {
		return false
	}
	parts := filepath.SplitList(val)
	return len(parts) > 1
}

// criConfigPathNeedsFix reports whether content has a broken colon config_path
// or lacks an explicit single-root override (so 2.2 defaults still apply).
func criConfigPathNeedsFix(content string) (needs bool, reason string) {
	if reDisabledCRI.MatchString(content) {
		return false, "cri disabled"
	}
	found := false
	for _, m := range reConfigPathAssign.FindAllStringSubmatch(content, -1) {
		val := m[3]
		if !strings.Contains(val, "certs.d") {
			continue
		}
		found = true
		if isColonSeparatedCertsPath(val) {
			return true, "colon-separated config_path"
		}
	}
	if !found {
		// Bare / comment-only configs inherit containerd 2.2's broken default.
		return true, "missing explicit config_path (2.2 default is colon-separated)"
	}
	return false, "already single-root"
}

// rewriteCRIConfigPath forces every certs.d config_path to preferred (single
// directory). When none exist, appends a v3 CRI images.registry stanza.
func rewriteCRIConfigPath(content, preferred string) (string, bool) {
	preferred = strings.TrimRight(strings.TrimSpace(preferred), "/")
	if preferred == "" {
		preferred = systemContainerdCerts
	}
	changed := false
	out := reConfigPathAssign.ReplaceAllStringFunc(content, func(line string) string {
		m := reConfigPathAssign.FindStringSubmatch(line)
		if m == nil {
			return line
		}
		val := m[3]
		if !strings.Contains(val, "certs.d") {
			return line
		}
		if val == preferred && !isColonSeparatedCertsPath(val) {
			return line
		}
		changed = true
		return m[1] + m[2] + preferred + m[4] + m[5]
	})
	if changed {
		return out, true
	}
	needs, _ := criConfigPathNeedsFix(content)
	if !needs {
		return content, false
	}
	block := fmt.Sprintf(`

# managed by specula integrate — single-root hosts.toml for CRI
# (containerd 2.2 colon-separated config_path is ignored by transfer service;
#  see https://github.com/containerd/containerd/issues/12808)
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = %q
`, preferred)
	// Also set legacy v1 CRI key when the file still uses it.
	if strings.Contains(content, "io.containerd.grpc.v1.cri") {
		block += fmt.Sprintf(`
[plugins."io.containerd.grpc.v1.cri".registry]
  config_path = %q
`, preferred)
	}
	return content + block, true
}

// integrateContainerdCRIConfigPath rewrites config.toml so CRI pulls honour
// hosts.toml under certsDir. Returns a Result for the OCI merge.
func integrateContainerdCRIConfigPath(certsDir string, dryRun, skipRoot bool) Result {
	certsDir = strings.TrimRight(strings.TrimSpace(certsDir), "/")
	if certsDir == "" {
		certsDir = systemContainerdCerts
	}
	paths := containerdConfigTOMLs()
	var (
		last Result
		any  bool
	)
	for _, path := range paths {
		r := fixOneContainerdConfigPath(path, certsDir, dryRun, skipRoot)
		if r.Action == "skipped" && r.Detail == "config.toml absent" {
			continue
		}
		any = true
		last = r
		if r.Action == "error" {
			return r
		}
		// Prefer reporting a successful rewrite over later "already".
		if r.Action == "added" {
			return r
		}
	}
	if !any {
		// No config.toml yet — write a minimal drop-in that only sets config_path
		// so kubeadm / package defaults cannot keep the colon list.
		path := paths[0]
		return fixOneContainerdConfigPath(path, certsDir, dryRun, skipRoot)
	}
	return last
}

func fixOneContainerdConfigPath(path, certsDir string, dryRun, skipRoot bool) Result {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if dryRun {
				return Result{
					Action: "added",
					Detail: "would create " + path + " with single-root config_path=" + certsDir + " (containerd 2.2 CRI colon-path bug)",
					Path:   path,
				}
			}
			if skipRoot && isSystemPath(path) {
				return Result{
					Action: "skipped",
					Detail: "skip-root: not writing " + path + " — CRI may ignore hosts.toml until config_path is a single directory; re-run: sudo specula integrate --protocols oci",
					Path:   path,
				}
			}
			body, _ := rewriteCRIConfigPath("", certsDir)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				if os.IsPermission(err) {
					return Result{Action: "skipped", Detail: "need root to write " + path, Path: path}
				}
				return Result{Action: "error", Err: err.Error(), Path: path}
			}
			if err := os.WriteFile(path, []byte(strings.TrimLeft(body, "\n")), 0o644); err != nil {
				if os.IsPermission(err) {
					return Result{Action: "skipped", Detail: "need root to write " + path, Path: path}
				}
				return Result{Action: "error", Err: err.Error(), Path: path}
			}
			return Result{
				Action: "added",
				Detail: "wrote " + path + " config_path=" + certsDir + " (fix containerd 2.2 CRI colon-path); restart containerd then: crictl pull registry.k8s.io/etcd:3.5.24-0",
				Path:   path,
			}
		}
		if os.IsPermission(err) {
			return Result{Action: "skipped", Detail: "need root to read " + path, Path: path}
		}
		return Result{Action: "error", Err: err.Error(), Path: path}
	}

	content := string(raw)
	needs, reason := criConfigPathNeedsFix(content)
	if !needs {
		return Result{
			Action: "already",
			Detail: "CRI config_path already single-root (" + reason + ")",
			Path:   path,
		}
	}
	next, changed := rewriteCRIConfigPath(content, certsDir)
	if !changed {
		return Result{Action: "already", Detail: "CRI config_path ok", Path: path}
	}
	detail := fmt.Sprintf("fix CRI config_path (%s) → %s — restart containerd so crictl/kubelet use hosts.toml", reason, certsDir)
	if dryRun {
		return Result{Action: "added", Detail: "would " + detail, Path: path}
	}
	if skipRoot && isSystemPath(path) {
		return Result{
			Action: "skipped",
			Detail: "skip-root: " + detail + "; re-run: sudo specula integrate --protocols oci",
			Path:   path,
		}
	}
	bak := path + ".bak.specula"
	_ = os.WriteFile(bak, raw, 0o644)
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		if os.IsPermission(err) {
			return Result{Action: "skipped", Detail: "need root to write " + path + " (" + detail + ")", Path: path}
		}
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	return Result{Action: "added", Detail: detail + " (backup: " + bak + ")", Path: path}
}

func isSystemPath(path string) bool {
	return strings.HasPrefix(path, "/etc/") || strings.HasPrefix(path, "/var/lib/rancher/")
}
