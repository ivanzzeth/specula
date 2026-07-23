package integrate

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/ivanzzeth/specula/internal/config"
)

func integrateHelm(addr string, dryRun bool, cfg *config.Config) Result {
	base := strings.TrimRight(addr, "/")
	repos := helmReposFromConfig(cfg)
	if dryRun {
		var names []string
		for _, r := range repos {
			names = append(names, r.name)
		}
		return Result{Action: "added", Detail: "would helm repo add " + strings.Join(names, ","), Path: "helm"}
	}
	if _, err := exec.LookPath("helm"); err != nil {
		return Result{Action: "skipped", Detail: "helm binary not found"}
	}
	var added []string
	for _, r := range repos {
		repoURL := base + r.path
		out, _ := exec.Command("helm", "repo", "list", "-o", "json").Output()
		if strings.Contains(string(out), `"name":"`+r.name+`"`) {
			cmd := exec.Command("helm", "repo", "add", r.name, repoURL, "--force-update")
			if err := cmd.Run(); err != nil {
				continue
			}
			added = append(added, r.name)
			continue
		}
		cmd := exec.Command("helm", "repo", "add", r.name, repoURL)
		if out, err := cmd.CombinedOutput(); err != nil {
			return Result{Action: "error", Err: fmt.Sprintf("%v: %s", err, bytesTrim(out)), Path: "helm"}
		}
		added = append(added, r.name)
	}
	if len(added) == 0 {
		return Result{Action: "already", Detail: "helm repos already present", Path: "helm"}
	}
	return Result{Action: "added", Detail: "helm repo add " + strings.Join(added, ","), Path: "helm"}
}

func integrateApt(addr string, dryRun, skipRoot bool, cfg *config.Config) Result {
	if skipRoot {
		return Result{Action: "skipped", Detail: "apt integrate requires root (skipped)"}
	}
	suite := detectAptSuite()
	path := "/etc/apt/sources.list.d/specula.list"
	base := strings.TrimRight(addr, "/")
	archive := aptArchiveFromConfig(cfg)
	// Point at the allowlisted archive prefix (protocols.apt.apt.repositories).
	archiveURL := base + "/apt/" + archive + "/"
	body := fmt.Sprintf("# Added by `specula integrate` — does not modify sources.list / ubuntu.sources\n"+
		"# Suite auto-detected from /etc/os-release (override by editing this file).\n"+
		"# Specula protocols.apt.apt.repositories must include name=%s.\n"+
		"deb [trusted=yes] %s %s main restricted universe multiverse\n"+
		"deb [trusted=yes] %s %s-updates main restricted universe multiverse\n"+
		"deb [trusted=yes] %s %s-security main restricted universe multiverse\n",
		archive, archiveURL, suite, archiveURL, suite, archiveURL, suite)
	if _, err := os.Stat(path); err == nil {
		cur, _ := os.ReadFile(path)
		wantNeedle := archiveURL + " " + suite + " "
		if strings.Contains(string(cur), wantNeedle) {
			return Result{Action: "already", Detail: "specula.list already points at Specula (" + suite + ", archive=" + archive + ")", Path: path}
		}
	}
	if dryRun {
		return Result{Action: "added", Detail: "would write " + path + " (suite=" + suite + ", archive=" + archive + ")", Path: path}
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		if os.IsPermission(err) {
			return Result{Action: "skipped", Detail: "need root to write " + path + " — re-run: sudo specula integrate --protocols apt", Path: path}
		}
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	return Result{Action: "added", Detail: "wrote apt source suite=" + suite + " archive=" + archive + " (apt-get update to refresh)", Path: path}
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
