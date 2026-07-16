# Specula — build & test orchestration
BINARY := specula
PKG := ./cmd/specula

# Ship builds are pure-Go/static: sqlite uses the modernc pure-Go driver, so CGO is off by
# default for reproducible static/cross builds. test-unit overrides this to 1 — see there.
export CGO_ENABLED := 0

.PHONY: all ui build build-go run clean vet fmt cover \
        test test-unit test-integration test-postgres test-conformance \
        test-realclient test-e2e test-ui test-all

all: build

# ───────────────────────────── build ─────────────────────────────

## ui: build the WebUI into web/dist (needs: node + npm; network on first install)
ui:
	cd web && npm install && npm run build

## build: WebUI + the single static binary with the WebUI embedded (needs: node + npm)
build: ui
	go build -o bin/$(BINARY) $(PKG)

## build-go: only the Go binary; assumes web/dist already exists (needs: nothing)
build-go:
	go build -o bin/$(BINARY) $(PKG)

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
#   test-realclient   real pip / npm / apt-get / helm / git / docker clients.  needs: network + those tools
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

## test-realclient: real pip/npm/apt-get/helm/git/docker clients (needs: network + those tools; docker daemon)
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

## test-e2e: the dimensions needing a real binary + real infra (needs: network + docker + clients)
test-e2e: test-conformance test-realclient

## test-ui: WebUI typecheck + production build (needs: node + npm)
#
# `npm run typecheck` is `tsc -b`, NOT `tsc --noEmit`. This matters: web/tsconfig.json is a
# solution-style config (`files: []` + `references`), against which `tsc --noEmit` checks
# NOTHING and always exits 0. It once "passed" while src/main.tsx imported a deleted file.
# Only the -b (build-mode) form follows the project references. Do not "simplify" this back.
test-ui:
	cd web && npm run typecheck && npm run build

## test-all: every dimension (needs: everything above — network, docker, node, clients)
test-all: test-unit test-integration test-postgres test-ui test-conformance test-realclient

## cover: coverage report only, no gate (needs: nothing)
# `|| true` tolerates the covdata noise documented under test-unit; coverage.out stays valid.
cover:
	@CGO_ENABLED=1 go test -short -coverprofile=coverage.out ./... || true
	go tool cover -func=coverage.out | tail -1
	@bash scripts/coverage-gate.sh coverage.out || true
