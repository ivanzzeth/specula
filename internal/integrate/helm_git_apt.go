package integrate

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func integrateHelm(addr string, dryRun bool) Result {
	repoURL := strings.TrimRight(addr, "/") + "/helm/"
	name := "specula"
	if dryRun {
		return Result{Action: "added", Detail: "would helm repo add " + name + " " + repoURL, Path: "helm"}
	}
	if _, err := exec.LookPath("helm"); err != nil {
		return Result{Action: "skipped", Detail: "helm binary not found"}
	}
	// Check existing.
	out, _ := exec.Command("helm", "repo", "list", "-o", "json").Output()
	if strings.Contains(string(out), `"name":"`+name+`"`) || strings.Contains(string(out), `"url":"`+repoURL) {
		// Update URL additively via force-update on our owned repo name only.
		cmd := exec.Command("helm", "repo", "add", name, repoURL, "--force-update")
		if err := cmd.Run(); err != nil {
			return Result{Action: "already", Detail: "helm repo " + name + " present", Path: "helm"}
		}
		return Result{Action: "added", Detail: "updated helm repo " + name + " → " + repoURL, Path: "helm"}
	}
	cmd := exec.Command("helm", "repo", "add", name, repoURL)
	if out, err := cmd.CombinedOutput(); err != nil {
		return Result{Action: "error", Err: fmt.Sprintf("%v: %s", err, bytesTrim(out)), Path: "helm"}
	}
	return Result{Action: "added", Detail: "helm repo add " + name + " " + repoURL, Path: "helm"}
}

func integrateGit(home, addr string, dryRun bool) Result {
	base := strings.TrimRight(addr, "/") + "/git/github.com/"
	insteadOf := "https://github.com/"
	key := "url." + base + ".insteadof"
	cur := gitConfig(home, key)
	if sameProxyURL(strings.TrimSpace(cur), insteadOf) || strings.TrimSpace(cur) == insteadOf {
		return Result{Action: "already", Detail: "insteadOf already set for github.com", Path: "git config --global"}
	}
	// Also detect if any value equals insteadOf for this key pattern via git config --get-regexp
	if gitHasInsteadOf(home, base, insteadOf) {
		return Result{Action: "already", Detail: "insteadOf already set for github.com", Path: "git config --global"}
	}
	if dryRun {
		return Result{Action: "added", Detail: "would set " + key + "=" + insteadOf, Path: "git config --global"}
	}
	cmd := exec.Command("git", "config", "--global", key, insteadOf)
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		return Result{Action: "error", Err: fmt.Sprintf("%v: %s", err, bytesTrim(out))}
	}
	return Result{Action: "added", Detail: "github.com HTTPS → " + base, Path: "~/.gitconfig"}
}

func gitConfig(home, key string) string {
	cmd := exec.Command("git", "config", "--global", "--get", key)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitHasInsteadOf(home, base, want string) bool {
	cmd := exec.Command("git", "config", "--global", "--get-regexp", `^url\..*\.insteadof$`)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.Contains(fields[0], base) && fields[1] == want {
			return true
		}
	}
	return false
}

func integrateApt(addr string, dryRun, skipRoot bool) Result {
	if skipRoot {
		return Result{Action: "skipped", Detail: "apt integrate requires root (skipped)"}
	}
	suite := detectAptSuite()
	path := "/etc/apt/sources.list.d/specula.list"
	base := strings.TrimRight(addr, "/")
	body := fmt.Sprintf("# Added by `specula integrate` — does not modify sources.list / ubuntu.sources\n"+
		"# Suite auto-detected from /etc/os-release (override by editing this file).\n"+
		"# Specula protocols.apt upstreams must serve this distro tree (e.g. ubuntu).\n"+
		"deb [trusted=yes] %s/apt/ %s main restricted universe multiverse\n"+
		"deb [trusted=yes] %s/apt/ %s-updates main restricted universe multiverse\n"+
		"deb [trusted=yes] %s/apt/ %s-security main restricted universe multiverse\n",
		base, suite, base, suite, base, suite)
	if _, err := os.Stat(path); err == nil {
		cur, _ := os.ReadFile(path)
		wantNeedle := base + "/apt/ " + suite + " "
		if strings.Contains(string(cur), wantNeedle) {
			return Result{Action: "already", Detail: "specula.list already points at Specula (" + suite + ")", Path: path}
		}
	}
	if dryRun {
		return Result{Action: "added", Detail: "would write " + path + " (suite=" + suite + ")", Path: path}
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		if os.IsPermission(err) {
			return Result{Action: "skipped", Detail: "need root to write " + path + " — re-run: sudo specula integrate --protocols apt", Path: path}
		}
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	return Result{Action: "added", Detail: "wrote apt source suite=" + suite + " (apt-get update to refresh)", Path: path}
}

// detectAptSuite returns VERSION_CODENAME from /etc/os-release, or "jammy" as fallback.
func detectAptSuite() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "jammy"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && k == "VERSION_CODENAME" {
			v = strings.Trim(v, `"'`)
			if v != "" {
				return v
			}
		}
	}
	return "jammy"
}

func bytesTrim(b []byte) string {
	return strings.TrimSpace(string(b))
}
