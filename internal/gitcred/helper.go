// Package gitcred rewrites Specula git-proxy credential requests back to the
// upstream host so private clones through url.<specula>/git/<host>/.insteadOf
// still pick up the operator's normal github.com / gitlab.com credentials.
//
// Ground truth this exists to fix: after insteadOf rewrites
// https://github.com/org/repo → http://127.0.0.1:7732/git/github.com/org/repo,
// git asks the credential helper for host=127.0.0.1:7732. Helpers keyed on
// github.com never fire → 401 from GitHub → "could not read Username for
// http://127.0.0.1:7732". Specula already passthroughs Authorization; the
// missing piece is mapping the rewritten URL back to the upstream for fill.
package gitcred

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// DefaultUpstreamHosts are the hosts Specula integrate writes insteadOf for.
// A path of git/<host>/… only rewrites when host is in this set (or an
// explicit allowlist passed by the caller).
var DefaultUpstreamHosts = []string{
	"github.com",
	"gitlab.com",
	"gitee.com",
	"codeberg.org",
	"git.sr.ht",
	"bitbucket.org",
}

// ParseAttrs reads the git-credential key=value protocol from r until a blank
// line (or EOF). Duplicate keys keep the last value.
func ParseAttrs(r io.Reader) (map[string]string, error) {
	out := make(map[string]string)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// WriteAttrs emits attrs in git-credential protocol order (stable key order
// for tests: protocol, host, path, then the rest alphabetically) ending with
// a blank line.
func WriteAttrs(w io.Writer, attrs map[string]string) error {
	order := []string{"protocol", "host", "path", "username", "password"}
	seen := map[string]bool{}
	for _, k := range order {
		if v, ok := attrs[k]; ok {
			if _, err := fmt.Fprintf(w, "%s=%s\n", k, v); err != nil {
				return err
			}
			seen[k] = true
		}
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	// insertion order of remaining keys is map-random; sort for stability.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, k := range keys {
		if _, err := fmt.Fprintf(w, "%s=%s\n", k, attrs[k]); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// RewriteForUpstream maps a Specula git-proxy credential request onto the
// upstream host. Returns ok=false when the request is not a Specula
// /git/<host>/… path (caller should then emit nothing so the next helper runs).
func RewriteForUpstream(attrs map[string]string, allowedHosts []string) (map[string]string, bool) {
	path := strings.TrimPrefix(attrs["path"], "/")
	if !strings.HasPrefix(path, "git/") {
		return nil, false
	}
	rest := strings.TrimPrefix(path, "git/")
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return nil, false
	}
	host := rest[:slash]
	project := rest[slash+1:]
	if project == "" || strings.Contains(project, "..") {
		return nil, false
	}
	if !hostAllowed(host, allowedHosts) {
		return nil, false
	}
	out := make(map[string]string, len(attrs)+2)
	for k, v := range attrs {
		out[k] = v
	}
	out["protocol"] = "https"
	out["host"] = host
	out["path"] = project
	return out, true
}

func hostAllowed(host string, allowed []string) bool {
	if len(allowed) == 0 {
		allowed = DefaultUpstreamHosts
	}
	for _, h := range allowed {
		if h == host {
			return true
		}
	}
	return false
}

// IsSpeculaHelper reports whether a credential.helper value is Specula's own
// helper (used to avoid recursion when re-invoking git credential fill).
func IsSpeculaHelper(value string) bool {
	v := strings.TrimSpace(value)
	v = strings.TrimPrefix(v, "!")
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	// Match "specula git-credential" / "/usr/local/bin/specula git-credential"
	// and quoted variants.
	v = strings.Trim(v, `"'`)
	fields := strings.Fields(v)
	if len(fields) < 2 {
		return false
	}
	bin := fields[0]
	return (bin == "specula" || strings.HasSuffix(bin, "/specula")) &&
		fields[1] == "git-credential"
}
