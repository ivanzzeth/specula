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
// Separately, plugins.'io.containerd.transfer.v1.local'.config_path defaults to
// '' (empty). Even when CRI registry config_path is fixed, leaving transfer empty
// means transfer-service consumers never load hosts.toml. Specula integrate
// always sets BOTH to the same single-root certs.d.

var (
	reConfigPathAssign = regexp.MustCompile(`(?m)^([ \t]*config_path[ \t]*=[ \t]*)(['"])([^'"]*)(['"])([ \t]*)$`)
	reDisabledCRI      = regexp.MustCompile(`(?im)^([ \t]*disabled_plugins[ \t]*=[ \t]*)\[([^\]]*)\]([ \t]*)$`)
	reCRIV1ImagesReg   = regexp.MustCompile(`(?m)^[ \t]*\[plugins\.(?:'io\.containerd\.cri\.v1\.images'|"io\.containerd\.cri\.v1\.images")\.registry\][ \t]*(?:\r?\n|$)`)
	// Allow leading whitespace: containerd config dump and some drop-ins indent
	// plugin tables ("  [plugins.'io.containerd.transfer.v1.local']"). Matching
	// only column-0 headers caused integrate to APPEND a duplicate table →
	// "table io.containerd.transfer.v1.local already exists" on restart.
	reTransferSection = regexp.MustCompile(`(?m)^[ \t]*\[plugins\.(?:'io\.containerd\.transfer\.v1\.local'|"io\.containerd\.transfer\.v1\.local")\][ \t]*(?:\r?\n|$)`)
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

// criV1ImagesRegistryBody returns the body of plugins.*.cri.v1.images.registry.
// containerd 2.x uses THIS table for crictl/kubelet pulls — NOT the legacy
// io.containerd.grpc.v1.cri.registry key. A single-root on the legacy key alone
// still leaves the 2.2 default colon path active (W2 aliyun-cd-b failure).
func criV1ImagesRegistryBody(content string) (body string, ok bool) {
	loc := reCRIV1ImagesReg.FindStringIndex(content)
	if loc == nil {
		return "", false
	}
	rest := content[loc[1]:]
	end := len(rest)
	off := 0
	for i, line := range strings.SplitAfter(rest, "\n") {
		trim := strings.TrimSpace(line)
		if i > 0 && strings.HasPrefix(trim, "[") {
			end = off
			break
		}
		off += len(line)
	}
	return rest[:end], true
}

// criDisabled reports Ubuntu/docker leftovers: disabled_plugins = ["cri"].
func criDisabled(content string) bool {
	for _, m := range reDisabledCRI.FindAllStringSubmatch(content, -1) {
		inner := strings.ToLower(m[2])
		if strings.Contains(inner, `"cri"`) || strings.Contains(inner, `'cri'`) ||
			strings.Contains(inner, "io.containerd.grpc.v1.cri") {
			return true
		}
	}
	return false
}

// enableCRI clears "cri" from disabled_plugins so ImageService comes up.
func enableCRI(content string) (string, bool) {
	if !criDisabled(content) {
		return content, false
	}
	changed := false
	out := reDisabledCRI.ReplaceAllStringFunc(content, func(line string) string {
		m := reDisabledCRI.FindStringSubmatch(line)
		if m == nil {
			return line
		}
		parts := strings.Split(m[2], ",")
		keep := make([]string, 0, len(parts))
		for _, p := range parts {
			t := strings.TrimSpace(p)
			tl := strings.ToLower(strings.Trim(t, `"'`))
			if tl == "cri" || tl == "io.containerd.grpc.v1.cri" {
				changed = true
				continue
			}
			if t != "" {
				keep = append(keep, t)
			}
		}
		return m[1] + "[" + strings.Join(keep, ", ") + "]" + m[3]
	})
	return out, changed
}

// criConfigPathNeedsFix reports whether content lacks an explicit single-root
// io.containerd.cri.v1.images.registry config_path (so 2.2 defaults still apply).
func criConfigPathNeedsFix(content string) (needs bool, reason string) {
	body, ok := criV1ImagesRegistryBody(content)
	if !ok {
		// Legacy-only or bare configs inherit containerd 2.2's broken default.
		return true, "missing io.containerd.cri.v1.images.registry config_path"
	}
	for _, m := range reConfigPathAssign.FindAllStringSubmatch(body, -1) {
		val := m[3]
		if !strings.Contains(val, "certs.d") {
			continue
		}
		if isColonSeparatedCertsPath(val) {
			return true, "colon-separated config_path"
		}
		return false, "already single-root"
	}
	return true, "missing config_path in cri.v1.images.registry"
}

// transferConfigPathNeedsFix reports whether transfer.v1.local config_path is
// missing, empty, colon-separated, or not equal to preferred.
func transferConfigPathNeedsFix(content, preferred string) (needs bool, reason string) {
	preferred = strings.TrimRight(strings.TrimSpace(preferred), "/")
	if preferred == "" {
		preferred = systemContainerdCerts
	}
	body, ok := transferSectionBody(content)
	if !ok {
		return true, "missing transfer.v1.local config_path"
	}
	for _, m := range reConfigPathAssign.FindAllStringSubmatch(body, -1) {
		val := strings.TrimSpace(m[3])
		if val == "" {
			return true, "empty transfer.v1.local config_path"
		}
		if isColonSeparatedCertsPath(val) {
			return true, "colon-separated transfer.v1.local config_path"
		}
		if val != preferred {
			return true, "transfer.v1.local config_path mismatch"
		}
		return false, "transfer already single-root"
	}
	return true, "missing transfer.v1.local config_path assignment"
}

// containerdHostsConfigNeedsFix is true when CRI must be enabled and/or
// CRI/transfer hosts roots need rewrite.
func containerdHostsConfigNeedsFix(content, preferred string) (needs bool, reason string) {
	if criDisabled(content) {
		return true, "cri listed in disabled_plugins"
	}
	if cri, r := criConfigPathNeedsFix(content); cri {
		return true, r
	}
	if xfer, r := transferConfigPathNeedsFix(content, preferred); xfer {
		return true, r
	}
	return false, "CRI+transfer config_path already single-root"
}

func transferSectionBody(content string) (body string, ok bool) {
	loc := reTransferSection.FindStringIndex(content)
	if loc == nil {
		return "", false
	}
	rest := content[loc[1]:]
	end := len(rest)
	off := 0
	for i, line := range strings.SplitAfter(rest, "\n") {
		trim := strings.TrimSpace(line)
		if i > 0 && strings.HasPrefix(trim, "[") {
			end = off
			break
		}
		off += len(line)
	}
	return rest[:end], true
}

// rewriteCRIConfigPath forces every certs.d config_path under CRI plugin tables
// to preferred (single directory) and ALWAYS ensures cri.v1.images.registry exists.
// Does not touch transfer.v1.local (see ensureTransferConfigPath).
func rewriteCRIConfigPath(content, preferred string) (string, bool) {
	preferred = strings.TrimRight(strings.TrimSpace(preferred), "/")
	if preferred == "" {
		preferred = systemContainerdCerts
	}
	changed := false
	// Only rewrite config_path lines that already mention certs.d (or colon lists).
	// Empty transfer config_path = '' is handled by ensureTransferConfigPath.
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
	// Even after rewriting a legacy grpc.v1.cri colon path, containerd 2.x still
	// needs an explicit cri.v1.images.registry stanza — otherwise the default
	// colon list wins and crictl ignores hosts.toml.
	if body, ok := criV1ImagesRegistryBody(out); !ok {
		block := fmt.Sprintf(`

# managed by specula integrate — single-root hosts.toml for CRI
# (containerd 2.2 colon-separated config_path is ignored by transfer service;
#  see https://github.com/containerd/containerd/issues/12808)
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = %q
`, preferred)
		out += block
		changed = true
	} else if needs, reason := criConfigPathNeedsFix(out); needs && strings.Contains(reason, "missing config_path in") {
		loc := reCRIV1ImagesReg.FindStringIndex(out)
		if loc != nil {
			insert := fmt.Sprintf("  config_path = %q\n", preferred)
			out = out[:loc[1]] + insert + out[loc[1]:]
			changed = true
			_ = body
		}
	}
	if !changed {
		if needs, _ := criConfigPathNeedsFix(out); !needs {
			return content, false
		}
	}
	return out, true
}

// ensureTransferConfigPath sets plugins.'io.containerd.transfer.v1.local' config_path
// to preferred (same certs.d as CRI).
func ensureTransferConfigPath(content, preferred string) (string, bool) {
	preferred = strings.TrimRight(strings.TrimSpace(preferred), "/")
	if preferred == "" {
		preferred = systemContainerdCerts
	}
	loc := reTransferSection.FindStringIndex(content)
	if loc == nil {
		block := fmt.Sprintf(`

# managed by specula integrate — transfer service hosts.toml (must match CRI)
[plugins.'io.containerd.transfer.v1.local']
  config_path = %q
`, preferred)
		return content + block, true
	}
	bodyStart := loc[1]
	body, _ := transferSectionBody(content)
	bodyEnd := bodyStart + len(body)

	if m := reConfigPathAssign.FindStringSubmatch(body); m != nil {
		if m[3] == preferred && !isColonSeparatedCertsPath(m[3]) {
			return content, false
		}
		newBody := reConfigPathAssign.ReplaceAllStringFunc(body, func(line string) string {
			mm := reConfigPathAssign.FindStringSubmatch(line)
			if mm == nil {
				return line
			}
			return mm[1] + mm[2] + preferred + mm[4] + mm[5]
		})
		return content[:bodyStart] + newBody + content[bodyEnd:], true
	}
	insert := fmt.Sprintf("  config_path = %q\n", preferred)
	return content[:bodyStart] + insert + content[bodyStart:], true
}

// rewriteContainerdHostsConfigPaths enables CRI if disabled, then fixes CRI
// registry + transfer.v1.local config_path.
func rewriteContainerdHostsConfigPaths(content, preferred string) (string, bool) {
	out, c0 := enableCRI(content)
	out, c1 := rewriteCRIConfigPath(out, preferred)
	out, c2 := ensureTransferConfigPath(out, preferred)
	return out, c0 || c1 || c2
}

// integrateContainerdCRIConfigPath rewrites config.toml so CRI + transfer pulls
// honour hosts.toml under certsDir. Returns a Result for the OCI merge.
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
					Detail: "would create " + path + " with single-root CRI+transfer config_path=" + certsDir + " (containerd 2.2 CRI/transfer hosts.toml)",
					Path:   path,
				}
			}
			if skipRoot && isSystemPath(path) {
				return Result{
					Action: "skipped",
					Detail: "skip-root: not writing " + path + " — CRI/transfer may ignore hosts.toml until config_path is a single directory; re-run: sudo specula integrate --protocols oci",
					Path:   path,
				}
			}
			body, _ := rewriteContainerdHostsConfigPaths("", certsDir)
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
				Detail: "wrote " + path + " CRI+transfer config_path=" + certsDir + "; restart containerd then: crictl pull registry.k8s.io/pause:3.10.1",
				Path:   path,
			}
		}
		if os.IsPermission(err) {
			return Result{Action: "skipped", Detail: "need root to read " + path, Path: path}
		}
		return Result{Action: "error", Err: err.Error(), Path: path}
	}

	content := string(raw)
	needs, reason := containerdHostsConfigNeedsFix(content, certsDir)
	if !needs {
		return Result{
			Action: "already",
			Detail: "CRI+transfer config_path already single-root (" + reason + ")",
			Path:   path,
		}
	}
	next, changed := rewriteContainerdHostsConfigPaths(content, certsDir)
	if !changed {
		return Result{Action: "already", Detail: "CRI+transfer config_path ok", Path: path}
	}
	detail := fmt.Sprintf("fix CRI+transfer config_path (%s) → %s — restart containerd so crictl/kubelet use hosts.toml", reason, certsDir)
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

// preferredCertsFromContent picks the single-root certs.d already present in
// config (CRI or transfer). Used by doctor so temp/test paths are not flagged
// as "mismatch" against /etc/containerd/certs.d.
func preferredCertsFromContent(content string) string {
	for _, m := range reConfigPathAssign.FindAllStringSubmatch(content, -1) {
		val := strings.TrimSpace(m[3])
		if strings.Contains(val, "certs.d") && !isColonSeparatedCertsPath(val) {
			return val
		}
	}
	dirs := resolveContainerdCertsDirs()
	if len(dirs) > 0 {
		return dirs[0]
	}
	return systemContainerdCerts
}
