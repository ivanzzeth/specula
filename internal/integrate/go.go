package integrate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func integrateGo(home, addr string, dryRun bool) Result {
	speculaProxy := strings.TrimRight(addr, "/") + "/go"
	cur := readGoEnv("GOPROXY")
	if cur == "" {
		cur = "https://proxy.golang.org,direct"
	}
	next := prependGoProxy(cur, speculaProxy)
	if next == cur {
		return Result{Action: "already", Detail: "GOPROXY already starts with Specula", Path: "go env GOPROXY"}
	}
	if dryRun {
		return Result{Action: "added", Detail: "would set GOPROXY=" + next, Path: "go env -w"}
	}
	if err := exec.Command("go", "env", "-w", "GOPROXY="+next).Run(); err != nil {
		if werr := writeGoEnvFile(home, "GOPROXY", next); werr != nil {
			return Result{Action: "error", Err: fmt.Sprintf("go env -w: %v; fallback: %v", err, werr)}
		}
		return Result{Action: "added", Detail: "GOPROXY=" + next + " (via GOENV file)", Path: goEnvFile(home)}
	}
	return Result{Action: "added", Detail: "GOPROXY=" + next, Path: "go env -w"}
}

// prependGoProxy puts Specula first and removes later duplicates; preserves the rest.
func prependGoProxy(cur, speculaProxy string) string {
	parts := splitCSV(cur)
	if len(parts) > 0 && sameProxyURL(parts[0], speculaProxy) {
		return strings.Join(parts, ",")
	}
	filtered := make([]string, 0, len(parts)+1)
	filtered = append(filtered, speculaProxy)
	for _, p := range parts {
		if sameProxyURL(p, speculaProxy) {
			continue
		}
		filtered = append(filtered, p)
	}
	return strings.Join(filtered, ",")
}

func readGoEnv(key string) string {
	out, err := exec.Command("go", "env", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func goEnvFile(home string) string {
	if v := os.Getenv("GOENV"); v != "" {
		return v
	}
	return filepath.Join(home, ".config", "go", "env")
}

func writeGoEnvFile(home, key, value string) error {
	path := goEnvFile(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var lines []string
	if b, err := os.ReadFile(path); err == nil {
		lines = strings.Split(string(b), "\n")
	}
	prefix := key + "="
	found := false
	for i, line := range lines {
		if strings.HasPrefix(line, prefix) {
			lines[i] = prefix + value
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, prefix+value)
	}
	body := strings.Join(lines, "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func splitCSV(s string) []string {
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sameProxyURL(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}
