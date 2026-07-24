package integrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const pipBackupKey = "# specula-integrate-previous-index-url="

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
		// Still scrub dangerous extras even when already pointing at Specula.
		if cleaned := scrubPublicExtraIndexes(global); cleaned && !dryRun {
			if err := writePipConf(path, cfg); err != nil {
				return Result{Action: "error", Err: err.Error(), Path: path}
			}
			return Result{Action: "added", Detail: "index-url already Specula; removed public extra-index-url (dep-confusion)", Path: path}
		}
		return Result{Action: "already", Detail: "index-url already Specula (sole index)", Path: path}
	}
	if dryRun {
		return Result{Action: "added", Detail: "would set sole index-url=" + index + " (NOT extra-index-url — dep-confusion safe)", Path: path}
	}

	prev := cur
	// Sole-index: Specula is the only index. Do NOT promote the previous
	// primary into extra-index-url — that is the classic confusion pattern.
	delete(global, "extra-index-url")
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
	// Persist previous index as a comment in a sidecar note file for recovery.
	if prev != "" && !sameProxyURL(prev, index) {
		_ = os.WriteFile(path+".specula-bak", []byte(pipBackupKey+prev+"\n"), 0o644)
	}
	detail := "sole index-url=" + index
	if prev != "" {
		detail += fmt.Sprintf("; previous %s NOT kept as extra-index-url (dep-confusion safe)", prev)
	}
	return Result{Action: "added", Detail: detail, Path: path}
}

// scrubPublicExtraIndexes removes extra-index-url entries that look like public
// PyPI / common mirrors. Returns true when the config map was mutated.
func scrubPublicExtraIndexes(global map[string]string) bool {
	raw := strings.TrimSpace(global["extra-index-url"])
	if raw == "" {
		return false
	}
	extras := splitCSV(raw)
	var kept []string
	changed := false
	for _, e := range extras {
		if looksLikePublicPyPI(e) {
			changed = true
			continue
		}
		kept = append(kept, e)
	}
	if !changed {
		return false
	}
	if len(kept) == 0 {
		delete(global, "extra-index-url")
	} else {
		global["extra-index-url"] = strings.Join(kept, " ")
	}
	return true
}

func looksLikePublicPyPI(u string) bool {
	u = strings.ToLower(strings.TrimSpace(u))
	needles := []string{
		"pypi.org",
		"pypi.python.org",
		"pythonhosted.org",
		"pypi.tuna.tsinghua.edu.cn",
		"mirrors.aliyun.com/pypi",
		"mirrors.cloud.tencent.com/pypi",
		"doubanio.com/pypi",
		"mirrors.ustc.edu.cn/pypi",
	}
	for _, n := range needles {
		if strings.Contains(u, n) {
			return true
		}
	}
	return false
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
	var b strings.Builder
	// Stable section order: global first, then alphabetical.
	sections := make([]string, 0, len(cfg))
	for s := range cfg {
		sections = append(sections, s)
	}
	// simple sort without importing sort again if already used — use sort in this package
	for i := 0; i < len(sections); i++ {
		for j := i + 1; j < len(sections); j++ {
			if sections[j] < sections[i] {
				sections[i], sections[j] = sections[j], sections[i]
			}
		}
	}
	// Move global to front.
	for i, s := range sections {
		if s == "global" {
			sections = append([]string{"global"}, append(sections[:i], sections[i+1:]...)...)
			break
		}
	}
	for _, s := range sections {
		b.WriteString("[" + s + "]\n")
		keys := make([]string, 0, len(cfg[s]))
		for k := range cfg[s] {
			keys = append(keys, k)
		}
		for i := 0; i < len(keys); i++ {
			for j := i + 1; j < len(keys); j++ {
				if keys[j] < keys[i] {
					keys[i], keys[j] = keys[j], keys[i]
				}
			}
		}
		for _, k := range keys {
			b.WriteString(k + " = " + cfg[s][k] + "\n")
		}
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
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
