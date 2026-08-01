package gitcred

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ListHelpers returns every credential.helper value from the global+local git
// config (in config order). Missing git / empty config → nil.
func ListHelpers() []string {
	cmd := exec.Command("git", "config", "--get-regexp", `^credential\.helper$`)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var helpers []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// "credential.helper value with spaces"
		_, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		helpers = append(helpers, val)
	}
	return helpers
}

// FillUpstream runs `git credential fill` for the rewritten upstream attrs,
// excluding Specula's own helper so we cannot recurse.
func FillUpstream(attrs map[string]string) (map[string]string, error) {
	args := []string{"-c", "credential.helper="}
	for _, h := range ListHelpers() {
		if IsSpeculaHelper(h) {
			continue
		}
		args = append(args, "-c", "credential.helper="+h)
	}
	// Prefer env tokens for github.com when no helper produced them — handled
	// after fill returns empty (see FillOrEnv).
	args = append(args, "credential", "fill")
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdin bytes.Buffer
	if err := WriteAttrs(&stdin, attrs); err != nil {
		return nil, err
	}
	cmd.Stdin = &stdin
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git credential fill: %w", err)
	}
	return ParseAttrs(bytes.NewReader(out))
}

// FillOrEnv fills via git helpers, then falls back to GH_TOKEN / GITHUB_TOKEN
// for github.com (common in CI / deploy mirrors).
func FillOrEnv(attrs map[string]string) (map[string]string, error) {
	filled, err := FillUpstream(attrs)
	if err == nil && strings.TrimSpace(filled["password"]) != "" {
		return filled, nil
	}
	if attrs["host"] == "github.com" {
		tok := strings.TrimSpace(os.Getenv("GH_TOKEN"))
		if tok == "" {
			tok = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
		}
		if tok != "" {
			out := make(map[string]string, len(attrs)+2)
			for k, v := range attrs {
				out[k] = v
			}
			out["username"] = "x-access-token"
			out["password"] = tok
			return out, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return filled, nil
}

// PassThroughStoreErase rewrites attrs and runs `git credential <action>`
// (store|erase) against the upstream, excluding Specula's helper.
func PassThroughStoreErase(action string, attrs map[string]string) error {
	if action != "store" && action != "erase" {
		return fmt.Errorf("unsupported action %q", action)
	}
	args := []string{"-c", "credential.helper="}
	for _, h := range ListHelpers() {
		if IsSpeculaHelper(h) {
			continue
		}
		args = append(args, "-c", "credential.helper="+h)
	}
	args = append(args, "credential", action)
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdin bytes.Buffer
	if err := WriteAttrs(&stdin, attrs); err != nil {
		return err
	}
	cmd.Stdin = &stdin
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git credential %s: %w (%s)", action, err, bytesTrim(out))
	}
	return nil
}

func bytesTrim(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
