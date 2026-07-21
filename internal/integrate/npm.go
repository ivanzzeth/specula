package integrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const npmBackupKey = "; specula-integrate-previous-registry="

func integrateNPM(home, addr string, dryRun bool) Result {
	registry := strings.TrimRight(addr, "/") + "/npm/"
	path := npmrcPath(home)
	cur, others, prevBackup, err := parseNPMRC(path)
	if err != nil && !os.IsNotExist(err) {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	if sameProxyURL(cur, registry) {
		return Result{Action: "already", Detail: "registry already Specula", Path: path}
	}
	if dryRun {
		return Result{Action: "added", Detail: fmt.Sprintf("would set registry=%s (keep other npmrc keys; backup old)", registry), Path: path}
	}
	backup := cur
	if backup == "" {
		backup = prevBackup
	}
	var b strings.Builder
	if backup != "" && !sameProxyURL(backup, registry) {
		b.WriteString(npmBackupKey + backup + "\n")
	}
	b.WriteString("registry=" + registry + "\n")
	for _, line := range others {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	detail := "registry=" + registry
	if backup != "" {
		detail += "; previous preserved in comment"
	}
	return Result{Action: "added", Detail: detail, Path: path}
}

func npmrcPath(home string) string {
	return filepath.Join(home, ".npmrc")
}

// parseNPMRC returns current registry, other non-registry lines (preserved),
// and any previous backup comment value.
func parseNPMRC(path string) (registry string, others []string, backup string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", nil, "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if strings.HasPrefix(trim, npmBackupKey) {
			backup = strings.TrimSpace(strings.TrimPrefix(trim, npmBackupKey))
			continue
		}
		if strings.HasPrefix(trim, "registry=") {
			registry = strings.TrimSpace(strings.TrimPrefix(trim, "registry="))
			continue
		}
		// Drop stale registry= duplicates; keep everything else (scopes, auth, …).
		others = append(others, line)
	}
	return registry, others, backup, nil
}
