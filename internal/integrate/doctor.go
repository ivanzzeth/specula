package integrate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DoctorOptions configures a node-side preflight that surfaces OCI/CRI footguns
// before kubeadm/crictl fail in production.
type DoctorOptions struct {
	// Home overrides the user home (tests / non-interactive).
	Home string
	// Addr is Specula's data-plane base URL (default https://127.0.0.1:7732).
	Addr string
	// SkipProbe skips HTTP reachability of Addr/v2/.
	SkipProbe bool
	// ConfigTOMLs overrides live containerd config.toml candidates (tests).
	ConfigTOMLs []string
	// CertsDirs overrides certs.d roots to scan (tests).
	CertsDirs []string
	// AptListPath / AptCAPath override live apt integrate paths (tests).
	AptListPath string
	AptCAPath   string
	// DumpConfig returns effective containerd config (default: containerd config dump).
	DumpConfig func(ctx context.Context) (string, error)
	// Probe checks Specula reachability (default: GET Addr/v2/).
	Probe func(ctx context.Context, addr string) error
}

// Doctor runs OCI/CRI/k3s preflight checks and appends package-client risk audits.
// Exit convention for CLI: any Action "risk" or "error" → non-zero.
func Doctor(opts DoctorOptions) (Report, error) {
	addr, err := normalizeAddr(opts.Addr)
	if err != nil {
		return Report{}, err
	}
	home := opts.Home
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return Report{}, err
		}
	}
	rep := Report{Addr: addr}
	rep.Results = append(rep.Results, auditOCIRisks(opts, addr)...)
	rep.Results = append(rep.Results, auditAptRisksFromOpts(opts, addr)...)
	rep.Results = append(rep.Results, AuditClientRisks(home)...)
	if !opts.SkipProbe {
		probe := opts.Probe
		if probe == nil {
			probe = defaultProbeSpecula
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := probe(ctx, addr); err != nil {
			rep.Results = append(rep.Results, Result{
				Protocol: "oci",
				Action:   "risk",
				Detail:   fmt.Sprintf("Specula unreachable at %s/v2/ (%v) — CRI pulls will hang or fall through to public registries", addr, err),
				Path:     addr + "/v2/",
			})
		} else {
			rep.Results = append(rep.Results, Result{
				Protocol: "oci",
				Action:   "already",
				Detail:   "Specula /v2/ reachable",
				Path:     addr + "/v2/",
			})
		}
	}
	if len(rep.Results) == 0 {
		rep.Results = append(rep.Results, Result{
			Protocol: "oci",
			Action:   "skipped",
			Detail:   "no containerd/k3s config or certs.d found on this node",
		})
	}
	return rep, nil
}

// AuditOCIRisks scans live containerd/k3s paths for the footguns that bypass
// Specula (colon config_path, residual server=, wrong certs.d root).
func AuditOCIRisks() []Result {
	home, _ := os.UserHomeDir()
	return auditOCIRisks(DoctorOptions{Home: home}, DefaultAddr)
}

func auditAptRisksFromOpts(opts DoctorOptions, addr string) []Result {
	list := opts.AptListPath
	if list == "" {
		list = defaultAptListPath
	}
	ca := opts.AptCAPath
	if ca == "" {
		ca = defaultAptCAPath
	}
	return auditAptRisksAt(list, ca, addr)
}

func auditOCIRisks(opts DoctorOptions, addr string) []Result {
	var out []Result
	tomls := opts.ConfigTOMLs
	if tomls == nil {
		tomls = containerdConfigTOMLs()
	}
	certsDirs := opts.CertsDirs
	if certsDirs == nil {
		certsDirs = append([]string(nil), resolveContainerdCertsDirs()...)
		if home := opts.Home; home != "" {
			certsDirs = append(certsDirs, filepath.Join(home, ".config", "specula", "certs.d"))
		}
	}

	out = append(out, auditCRIConfigPathFiles(tomls)...)
	out = append(out, auditEffectiveCRIConfigPath(opts)...)
	out = append(out, auditHostsTOMLRisks(certsDirs)...)
	out = append(out, auditK3sCertsDirMismatch(certsDirs)...)
	out = append(out, auditCriticalHostsPresent(certsDirs, addr)...)
	return out
}

func auditCRIConfigPathFiles(tomls []string) []Result {
	var out []Result
	sawAny := false
	for _, path := range tomls {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if os.IsPermission(err) {
				out = append(out, Result{
					Protocol: "oci",
					Action:   "risk",
					Detail:   "cannot read containerd config.toml (permission) — CRI may still use colon-separated config_path; re-check as root",
					Path:     path,
				})
				continue
			}
			out = append(out, Result{Protocol: "oci", Action: "error", Err: err.Error(), Path: path})
			continue
		}
		sawAny = true
		content := string(raw)
		needs, reason := containerdHostsConfigNeedsFix(content, preferredCertsFromContent(content))
		if needs {
			out = append(out, Result{
				Protocol: "oci",
				Action:   "risk",
				Detail:   fmt.Sprintf("CRI/transfer config_path footgun (%s) — crictl/kubelet may ignore hosts.toml and dial public origins (*.pkg.dev); fix: sudo specula integrate --protocols oci && systemctl restart containerd", reason),
				Path:     path,
			})
		}
	}
	if !sawAny && len(tomls) > 0 && containerdLikelyInstalled() {
		// No file yet: containerd 2.2 still applies the broken default when CRI is on.
		out = append(out, Result{
			Protocol: "oci",
			Action:   "risk",
			Detail:   "no containerd config.toml found — if CRI is enabled, containerd 2.2 default config_path is colon-separated and bypasses Specula; run: sudo specula integrate --protocols oci",
			Path:     tomls[0],
		})
	}
	return out
}

func containerdLikelyInstalled() bool {
	if _, err := exec.LookPath("containerd"); err == nil {
		return true
	}
	if _, err := exec.LookPath("k3s"); err == nil {
		return true
	}
	return isK3sNode() || fileExists("/usr/bin/containerd") || fileExists("/usr/local/bin/containerd")
}

func auditEffectiveCRIConfigPath(opts DoctorOptions) []Result {
	dumpFn := opts.DumpConfig
	if dumpFn == nil {
		dumpFn = defaultContainerdConfigDump
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	dump, err := dumpFn(ctx)
	if err != nil {
		// Binary missing / not runnable is fine on laptop CI without containerd.
		return nil
	}
	// Only flag explicit colon-separated certs.d paths. Do not use the "missing
	// config_path" branch of criConfigPathNeedsFix — dumps are huge and may omit
	// or reshape keys even when the live CRI path is fine.
	for _, m := range reConfigPathAssign.FindAllStringSubmatch(dump, -1) {
		if isColonSeparatedCertsPath(m[3]) {
			return []Result{{
				Protocol: "oci",
				Action:   "risk",
				Detail:   "effective `containerd config dump` still has colon-separated CRI config_path — hosts.toml ignored by transfer service; fix config.toml then systemctl restart containerd (or k3s)",
				Path:     "containerd config dump",
			}}
		}
	}
	if strings.Contains(dump, "certs.d:/etc/docker/certs.d") {
		return []Result{{
			Protocol: "oci",
			Action:   "risk",
			Detail:   "effective `containerd config dump` still embeds certs.d:/etc/docker/certs.d — restart containerd after integrate, or re-run: sudo specula integrate --protocols oci",
			Path:     "containerd config dump",
		}}
	}
	// Empty transfer config_path is the default and means transfer-service
	// consumers never load hosts.toml — sync with CRI certs.d.
	if body, ok := transferSectionBody(dump); ok {
		for _, m := range reConfigPathAssign.FindAllStringSubmatch(body, -1) {
			if strings.TrimSpace(m[3]) == "" {
				return []Result{{
					Protocol: "oci",
					Action:   "risk",
					Detail:   "effective `containerd config dump` has empty transfer.v1.local config_path — set it to the same certs.d as CRI (sudo specula integrate --protocols oci && systemctl restart containerd)",
					Path:     "containerd config dump",
				}}
			}
		}
	}
	return nil
}

func auditHostsTOMLRisks(certsDirs []string) []Result {
	var out []Result
	seen := map[string]struct{}{}
	for _, dir := range certsDirs {
		if dir == "" {
			continue
		}
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if d.Name() != "hosts.toml" {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			body := string(b)
			if hostsHasPublicServerFallback(body) {
				out = append(out, Result{
					Protocol: "oci",
					Action:   "risk",
					Detail:   "hosts.toml has server= public fallback — pulls may leave Specula on miss; strip with integrate/bootstrap or StripPublicServerFallback",
					Path:     path,
				})
			}
			return nil
		})
	}
	return out
}

func hostsHasPublicServerFallback(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.HasPrefix(trim, "server") && strings.Contains(trim, "=") {
			return true
		}
	}
	return false
}

func auditK3sCertsDirMismatch(certsDirs []string) []Result {
	if !isK3sNode() {
		return nil
	}
	agentHas := certsDirHasRegistry(k3sAgentContainerdCerts, "registry.k8s.io") ||
		certsDirHasRegistry(k3sAgentContainerdCerts, "docker.io")
	systemHas := certsDirHasRegistry(systemContainerdCerts, "registry.k8s.io") ||
		certsDirHasRegistry(systemContainerdCerts, "docker.io")
	// Tests may pass only synthetic dirs — still check live constants when present.
	_ = certsDirs
	if systemHas && !agentHas {
		return []Result{{
			Protocol: "oci",
			Action:   "risk",
			Detail:   "k3s detected: hosts.toml under /etc/containerd/certs.d but missing under " + k3sAgentContainerdCerts + " — k3s containerd ignores the system path; re-run: sudo specula integrate --protocols oci",
			Path:     k3sAgentContainerdCerts,
		}}
	}
	return nil
}

func certsDirHasRegistry(dir, registry string) bool {
	_, err := os.Stat(filepath.Join(dir, registry, "hosts.toml"))
	return err == nil
}

func auditCriticalHostsPresent(certsDirs []string, addr string) []Result {
	var live []string
	for _, d := range certsDirs {
		if dirExists(d) {
			live = append(live, d)
		}
	}
	if len(live) == 0 {
		return nil
	}
	// Prefer the primary runtime certs.d (first resolveContainerdCertsDirs entry).
	primary := live[0]
	critical := []string{"registry.k8s.io", "docker.io"}
	var missing []string
	for _, reg := range critical {
		if !certsDirHasRegistry(primary, reg) {
			missing = append(missing, reg)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []Result{{
		Protocol: "oci",
		Action:   "risk",
		Detail:   fmt.Sprintf("certs.d at %s missing hosts.toml for %s — kubeadm/crictl will dial public origins; wire with: sudo specula integrate --protocols oci --addr %s", primary, strings.Join(missing, ","), addr),
		Path:     primary,
	}}
}

func defaultContainerdConfigDump(ctx context.Context) (string, error) {
	bin, err := exec.LookPath("containerd")
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, bin, "config", "dump")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, truncate(string(out), 200))
	}
	return string(out), nil
}

func defaultProbeSpecula(ctx context.Context, addr string) error {
	url := strings.TrimRight(addr, "/") + "/v2/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1024))
	// Registry protocol: 200 or 401 both mean the daemon answered.
	if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusUnauthorized {
		return nil
	}
	return fmt.Errorf("HTTP %d", res.StatusCode)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ReportHasBlockingFindings is true when doctor/status should exit non-zero.
func ReportHasBlockingFindings(rep Report) bool {
	for _, r := range rep.Results {
		if r.Action == "risk" || r.Action == "error" {
			return true
		}
	}
	return false
}
