package cluster

import (
	"fmt"
	"strings"
)

// PersistenceMode describes how Specula stores blob/SQLite data.
type PersistenceMode struct {
	Enabled       bool
	ExistingClaim string
	HostPath      string
	StorageClass  string
	Size          string
}

// HelmPersistenceArgs returns --set pairs for the bootstrap chart.
// Priority: existingClaim > hostPath > created PVC (if Enabled) > emptyDir.
func HelmPersistenceArgs(p PersistenceMode) []string {
	claim := strings.TrimSpace(p.ExistingClaim)
	host := strings.TrimSpace(p.HostPath)
	sc := strings.TrimSpace(p.StorageClass)
	size := strings.TrimSpace(p.Size)
	if size == "" {
		size = "20Gi"
	}

	if claim != "" {
		return []string{
			"--set", "persistence.enabled=true",
			"--set", "persistence.existingClaim=" + claim,
			"--set", "persistence.hostPath=",
		}
	}
	if host != "" {
		return []string{
			"--set", "persistence.enabled=true",
			"--set", "persistence.existingClaim=",
			"--set", "persistence.hostPath=" + host,
		}
	}
	if !p.Enabled {
		return []string{
			"--set", "persistence.enabled=false",
			"--set", "persistence.existingClaim=",
			"--set", "persistence.hostPath=",
		}
	}
	args := []string{
		"--set", "persistence.enabled=true",
		"--set", "persistence.existingClaim=",
		"--set", "persistence.hostPath=",
		"--set", "persistence.size=" + size,
	}
	if sc != "" {
		args = append(args, "--set", "persistence.storageClass="+sc)
	}
	return args
}

// PickPinHostname chooses a Ready node to pin Specula onto.
// Prefer a worker (no control-plane/master role); else first Ready node.
// lines are "name\troleFlags\tReady" where roleFlags contains "cp" if control-plane.
func PickPinHostname(lines []string) string {
	var workers, cps []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		name, roles, ready := parts[0], parts[1], parts[2]
		if !strings.EqualFold(ready, "True") {
			continue
		}
		if strings.Contains(roles, "cp") || strings.Contains(roles, "master") {
			cps = append(cps, name)
			continue
		}
		workers = append(workers, name)
	}
	if len(workers) > 0 {
		return workers[0]
	}
	if len(cps) > 0 {
		return cps[0]
	}
	return ""
}

// ParseNodePinLines turns kubectl jsonpath multi-line output into PickPinHostname input.
// Expected per line: name<TAB>roles<TAB>Ready
func FormatNodePinLine(name, rolesCSV, ready string) string {
	roles := "worker"
	lower := strings.ToLower(rolesCSV + " " + name)
	if strings.Contains(lower, "control-plane") || strings.Contains(lower, "master") {
		roles = "cp"
	}
	return fmt.Sprintf("%s\t%s\t%s", name, roles, ready)
}
