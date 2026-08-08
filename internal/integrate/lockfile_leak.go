package integrate

import "strings"

// Pointing a package manager at Specula must not put Specula's address into a
// file the project COMMITS.
//
// npm taught us this: it stamps the registry it downloaded from into every
// `resolved` URL in package-lock.json, so a machine wired to Specula produced
// lockfiles pinned to `http://127.0.0.1:<port>/npm/`. Those keep installing on
// that machine, and on any build whose docker layer cache still holds
// node_modules, so they commit and review cleanly — then break the first build
// that actually downloads, with ECONNREFUSED reported only as npm's useless
// "Exit handler never called!". 48 such URLs reached a downstream repo's main
// and broke its release build hours later.
//
// Every ecosystem gets audited against that same question, and the answers live
// here rather than scattered across the per-protocol files, so adding the next
// protocol costs one row instead of one more forgotten investigation.
type lockfileLeak struct {
	// protocol is the integrate protocol name.
	protocol string
	// artifact is the committed file that would carry Specula's address.
	artifact string
	// mitigation is the tool's OWN mechanism for keeping the mirror out of it.
	// Empty means the tool offers none — say so plainly rather than inventing a
	// scrubber that every future tool version can silently outgrow.
	mitigation string
	// note is the one line an operator needs to act, shown at integrate time.
	note string
}

// lockfileLeaks lists only the protocols where a leak is POSSIBLE. Protocols
// absent from this table were checked against the real tool and cannot leak;
// the evidence for each is recorded in lockfile_leak_test.go so the next reader
// does not repeat the experiment.
var lockfileLeaks = []lockfileLeak{
	{
		protocol:   "npm",
		artifact:   "package-lock.json",
		mitigation: npmOmitResolvedKey + "=true",
		note:       "npm writes the download registry into every `resolved` URL; the omit key drops the field and keeps version+integrity",
	},
	{
		protocol:   "pypi",
		artifact:   "requirements.txt (pip-compile)",
		mitigation: "pip-compile --no-emit-index-url --no-emit-trusted-host",
		note:       "pip-tools reads this pip.conf and echoes index-url into the file it generates; it has no user-level config, so set the flags (or [tool.pip-tools] in the project's pyproject.toml)",
	},
	{
		protocol: "helm",
		artifact: "Chart.lock",
		// Verified against the live binary: `helm dependency update` writes the
		// RESOLVED url, and the "@alias" spelling does not protect you — alias
		// and literal URL produced identical Chart.lock entries. Helm has no
		// omit-equivalent, so the honest fix is upstream URLs in Chart.yaml.
		mitigation: "",
		note:       "`helm dependency update` records the resolved repo URL in Chart.lock; keep upstream URLs in Chart.yaml and let Specula redirect at the network layer",
	},
}

// leakNote returns the operator-facing note for a protocol, or "" when that
// protocol cannot leak.
func leakNote(protocol string) string {
	for _, l := range lockfileLeaks {
		if l.protocol == protocol {
			return l.note
		}
	}
	return ""
}

// leakWarning renders the note plus the official mitigation (or an explicit
// statement that the tool has none) for use in integrate details and audits.
func leakWarning(protocol string) string {
	for _, l := range lockfileLeaks {
		if l.protocol != protocol {
			continue
		}
		var b strings.Builder
		b.WriteString(l.artifact)
		b.WriteString(" would carry this address — ")
		b.WriteString(l.note)
		if l.mitigation != "" {
			b.WriteString("; use: ")
			b.WriteString(l.mitigation)
		} else {
			b.WriteString("; the tool offers no switch to suppress it")
		}
		return b.String()
	}
	return ""
}
