package integrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers ONE bug class across every protocol `specula integrate`
// touches: pointing a package manager at Specula must not put Specula's address
// into a file the project COMMITS.
//
// The class was found in npm (see integrate_test.go). npm writes the registry it
// downloaded from into every `resolved` URL in package-lock.json, so a machine
// wired to Specula produced lockfiles pinned to `http://127.0.0.1:<port>/npm/`.
// It kept installing on that machine and on any build whose docker layer cache
// still held node_modules — so it committed and reviewed cleanly, then broke the
// first build that actually downloaded: ECONNREFUSED on every package, reported
// only as npm's useless "Exit handler never called!". 48 such URLs reached a
// downstream repo's main and broke its release build hours later.
//
// Wiring the mirror is Specula's action, so keeping the mirror out of the
// consumer's committed files is Specula's job — a per-project workaround would
// have to be re-invented by everyone who ever runs `specula integrate`.
//
// Every claim below was verified against the real tool, not inferred from docs.
// Where a tool is UNAFFECTED the reason is recorded here too, so a later reader
// does not have to redo the experiment to find out why it was skipped.

// ---------------------------------------------------------------------------
// pypi — pip-compile writes the active index-url into requirements.txt
// ---------------------------------------------------------------------------

// pip itself is safe: requirements.txt is hand-written and pip records nothing.
// But pip-tools, the standard pinning tool for the pip ecosystem, reads the same
// pip.conf Specula rewrites and echoes the index into the file it generates —
// and requirements.txt IS committed. Verified with pip-tools 7.6.0 against a
// pip.conf written exactly as integratePip writes it:
//
//	#    pip-compile --output-file=requirements.txt requirements.in
//	--index-url http://127.0.0.1:7739/pypi/simple
//	--trusted-host 127.0.0.1
//
//	idna==3.18
//
// Same run with --no-emit-index-url --no-emit-trusted-host produced the pins and
// nothing else. Those are pip-tools' own documented flags ("Add index URL to
// generated file"), and pip-tools reads defaults for them from the
// [tool.pip-tools] table of a per-project pyproject.toml / .pip-tools.toml.
//
// pip-tools has NO user-level config file and NO env var for these options
// (only PIP_TOOLS_CACHE_DIR and PIP_TOOLS_RESOLVER carry envvar= in its source),
// so unlike npm there is nowhere machine-wide for Specula to set them. What
// Specula CAN do from ~/.config is warn: the audit must name the exact flag, so
// whoever hits it fixes it in one step instead of rediscovering the npm story.
func TestAuditFlagsPipCompileIndexURLLeak(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", "pip", "pip.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// The exact shape integratePip leaves behind.
	body := "[global]\nindex-url = http://127.0.0.1:7732/pypi/simple\ntrusted-host = 127.0.0.1\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	risks := AuditClientRisks(home)
	var found *Result
	for i := range risks {
		if risks[i].Protocol == "pypi" && strings.Contains(risks[i].Detail, "pip-compile") {
			found = &risks[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("pip.conf points at Specula but nothing warns that pip-compile will\n"+
			"bake that address into the committed requirements.txt; got %+v", risks)
	}
	// Naming the flag is the point — a warning that says only "be careful" costs
	// the reader the same investigation the npm incident already paid for.
	if !strings.Contains(found.Detail, "--no-emit-index-url") {
		t.Fatalf("warning must name the official flag --no-emit-index-url, got: %s", found.Detail)
	}
}

// A pip.conf that is NOT pointed at Specula cannot leak Specula's address, so
// warning there would be noise that trains people to ignore the audit.
func TestAuditNoPipCompileWarningForPublicIndex(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", "pip", "pip.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[global]\nindex-url = https://pypi.org/simple\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, r := range AuditClientRisks(home) {
		if strings.Contains(r.Detail, "pip-compile") {
			t.Fatalf("public index cannot leak a mirror address; spurious warning: %+v", r)
		}
	}
}

// ---------------------------------------------------------------------------
// conda — channels: <url> leaks; custom_channels keeps the NAME
// ---------------------------------------------------------------------------

// Verified with conda 24.9.2. With Specula's current output
// (`channels: [http://127.0.0.1:7739/conda/conda-forge]`), `conda env export`
// emits that literal address into environment.yml, which is committed:
//
//	channels:
//	  - http://127.0.0.1:7739/conda/conda-forge
//
// `--ignore-channels` does NOT help — it only strips per-package channel
// prefixes, the `channels:` block keeps the full URL.
//
// conda's own answer is `custom_channels`, its documented name → location
// indirection ("A map of key-value pairs where the key is a channel name and
// the value is a channel location"). Listing the NAME in `channels:` and the
// mirror in `custom_channels` was verified to do both halves:
//
//	channels: [conda-forge] + custom_channels: {conda-forge: http://127.0.0.1:19999/conda}
//	  → fetch really goes to the mirror:
//	    CondaHTTPError: HTTP 000 CONNECTION FAILED for url
//	    <http://127.0.0.1:19999/conda/conda-forge/linux-64/repodata.json>
//	  → but the recorded identity stays upstream:
//	    Channel('conda-forge').canonical_name == 'conda-forge'
//	  → and `conda env export` writes `- conda-forge`, no host at all.
//
// This is conda's structural equivalent of npm's omit key and of Cargo's source
// replacement: fetch through the mirror, record the canonical name.
func TestIntegrateCondaKeepsMirrorOutOfEnvironmentYML(t *testing.T) {
	home := t.TempDir()
	r := integrateConda(home, "http://127.0.0.1:7732", false, nil)
	if r.Action != "added" {
		t.Fatalf("%+v", r)
	}
	b, err := os.ReadFile(filepath.Join(home, ".condarc"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)

	// The channel is listed by NAME, so `conda env export` records the name.
	if !strings.Contains(s, "\n  - conda-forge\n") {
		t.Fatalf("channels must list the channel NAME (conda env export copies this\n"+
			"block verbatim into the committed environment.yml):\n%s", s)
	}
	// The mirror lives only in custom_channels, which export never emits.
	if !strings.Contains(s, "custom_channels:") {
		t.Fatalf("missing custom_channels indirection:\n%s", s)
	}
	if !strings.Contains(s, "conda-forge: http://127.0.0.1:7732/conda") {
		t.Fatalf("custom_channels must point conda-forge at Specula:\n%s", s)
	}
	// The whole point: no bare mirror URL in the channels list.
	if strings.Contains(s, "channels:\n  - http://") {
		t.Fatalf("channels list still holds a raw mirror URL — it will be copied\n"+
			"into every environment.yml exported on this machine:\n%s", s)
	}
	// Re-running must not duplicate or drift.
	r2 := integrateConda(home, "http://127.0.0.1:7732", false, nil)
	if r2.Action != "already" {
		t.Fatalf("want already, got %+v", r2)
	}
	b2, err := os.ReadFile(filepath.Join(home, ".condarc"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b2), "custom_channels:"); n != 1 {
		t.Fatalf("want exactly 1 custom_channels block, got %d:\n%s", n, b2)
	}
}

// A machine integrated BEFORE this fix has `channels: [<mirror url>]` and no
// custom_channels. Re-running integrate must repair it rather than report
// "already" — the same reason npm's idempotence check had to include the omit
// key. Without this, exactly the machines that already leak stay leaking.
func TestIntegrateCondaUpgradesLegacyURLChannel(t *testing.T) {
	home := t.TempDir()
	legacy := "# managed-by-specula-integrate\nchannels:\n  - http://127.0.0.1:7732/conda/conda-forge\nchannel_priority: strict\n"
	if err := os.WriteFile(filepath.Join(home, ".condarc"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	r := integrateConda(home, "http://127.0.0.1:7732", false, nil)
	if r.Action != "added" {
		t.Fatalf("a pre-fix .condarc still leaks the mirror into environment.yml;\n"+
			"integrate must repair it, got %+v", r)
	}
	b, err := os.ReadFile(filepath.Join(home, ".condarc"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "- http://127.0.0.1:7732/conda/conda-forge") {
		t.Fatalf("legacy URL channel not replaced by the name:\n%s", s)
	}
	if !strings.Contains(s, "custom_channels:") {
		t.Fatalf("missing custom_channels after upgrade:\n%s", s)
	}
}

// Everything the operator put in ~/.condarc themselves must survive.
func TestIntegrateCondaPreservesUserKeys(t *testing.T) {
	home := t.TempDir()
	orig := "auto_activate_base: false\nssl_verify: true\n"
	if err := os.WriteFile(filepath.Join(home, ".condarc"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := integrateConda(home, "http://127.0.0.1:7732", false, nil); r.Action != "added" {
		t.Fatalf("%+v", r)
	}
	b, err := os.ReadFile(filepath.Join(home, ".condarc"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "auto_activate_base: false") || !strings.Contains(s, "ssl_verify: true") {
		t.Fatalf("user keys lost:\n%s", s)
	}
}

// ---------------------------------------------------------------------------
// helm — Chart.lock records the resolved repository URL, always
// ---------------------------------------------------------------------------

// Verified with the live helm binary against a local chart repo. `helm
// dependency update` writes the RESOLVED url into Chart.lock, and the alias form
// does not protect you — both spellings produced the identical leak:
//
//	Chart.yaml: repository: "@specula-bitnami"        (alias)
//	Chart.yaml: repository: "http://127.0.0.1:7740/…" (literal)
//	  → both →  Chart.lock: repository: http://127.0.0.1:7740/helm/bitnami
//
// Chart.lock is committed. There is NO helm flag to suppress it — helm has no
// equivalent of npm's omit key, and `helm repo add` (which is all integrate
// does) is not itself the leak: the leak happens later, when someone runs
// `helm dependency update` in a chart whose Chart.yaml names the Specula repo.
//
// So this one is honestly unfixable at the integrate layer, and inventing a
// scrubber would be worse than saying so. What integrate CAN do is state the
// constraint at the moment it adds the repos, so the operator learns it before
// a Chart.lock reaches main rather than after.
func TestIntegrateHelmWarnsChartLockRecordsRepoURL(t *testing.T) {
	r := integrateHelm("http://127.0.0.1:7732", true, nil) // dry-run: no helm binary needed
	if r.Action != "added" {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.Detail, "Chart.lock") {
		t.Fatalf("adding Specula helm repos without saying that `helm dependency update`\n"+
			"will write this address into the committed Chart.lock; detail: %q", r.Detail)
	}
}

// ---------------------------------------------------------------------------
// Protocols verified UNAFFECTED — recorded so nobody re-runs the experiment
// ---------------------------------------------------------------------------

// go     — `go.sum` holds only module path + version + h1: hashes, and `go.mod`
//           never mentions a proxy. GOPROXY is env/`go env -w` state, never a
//           committed file; GOPRIVATE/GONOPROXY/GONOSUMDB are the same. Nothing
//           to fix.
//
// cargo  — the load-bearing one. Verified with cargo 1.93.1: with Specula's
//           source-replacement config
//           ([source.crates-io] replace-with + [source.specula] registry =
//           "sparse+http://127.0.0.1:7739/cargo/index/"), cargo logged
//           `Updating 'specula' index` — it really fetched through the mirror —
//           and wrote the CANONICAL url into Cargo.lock:
//               source = "registry+https://github.com/rust-lang/crates.io-index"
//           byte-identical to a lockfile generated with no mirror at all.
//           This holds because source replacement is defined as serving
//           identical content, so it is a fetch-time detail, not lock identity.
//           It does NOT hold for cargo's OTHER redirection mode, alternate
//           registries, which write `source = "sparse+<mirror>"`. Specula's
//           choice of source replacement is therefore a correctness
//           requirement, not a style preference — the test below pins it.
//
// git    — verified: with a global insteadOf rewrite active, `git submodule add
//           https://github.com/org/repo` wrote the ORIGINAL url to .gitmodules
//           (the committed file); the rewrite applied only to the transport.
//           insteadOf is by design a local-transport rewrite. Nothing to fix.
//
// oci    — daemon.json / hosts.toml / registries.yaml are node configuration,
//           never committed by the projects being built. Image references in
//           Dockerfiles and manifests stay `docker.io/...`; the redirect lives
//           in containerd's hosts.toml, which is the whole point of that design.
//
// apt    — /etc/apt/sources.list.d/specula.list is machine state. apt has no
//           project-level lockfile.
//
// hf     — HF_ENDPOINT is env only; everything persisted lands in the local
//           HF cache, which is not a committed artifact. No lockfile exists.
//
// poetry — verified with poetry 2.4.1 and pip.conf pointed at the mirror:
//           poetry.lock recorded filenames + sha256 only, no URL and no
//           [package.source] block. Poetry does not read pip.conf at all, so
//           integratePip cannot reach it. (A url DOES appear if a project
//           declares [[tool.poetry.source]] itself — but that is the project's
//           own choice, not something integrate causes.)
//
// uv     — verified with uv 0.9.18: `uv.lock` DOES record the index it resolved
//           through (`source = { registry = "http://127.0.0.1:7739/pypi/simple" }`
//           when UV_INDEX_URL was set). But uv ignores pip.conf AND PIP_INDEX_URL
//           — both runs produced `registry = "https://pypi.org/simple"`. Specula
//           never sets UV_INDEX_URL, so integrate does not cause this leak and
//           has no config file to fix it in. Recorded as a known hazard: if
//           Specula ever starts exporting UV_INDEX_URL, this becomes a real bug
//           with no official mitigation (uv has no omit-equivalent).

// Cargo's immunity comes specifically from SOURCE REPLACEMENT. Alternate
// registries redirect just as well but write `source = "sparse+<mirror>"` into
// Cargo.lock. This test exists so that a future refactor toward the
// "cleaner-looking" [registries.*] form fails here instead of in someone's CI.
func TestIntegrateCargoUsesSourceReplacementNotAlternateRegistry(t *testing.T) {
	home := t.TempDir()
	if r := integrateCargo(home, "http://127.0.0.1:7732", false); r.Action != "added" {
		t.Fatalf("%+v", r)
	}
	b, err := os.ReadFile(filepath.Join(home, ".cargo", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// Source replacement: cargo records the canonical crates-io url in Cargo.lock.
	if !strings.Contains(s, "[source.crates-io]") || !strings.Contains(s, `replace-with = "specula"`) {
		t.Fatalf("cargo must redirect via source replacement — it is the only mode\n"+
			"that keeps the mirror out of Cargo.lock:\n%s", s)
	}
	// Alternate registries would leak `source = "sparse+<mirror>"` into Cargo.lock.
	if strings.Contains(s, "[registries.") {
		t.Fatalf("alternate-registry form writes the mirror into Cargo.lock;\n"+
			"use source replacement instead:\n%s", s)
	}
}
