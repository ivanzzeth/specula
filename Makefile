# Specula — build & test orchestration
BINARY := specula
PKG := ./cmd/specula
VERSION_PKG := github.com/ivanzzeth/specula/internal/version

# Version identity comes from git tags (exact tag on a release commit, else describe).
VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null || git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(VERSION_PKG).Version=$(VERSION) \
	-X $(VERSION_PKG).Commit=$(COMMIT) \
	-X $(VERSION_PKG).BuildDate=$(DATE)

# Ship builds are pure-Go/static: sqlite uses the modernc pure-Go driver, so CGO is off by
# default for reproducible static/cross builds. test-unit overrides this to 1 — see there.
export CGO_ENABLED := 0

.PHONY: all ui build build-go run clean vet fmt cover install bench \
        image image-smoke \
        test test-unit test-integration test-postgres test-conformance \
        test-trust-oracle test-trust-oracle-mutations test-trust-oracle-signed \
        test-groundtruth test-groundtruth-meta \
        test-mutation \
        test-realclient test-e2e test-ui test-all

# Container image (Docker Hub: ivanzz/specula)
IMAGE_NAME ?= specula
IMAGE_REPO ?= ivanzz/specula

all: build

# ───────────────────────────── build ─────────────────────────────

## ui: build the WebUI into web/dist (needs: node + npm; network on first install)
ui:
	cd web && npm install && npm run build

## build: WebUI + the single static binary with the WebUI embedded (needs: node + npm)
build: ui
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

## build-go: only the Go binary; assumes web/dist already exists (needs: nothing)
build-go:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

## bench: throughput table against a running data plane (needs: daemon on --addr)
bench: build-go
	./bin/$(BINARY) bench --addr http://127.0.0.1:7732

## install: build-go then install systemd service (needs: root for service install)
install: build-go
	sudo ./bin/$(BINARY) install

## image: build the Specula container image (needs: docker; node+go inside Dockerfile)
image:
	docker build \
		--build-arg VERSION="$(VERSION)" \
		--build-arg COMMIT="$(COMMIT)" \
		--build-arg DATE="$(DATE)" \
		-t "$(IMAGE_NAME):$(VERSION)" \
		-t "$(IMAGE_NAME):local" \
		.

## image-smoke: build image, push to ephemeral Specula hosted OCI, pull back (needs: docker)
image-smoke: image
	IMAGE="$(IMAGE_NAME):$(VERSION)" HOSTED_TAG="$(VERSION)" bash scripts/publish-image-smoke.sh

## vet: go vet over everything, including the tagged suites (needs: nothing)
# The tagged files are invisible to a bare `go vet ./...`, so vet them explicitly —
# otherwise the integration suite rots undetected until someone runs it.
vet:
	go vet ./...
	go vet -tags=integration ./test/...

## fmt: gofmt the tree (needs: nothing)
fmt:
	gofmt -w .

## run: build and run against the example config (needs: node + npm; binds example ports)
run: build
	./bin/$(BINARY) --config specula.example.yaml

clean:
	rm -rf bin web/dist/assets web/dist/index.html coverage.out

# ───────────────────────── the test matrix ─────────────────────────
#
# The dimensions, and what each one actually covers. Read this before assuming
# `go test ./...` is the whole story — IT IS NOT:
#
#   test-unit         pure Go, per-package self-coverage + gate.  needs: nothing
#                     NOTE: `go test ./...` here does NOT include test/e2e — those files
#                     are behind `//go:build integration` and compile out. The protocol
#                     handlers are therefore barely touched by this dimension.
#   test-integration  the test/e2e suite: hermetic, in-process (httptest + temp sqlite).
#                     This is what actually exercises the handlers end to end.  needs: nothing
#   test-postgres     the live-PostgreSQL paths.  needs: docker (or a DSN)
#   test-conformance  the OFFICIAL OCI distribution-spec suite vs the real binary.
#                     Our unique gate — ai-sandbox has no equivalent.  needs: network (first run)
#   test-realclient   real pip / npm / apt-get / helm / git / docker / cargo / conda / hf clients.  needs: network + those tools
#   test-trust-oracle INDEPENDENT re-derivation of the tier each artifact DESERVES, using the
#                     ecosystems' own tooling (gpg / go sumdb / PEP 503 re-fetch). The only
#                     dimension that does not take our own word for anything — every other
#                     one, including the tier counter, is written by the code under test.
#                     needs: network + gpg + go + apt-get
#   test-ui           WebUI typecheck + production build.  needs: node + npm
#
# `test` = test-unit + test-integration = the pure-Go default developer loop: no docker,
# no network, no root. Everything heavier is opt-in.
test: test-unit test-integration

## test-unit: pure-Go unit tests + per-package coverage gate (needs: nothing)
#
# -race requires cgo, hence CGO_ENABLED=1 despite the global CGO_ENABLED=0 above (that one
# governs shipped builds, not local test binaries). -short lets slow tests opt out.
#
# Exit-code semantics, ported from ai-sandbox and RE-VERIFIED here rather than taken on
# faith: we ignore `go test`'s own exit code and decide failure by grepping the log for real
# failure markers. Reason, confirmed on this machine: the goenv-managed toolchain at
# /home/ivan/go/1.24.13/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.7.linux-amd64 ships
# WITHOUT the `covdata` tool, so `go test -coverprofile ./...` prints `go: no such tool
# "covdata"` and exits 1 for every package that has NO test files — while emitting zero FAIL
# lines and leaving coverage.out completely valid. Trusting the exit code makes this target
# permanently red for a non-problem. The grep is also correct on a complete toolchain: a
# genuine failure always prints one of these markers.
#
# The report is printed BEFORE red/green is decided, so CI logs always carry the numbers.
test-unit:
	@log=$$(mktemp); CGO_ENABLED=1 go test -short -race -coverprofile=coverage.out ./... >"$$log" 2>&1 || true; \
	  cat "$$log"; \
	  echo "── coverage summary ──"; go tool cover -func=coverage.out | tail -1; \
	  bash scripts/coverage-gate.sh coverage.out; gate=$$?; \
	  fail=0; grep -qE '^--- FAIL|^FAIL[[:space:]]|\[build failed\]|\[setup failed\]' "$$log" && fail=1; \
	  rm -f "$$log"; \
	  if [ $$fail -ne 0 ]; then echo "✗ real test/build failure detected"; exit 1; fi; \
	  exit $$gate

## test-integration: hermetic e2e suite — in-process httptest + temp sqlite (needs: nothing)
#
# Named for the dimension it occupies: despite living in test/e2e, this suite needs no
# docker, no network and no free-port coordination — every test builds a real Specula stack
# in-process behind httptest (kernel-assigned port) over a t.TempDir() database. The
# dimension needing a REAL binary and REAL infra is test-e2e below.
#
# -count=1 disables the test cache so this is a real run every time.
test-integration:
	go test -tags=integration -count=1 ./test/e2e/...

## test-postgres: live-PostgreSQL store tests (needs: docker, or SPECULA_TEST_POSTGRES_DSN)
#
# Kept out of the hermetic loop so it can never slow it down. Without a DSN these tests
# t.Skip() instantly, so this target provisions a throwaway PostgreSQL on a FREE port and
# drops it afterwards (see scripts/test-postgres.sh).
#
# -tags=postgres is passed for forward compatibility only: NO file in the tree carries that
# tag today; gating is by the SPECULA_TEST_POSTGRES_DSN env var, checked at runtime. This is
# a deliberate divergence from the ported design — internal/store/postgres/postgres_test.go
# mixes pure unit tests (TestHashKey, TestRandomToken) with live-DB tests in the SAME file,
# so tagging that file would delete real unit tests from the default loop for no benefit.
test-postgres:
	bash scripts/test-postgres.sh

## test-conformance: official OCI distribution-spec suite vs the real binary (needs: network on first run)
#
# Our unique gate; ai-sandbox has no equivalent. Runs on kernel-picked free ports against a
# throwaway CAS + sqlite, and proves its OWN daemon answers before grading anything.
test-conformance:
	bash scripts/oci-conformance.sh

## test-trust-oracle: INDEPENDENT oracle vs our honest-tier claims (needs: network + gpg + go + apt-get)
#
# The only dimension that does not take our own word for anything. Every other gate — and
# both places a tier appears (`cache_entries.tier` and `specula_verification_total{tier}`) —
# is written by our own code, so a single bug upstream of both satisfies both: cross-checking
# them agrees row-for-row while the claim is false. We have shipped that exact class four
# times (apt recording tofu x6 behind a "gold standard" claim; go's sumdb verifier never
# running in the documented CN config; git recording no tier with TOFU live; every verifier
# encoding "I skipped this" as StatusPass @ TierChecksum). Each was caught by a human poking
# at it, which neither scales nor is something a customer can do.
#
# This gate re-derives the deserved tier from OUTSIDE, using each ecosystem's own reference
# tooling (gpg(1) + the real distro keyring; the go toolchain's own sumdb verification;
# independent PEP 503 re-fetches), and fails on disagreement. The oracle is Python that
# shells out — it CANNOT import internal/verify, which is the point: an oracle sharing code
# with the thing it grades is a mirror, and a mirror agrees with a lie.
#
# Slow and network-bound by nature, like test-conformance. Writes a machine-readable verdict
# to results/trust-oracle.json so no one can summarise past a disagreement.
# NOT covered here, by design (it grades against real CN mirrors, which serve no signatures):
# cosign keyed / OCI signed and helm .prov signed — those two `signed` tiers are graded by
# test-trust-oracle-signed below, hermetically, against the real cosign/helm binaries.
test-trust-oracle:
	bash scripts/trust-oracle.sh

## test-trust-oracle-signed: INDEPENDENT oracle for the two `signed` tiers the CN-mirror gate
## cannot reach — oci (cosign keyed) and helm (.prov) (needs: docker + cosign + helm + gpg)
#
# scripts/trust-oracle.sh is explicit that it says NOTHING about cosign or helm .prov: no CN
# mirror serves either signature. This gate supplies the missing evidence hermetically — it
# builds a REAL cosign-signed image on a local registry and a REAL helm-signed chart on a
# local repo, drives them through a real Specula, and asserts the recorded tier agrees across
# cache_entries + specula_verification_total + the extended oracle (which re-verifies with the
# real cosign/gpg binaries and an out-of-band key). It also proves the NEGATIVE (unsigned =>
# never signed) and TAMPER (bad signature => refused, reaching the verifier) cases fail closed,
# and INJECTS a fabricated `signed` to prove each new oracle check goes RED.
#
# HONEST SCOPE: this proves the verifier + pipeline reach `signed` against real cosign/helm
# output in a lab we built. It does NOT claim any public CN mirror serves such signatures.
# Set SPECULA_COSIGN_BIN to a cosign binary (buildable in CN via goproxy.cn).
test-trust-oracle-signed:
	bash scripts/trust-oracle-signed.sh

## test-trust-oracle-mutations: prove the oracle CATCHES lies (needs: everything above + a clean tree)
#
# The meta-gate. `make test-trust-oracle` going green proves nothing on its own — an oracle
# that grades nothing, or grades a fresh upstream copy instead of the bytes we stored, passes
# identically. A check that has never been observed to fail is not evidence.
#
# So this injects real lies into the verify code (tier upgrade / apt under-claim / skip-as-pass),
# rebuilds, and asserts the oracle goes RED for each. Every mutation is compiled first: one
# that fails to build would turn the gate red for a build error and prove nothing.
#
# Mutates tracked files IN PLACE and restores them via git, refusing to start unless the
# targets are clean — so it is NOT part of `test-e2e` and never runs unattended alongside
# edits to internal/verify.
test-trust-oracle-mutations:
	bash scripts/trust-oracle-mutations.sh

## test-realclient: real pip/npm/apt/helm/git/docker/cargo/conda/hf clients (needs: network + tools)
#
# Each script picks its own free ports and temp dirs, so these collide neither with each
# other nor with any running instance. Sequential: several drive shared client caches.
test-realclient:
	bash scripts/realclient-pypi.sh
	bash scripts/realclient-npm.sh
	bash scripts/realclient-apt.sh
	bash scripts/realclient-helm.sh
	bash scripts/realclient-git.sh
	bash scripts/realclient-docker.sh
	bash scripts/realclient-oci-remote.sh
	bash scripts/realclient-offline.sh
	bash scripts/realclient-cargo.sh
	bash scripts/realclient-conda.sh
	bash scripts/realclient-hf.sh
	bash scripts/realclient-multisource.sh
	bash scripts/realclient-maturity.sh

## test-e2e: the dimensions needing a real binary + real infra (needs: network + docker + clients)
test-e2e: test-conformance test-realclient

## test-ui: WebUI lint + typecheck + production build (needs: node + npm)
#
# `npm run lint` exists for one bug class neither tsc nor vite can see: React's rules of
# hooks. A hook called after an early return, or from a plain helper, type-checks and builds
# cleanly and then corrupts state at runtime. We shipped both variants; a human reading the
# file caught one, eslint caught the other. Reading files is not a gate.
#
# `npm run typecheck` is `tsc -b`, NOT `tsc --noEmit`. This matters: web/tsconfig.json is a
# solution-style config (`files: []` + `references`), against which `tsc --noEmit` checks
# NOTHING and always exits 0. It once "passed" while src/main.tsx imported a deleted file.
# Only the -b (build-mode) form follows the project references. Do not "simplify" this back.
test-ui:
	cd web && npm run lint && npm run typecheck && npm run build

## test-groundtruth: our COUNTERS vs reality, arbitrated by an interposer (needs: network + sqlite3 + jq)
#
# The dimension that stops Specula being the only witness to its own behaviour.
#
# Every other gate reads specula_cache_hits_total, specula_cache_bytes and
# specula_upstream_latency_seconds — the counters incremented by the very code paths they
# describe. A single bug therefore satisfies both the behaviour AND its own measurement, and
# every one of those tests agrees with it. We have shipped that three times: serve-stale dead
# across five handlers with a green suite; git bytes that existed only if a human clicked the
# WebUI; a ?digest= pin ignored on every cache hit. Humans found all three.
#
# This target believes none of it. Ground truth comes from three places that share no code
# with the counters: a recording proxy interposed between Specula and the real CN mirror
# (the only honest answer to "did this request actually contact upstream?"), the filesystem
# under the CAS root, and the sqlite3 CLI reading the metadata DB directly.
#
# Emits results/groundtruth/agreement.json: one row per claim, {claim, specula_says,
# ground_truth_says, agree}. Read the artifact — nobody, agent or human, can summarise their
# way past a row that says agree:false.
#
# EXPECTED RED at 455f11f: `cache_bytes_visible_at_startup` and
# `single_flight_collapses_stampede` are real, reproduced defects, not gate bugs. See the
# `detail` field of each row.
test-groundtruth:
	bash scripts/groundtruth-gate.sh

## test-groundtruth-meta: prove the groundtruth gate actually catches lies (needs: network + go)
#
# A check that has never been observed to fail is not evidence.
#
# Injects four defects into a pristine `git archive HEAD` — a cache hit that secretly
# refetches upstream, serve-stale failing closed (the bug we really shipped), a fabricated
# cache_bytes, and a single-flight REPAIR as a positive control — builds each mutant, and
# requires the gate's verdict to flip against a control run of the same claims. A mutation
# that does not COMPILE proves nothing, so a build failure here is a hard error.
#
# Emits results/groundtruth/injections.json.
test-groundtruth-meta:
	bash scripts/groundtruth-inject.sh

## test-mutation: do our tests TEST, or do they merely EXECUTE? (needs: gremlins; ~6 min)
#
# The dimension that grades the TESTS rather than the code.
#
# coverage-gate.sh asks "was this line executed?". This asks the only question that
# matters: "if this line were WRONG, would any test notice?". Those differ, and we have
# shipped the gap between them at least four times — every time with the same shape, a test
# double that answered whatever the code asked instead of what the real dependency does, so
# the test could not fail no matter what production did. fakeMetaStore.Get keyed on
# ref.Digest, the exact OPPOSITE of production (3ccd5ad): a wrong digest pin looked like a
# clean cache miss, and that passed unit tests, the OCI conformance suite AND the coverage
# gate. fakeStatsCollector.AddOpaquePath was {}. fakeMetaStore.GetMutable always returned
# (nil, nil), which is how tier="" shipped beside 6 real pins. sumdb_test.go answered
# whatever URL the handler built, so any URL shape passed — including the broken one.
#
# A lying double means the tests never exercise production behaviour, so mutants in that
# code SURVIVE, in clusters. This gate enumerates them mechanically. That is the whole
# point: the hand-written "mutation proofs" in test-trust-oracle-mutations are chosen by the
# same agent that wrote the fix — it picks mutations it already knows its tests catch. That
# is self-certification. A tool cannot be cherry-picked. (The two are complements, not
# rivals: that gate proves the ORACLE catches lies; this one proves the TESTS do.)
#
# Tool: go-gremlins/gremlins v0.6.0 — not hand-rolled. Scoped to the trust-bearing packages
# (internal/verify, internal/cache, internal/metrics); full-repo is O(mutants x package test
# time) and far too slow to be useful. internal/artifact is deliberately absent: it is
# declarations only and yields ZERO mutants.
#
# REPORT-ONLY today (proposed threshold: 85% efficacy). The per-mutant timeout is derived
# from wall-clock and so is machine-dependent — and a too-tight budget does not redden this
# gate, it INFLATES it (a timed-out mutant leaves the efficacy denominator), so the failure
# mode of a flaky budget is a false GREEN. That is also why gremlins is driven with
# GOFLAGS=-count=1 here: run as shipped, it measures a CACHED coverage run, and printed
# "efficacy: 100.00%" on internal/verify while silently discarding 240 of 288 mutants
# (upstream #267). An UNEXPLAINED timeout therefore fails this gate even in report-only mode:
# that is an invalid measurement, not a low score. See the THRESHOLD note in the script.
#
# Emits results/mutation/survivors.tsv + summary.json: every survivor with file:line:col,
# the operator applied, and its source line. A headline score is summarisable; a survivor
# list is not. Read the artifact — each row is a concrete claim that production can be
# broken THAT way with no test noticing.
#
# Equivalent mutants are NOT excluded — they cost us points on purpose, and each is listed
# with a proof in docs/MUTATION-TESTING.md. A score propped up by hidden exclusions is the
# exact dishonesty this repo exists to fight.
test-mutation:
	bash scripts/mutation-gate.sh

## test-all: every dimension (needs: everything above — network, docker, node, clients)
test-all: test-unit test-integration test-postgres test-ui test-conformance test-realclient test-trust-oracle test-trust-oracle-signed

## cover: coverage report only, no gate (needs: nothing)
# `|| true` tolerates the covdata noise documented under test-unit; coverage.out stays valid.
cover:
	@CGO_ENABLED=1 go test -short -coverprofile=coverage.out ./... || true
	go tool cover -func=coverage.out | tail -1
	@bash scripts/coverage-gate.sh coverage.out || true
