package integrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const npmBackupKey = "; specula-integrate-previous-registry="

// npm stamps the registry it downloaded from into every `resolved` URL it
// writes to package-lock.json. Once ~/.npmrc points at Specula, every lockfile
// produced on this machine is pinned to `http://127.0.0.1:<port>/npm/...` — an
// address that exists nowhere else.
//
// The failure is badly delayed and badly reported. Such a lockfile keeps
// installing on THIS machine, and on any build whose docker layer cache still
// holds node_modules, so it commits and reviews cleanly. It breaks the first
// time a build actually downloads: `npm ci` honours `resolved` verbatim, every
// package gets ECONNREFUSED, and npm surfaces that as the uninformative
// "Exit handler never called!". A downstream project hit exactly this — 48
// pinned URLs reached main and broke its release build hours later.
//
// Integrating the mirror is OUR action, so containing its blast radius is our
// job: a fix in the consumer (patch the Dockerfile, scrub the lockfile in a
// hook) would have to be reinvented by every project that ever runs
// `specula integrate`.
//
// This is npm's own switch for the problem. It omits the `resolved` field and
// keeps `version` + `integrity`, so integrity checking is NOT weakened and each
// machine resolves through whatever registry it has configured — the mirror
// still serves every download here, it just stops being a fact about the repo.
const npmOmitResolvedKey = "omit-lockfile-registry-resolved"

func integrateNPM(home, addr string, dryRun bool) Result {
	registry := strings.TrimRight(addr, "/") + "/npm/"
	path := npmrcPath(home)
	cur, others, prevBackup, err := parseNPMRC(path)
	if err != nil && !os.IsNotExist(err) {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	// An explicit setting is the user's decision; only ADD the key when absent.
	// Checked against the preserved lines, so "registry already ours but the
	// guard line is missing" is still work to do — a machine integrated before
	// this existed must get the fix on its next `integrate`, not be skipped as
	// already-done.
	hasOmit := false
	for _, line := range others {
		if strings.HasPrefix(strings.TrimSpace(line), npmOmitResolvedKey+"=") {
			hasOmit = true
			break
		}
	}
	if sameProxyURL(cur, registry) && hasOmit {
		return Result{Action: "already", Detail: "registry already Specula", Path: path}
	}
	if dryRun {
		return Result{Action: "added", Detail: fmt.Sprintf("would set registry=%s + %s=true (keep other npmrc keys; backup old)", registry, npmOmitResolvedKey), Path: path}
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
	if !hasOmit {
		b.WriteString(npmOmitResolvedKey + "=true\n")
	}
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
