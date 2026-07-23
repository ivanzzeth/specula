package integrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func integrateConda(home, addr string, dryRun bool) Result {
	path := filepath.Join(home, ".condarc")
	channel := strings.TrimRight(addr, "/") + "/conda/conda-forge"
	block := fmt.Sprintf(`%s
channels:
  - %s
channel_priority: strict
`, condaMarker, channel)

	existing, _ := os.ReadFile(path)
	if strings.Contains(string(existing), channel) {
		return Result{Action: "already", Detail: "conda-forge channel already points at Specula", Path: path}
	}
	if dryRun {
		return Result{Action: "added", Detail: "would prepend channel " + channel, Path: path}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != home {
		// ~/.condarc lives in home; mkdir may be unnecessary
	}
	var out string
	if len(existing) == 0 {
		out = block
	} else {
		// Additive: write a side fragment Specula reads first via env note;
		// also prepend channel into .condarc channels list when possible.
		out = block + "\n" + string(existing)
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return Result{Action: "error", Err: err.Error(), Path: path}
	}
	return Result{Action: "added", Detail: "channel " + channel, Path: path}
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
