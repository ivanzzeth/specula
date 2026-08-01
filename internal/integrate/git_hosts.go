package integrate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

	helperValue := speculaGitCredentialHelper()
	helperMissing := helperValue != "" && !gitHasCredentialHelper(home, helperValue)
	useHttpPathKey, useHttpPathMissing := speculaUseHttpPathKey(baseAddr, home)

	allAlready := !helperMissing && !useHttpPathMissing
	if allAlready {
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
	}
	if allAlready {
		return Result{
			Action: "already",
			Detail: "insteadOf + git-credential helper already set for " + strings.Join(hosts, ","),
			Path:   "git config --global",
		}
	}

	if dryRun {
		detail := "would set insteadOf for " + strings.Join(hosts, ",")
		if helperMissing {
			detail += " + credential.helper=!specula git-credential"
		}
		if useHttpPathMissing {
			detail += " + credential.useHttpPath"
		}
		return Result{
			Action: "added",
			Detail: detail,
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

	helperAdded := false
	if helperMissing {
		cmd := exec.Command("git", "config", "--global", "--add", "credential.helper", helperValue)
		cmd.Env = append(os.Environ(), "HOME="+home)
		if out, err := cmd.CombinedOutput(); err != nil {
			return Result{Action: "error", Err: fmt.Sprintf("credential.helper: %v: %s", err, bytesTrim(out))}
		}
		helperAdded = true
	}

	useHttpPathAdded := false
	if useHttpPathMissing && useHttpPathKey != "" {
		cmd := exec.Command("git", "config", "--global", useHttpPathKey, "true")
		cmd.Env = append(os.Environ(), "HOME="+home)
		if out, err := cmd.CombinedOutput(); err != nil {
			return Result{Action: "error", Err: fmt.Sprintf("useHttpPath: %v: %s", err, bytesTrim(out))}
		}
		useHttpPathAdded = true
	}

	if len(added) == 0 && !helperAdded && !useHttpPathAdded {
		return Result{
			Action: "already",
			Detail: "insteadOf + git-credential helper already set for " + strings.Join(hosts, ","),
			Path:   "git config --global",
		}
	}
	detail := "HTTPS → Specula /git/<host>/"
	if len(added) > 0 {
		detail += " for " + strings.Join(added, ",")
	}
	if helperAdded {
		detail += "; credential.helper=!specula git-credential (maps proxy URL → upstream creds)"
	}
	if useHttpPathAdded {
		detail += "; credential.useHttpPath on Specula base (path reaches the helper)"
	}
	return Result{
		Action: "added",
		Detail: detail,
		Path:   "~/.gitconfig",
	}
}

// speculaUseHttpPathKey returns credential.<specula-origin>.useHttpPath so git
// includes /git/<host>/… in the helper request. Without it the helper only sees
// host=127.0.0.1:7732 and cannot map back to github.com.
func speculaUseHttpPathKey(addr, home string) (key string, missing bool) {
	u := strings.TrimRight(addr, "/")
	// addr is like http://127.0.0.1:7732 — strip any path.
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			u = u[:i+3+slash]
		}
	}
	if u == "" {
		return "", false
	}
	key = "credential." + u + ".useHttpPath"
	cur := gitConfig(home, key)
	return key, strings.ToLower(strings.TrimSpace(cur)) != "true"
}

// speculaGitCredentialHelper returns the git credential.helper value that
// invokes this Specula binary's git-credential subcommand. Empty when the
// binary path cannot be resolved.
func speculaGitCredentialHelper() string {
	exe, err := os.Executable()
	if err != nil {
		if p, lookErr := exec.LookPath("specula"); lookErr == nil {
			exe = p
		} else {
			return ""
		}
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return "!" + exe + " git-credential"
}

func gitHasCredentialHelper(home, want string) bool {
	cmd := exec.Command("git", "config", "--global", "--get-all", "credential.helper")
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	want = strings.TrimSpace(want)
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
		// Match any "!…/specula git-credential" so reinstalls with a new
		// absolute path don't duplicate forever when the old path still works.
		if strings.Contains(line, "specula git-credential") {
			return true
		}
	}
	return false
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
