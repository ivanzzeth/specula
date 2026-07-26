package integrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Competing ubuntu archive URIs that Specula replace when protocols include apt.
// Cloud-init images (esp. Aliyun) ship VPC-only mirrors that hang apt-get update
// off-VPC and make ansible's apt module fail even after Specula.list is present.
var defaultUbuntuArchiveMarkers = []string{
	"mirrors.cloud.aliyuncs.com/ubuntu",
	"mirrors.aliyuncs.com/ubuntu",
	"archive.ubuntu.com/ubuntu",
	"security.ubuntu.com/ubuntu",
	"mirrors.tuna.tsinghua.edu.cn/ubuntu",
	"mirrors.aliyun.com/ubuntu",
	"mirrors.ustc.edu.cn/ubuntu",
	"mirrors.cloud.tencent.com/ubuntu",
	"mirrors.huaweicloud.com/ubuntu",
}

const disabledBySpeculaSuffix = ".disabled-by-specula"

// disableConflictingUbuntuArchives comments out / renames host ubuntu archive
// sources so Specula's sources.list.d/specula.list is the sole archive path.
// Leaves third-party lists (docker, k8s, tailscale, …) untouched.
func disableConflictingUbuntuArchives(markers []string) (changed int, detail string, err error) {
	if len(markers) == 0 {
		markers = defaultUbuntuArchiveMarkers
	}
	var notes []string

	n, note, e := disableSourcesListLines("/etc/apt/sources.list", markers)
	if e != nil && !os.IsNotExist(e) {
		return 0, "", e
	}
	changed += n
	if note != "" {
		notes = append(notes, note)
	}

	entries, e := os.ReadDir("/etc/apt/sources.list.d")
	if e != nil && !os.IsNotExist(e) {
		return changed, strings.Join(notes, "; "), e
	}
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || strings.HasSuffix(name, disabledBySpeculaSuffix) {
			continue
		}
		// Never touch Specula's own list or non-archive vendor lists by name.
		if name == "specula.list" || strings.HasPrefix(name, "specula.") {
			continue
		}
		path := filepath.Join("/etc/apt/sources.list.d", name)
		switch {
		case strings.HasSuffix(name, ".list"):
			n, note, e = disableSourcesListLines(path, markers)
		case strings.HasSuffix(name, ".sources"):
			n, note, e = disableDeb822IfMatches(path, markers)
		default:
			continue
		}
		if e != nil {
			return changed, strings.Join(notes, "; "), e
		}
		changed += n
		if note != "" {
			notes = append(notes, note)
		}
	}
	return changed, strings.Join(notes, "; "), nil
}

func disableSourcesListLines(path string, markers []string) (int, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, "", err
	}
	lines := strings.Split(string(raw), "\n")
	changed := 0
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if !(strings.HasPrefix(trim, "deb ") || strings.HasPrefix(trim, "deb-src ")) {
			continue
		}
		if !lineMatchesMarkers(trim, markers) {
			continue
		}
		lines[i] = "# disabled-by-specula: " + line
		changed++
	}
	if changed == 0 {
		return 0, "", nil
	}
	out := strings.Join(lines, "\n")
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return 0, "", err
	}
	return changed, fmt.Sprintf("commented %d line(s) in %s", changed, path), nil
}

func disableDeb822IfMatches(path string, markers []string) (int, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, "", err
	}
	if !lineMatchesMarkers(string(raw), markers) {
		return 0, "", nil
	}
	dest := path + disabledBySpeculaSuffix
	if err := os.Rename(path, dest); err != nil {
		return 0, "", err
	}
	return 1, "renamed " + path + " → " + dest, nil
}

func lineMatchesMarkers(text string, markers []string) bool {
	lower := strings.ToLower(text)
	for _, m := range markers {
		if m != "" && strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}
