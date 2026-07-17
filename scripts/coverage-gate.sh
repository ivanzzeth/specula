#!/usr/bin/env bash
# Per-package statement-coverage gate.
#
# Usage:
#   scripts/coverage-gate.sh [coverage.out]   # defaults to ./coverage.out
#   THRESHOLD=85 scripts/coverage-gate.sh     # override the default threshold (80)
#   GATE_PKGS="internal/acl internal/org" scripts/coverage-gate.sh   # blocking-mode whitelist
#
# This script CONSUMES an existing profile; it never runs tests. That keeps CI free to
# cache/reuse the profile produced by `make test-unit`.
#
# Classification:
#   EXCLUDE — generated / wiring-only / declaration-only packages. Each entry carries a
#             written reason below. Not gated, not reported.
#   WATCH   — packages whose uncovered paths fundamentally require real infrastructure
#             (live database, real subprocess). The real number is printed; never blocks.
#   GATED   — everything else. Must be >= THRESHOLD or this script exits non-zero.
#
# Anti-gaming property (an intentional divergence from the ai-sandbox original this is
# ported from): the package list comes from `go list ./...`, NOT from the profile's own
# keys. A profile-driven loop can only ever gate packages that appear in the profile, so a
# package with ZERO test files is invisible to it and silently "passes". Here such a
# package is reported as MISSING and FAILS the gate. Specula really has packages in that
# state (internal/handler/apt, internal/handler/helm, internal/registryauthz), so this is
# not a hypothetical.
set -uo pipefail
cd "$(dirname "$0")/.."

PROFILE="${1:-coverage.out}"
THRESHOLD="${THRESHOLD:-80}"
MODULE="github.com/ivanzzeth/specula"

# EXCLUDE — not gated. Reason required for every entry.
#   cmd/specula          → main: flag parsing + dependency wiring only; the logic it wires
#                          lives in internal/* and is gated there.
#   web                  → go:embed of web/dist plus a static file handler; nothing to test.
#   internal/store/blob  → a single interface declaration (29 lines, 0 funcs).
#   internal/store/meta  → interface declaration + trivial constructors (0 statements
#                          recorded by the compiler).
#   internal/migrate     → one 19-line func that delegates to the driver-specific migrators
#                          in internal/store/{sqlite,postgres}, which are gated/watched.
#   test/groundtruth/interposer
#                        → main: flag parsing + listener wiring only, for the same reason
#                          cmd/specula is excluded. The arbitration logic it wires lives in
#                          test/groundtruth/interposer/proxy, which is GATED at the normal
#                          threshold — deliberately, because that package decides whether
#                          Specula's counters are lying, and an untested arbiter would just
#                          relocate the problem it exists to solve.
EXCLUDE_PKGS=(
  "cmd/specula"
  "web"
  "internal/store/blob"
  "internal/store/meta"
  "internal/migrate"
  "test/groundtruth/interposer"
)

# WATCH — reported, never blocking. Deliberately kept SHORT. A package earns a WATCH slot
# only if its uncovered paths cannot be exercised without real infrastructure. "It is
# covered by e2e" is NOT sufficient on its own — most handler packages are covered by e2e
# and are still gated below.
#   internal/store/postgres → every non-trivial path needs a live PostgreSQL. The pure-logic
#                             parts (hashKey, DSN parsing) ARE unit-tested; the rest is
#                             gated behind SPECULA_TEST_POSTGRES_DSN and runs in
#                             `make test-postgres`. Unit-only number is ~2% by construction.
#   internal/handler/git    → clone acceleration shells out to the real `git` binary against
#                             a real upstream and mirrors packfiles to disk. Exercised by
#                             test/e2e/git_e2e_test.go (61.5% with e2e attributed) and
#                             scripts/realclient-git.sh against real git.
WATCH_PKGS=(
  "internal/store/postgres"
  "internal/handler/git"
)

# GATE_PKGS (optional): space-separated whitelist. When set, ONLY these packages are
# judged (blocking mode) and everything else is ignored — no gate, no watch. Intended for
# CI: run once with GATE_PKGS for a hard red/green on the security/tenancy core, and once
# without it (`|| true`) for the full report-only table.
GATE_ONLY=()
if [[ -n "${GATE_PKGS:-}" ]]; then
  read -r -a GATE_ONLY <<< "$GATE_PKGS"
fi
CORE_MODE=0
[[ ${#GATE_ONLY[@]} -gt 0 ]] && CORE_MODE=1

if [[ ! -f "$PROFILE" ]]; then
  echo "coverage-gate: profile not found: $PROFILE (run 'make test-unit' or 'make cover' first)" >&2
  exit 2
fi

in_list() {
  local needle="$1"; shift
  local x
  for x in "$@"; do [[ "$x" == "$needle" ]] && return 0; done
  return 1
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Per-package coverage straight from the profile.
#
# We compute it ourselves rather than parsing `go tool cover -func`, because -func emits
# per-FUNCTION percentages and averaging those unweighted misrepresents a package (a
# one-line helper would count as much as a 200-statement handler). Statement-weighted is
# the same denominator `go test` reports.
#
# Profile line format: <file>:<startLine>.<col>,<endLine>.<col> <numStmt> <count>
awk -v module="$MODULE" '
  NR == 1 { next }   # skip the "mode:" header
  {
    split($1, a, ":")
    file = a[1]
    sub("^" module "/", "", file)
    n = split(file, parts, "/")
    pkg = parts[1]
    for (i = 2; i < n; i++) pkg = pkg "/" parts[i]
    total[pkg] += $2
    if ($3 > 0) covered[pkg] += $2
  }
  END {
    for (p in total) {
      pct = (total[p] > 0) ? (100.0 * covered[p] / total[p]) : 0
      printf "%s %.1f\n", p, pct
    }
  }
' "$PROFILE" | sort > "$TMP/cov.txt"

# Authoritative package list from the toolchain, so zero-test packages cannot hide.
#
# node_modules is filtered out, and this is NOT an exclusion in the EXCLUDE_PKGS sense —
# it is a correction to the DERIVATION of the list. `go list ./...` does not skip
# node_modules, so once anyone runs `make ui` (or `make build`, which depends on it) an
# npm install drops third-party Go source into web/node_modules — today
# web/node_modules/flatted/golang/pkg/flatted, a Go port shipped inside a JS package. That
# directory is gitignored and untracked: it is not Specula source, no Specula test will
# ever cover it, and its presence depends on whether npm has run on the machine. Gating it
# makes the gate fail for a non-problem and, worse, makes the result depend on build order.
#
# The anti-gaming property above is preserved in full: it exists so that a SPECULA package
# with zero test files cannot hide from the gate. Vendored JS dependencies were never in
# that set. Every path under the module that we actually author is still enumerated.
go list ./... 2>/dev/null \
  | grep -v '/node_modules/' \
  | sed "s|^$MODULE/||; s|^$MODULE\$|.|" | sort > "$TMP/pkgs.txt"

pct_of() { awk -v p="$1" '$1==p{print $2; found=1} END{if(!found) print "MISSING"}' "$TMP/cov.txt"; }

if [[ "$CORE_MODE" -eq 1 ]]; then
  echo "Coverage gate · core blocking mode (threshold ${THRESHOLD}%, judging GATE_PKGS only)"
else
  echo "Coverage gate (threshold ${THRESHOLD}%)"
fi
echo "================================================================"

fail=0
gated_n=0
watch_n=0

echo "[GATED] (must be >= ${THRESHOLD}%)"
while read -r pkg; do
  [[ -z "$pkg" ]] && continue
  # test/e2e is the test code itself, not a subject of the gate.
  [[ "$pkg" == test/e2e ]] && continue
  if [[ "$CORE_MODE" -eq 1 ]]; then
    in_list "$pkg" "${GATE_ONLY[@]}" || continue
  else
    in_list "$pkg" "${EXCLUDE_PKGS[@]}" && continue
    in_list "$pkg" "${WATCH_PKGS[@]}" && continue
  fi
  gated_n=$((gated_n+1))
  pct="$(pct_of "$pkg")"
  if [[ "$pct" == "MISSING" ]]; then
    # Not in the profile => the package has no test files at all. Never silently pass this.
    printf "  ✗ %-40s %8s  (no unit tests — package absent from profile)\n" "$pkg" "MISSING"
    fail=1
  elif awk "BEGIN{exit !($pct < $THRESHOLD)}"; then
    printf "  ✗ %-40s %7.1f%%\n" "$pkg" "$pct"
    fail=1
  else
    printf "  ✓ %-40s %7.1f%%\n" "$pkg" "$pct"
  fi
done < "$TMP/pkgs.txt"

if [[ "$CORE_MODE" -eq 0 ]]; then
  echo
  echo "[WATCH] (reported only, never blocks)"
  for pkg in "${WATCH_PKGS[@]}"; do
    watch_n=$((watch_n+1))
    pct="$(pct_of "$pkg")"
    if [[ "$pct" == "MISSING" ]]; then
      printf "  · %-40s %8s  (watch)\n" "$pkg" "MISSING"
    elif awk "BEGIN{exit !($pct < $THRESHOLD)}"; then
      printf "  · %-40s %7.1f%%  (watch)\n" "$pkg" "$pct"
    else
      printf "  · %-40s %7.1f%%  (ok)\n" "$pkg" "$pct"
    fi
  done

  # Guard the EXCLUDE/WATCH lists against rot: an entry naming a package that no longer
  # exists means the list is stale and may be silently un-gating nothing (or hiding a
  # rename). Fail loudly rather than drift.
  for want in "${EXCLUDE_PKGS[@]}" "${WATCH_PKGS[@]}"; do
    if ! awk -v w="$want" '$1==w{f=1} END{exit !f}' "$TMP/pkgs.txt"; then
      printf "  ! %-40s  (EXCLUDE/WATCH names a package that does not exist — stale list?)\n" "$want"
      fail=1
    fi
  done
fi

# Core mode: verify every GATE_PKGS entry actually exists, so a typo cannot silently
# reduce the blocking set to nothing.
if [[ "$CORE_MODE" -eq 1 ]]; then
  for want in "${GATE_ONLY[@]}"; do
    if ! awk -v w="$want" '$1==w{f=1} END{exit !f}' "$TMP/pkgs.txt"; then
      printf "  ! %-40s  (GATE_PKGS names a package that does not exist — typo?)\n" "$want"
      fail=1
    fi
  done
fi

echo "================================================================"
if [[ "$CORE_MODE" -eq 1 ]]; then
  echo "${gated_n} core gated package(s), threshold ${THRESHOLD}%"
else
  echo "${gated_n} gated, ${watch_n} watched, threshold ${THRESHOLD}%"
fi
if [[ "$fail" -ne 0 ]]; then
  echo "RESULT: FAIL — packages marked ✗ / ! above are below threshold, untested, or stale."
  exit 1
fi
echo "RESULT: PASS"
