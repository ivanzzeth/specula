package integrate

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// DefaultGitHosts is the host allowlist used when integrate --protocols git
// writes insteadOf rules (and the recommended example.yaml list).
var DefaultGitHosts = []string{
	"github.com",
	"gitlab.com",
	"gitee.com",
	"codeberg.org",
	"git.sr.ht",
	"bitbucket.org",
}

func integrateGit(home, addr string, dryRun bool) Result {
	baseAddr := strings.TrimRight(addr, "/")
	hosts := append([]string(nil), DefaultGitHosts...)

	type pair struct{ host, proxyBase, insteadOf, key string }
	pairs := make([]pair, 0, len(hosts))
	for _, host := range hosts {
		proxyBase := baseAddr + "/git/" + host + "/"
		insteadOf := "https://" + host + "/"
		pairs = append(pairs, pair{
			host:      host,
			proxyBase: proxyBase,
			insteadOf: insteadOf,
			key:       "url." + proxyBase + ".insteadof",
		})
	}

	allAlready := true
	for _, p := range pairs {
		cur := gitConfig(home, p.key)
		if sameProxyURL(strings.TrimSpace(cur), p.insteadOf) || strings.TrimSpace(cur) == p.insteadOf {
			continue
		}
		if gitHasInsteadOf(home, p.proxyBase, p.insteadOf) {
			continue
		}
		allAlready = false
		break
	}
	if allAlready {
		return Result{
			Action: "already",
			Detail: "insteadOf already set for " + strings.Join(hosts, ","),
			Path:   "git config --global",
		}
	}

	if dryRun {
		return Result{
			Action: "added",
			Detail: "would set insteadOf for " + strings.Join(hosts, ","),
			Path:   "git config --global",
		}
	}

	var added []string
	for _, p := range pairs {
		cur := gitConfig(home, p.key)
		if sameProxyURL(strings.TrimSpace(cur), p.insteadOf) || strings.TrimSpace(cur) == p.insteadOf {
			continue
		}
		if gitHasInsteadOf(home, p.proxyBase, p.insteadOf) {
			continue
		}
		cmd := exec.Command("git", "config", "--global", p.key, p.insteadOf)
		cmd.Env = append(os.Environ(), "HOME="+home)
		if out, err := cmd.CombinedOutput(); err != nil {
			return Result{Action: "error", Err: fmt.Sprintf("%v: %s", err, bytesTrim(out))}
		}
		added = append(added, p.host)
	}
	if len(added) == 0 {
		return Result{
			Action: "already",
			Detail: "insteadOf already set for " + strings.Join(hosts, ","),
			Path:   "git config --global",
		}
	}
	return Result{
		Action: "added",
		Detail: "HTTPS → Specula /git/<host>/ for " + strings.Join(added, ","),
		Path:   "~/.gitconfig",
	}
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
