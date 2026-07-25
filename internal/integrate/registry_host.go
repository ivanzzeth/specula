package integrate

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// k3sRegistriesYAML is where k3s reads mirror/config for the container runtime.
const k3sRegistriesYAML = "/etc/rancher/k3s/registries.yaml"

// integrateRegistryHost wires a cluster-local OCI hostname (e.g. Specula's
// pointer DNS name) so pulls/pushes of that host dial --addr.
//
// k3s: merge mirrors+configs into registries.yaml (do NOT seed public upstreams).
// All distros: write certs.d/<host>/hosts.toml under the live containerd root.
func integrateRegistryHost(host, addr string, dryRun, skipRoot bool) Result {
	host = strings.TrimSpace(host)
	if host == "" {
		return Result{Action: "skipped", Detail: "no --registry-host"}
	}
	endpoint := strings.TrimRight(strings.TrimSpace(addr), "/")
	if endpoint == "" {
		return Result{Action: "error", Err: "empty Specula addr"}
	}
	insecure := strings.HasPrefix(strings.ToLower(endpoint), "http://")

	var parts []string
	var path string
	action := "already"

	if isK3sNode() {
		yr := mergeK3sRegistriesYAML(k3sRegistriesYAML, host, endpoint, insecure, dryRun, skipRoot)
		if yr.Action == "error" {
			return yr
		}
		if yr.Detail != "" {
			parts = append(parts, yr.Detail)
		}
		path = yr.Path
		if yr.Action == "added" {
			action = "added"
		}
	}

	certsDirs := resolveContainerdCertsDirs()
	for _, dir := range certsDirs {
		cr := writeRegistryHostCerts(dir, host, endpoint, insecure, dryRun, skipRoot)
		if cr.Action == "error" {
			return cr
		}
		if cr.Detail != "" {
			parts = append(parts, cr.Detail)
		}
		if path == "" {
			path = cr.Path
		}
		if cr.Action == "added" {
			action = "added"
		}
	}

	if len(parts) == 0 {
		parts = append(parts, "registry-host "+host+" → "+endpoint)
	}
	return Result{
		Action: action,
		Detail: strings.Join(parts, "; "),
		Path:   path,
	}
}

func mergeK3sRegistriesYAML(path, host, endpoint string, insecure, dryRun, skipRoot bool) Result {
	detail := fmt.Sprintf("k3s registries.yaml mirrors[%s] → %s", host, endpoint)
	if dryRun {
		return Result{Action: "added", Detail: "would write: " + detail, Path: path}
	}
	if skipRoot {
		return Result{
			Action: "skipped",
			Detail: "skip-root: not writing " + path + " (" + detail + ")",
			Path:   path,
		}
	}

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}

	merged, already, err := mergeRegistriesYAMLBytes(existing, host, endpoint, insecure)
	if err != nil {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	if already {
		return Result{Action: "already", Detail: detail + " (already)", Path: path}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	if err := os.WriteFile(path, merged, 0o644); err != nil {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	return Result{Action: "added", Detail: detail, Path: path}
}

// mergeRegistriesYAMLBytes upserts one host→endpoint into k3s registries.yaml,
// preserving every other host and top-level key.
func mergeRegistriesYAMLBytes(existing []byte, host, endpoint string, insecure bool) ([]byte, bool, error) {
	var data map[string]any
	text := strings.TrimSpace(string(existing))
	if text != "" {
		if err := yaml.Unmarshal([]byte(text), &data); err != nil {
			return nil, false, fmt.Errorf("parse registries.yaml: %w", err)
		}
	}
	if data == nil {
		data = map[string]any{}
	}

	mirrors, _ := data["mirrors"].(map[string]any)
	if mirrors == nil {
		mirrors = map[string]any{}
	}
	configs, _ := data["configs"].(map[string]any)
	if configs == nil {
		configs = map[string]any{}
	}

	wantMirror := map[string]any{"endpoint": []any{endpoint}}
	already := false
	if prev, ok := mirrors[host].(map[string]any); ok {
		if eps, ok := prev["endpoint"].([]any); ok && len(eps) == 1 && fmt.Sprint(eps[0]) == endpoint {
			already = true
		}
	}
	mirrors[host] = wantMirror

	eh := endpointDialHost(endpoint)
	if eh != "" {
		if insecure {
			configs[eh] = map[string]any{
				"tls": map[string]any{"insecure_skip_verify": true},
			}
		} else if _, ok := configs[eh]; !ok {
			// leave other configs; no tls block required for https
		}
	}

	data["mirrors"] = mirrors
	if len(configs) > 0 {
		data["configs"] = configs
	}

	out, err := yaml.Marshal(data)
	if err != nil {
		return nil, false, err
	}
	return out, already, nil
}

func writeRegistryHostCerts(certsDir, host, endpoint string, insecure, dryRun, skipRoot bool) Result {
	dir := filepath.Join(certsDir, host)
	path := filepath.Join(dir, "hosts.toml")
	body := renderRegistryHostTOML(endpoint, insecure)
	detail := fmt.Sprintf("certs.d/%s → %s", host, endpoint)

	if dryRun {
		return Result{Action: "added", Detail: "would write: " + detail, Path: path}
	}
	liveSystem := strings.HasPrefix(certsDir, "/etc/") ||
		strings.HasPrefix(certsDir, "/var/lib/rancher/")
	if skipRoot && liveSystem {
		return Result{
			Action: "skipped",
			Detail: "skip-root: not writing " + path + " (" + detail + ")",
			Path:   path,
		}
	}

	if b, err := os.ReadFile(path); err == nil && string(b) == body {
		return Result{Action: "already", Detail: detail + " (already)", Path: path}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		if os.IsPermission(err) {
			return Result{Action: "error", Err: err.Error() + " — run: sudo specula integrate --protocols oci", Path: path}
		}
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	return Result{Action: "added", Detail: detail, Path: path}
}

func renderRegistryHostTOML(endpoint string, insecure bool) string {
	ep := strings.TrimRight(endpoint, "/")
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# managed by specula integrate --registry-host\n"))
	b.WriteString(fmt.Sprintf("[host.%q]\n", ep))
	b.WriteString("  capabilities = [\"pull\", \"resolve\", \"push\"]\n")
	if insecure {
		b.WriteString("  skip_verify = true\n")
	}
	return b.String()
}

func endpointDialHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		// bare host:port
		s := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
		if i := strings.IndexByte(s, '/'); i >= 0 {
			s = s[:i]
		}
		return s
	}
	return u.Host
}
