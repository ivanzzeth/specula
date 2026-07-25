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
func integrateRegistryHost(host, addr, caFile string, dryRun, skipRoot bool) Result {
	host = strings.TrimSpace(host)
	if host == "" {
		return Result{Action: "skipped", Detail: "no --registry-host"}
	}
	endpoint := strings.TrimRight(strings.TrimSpace(addr), "/")
	if endpoint == "" {
		return Result{Action: "error", Err: "empty Specula addr"}
	}
	skipVerify, ca := tlsTrustForEndpoint(endpoint, caFile)

	var parts []string
	var path string
	action := "already"

	if isK3sNode() {
		yr := mergeK3sRegistriesYAML(k3sRegistriesYAML, host, endpoint, ca, dryRun, skipRoot)
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
		cr := writeRegistryHostCerts(dir, host, endpoint, skipVerify, ca, dryRun, skipRoot)
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

// tlsTrustForEndpoint returns skipVerify and ca path for containerd/k3s TLS wiring.
// http:// → never skip_verify, never ca. https:// + caFile → ca only. https:// without ca → skip_verify.
func tlsTrustForEndpoint(endpoint, caFile string) (skipVerify bool, ca string) {
	if !strings.HasPrefix(strings.ToLower(endpoint), "https://") {
		return false, ""
	}
	ca = strings.TrimSpace(caFile)
	if ca != "" {
		return false, ca
	}
	return true, ""
}

func mergeK3sRegistriesYAML(path, host, endpoint, caFile string, dryRun, skipRoot bool) Result {
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

	merged, already, err := mergeRegistriesYAMLBytes(existing, host, endpoint, caFile)
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
func mergeRegistriesYAMLBytes(existing []byte, host, endpoint, caFile string) ([]byte, bool, error) {
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

	isHTTPS := strings.HasPrefix(strings.ToLower(endpoint), "https://")
	ca := strings.TrimSpace(caFile)
	eh := endpointDialHost(endpoint)

	if isHTTPS {
		if ca != "" {
			mergeRegistryConfigTLS(configs, host, false, ca)
			if eh != "" {
				mergeRegistryConfigTLS(configs, eh, false, ca)
			}
		} else {
			mergeRegistryConfigTLS(configs, host, true, "")
			if eh != "" {
				mergeRegistryConfigTLS(configs, eh, true, "")
			}
		}
	} else {
		// Plain HTTP: never attach tls on registry hostname or dial host (k3s
		// would emit server=https://… and break blob pulls).
		clearRegistryConfigTLS(configs, host)
		if eh != "" {
			clearRegistryConfigTLS(configs, eh)
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

func writeRegistryHostCerts(certsDir, host, endpoint string, skipVerify bool, caFile string, dryRun, skipRoot bool) Result {
	dir := filepath.Join(certsDir, host)
	path := filepath.Join(dir, "hosts.toml")
	body := renderRegistryHostTOML(endpoint, skipVerify, caFile)
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

func renderRegistryHostTOML(endpoint string, skipVerify bool, caFile string) string {
	ep := strings.TrimRight(endpoint, "/")
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# managed by specula integrate --registry-host\n"))
	b.WriteString(fmt.Sprintf("[host.%q]\n", ep))
	b.WriteString("  capabilities = [\"pull\", \"resolve\", \"push\"]\n")
	if ca := strings.TrimSpace(caFile); ca != "" {
		b.WriteString(fmt.Sprintf("  ca = [%q]\n", ca))
	} else if skipVerify {
		b.WriteString("  skip_verify = true\n")
	}
	return b.String()
}

// mergeRegistryConfigTLS upserts tls on configs[key], preserving other keys (e.g. auth).
func mergeRegistryConfigTLS(configs map[string]any, key string, skipVerify bool, caFile string) {
	var cfg map[string]any
	if prev, ok := configs[key].(map[string]any); ok {
		cfg = make(map[string]any, len(prev))
		for k, v := range prev {
			cfg[k] = v
		}
	} else {
		cfg = map[string]any{}
	}
	if ca := strings.TrimSpace(caFile); ca != "" {
		cfg["tls"] = map[string]any{"ca_file": ca}
	} else if skipVerify {
		cfg["tls"] = map[string]any{"insecure_skip_verify": true}
	}
	configs[key] = cfg
}

func clearRegistryConfigTLS(configs map[string]any, key string) {
	prev, ok := configs[key].(map[string]any)
	if !ok {
		return
	}
	delete(prev, "tls")
	if len(prev) == 0 {
		delete(configs, key)
	} else {
		configs[key] = prev
	}
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
