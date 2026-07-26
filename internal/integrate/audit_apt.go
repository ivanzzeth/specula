package integrate

import (
	"fmt"
	"os"
	"strings"
)

const (
	defaultAptListPath = "/etc/apt/sources.list.d/specula.list"
	defaultAptCAPath   = "/usr/local/share/ca-certificates/specula.crt"
)

// AuditAptRisks scans Specula apt integrate drop-ins for TLS footguns:
// deb [trusted=yes] skips GPG only — HTTPS still needs the Specula CA in the
// system trust store; http:// lists against an https:// data plane fail loudly.
func AuditAptRisks(addr string) []Result {
	return auditAptRisksAt(defaultAptListPath, defaultAptCAPath, addr)
}

func auditAptRisksAt(listPath, caPath, addr string) []Result {
	raw, err := os.ReadFile(listPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []Result{{Protocol: "apt", Action: "error", Err: err.Error(), Path: listPath}}
	}
	body := string(raw)
	if !strings.Contains(body, "/apt/") {
		return nil
	}

	var out []Result
	hasHTTPS := false
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.HasPrefix(trim, "deb") && strings.Contains(trim, "https://") {
			hasHTTPS = true
			break
		}
	}

	if hasHTTPS {
		if _, err := os.Stat(caPath); err != nil {
			out = append(out, Result{
				Protocol: "apt",
				Action:   "risk",
				Detail:   "specula.list uses https:// but Specula CA is missing — apt still verifies TLS (trusted=yes only skips GPG); apt-get update will fail with \"certificate issuer is unknown\". Fix: sudo specula integrate --protocols apt --ca-file /path/to/ca.crt",
				Path:     caPath,
			})
		}
	}

	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = DefaultAddr
	}
	if strings.HasPrefix(strings.ToLower(addr), "https://") && listUsesCleartextSpecula(body) {
		out = append(out, Result{
			Protocol: "apt",
			Action:   "risk",
			Detail:   fmt.Sprintf("specula.list uses http:// while Specula addr is %s — rewrite list to https or pass matching --addr; otherwise TLS-only data planes return HTTP 400", addr),
			Path:     listPath,
		})
	}

	return out
}

func listUsesCleartextSpecula(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.HasPrefix(trim, "deb") && strings.Contains(trim, "http://") && strings.Contains(trim, "/apt/") {
			return true
		}
	}
	return false
}
