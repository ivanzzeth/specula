#!/usr/bin/env bash
#
# groundtruth-inject.sh — the meta-gate.
#
# ─────────────────────────────────────────────────────────────────────────────
# A check that has never been observed to fail is not evidence.
#
# groundtruth-gate.sh asserts that Specula's counters match reality. That claim
# is worthless until we have watched each check FAIL on a Specula that is lying.
# So this harness lies to it on purpose: it exports a pristine HEAD, applies one
# surgical mutation, builds a mutant binary to its own temp path, and re-runs the
# affected claims against it. The gate must go red. If it does not, the check is
# decoration and should be deleted rather than trusted.
#
# Two rules learned the hard way:
#
#   * A mutation that fails to COMPILE proves nothing. Every patch here is
#     verified to build before it is graded, and a build failure is a hard error
#     rather than a skipped injection.
#   * Every injection is paired with a CONTROL run of the same claims on the
#     unmutated tree. Without the control, "the gate is red" could just mean
#     "the gate is always red", which is the same failure of evidence in a
#     different costume.
#
# The baseline is `git archive HEAD`, not the working tree: injections must
# differ from their control by the mutation and nothing else, and other agents
# are editing this tree concurrently.
#
# Usage:  bash scripts/groundtruth-inject.sh [injection-name ...]
# Needs:  network, go, sqlite3, jq, curl, ss. Slow (each injection is a full
#         gate run against real CN mirrors).
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d /tmp/specula-inject.XXXXXX)"
OUT="${OUT:-${REPO_ROOT}/results/groundtruth/injections.json}"
RESULTS="${WORK}/results.jsonl"
: > "$RESULTS"

log() { printf '\n\033[1m══ %s\033[0m\n' "$*"; }

# ── the mutations ────────────────────────────────────────────────────────────
#
# Each is a python3 patch over the exported HEAD tree. They are written as exact
# string replacements that fail loudly if the anchor is not found, so a refactor
# upstream breaks the injection visibly instead of silently turning it into a
# no-op that "passes".

patch_hit_refetches() {
  python3 - "$1" <<'PY'
import sys, pathlib
# LIE: a cache hit that secretly refetches from upstream anyway.
# The hit counter still says "hit" — and it is not even wrong by its own
# definition, since the body IS served from cache. Only something watching the
# wire can tell that the round trip (and the CN egress bill) happened anyway.
p = pathlib.Path(sys.argv[1]) / "internal/handler/gomod/endpoints.go"
s = p.read_text()
old = """	if entry != nil {
		// CAS hit: the body comes from cache, no upstream body transfer.
		metrics.MarkHit(ctx)
		h.serveFromCache(w, r, ref, entry, ct)
		return
	}"""
new = """	if entry != nil {
		// CAS hit: the body comes from cache, no upstream body transfer.
		metrics.MarkHit(ctx)
		if h.upstreamClt != nil && len(h.upstreams) > 0 {
			if body, _, ferr := h.upstreamClt.Fetch(ctx, ref, h.upstreams); ferr == nil {
				_ = body.Close()
			}
		}
		h.serveFromCache(w, r, ref, entry, ct)
		return
	}"""
assert old in s, "anchor not found in gomod/endpoints.go serveImmutable"
p.write_text(s.replace(old, new, 1))
print("patched: gomod serveImmutable refetches upstream on every CAS hit")
PY
}

patch_stale_fail_closed() {
  python3 - "$1" <<'PY'
import sys, pathlib
# LIE: serve-stale fails closed — the exact bug this repo actually shipped, dead
# across five handlers while the whole suite stayed green. LookupStale returning
# nil makes every handler's serve-stale branch unreachable at once.
p = pathlib.Path(sys.argv[1]) / "internal/cache/cache.go"
s = p.read_text()
old = """func (m *manager) LookupStale(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return m.lookupMutable(ctx, ref, true)
}"""
new = """func (m *manager) LookupStale(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, nil
}"""
assert old in s, "anchor not found in cache.go LookupStale"
p.write_text(s.replace(old, new, 1))
print("patched: cache.LookupStale always returns nil (serve-stale fails closed)")
PY
}

patch_fabricated_bytes() {
  python3 - "$1" <<'PY'
import sys, pathlib
# LIE: specula_cache_bytes reports a fabricated number. The gauge stays
# internally consistent and every stats unit test that reads the gauge back
# still agrees with it; only the filesystem and the DB know better.
p = pathlib.Path(sys.argv[1]) / "internal/stats/stats.go"
s = p.read_text()
old = "c.cacheBytes.WithLabelValues(proto).Set(float64(s.Bytes))"
new = "c.cacheBytes.WithLabelValues(proto).Set(float64(s.Bytes) + 4096)"
n = s.count(old)
assert n >= 1, "anchor not found in stats.go"
p.write_text(s.replace(old, new))
print(f"patched: specula_cache_bytes inflated by 4096 at {n} site(s)")
PY
}

patch_singleflight_repair() {
  python3 - "$1" <<'PY'
import sys, pathlib
# NOT a lie — a REPAIR, and it is here as a positive control.
#
# single_flight_collapses_stampede is already RED at HEAD, so mutating
# single-flight to break it proves nothing: you cannot break what is already
# broken. But a claim that is red no matter what is just as worthless as one
# that is green no matter what. So we do the opposite: wrap the gomod cold-fetch
# in the coalescer that ARCHITECTURE §7 says should already be there, and prove
# the claim goes GREEN. That establishes the check discriminates 1 from N and is
# measuring single-flight rather than merely always failing.
p = pathlib.Path(sys.argv[1]) / "internal/handler/gomod/endpoints.go"
s = p.read_text()

old_imp = '"github.com/ivanzzeth/specula/internal/artifact"'
assert old_imp in s, "import anchor not found"
s = s.replace(old_imp, old_imp + '\n\t"github.com/ivanzzeth/specula/internal/coalesce"', 1)

old = "	entry, err = h.fetchAndStoreImmutable(ctx, ref)"
assert old in s, "fetchAndStoreImmutable call anchor not found"
new = """	sfKey := ref.Protocol + "|" + ref.Name + "|" + ref.Version
	sfVal, sfErr, _ := injectedFetchSF.Do(ctx, sfKey, func() (any, error) {
		return h.fetchAndStoreImmutable(ctx, ref)
	})
	entry, err = nil, sfErr
	if sfErr == nil {
		if e, ok := sfVal.(*artifact.CacheEntry); ok {
			entry = e
		}
	}"""
s = s.replace(old, new, 1)
s += "\n\n// injectedFetchSF collapses concurrent cold fetches for one artifact.\nvar injectedFetchSF = coalesce.NewLocalCoalescer()\n"
p.write_text(s)
print("patched: gomod cold fetch wrapped in a single-flight coalescer (REPAIR)")
PY
}

# injection metadata: name → "patch_fn|claims to run|claim that must flip|expected"
declare -A INJ_PATCH=(
  [hit_refetches]=patch_hit_refetches
  [stale_fail_closed]=patch_stale_fail_closed
  [fabricated_bytes]=patch_fabricated_bytes
  [singleflight_repair]=patch_singleflight_repair
)
declare -A INJ_CLAIMS=(
  [hit_refetches]="cold_miss_contacts_upstream warm_immutable_hit_zero_upstream"
  [stale_fail_closed]="serve_stale_on_upstream_failure"
  [fabricated_bytes]="cold_miss_contacts_upstream cache_bytes_gauge_matches_db"
  [singleflight_repair]="single_flight_collapses_stampede"
)
declare -A INJ_TARGET=(
  [hit_refetches]="warm_immutable_hit_zero_upstream"
  [stale_fail_closed]="serve_stale_on_upstream_failure"
  [fabricated_bytes]="cache_bytes_gauge_matches_db"
  [singleflight_repair]="single_flight_collapses_stampede"
)
# What the target claim must read AFTER the mutation. Lies must turn a green
# check red; the repair must turn the already-red check green.
declare -A INJ_EXPECT=(
  [hit_refetches]=false
  [stale_fail_closed]=false
  [fabricated_bytes]=false
  [singleflight_repair]=true
)
declare -A INJ_KIND=(
  [hit_refetches]=lie
  [stale_fail_closed]=lie
  [fabricated_bytes]=lie
  [singleflight_repair]=repair
)

# export_head <dir> — pristine HEAD, no working-tree contamination.
#
# web/dist is deliberately seeded from the working tree afterwards. It is a Vite
# BUILD ARTIFACT and is not committed (only web/dist/.gitkeep is), so an exported
# HEAD compiles happily and then panics at startup inside the WebUI embed:
#   panic: webui: read dist/index.html: open index.html: file does not exist
# Copying it in is not contamination — control and mutant receive byte-identical
# dist trees, so the ONLY difference between the two builds remains the mutation.
# If web/dist is missing entirely, run `make ui` first.
export_head() {
  mkdir -p "$1"
  git -C "$REPO_ROOT" archive HEAD | tar -x -C "$1"
  if [[ -f "${REPO_ROOT}/web/dist/index.html" ]]; then
    cp -a "${REPO_ROOT}/web/dist/." "$1/web/dist/"
  else
    echo "FATAL: web/dist/index.html missing — run 'make ui' first (the embedded"
    echo "       WebUI is not committed, and the binary panics at startup without it)."
    exit 1
  fi
}

# run_claims <binary> <claims> <outfile> → runs the gate, tolerating red.
run_claims() {
  GT_SPECULA_BIN="$1" CLAIMS="$2" OUT="$3" ALLOW_FAIL=1 \
    bash "${REPO_ROOT}/scripts/groundtruth-gate.sh" > "${3}.log" 2>&1
  [[ -s "$3" ]] || { echo "FATAL: gate produced no table; see ${3}.log"; tail -30 "${3}.log"; return 1; }
}

verdict() { jq -r --arg c "$2" '.rows[] | select(.claim==$c) | .agree' "$1"; }
detail()  { jq -r --arg c "$2" '.rows[] | select(.claim==$c) | .ground_truth_says' "$1"; }

# INJ_SOURCE_ONLY=1 defines the patch functions and stops, so a caller can verify
# every mutation still APPLIES and COMPILES without spending a network run on it.
# The anchors are exact source strings: when someone refactors the code they
# target, the patch must break loudly rather than degrade into a no-op that
# "passes".
if [[ "${INJ_SOURCE_ONLY:-0}" == "1" ]]; then
  return 0 2>/dev/null || exit 0
fi

WANT=("$@")
[[ ${#WANT[@]} -eq 0 ]] && WANT=(hit_refetches stale_fail_closed fabricated_bytes singleflight_repair)

for name in "${WANT[@]}"; do
  log "INJECTION: ${name} (${INJ_KIND[$name]})"
  claims="${INJ_CLAIMS[$name]}"
  target="${INJ_TARGET[$name]}"
  expect="${INJ_EXPECT[$name]}"

  # ── control: pristine HEAD ──
  ctl_dir="${WORK}/${name}-control"
  export_head "$ctl_dir"
  ctl_bin="${WORK}/${name}-control-bin"
  (cd "$ctl_dir" && go build -o "$ctl_bin" ./cmd/specula) || {
    echo "FATAL: control build failed"; exit 1; }
  echo "  control: running claims [${claims}] on pristine HEAD"
  run_claims "$ctl_bin" "$claims" "${WORK}/${name}-control.json" || exit 1
  ctl_verdict="$(verdict "${WORK}/${name}-control.json" "$target")"
  echo "  control: ${target} agree=${ctl_verdict}"

  # ── mutant ──
  mut_dir="${WORK}/${name}-mutant"
  export_head "$mut_dir"
  "${INJ_PATCH[$name]}" "$mut_dir" || { echo "FATAL: patch failed"; exit 1; }
  mut_bin="${WORK}/${name}-mutant-bin"
  # A mutation that does not compile proves nothing. Hard-fail here.
  if ! (cd "$mut_dir" && go build -o "$mut_bin" ./cmd/specula 2>"${WORK}/${name}-build.err"); then
    echo "FATAL: mutant did not COMPILE — the injection proves nothing:"
    cat "${WORK}/${name}-build.err"
    exit 1
  fi
  echo "  mutant: compiles ✓"
  echo "  mutant: running claims [${claims}]"
  run_claims "$mut_bin" "$claims" "${WORK}/${name}-mutant.json" || exit 1
  mut_verdict="$(verdict "${WORK}/${name}-mutant.json" "$target")"
  mut_detail="$(detail "${WORK}/${name}-mutant.json" "$target")"
  echo "  mutant: ${target} agree=${mut_verdict}"

  caught=false
  [[ "$mut_verdict" == "$expect" && "$ctl_verdict" != "$mut_verdict" ]] && caught=true

  if $caught; then
    printf '  \033[32m✓ GATE CAUGHT IT\033[0m  %s: control=%s → mutant=%s\n' "$target" "$ctl_verdict" "$mut_verdict"
  else
    printf '  \033[31m✗ GATE BLIND\033[0m  %s: control=%s → mutant=%s (expected %s)\n' \
      "$target" "$ctl_verdict" "$mut_verdict" "$expect"
  fi

  jq -nc --arg n "$name" --arg k "${INJ_KIND[$name]}" --arg t "$target" \
         --arg c "$ctl_verdict" --arg m "$mut_verdict" --arg d "$mut_detail" \
         --argjson caught "$caught" \
    '{injection:$n, kind:$k, target_claim:$t, control_agree:$c, mutant_agree:$m,
      gate_caught_it:$caught, mutant_ground_truth:$d}' >> "$RESULTS"
done

mkdir -p "$(dirname "$OUT")"
total="$(wc -l < "$RESULTS")"
caught="$(jq -s '[.[] | select(.gate_caught_it)] | length' "$RESULTS")"
jq -s --arg ts "$(date -Iseconds)" --argjson t "$total" --argjson c "$caught" \
  '{generated_at:$ts, injections_total:$t, injections_caught:$c, results:.}' \
  "$RESULTS" > "$OUT"

log "Meta-gate summary"
jq -r '["INJECTION","KIND","TARGET_CLAIM","CONTROL","MUTANT","CAUGHT"],
       ["---------","----","------------","-------","------","------"],
       (.results[] | [.injection, .kind, .target_claim, .control_agree, .mutant_agree,
        (.gate_caught_it|tostring)]) | @tsv' "$OUT" | column -t -s$'\t'
printf '\n   artifact: %s\n   %s/%s injections caught\n\n' "$OUT" "$caught" "$total"

[[ "$caught" == "$total" ]] || { echo "✗ meta-gate: the gate is BLIND to $(( total - caught )) injected defect(s)"; exit 1; }
echo "✓ meta-gate: every injected defect was caught"
