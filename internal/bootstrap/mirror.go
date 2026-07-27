// Package bootstrap implements China / air-gapped cluster self-bootstrap helpers:
// writing containerd certs.d hosts.toml drop-ins, and warming OCI manifests through
// a running Specula pull-through mirror.
package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultOCIRegistries is the default set for bootstrap-mirror / integrate oci
// containerd hosts.toml wiring.
var DefaultOCIRegistries = []string{
	"docker.io",
	"registry.k8s.io",
	"ghcr.io",
	"quay.io",
	"gcr.io",
	"k8s.gcr.io",
	"mcr.microsoft.com",
	"codeberg.org",
}

// MirrorOptions configures WriteContainerdHosts.
type MirrorOptions struct {
	// CertsDir is the containerd certs.d root (e.g. /etc/containerd/certs.d).
	CertsDir string
	// Endpoint is the mirror URL the node dials (typically http://127.0.0.1:<port>).
	Endpoint string
	// Registries are registry hostnames to redirect (docker.io, registry.k8s.io, …).
	Registries []string
	// SkipVerify sets skip_verify = true on the mirror host entry.
	// Use ONLY for HTTPS mirrors with a non-public CA (self-signed Specula).
	// Never for plain http:// — containerd then dials TLS against an HTTP port.
	SkipVerify bool
	// CaFile is a PEM CA cert path on the node. When set for HTTPS endpoints,
	// writes ca = ["path"] instead of skip_verify.
	CaFile string
}

// WriteContainerdHosts writes certs.d/<registry>/hosts.toml for each registry.
// Idempotent: overwrites existing hosts.toml files.
//
// docker.io keeps a plain mirror host (Hub-relative paths: library/nginx).
// Other registries use override_path so Specula receives
// /v2/<registry>/<repo>/… and can strip the host for upstream fetch.
//
// Intentionally omits containerd's top-level `server = "https://<registry>"`.
// That field is a FALLBACK: when the Specula host entry fails or is slow,
// containerd dials the public registry (e.g. registry.k8s.io → *.pkg.dev),
// which is unreachable from many CN clouds and defeats Specula-only delivery.
func WriteContainerdHosts(opts MirrorOptions) error {
	certs := strings.TrimSpace(opts.CertsDir)
	if certs == "" {
		return fmt.Errorf("bootstrap: certs-dir is required")
	}
	endpoint := strings.TrimSpace(opts.Endpoint)
	if endpoint == "" {
		return fmt.Errorf("bootstrap: endpoint is required")
	}
	if len(opts.Registries) == 0 {
		return fmt.Errorf("bootstrap: at least one registry is required")
	}
	for _, reg := range opts.Registries {
		reg = strings.TrimSpace(reg)
		if reg == "" {
			continue
		}
		dir := filepath.Join(certs, reg)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("bootstrap: mkdir %s: %w", dir, err)
		}
		body := renderHostsTOML(reg, endpoint, opts.SkipVerify, opts.CaFile)
		path := filepath.Join(dir, "hosts.toml")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return fmt.Errorf("bootstrap: write %s: %w", path, err)
		}
	}
	// Upgrade path: older installs may still have top-level `server =` on
	// registries we did not rewrite above (or that alreadyOK skipped). Strip
	// residual public fallbacks so containerd cannot dial Hub/pkg.dev.
	if _, err := StripPublicServerFallback(certs); err != nil {
		return err
	}
	return nil
}

// StripPublicServerFallback walks certs.d/**/hosts.toml and removes any
// top-level `server = "..."` lines (containerd public-registry fallback).
// Returns how many files were rewritten.
func StripPublicServerFallback(certsDir string) (int, error) {
	certs := strings.TrimSpace(certsDir)
	if certs == "" {
		return 0, fmt.Errorf("bootstrap: certs-dir is required")
	}
	entries, err := os.ReadDir(certs)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("bootstrap: readdir %s: %w", certs, err)
	}
	rewritten := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(certs, e.Name(), "hosts.toml")
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return rewritten, fmt.Errorf("bootstrap: read %s: %w", path, err)
		}
		cleaned, changed := stripServerLines(string(raw))
		if !changed {
			continue
		}
		if err := os.WriteFile(path, []byte(cleaned), 0o644); err != nil {
			return rewritten, fmt.Errorf("bootstrap: write %s: %w", path, err)
		}
		rewritten++
	}
	return rewritten, nil
}

// stripServerLines removes TOML lines that set top-level `server = ...`.
// Nested keys under [host."…"] are left alone (none use that name today).
func stripServerLines(body string) (string, bool) {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	changed := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "server") {
			rest := strings.TrimSpace(strings.TrimPrefix(trim, "server"))
			if strings.HasPrefix(rest, "=") {
				changed = true
				continue
			}
		}
		out = append(out, line)
	}
	if !changed {
		return body, false
	}
	return strings.Join(out, "\n"), true
}

func renderHostsTOML(registry, endpoint string, skipVerify bool, caFile string) string {
	var b strings.Builder

	hostKey := strings.TrimRight(endpoint, "/")
	overridePath := !isDockerIO(registry)
	if overridePath {
		// Path-style multi-registry: Specula sees /v2/<registry>/<repo>/…
		hostKey = strings.TrimRight(endpoint, "/") + "/v2/" + registry
	}

	b.WriteString(fmt.Sprintf("[host.%q]\n", hostKey))
	b.WriteString("  capabilities = [\"pull\", \"resolve\"]\n")
	if overridePath {
		b.WriteString("  override_path = true\n")
	}
	if ca := strings.TrimSpace(caFile); ca != "" {
		b.WriteString(fmt.Sprintf("  ca = [%q]\n", ca))
	} else if skipVerify {
		b.WriteString("  skip_verify = true\n")
	}
	return b.String()
}

func isDockerIO(reg string) bool {
	r := strings.ToLower(strings.TrimSpace(reg))
	return r == "docker.io" || r == "registry-1.docker.io" || r == "index.docker.io"
}

// RemoveContainerdHosts deletes the certs.d/<registry>/hosts.toml drop-ins that
// point at the given Specula endpoint, and the directories they leave empty.
// Returns how many files were removed.
//
// Why this is needed: `cluster uninstall` removes the helm release but the nodes
// keep their hosts.toml, so every redirected registry keeps resolving to a
// NodePort with nothing behind it. CN mode writes no public `server =` fallback on
// purpose, so those pulls do not degrade — they fail. Observed on a real ACK
// cluster: registry.k8s.io/pause:3.10 → ErrImagePull after Specula was gone.
//
// Matching is by host:port found in the file body, so only OUR drop-ins go. A
// hosts.toml naming a different endpoint (another cluster, a hand-written corporate
// mirror) is left exactly as it was — deleting an operator's own config would be a
// worse bug than the one this fixes. An empty endpoint or certs dir is refused
// rather than interpreted as "match everything".
func RemoveContainerdHosts(certsDir, endpoint string) (int, error) {
	certs := strings.TrimSpace(certsDir)
	if certs == "" {
		return 0, fmt.Errorf("bootstrap: certs-dir is required")
	}
	needle := endpointHostPort(endpoint)
	if needle == "" {
		return 0, fmt.Errorf("bootstrap: endpoint is required")
	}
	entries, err := os.ReadDir(certs)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // node never had Specula, or already cleaned
		}
		return 0, fmt.Errorf("bootstrap: read %s: %w", certs, err)
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(certs, e.Name())
		path := filepath.Join(dir, "hosts.toml")
		body, err := os.ReadFile(path)
		if err != nil {
			continue // no drop-in here, or unreadable — leave it alone
		}
		if !strings.Contains(string(body), needle) {
			continue // someone else's config
		}
		if err := os.Remove(path); err != nil {
			return removed, fmt.Errorf("bootstrap: remove %s: %w", path, err)
		}
		removed++
		// Drop the directory only when nothing else lives in it (a node may keep
		// ca.crt or client certs next to hosts.toml).
		if rest, err := os.ReadDir(dir); err == nil && len(rest) == 0 {
			_ = os.Remove(dir)
		}
	}
	return removed, nil
}

// endpointHostPort reduces http://host:port/, host:port and bare host to the
// host:port form that appears inside a rendered hosts.toml.
func endpointHostPort(endpoint string) string {
	e := strings.TrimSpace(endpoint)
	if e == "" {
		return ""
	}
	for _, scheme := range []string{"https://", "http://"} {
		e = strings.TrimPrefix(e, scheme)
	}
	e = strings.TrimSuffix(e, "/")
	if i := strings.IndexAny(e, "/?"); i >= 0 {
		e = e[:i]
	}
	return strings.TrimSpace(e)
}
