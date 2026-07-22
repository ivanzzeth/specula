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

// MirrorOptions configures WriteContainerdHosts.
type MirrorOptions struct {
	// CertsDir is the containerd certs.d root (e.g. /etc/containerd/certs.d).
	CertsDir string
	// Endpoint is the mirror URL the node dials (typically http://127.0.0.1:<nodePort>).
	Endpoint string
	// Registries are registry hostnames to redirect (docker.io, registry.k8s.io, …).
	Registries []string
	// SkipVerify sets skip_verify = true on the mirror host entry (plain HTTP mirrors).
	SkipVerify bool
}

// WriteContainerdHosts writes certs.d/<registry>/hosts.toml for each registry.
// Idempotent: overwrites existing hosts.toml files.
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
		server := registryServer(reg)
		dir := filepath.Join(certs, reg)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("bootstrap: mkdir %s: %w", dir, err)
		}
		body := renderHostsTOML(server, endpoint, opts.SkipVerify)
		path := filepath.Join(dir, "hosts.toml")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return fmt.Errorf("bootstrap: write %s: %w", path, err)
		}
	}
	return nil
}

func registryServer(reg string) string {
	if reg == "docker.io" {
		return "https://registry-1.docker.io"
	}
	if strings.HasPrefix(reg, "http://") || strings.HasPrefix(reg, "https://") {
		return reg
	}
	return "https://" + reg
}

func renderHostsTOML(server, endpoint string, skipVerify bool) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("server = %q\n\n", server))
	b.WriteString(fmt.Sprintf("[host.%q]\n", endpoint))
	b.WriteString("  capabilities = [\"pull\", \"resolve\"]\n")
	if skipVerify {
		b.WriteString("  skip_verify = true\n")
	}
	return b.String()
}
