#!/usr/bin/env bash
# Mutation-testing gate — do our tests actually TEST, or do they merely EXECUTE?
#
# The dimension this occupies, and why it is not redundant with the others:
#
#   coverage-gate.sh answers "was this line executed?". Mutation testing answers the only
#   question that matters: "if this line were WRONG, would any test notice?". Those are
#   different questions and this repo has shipped the gap between them at least four times.
#   Every instance had the same shape — a test double that answered whatever the code asked
#   instead of what the real dependency does, so the test could not fail no matter what
#   production did:
#
#     · fakeMetaStore.Get keyed on ref.Digest — the exact OPPOSITE of production, which
#       ignores digest. A wrong digest pin looked like a clean cache miss. The bug passed
#       unit tests, the OCI conformance suite AND the coverage gate (3ccd5ad).
#     · fakeStatsCollector.AddOpaquePath was {} and ByProtocol returned a preset map — the
#       admin suite structurally could not fail for the bug it existed to cover.
#     · fakeMetaStore.GetMutable always returned (nil, nil) — a TOFU pin could never be
#       observed, which is how tier="" shipped alongside 6 real pins.
#     · sumdb_test.go keyed its double on responses[r.URL.Path] — answering whatever path
#       the handler built, so ANY URL shape passed, including the broken one it enshrined
#       as expected (02186f7).
#
#   A lying double means the tests do not exercise production behaviour, so mutants in that
#   production code SURVIVE. This gate enumerates the mutations mechanically. That is the
#   whole point: our existing hand-written "mutation proofs" are chosen by the same agent
#   that wrote the fix — i.e. it picks mutations it already knows its tests catch. That is
#   self-certification. A tool cannot be cherry-picked.
#
# Tool: go-gremlins/gremlins v0.6.0 (Apache-2.0, active, go 1.25). Chosen over
# zimmski/go-mutesting (last commit 2021, dead) and gtramontina/ooze (requires embedding a
# runner in the test tree; no package-scoped CLI). See docs at https://gremlins.dev.
#
# Usage:
#   scripts/mutation-gate.sh                 # run the scoped gate, write results/mutation/
#   MUTATION_PKGS="./internal/cache" scripts/mutation-gate.sh
#   MUTATION_BLOCKING=1 scripts/mutation-gate.sh   # exit non-zero on threshold breach
#
# Report-only by default. See "THRESHOLD" below for the rationale.
set -uo pipefail
cd "$(dirname "$0")/.."

OUT_DIR="results/mutation"
BIN="${GREMLINS_BIN:-gremlins}"
GREMLINS_VERSION="v0.6.0"

# ─────────────────────────── why -count=1 is not optional ───────────────────────────
#
# THIS IS THE LOAD-BEARING LINE OF THIS SCRIPT. Do not remove it.
#
# gremlins derives its per-mutant test timeout from the wall-clock of ONE coverage run:
#   internal/coverage/coverage.go executeCoverage() -> `go test -cover -coverprofile ...`
#   internal/engine/executor.go   testExecutionTime = elapsed * coefficient
# That `go test` carries NO -count=1, so Go's test cache replays it. On this repo
# internal/verify really takes 11.8s, but a cached replay returns in 0.66s — an 18x
# under-measurement. The resulting timeout (0.66 * 5 = 3.3s) is shorter than the mutant's
# own COMPILE step, so every mutant is recorded TIMED OUT.
#
# Measured here, on internal/verify (288 mutants):
#   without -count=1 :  48 KILLED,   0 LIVED, 240 TIMED OUT -> "Test efficacy: 100.00%"
#   with    -count=1 : 256 KILLED,  30 LIVED,   2 TIMED OUT -> 89.51%
#
# Read that first line again: gremlins printed a PERFECT 100% efficacy score while silently
# discarding 240 of 288 mutants. Efficacy is KILLED/(KILLED+LIVED), so TIMED OUT mutants
# leave the denominator entirely — a too-short timeout does not fail the gate, it FLATTERS
# it. That is precisely the hidden-exclusion dishonesty this repo exists to fight, arriving
# via the tool rather than via us. It is upstream bug go-gremlins/gremlins#267 ("Everything
# times out when running twice in a row"), open since 2026-01-04.
#
# GOFLAGS=-count=1 reaches BOTH the coverage run and every mutant run, so the measurement is
# honest and no mutant run can be served from cache either. This is the same "-count=1
# matters — a cached pass is not a pass" rule the rest of the test matrix already follows.
export GOFLAGS="${GOFLAGS:-} -count=1"

# ───────────────────────────────── scope ─────────────────────────────────
#
# Full-repo mutation testing is not useful here: each mutant costs one full compile+test of
# its package, so cost is O(mutants x package test time). We scope to the TRUST-BEARING
# code — the packages where a survivor is a statement about supply-chain safety rather than
# about plumbing:
#
#   internal/verify   the four-tier trust model itself (PRD §G2). A survivor here means the
#                     tier logic can be broken with no test noticing. Highest stakes in the
#                     tree: giving a checksum-only artifact tier="signed" is, per PRD §7.5,
#                     "the most serious error this codebase can make".
#   internal/cache    digest pinning, freshness, serve-stale, Range serving. Source of the
#                     3ccd5ad pin bug and the 45674be serve-stale bug — both shipped green.
#   internal/metrics  the tier counter. PRD §7.5 makes /metrics the operator-facing evidence
#                     for G2; a mislabelled series is a false claim about trust.
#
# internal/artifact is deliberately NOT scoped: it is 197 lines of type and interface
# declarations and gremlins finds ZERO mutants in it (verified by dry-run). Listing it would
# be cargo-cult — an empty target that always passes and implies coverage it cannot provide.
MUTATION_PKGS="${MUTATION_PKGS:-./internal/verify ./internal/cache ./internal/metrics}"

# ─────────────────────────────── timeout coefficient ───────────────────────────────
#
# timeout = (real coverage-run elapsed) * coefficient, per package. With -count=1 the
# elapsed is honest, so a modest coefficient suffices for the slow package (verify: 11.8s
# * 5 = 59s). But the FAST packages are the hazard: internal/metrics' suite runs in 0.036s,
# giving a 0.18s budget at coefficient 5 — far below the mutant's compile time, so all 18
# mutants time out and the score reads a fraudulent 100%.
#
# A single coefficient therefore cannot serve both. We set it per package so that every
# package gets a floor of roughly 30s of real budget, which comfortably exceeds compile+run
# for anything in scope while still catching a genuine infinite loop.
coefficient_for() {
  case "$1" in
    ./internal/verify)  echo 8  ;;   # 11.8s * 8  ~= 94s; at 5 (59s) two mutants still timed out
    ./internal/cache)   echo 60 ;;   # 0.008s is under the clock's resolution; be generous
    ./internal/metrics) echo 60 ;;   # 0.036s * 60 ~= 2.2s min, plus gremlins' own floor
    *)                  echo 30 ;;
  esac
}

# Local helper. NOT shared with coverage-gate.sh: that script defines its own in_list, and
# sourcing it here to borrow one 4-line function would drag in its whole gating side-effects.
mut_in_list() {
  local needle="$1"; shift
  local x
  for x in "$@"; do [[ "$x" == "$needle" ]] && return 0; done
  return 1
}

command -v "$BIN" >/dev/null 2>&1 || {
  cat >&2 <<EOF
mutation-gate: '$BIN' not found in PATH.

Install it (works from CN via goproxy.cn — this is why we pin a version rather than @latest):
  go install github.com/go-gremlins/gremlins/cmd/gremlins@${GREMLINS_VERSION}
Then ensure \$(go env GOPATH)/bin is in PATH, or set GREMLINS_BIN=/path/to/gremlins.
EOF
  exit 2
}

mkdir -p "$OUT_DIR"
echo "Mutation gate · tool=gremlins ${GREMLINS_VERSION} · GOFLAGS='${GOFLAGS}'"
echo "Scope: ${MUTATION_PKGS}"
echo "================================================================"

overall_start=$(date +%s)
raw_files=()
fail=0

for pkg in $MUTATION_PKGS; do
  name="$(basename "$pkg")"
  raw="$OUT_DIR/raw-${name}.json"
  log="$OUT_DIR/run-${name}.log"
  coef="$(coefficient_for "$pkg")"

  echo
  echo "── $pkg (timeout-coefficient=${coef}) ──"
  start=$(date +%s)
  "$BIN" unleash --timeout-coefficient "$coef" -o "$raw" "$pkg" >"$log" 2>&1
  rc=$?
  elapsed=$(( $(date +%s) - start ))

  if [[ ! -s "$raw" ]]; then
    echo "  ✗ gremlins produced no report for $pkg (exit $rc). Log tail:"
    tail -5 "$log" | sed 's/^/      /'
    fail=1
    continue
  fi
  grep -E "Killed:|Test efficacy:|Mutator coverage:" "$log" | sed 's/^/  /'
  echo "  wall: ${elapsed}s"
  raw_files+=("$raw")
done

[[ ${#raw_files[@]} -eq 0 ]] && { echo "mutation-gate: no reports produced" >&2; exit 1; }

overall_elapsed=$(( $(date +%s) - overall_start ))

# ───────────────────────── the artifact that actually matters ─────────────────────────
#
# A headline score is summarisable; a survivor list is not. Each survivor is a concrete,
# checkable statement: "production can be broken THIS WAY, at THIS line, and no test
# notices." We therefore emit every survivor with file:line:col + the mutation operator +
# the source line it sits on, so a reader can judge it at a glance without re-running the
# tool. Neither an agent nor a human should be able to summarise past this file.
#
# TIMED OUT is reported as its own status and NOT folded into anything. gremlins drops it
# from the efficacy denominator; if timeouts appear, the score is computed over a subset and
# must not be read as a result. See the -count=1 note above.
jq -s --arg elapsed "$overall_elapsed" --arg tool "gremlins ${GREMLINS_VERSION}" '
  {
    tool: $tool,
    wall_clock_seconds: ($elapsed | tonumber),
    packages: [ .[] | .go_module ] | unique,
    counts: (
      [ .[] | .files[]?.mutations[]?.status ]
      | group_by(.) | map({key: .[0], value: length}) | from_entries
    ),
    survivors: [
      .[] | .files[]? | .file_name as $f | .mutations[]?
      | select(.status == "LIVED" or .status == "NOT COVERED" or .status == "TIMED OUT")
      | {status, file: $f, line, column, mutation: .type}
    ] | sort_by(.file, .line, .column)
  }
  | .efficacy_percent = (
      (.counts.KILLED // 0) as $k | (.counts.LIVED // 0) as $l
      | if ($k + $l) == 0 then null else (100 * $k / ($k + $l) | .*100|round|./100) end
    )
' "${raw_files[@]}" > "$OUT_DIR/summary.json"

# Annotate each survivor with its actual source line — the JSON alone gives only an
# operator and a coordinate, which is not enough to judge a survivor without opening the
# file. (Upstream issue #292 asks for richer spans; until then we resolve it ourselves.)
jq -r '.survivors[] | "\(.status)\t\(.file)\t\(.line)\t\(.column)\t\(.mutation)"' "$OUT_DIR/summary.json" \
| while IFS=$'\t' read -r status file line col mut; do
    src=""
    for pkg in $MUTATION_PKGS; do
      p="${pkg#./}/$file"
      [[ -f "$p" ]] && { src="$(sed -n "${line}p" "$p" | sed 's/^[[:space:]]*//')"; break; }
    done
    printf '%s\t%s:%s:%s\t%s\t%s\n' "$status" "$file" "$line" "$col" "$mut" "$src"
  done > "$OUT_DIR/survivors.tsv"

echo
echo "================================================================"
# LIVED / NOT COVERED are true survivors: production can be broken that way and no test
# notices. TIMED OUT is listed in the same table on purpose — it is NOT a survivor (the
# mutant was caught, by hanging), but hiding it would conceal the one status that can
# silently shrink the efficacy denominator. It is adjudicated below, per site.
echo "Survivors (LIVED / NOT COVERED = production code that can be broken with no test noticing):"
echo
if [[ -s "$OUT_DIR/survivors.tsv" ]]; then
  printf '  %-12s %-28s %-24s %s\n' "STATUS" "SITE" "MUTATION" "SOURCE"
  while IFS=$'\t' read -r status site mut src; do
    printf '  %-12s %-28s %-24s %s\n' "$status" "$site" "$mut" "${src:0:70}"
  done < "$OUT_DIR/survivors.tsv"
else
  echo "  (none)"
fi

efficacy="$(jq -r '.efficacy_percent // "n/a"' "$OUT_DIR/summary.json")"
timeouts="$(jq -r '.counts["TIMED OUT"] // 0' "$OUT_DIR/summary.json")"

echo
echo "================================================================"
jq -r '.counts | to_entries | map("\(.key): \(.value)") | join("   ")' "$OUT_DIR/summary.json" | sed 's/^/  /'
echo "  test efficacy: ${efficacy}%   wall: ${overall_elapsed}s"
echo "  artifacts: $OUT_DIR/summary.json  $OUT_DIR/survivors.tsv"

# ─────────────────────────────── timeouts: two kinds ───────────────────────────────
#
# A timeout is not a neutral event — it silently removes a mutant from the efficacy
# denominator — but it is not automatically a gate bug either. There are two kinds, and
# conflating them would make this gate either a liar or permanently red:
#
#   GENUINE  the mutation really does hang the suite (e.g. a loop bound mutated so the loop
#            out-reads its channel and blocks forever). gremlins' own docs: "the mutation
#            actually made the tests fail, but not explicitly." The mutant WAS caught; it
#            just cannot be credited cleanly. Raising the budget does not make it go away.
#   ARTIFACT the budget was too short (see the -count=1 note above). The mutant was NOT
#            caught by anything; the clock simply ran out, and the score is inflated.
#
# The distinguishing EVIDENCE is that a genuine timeout persists when the budget rises,
# while an artifact disappears. So we do not guess by ratio — a ratio rule is exactly the
# kind of fudge this gate exists to refuse. We pin the sites verified genuine, each with a
# proof, and treat a timeout ANYWHERE ELSE as a measurement failure.
#
# This list has the same contract as the equivalent-mutant list in docs/MUTATION-TESTING.md:
# an entry without a proof beside it is a bug, not a waiver. A new site appearing here
# unexplained means someone silenced a real problem.
#
#   consensus.go:191  `for i := 0; i < total; i++ { r := <-resultCh; ... }`
#                     CONDITIONALS_BOUNDARY (`<` -> `<=`) and INCREMENT_DECREMENT (`i++` ->
#                     `i--`) both make the loop receive more values than were ever sent on
#                     resultCh, so it blocks forever on the channel. Deadlock by
#                     construction, not slowness. Verified identical at coefficient 5 and 8
#                     (and the coverage elapsed differed 7.4s vs 11.8s between those runs) —
#                     the timeout is budget-independent, which is the proof.
KNOWN_GENUINE_TIMEOUTS=(
  "consensus.go:191"
)

if [[ "$timeouts" -gt 0 ]]; then
  echo
  unexplained=0
  while IFS=$'\t' read -r status site mut src; do
    [[ "$status" == "TIMED OUT" ]] || continue
    # site is file:line:col — match on file:line, the coefficient cannot change a column.
    base="${site%:*}"
    if mut_in_list "$base" "${KNOWN_GENUINE_TIMEOUTS[@]}"; then
      echo "  · TIMED OUT $site ($mut) — known genuine hang (see KNOWN_GENUINE_TIMEOUTS)"
    else
      echo "  ! TIMED OUT $site ($mut) — NOT a known genuine hang."
      unexplained=$((unexplained+1))
    fi
  done < "$OUT_DIR/survivors.tsv"

  if [[ "$unexplained" -gt 0 ]]; then
    echo
    echo "  ! ${unexplained} unexplained timeout(s). Either the budget is too short — in which"
    echo "    case efficacy above is computed over a SUBSET and is NOT a valid score — or a new"
    echo "    genuine hang exists. Raise coefficient_for() for the package and re-run: if the"
    echo "    timeout persists it is genuine (add it above WITH A PROOF); if it vanishes, the"
    echo "    budget was the problem and every number printed before this was inflated."
    fail=1
  else
    echo
    echo "  ${timeouts} timeout(s), all known-genuine hangs: the mutants WERE caught (the suite"
    echo "  hung), but gremlins cannot credit them, so they sit outside the efficacy"
    echo "  denominator. The score below is therefore CONSERVATIVE, not inflated."
  fi
fi

# ───────────────────────────────── THRESHOLD ─────────────────────────────────
#
# Measured baseline at 5d5ec3a (see results/mutation/summary.json): efficacy is well above
# 80% across the scoped packages, and every survivor is triaged in docs/ rather than hidden.
#
# We propose 85% efficacy, REPORT-ONLY to start. Rationale, and why not blocking yet:
#
#   · The number is only meaningful once timeouts are 0, and the timeout budget is derived
#     from wall-clock — i.e. it is machine- and load-dependent. A gate that goes red because
#     CI was busy teaches people to ignore it. We need a few runs of evidence that the
#     coefficients hold on other machines before this can block.
#   · Some survivors are genuinely unkillable (equivalent mutants — see below). Blocking on
#     a score that includes them creates pressure to "fix" the score by excluding them,
#     which is the exact dishonesty we are trying to prevent. Report-only removes that
#     incentive while we build the list honestly.
#
# This mirrors the coverage gate's own GATED/WATCH precedent: prove the number is stable and
# fair before you let it block, and never let a gate's convenience shape what it reports.
#
# EQUIVALENT MUTANTS — we exclude NOTHING. Not one.
#
# Some survivors are semantically identical to the original and can never be killed by any
# test. The honest move is to name them, not to filter them out of the denominator: a score
# propped up by hidden exclusions is worse than a lower honest one. The known-equivalent
# survivors at this commit are listed individually, with a proof sketch each, in
# docs/MUTATION-TESTING.md §Equivalent mutants. They remain in summary.json, they remain in
# the table above, and they remain in the denominator — they cost us efficacy points on
# purpose. If that list ever grows without a proof beside each entry, distrust it.
THRESHOLD_EFFICACY="${THRESHOLD_EFFICACY:-85}"
BLOCKING="${MUTATION_BLOCKING:-0}"

if [[ "$efficacy" != "n/a" ]] && awk "BEGIN{exit !($efficacy < $THRESHOLD_EFFICACY)}"; then
  echo "  · efficacy ${efficacy}% is below the proposed ${THRESHOLD_EFFICACY}% threshold"
  [[ "$BLOCKING" == "1" ]] && fail=1
fi

if [[ "$BLOCKING" != "1" ]]; then
  echo
  # An UNEXPLAINED timeout still fails loudly even here: that is an invalid MEASUREMENT, not
  # a low score, and report-only mode is a statement about thresholds — never a licence to
  # report a number computed over a silently-shrunk denominator.
  if [[ "$fail" -ne 0 ]]; then
    echo "RESULT: FAIL (measurement invalid — see the ! lines above; not a threshold failure)"
    exit 1
  fi
  echo "RESULT: REPORTED (report-only mode; set MUTATION_BLOCKING=1 to enforce)"
  exit 0
fi

if [[ "$fail" -ne 0 ]]; then
  echo "RESULT: FAIL"
  exit 1
fi
echo "RESULT: PASS"
