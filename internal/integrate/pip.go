package integrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func integratePip(home, addr string, dryRun bool) Result {
	index := strings.TrimRight(addr, "/") + "/pypi/simple"
	path := pipConfPath(home)
	cfg, err := readPipConf(path)
	if err != nil && !os.IsNotExist(err) {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	if cfg == nil {
		cfg = map[string]map[string]string{}
	}
	global := cfg["global"]
	if global == nil {
		global = map[string]string{}
		cfg["global"] = global
	}
	cur := global["index-url"]
	if sameProxyURL(cur, index) {
		return Result{Action: "already", Detail: "index-url already Specula", Path: path}
	}
	if dryRun {
		return Result{Action: "added", Detail: "would set index-url=" + index + " (old → extra-index-url)", Path: path}
	}
	// Move previous primary index to extra-index-url (additive) if present and distinct.
	if cur != "" && !sameProxyURL(cur, index) {
		extras := splitCSV(global["extra-index-url"])
		if !containsURL(extras, cur) {
			extras = append(extras, cur)
		}
		// Drop Specula from extras if somehow present.
		filtered := extras[:0]
		for _, e := range extras {
			if !sameProxyURL(e, index) {
				filtered = append(filtered, e)
			}
		}
		if len(filtered) > 0 {
			global["extra-index-url"] = strings.Join(filtered, " ")
		}
	}
	global["index-url"] = index
	host := hostOf(addr)
	if host != "" {
		th := splitCSV(strings.ReplaceAll(global["trusted-host"], " ", ","))
		if !containsFold(th, host) {
			th = append(th, host)
		}
		global["trusted-host"] = strings.Join(th, " ")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	if err := writePipConf(path, cfg); err != nil {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	detail := "index-url=" + index
	if cur != "" {
		detail += fmt.Sprintf("; previous %s kept as extra-index-url", cur)
	}
	return Result{Action: "added", Detail: detail, Path: path}
}

func pipConfPath(home string) string {
	// Prefer XDG location; fall back to legacy ~/pip/pip.conf.
	xdg := filepath.Join(home, ".config", "pip", "pip.conf")
	legacy := filepath.Join(home, "pip", "pip.conf")
	if _, err := os.Stat(xdg); err == nil {
		return xdg
	}
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return xdg
}

func readPipConf(path string) (map[string]map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := map[string]map[string]string{}
	section := ""
	for _, line := range strings.Split(string(b), "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") || strings.HasPrefix(trim, ";") {
			continue
		}
		if strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]") {
			section = strings.ToLower(strings.TrimSpace(trim[1 : len(trim)-1]))
			if cfg[section] == nil {
				cfg[section] = map[string]string{}
			}
			continue
		}
		k, v, ok := strings.Cut(trim, "=")
		if !ok {
			continue
		}
		if section == "" {
			section = "global"
			if cfg[section] == nil {
				cfg[section] = map[string]string{}
			}
		}
		cfg[section][strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return cfg, nil
}

func writePipConf(path string, cfg map[string]map[string]string) error {
	order := []string{"global", "install", "download", "list"}
	seen := map[string]bool{}
	var b strings.Builder
	b.WriteString("# Managed in part by `specula integrate` — other keys preserved.\n")
	writeSec := func(name string, kv map[string]string) {
		if len(kv) == 0 {
			return
		}
		fmt.Fprintf(&b, "[%s]\n", name)
		// Stable key order for diffs.
		keys := make([]string, 0, len(kv))
		for k := range kv {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s = %s\n", k, kv[k])
		}
		b.WriteByte('\n')
		seen[name] = true
	}
	for _, name := range order {
		if kv := cfg[name]; kv != nil {
			writeSec(name, kv)
		}
	}
	rest := make([]string, 0)
	for name := range cfg {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sortStrings(rest)
	for _, name := range rest {
		writeSec(name, cfg[name])
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func sortStrings(s []string) {
	for i := 0; i < len(s); i++ {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

func containsURL(list []string, want string) bool {
	for _, e := range list {
		if sameProxyURL(e, want) {
			return true
		}
	}
	return false
}

func containsFold(list []string, want string) bool {
	want = strings.ToLower(want)
	for _, e := range list {
		if strings.ToLower(e) == want {
			return true
		}
	}
	return false
}

func hostOf(addr string) string {
	addr = strings.TrimPrefix(addr, "http://")
	addr = strings.TrimPrefix(addr, "https://")
	host, _, _ := strings.Cut(addr, "/")
	host, _, _ = strings.Cut(host, ":")
	return host
}
