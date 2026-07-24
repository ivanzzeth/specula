package integrate

import (
	"fmt"
	"os"
	"strings"
)

// AuditClientRisks scans local client configs for dependency-confusion
// anti-patterns (dual public indexes, leftover extras). Results use Action
// "risk" so callers can surface them alongside integrate status.
func AuditClientRisks(home string) []Result {
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return []Result{{Protocol: "audit", Action: "error", Err: err.Error()}}
		}
	}
	var out []Result
	out = append(out, auditPipRisks(home)...)
	out = append(out, auditNPMRisks(home)...)
	out = append(out, auditEnvRisks()...)
	return out
}

func auditPipRisks(home string) []Result {
	path := pipConfPath(home)
	cfg, err := readPipConf(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []Result{{Protocol: "pypi", Action: "error", Err: err.Error(), Path: path}}
	}
	global := cfg["global"]
	if global == nil {
		return nil
	}
	var out []Result
	if extras := strings.TrimSpace(global["extra-index-url"]); extras != "" {
		out = append(out, Result{
			Protocol: "pypi",
			Action:   "risk",
			Detail:   fmt.Sprintf("extra-index-url is set (%s) — dual indexes enable dependency confusion; prefer sole Specula index-url", extras),
			Path:     path,
		})
	}
	idx := strings.TrimSpace(global["index-url"])
	if idx != "" && looksLikePublicPyPI(idx) {
		out = append(out, Result{
			Protocol: "pypi",
			Action:   "risk",
			Detail:   "index-url points at a public PyPI mirror, not Specula — private names may resolve publicly",
			Path:     path,
		})
	}
	return out
}

func auditNPMRisks(home string) []Result {
	path := npmrcPath(home)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []Result{{Protocol: "npm", Action: "error", Err: err.Error(), Path: path}}
	}
	var out []Result
	var registries []string
	for _, line := range strings.Split(string(b), "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "registry=") {
			registries = append(registries, strings.TrimSpace(strings.TrimPrefix(trim, "registry=")))
		}
		// scoped registries are OK; warn on unscoped dual only via multiple registry=
	}
	if len(registries) > 1 {
		out = append(out, Result{
			Protocol: "npm",
			Action:   "risk",
			Detail:   "multiple registry= lines in .npmrc — ensure private scopes map correctly",
			Path:     path,
		})
	}
	for _, r := range registries {
		if strings.Contains(strings.ToLower(r), "registry.npmjs.org") {
			out = append(out, Result{
				Protocol: "npm",
				Action:   "risk",
				Detail:   "default registry is registry.npmjs.org — Specula should be the sole registry for unscoped packages",
				Path:     path,
			})
			break
		}
	}
	return out
}

func auditEnvRisks() []Result {
	var out []Result
	if v := strings.TrimSpace(os.Getenv("PIP_EXTRA_INDEX_URL")); v != "" {
		out = append(out, Result{
			Protocol: "pypi",
			Action:   "risk",
			Detail:   "env PIP_EXTRA_INDEX_URL is set — dual indexes enable dependency confusion",
			Path:     "env:PIP_EXTRA_INDEX_URL",
		})
	}
	if v := strings.TrimSpace(os.Getenv("PIP_INDEX_URL")); v != "" && looksLikePublicPyPI(v) {
		out = append(out, Result{
			Protocol: "pypi",
			Action:   "risk",
			Detail:   "env PIP_INDEX_URL points at a public PyPI mirror",
			Path:     "env:PIP_INDEX_URL",
		})
	}
	return out
}

// AppendRiskAudit appends AuditClientRisks rows onto a report (status / post-integrate).
func AppendRiskAudit(home string, rep Report) Report {
	risks := AuditClientRisks(home)
	if len(risks) == 0 {
		return rep
	}
	rep.Results = append(rep.Results, risks...)
	return rep
}
