package integrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ivanzzeth/specula/internal/config"
)

const cargoConfigMarker = "# managed-by-specula-integrate"

func integrateCargo(home, addr string, dryRun bool) Result {
	path := filepath.Join(home, ".cargo", "config.toml")
	registry := "sparse+" + strings.TrimRight(addr, "/") + "/cargo/index/"
	block := fmt.Sprintf(`
%s
[source.crates-io]
replace-with = "specula"
[source.specula]
registry = %q
`, cargoConfigMarker, registry)

	existing, _ := os.ReadFile(path)
	if strings.Contains(string(existing), cargoConfigMarker) && strings.Contains(string(existing), registry) {
		return Result{Action: "already", Detail: "crates-io already replaced with Specula", Path: path}
	}
	if dryRun {
		return Result{Action: "added", Detail: "would write source.replace crates-io → " + registry, Path: path}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	var out string
	if len(existing) > 0 {
		// Strip prior Specula-managed block if present.
		out = stripCargoManaged(string(existing)) + "\n" + strings.TrimPrefix(block, "\n")
	} else {
		out = strings.TrimPrefix(block, "\n")
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	return Result{Action: "added", Detail: "crates-io → " + registry, Path: path}
}

func stripCargoManaged(s string) string {
	lines := strings.Split(s, "\n")
	var keep []string
	skipping := false
	for _, line := range lines {
		if strings.Contains(line, cargoConfigMarker) {
			skipping = true
			continue
		}
		if skipping {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if strings.HasPrefix(trimmed, "[") && !strings.HasPrefix(trimmed, "[source.") {
				skipping = false
				keep = append(keep, line)
				continue
			}
			if strings.HasPrefix(trimmed, "[source.crates-io]") || strings.HasPrefix(trimmed, "[source.specula]") {
				continue
			}
			if strings.HasPrefix(trimmed, "replace-with") || strings.HasPrefix(trimmed, "registry") {
				continue
			}
			if strings.HasPrefix(trimmed, "[") {
				skipping = false
				keep = append(keep, line)
			}
			continue
		}
		keep = append(keep, line)
	}
	return strings.TrimRight(strings.Join(keep, "\n"), "\n")
}

const condaMarker = "# managed-by-specula-integrate"

// Listing Specula's URL directly under `channels:` leaks it into every
// environment.yml exported on this machine — `conda env export` copies that
// block verbatim, and environment.yml is committed. `--ignore-channels` does not
// help: it only strips per-package channel prefixes, the `channels:` block keeps
// the full URL.
//
// conda's own answer is `custom_channels`, its documented name → location
// indirection. Listing the NAME under `channels:` and the mirror under
// `custom_channels` does both halves — verified against conda 24.9.2:
// `Channel('conda-forge').urls()` resolved to the mirror (a dead-port mirror
// produced "CONNECTION FAILED for url <http://127.0.0.1:19999/conda/...>", so
// fetches really go through Specula) while `canonical_name` stayed
// "conda-forge" and `conda env export` wrote `- conda-forge` with no host.
//
// Same shape as npm's omit key and Cargo's source replacement: fetch through the
// mirror, record the canonical name.
func integrateConda(home, addr string, dryRun bool, cfg *config.Config) Result {
	path := filepath.Join(home, ".condarc")
	names := condaChannelNamesFromConfig(cfg)
	base := strings.TrimRight(addr, "/") + "/conda"

	var b strings.Builder
	b.WriteString(condaMarker)
	b.WriteString("\nchannels:\n")
	for _, n := range names {
		b.WriteString("  - ")
		b.WriteString(n)
		b.WriteByte('\n')
	}
	// custom_channels maps the NAME to Specula. conda appends "/<name>" itself,
	// so the value is the base, not the per-channel URL.
	b.WriteString("custom_channels:\n")
	for _, n := range names {
		b.WriteString("  ")
		b.WriteString(n)
		b.WriteString(": ")
		b.WriteString(base)
		b.WriteByte('\n')
	}
	b.WriteString("channel_priority: strict\n")
	block := b.String()

	existing, _ := os.ReadFile(path)
	existingStr := string(existing)

	// "already" requires the custom_channels indirection to be present, not just
	// the channel names. A machine integrated before this fix has the mirror URL
	// sitting in `channels:` and would otherwise be reported done — leaving
	// exactly the machines that already leak still leaking. Same reason npm's
	// idempotence check had to include its omit key.
	already := len(existing) > 0 && strings.Contains(existingStr, "custom_channels:")
	if already {
		for _, n := range names {
			if !strings.Contains(existingStr, n+": "+base) {
				already = false
				break
			}
		}
	}
	if already {
		return Result{Action: "already", Detail: "conda channels already resolve to Specula by name", Path: path}
	}
	if dryRun {
		return Result{Action: "added", Detail: "would set channels " + strings.Join(names, ",") + " + custom_channels → " + base + " (keeps the mirror out of environment.yml)", Path: path}
	}

	// Drop any previous Specula-managed block so re-running does not stack
	// duplicate channels/custom_channels; user keys are preserved.
	rest := stripCondaManaged(existingStr)
	out := block
	if strings.TrimSpace(rest) != "" {
		out = block + "\n" + rest + "\n"
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	return Result{Action: "added", Detail: "channels " + strings.Join(names, ",") + " via custom_channels → " + base + " (environment.yml records names, not this address)", Path: path}
}

// stripCondaManaged removes a previously written Specula block (marker,
// channels:, custom_channels:, channel_priority) and keeps every other key the
// operator put in ~/.condarc. It also drops a legacy bare `channels:` list of
// Specula URLs written before the custom_channels fix.
func stripCondaManaged(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	var keep []string
	// skipList is set while inside a managed block-scalar key whose indented
	// entries must go too.
	skipList := false
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if skipList {
				continue
			}
			keep = append(keep, line)
			continue
		}
		// Indented continuation of a managed list/map.
		if skipList && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			continue
		}
		skipList = false
		if trimmed == condaMarker {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "channels:"), strings.HasPrefix(trimmed, "custom_channels:"):
			skipList = true
			continue
		case strings.HasPrefix(trimmed, "channel_priority:"):
			continue
		}
		keep = append(keep, line)
	}
	return strings.TrimRight(strings.Join(keep, "\n"), "\n")
}

func integrateHF(home, addr string, dryRun bool) Result {
	endpoint := strings.TrimRight(addr, "/") + "/hf"
	path := filepath.Join(home, ".config", "specula", "env.sh")
	if dryRun {
		return Result{Action: "added", Detail: "would export HF_ENDPOINT=" + endpoint, Path: path}
	}
	// Actual write happens in writeEnvFile; here we only signal success so
	// writeEnvFile includes HF_ENDPOINT for this protocol.
	return Result{Action: "added", Detail: "HF_ENDPOINT=" + endpoint + " (via env.sh)", Path: path}
}
