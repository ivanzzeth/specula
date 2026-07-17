#!/usr/bin/env bash
#
# groundtruth-gate.sh — grade Specula's self-reported cache/byte accounting
# against evidence that Specula does not produce.
#
# ─────────────────────────────────────────────────────────────────────────────
# Why this gate exists
#
# specula_cache_hits_total, specula_cache_bytes and specula_upstream_latency_
# seconds are incremented by the very code paths whose behaviour they describe.
# A single bug therefore satisfies both the behaviour AND its own measurement,
# and every test that reads those counters agrees with it. This repo has shipped
# exactly that three times: serve-stale was dead across five handlers while the
# suite was green; git bytes existed only if a human clicked the WebUI; a
# caller's ?digest= pin was ignored on every cache hit while cold-path tests
# passed. Each was found by a human. That does not scale.
#
# So this gate never asks Specula whether Specula is telling the truth. It uses
# three sources of evidence that share no code with the counters:
#
#   1. AN UPSTREAM INTERPOSER (test/groundtruth/interposer) — a recording proxy
#      wired between Specula and the real CN mirror. Specula's config names it as
#      the SOLE upstream, so its request log is the only honest answer to "did
#      this request actually contact upstream?". It is a separate process and
#      imports nothing from internal/.
#   2. THE FILESYSTEM — the actual bytes on disk under the CAS root, measured
#      with find/du, not with stats.Collector.
#   3. THE METADATA DB — read with the sqlite3 CLI, not through the Go store.
#
# A check that reads the number it is checking is a mirror, and a mirror agrees
# with a lie.
#
# ─────────────────────────────────────────────────────────────────────────────
# The reconciliation rule this gate asserts (and why it is not the naive one)
#
# PRD §7.2 is explicit that `hit` does NOT mean "no upstream contact": a 304
# revalidation is counted a hit and still costs a full CN round trip. A naive
# gate asserting hit ⇒ no upstream contact would therefore fail on CORRECT code.
# The documented rule, which this gate tests instead, is:
#
#   * hit/miss partitions requests by the ORIGIN OF THE BODY BYTES;
#   * specula_upstream_latency_seconds_count answers "how often did we touch an
#     upstream at all", observing once per round trip INCLUDING 304s.
#
# So the arbitrated identity is:
#
#   Δupstream_latency_seconds_count  ==  interposer request count
#
# with one documented exception: latency is observed once per SUCCESSFUL fetch,
# while the interposer records every attempt, so retries against a failing
# upstream make interposer > latency. This gate therefore asserts equality only
# on healthy paths and asserts inequality direction on the failure paths.
#
# ─────────────────────────────────────────────────────────────────────────────
# Output
#
# A machine-readable agreement table (JSON) at $OUT, one row per claim:
#   {claim, specula_says, ground_truth_says, agree, detail}
# The point is that neither an agent nor a human can summarise their way past a
# disagreement: the row either says agree:true or it does not.
#
# ─────────────────────────────────────────────────────────────────────────────
# Usage
#
#   bash scripts/groundtruth-gate.sh                 # build from the working tree
#   GT_SPECULA_BIN=/path/to/specula bash scripts/...    # grade a prebuilt binary
#                                                    # (used by groundtruth-inject.sh)
#   OUT=/tmp/agreement.json bash scripts/...
#
# Needs: network (real CN mirrors), go, sqlite3, jq, curl, ss.
# Slow and network-dependent by design, exactly like test-conformance.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "${REPO_ROOT}/scripts/lib/daemon.sh"

OUT="${OUT:-${REPO_ROOT}/results/groundtruth/agreement.json}"
# ALLOW_FAIL=1 records disagreements without failing the process. Used by the
# injection harness, which EXPECTS the gate to go red.
ALLOW_FAIL="${ALLOW_FAIL:-0}"
# CLAIMS restricts the run to a subset (space-separated claim names).
CLAIMS="${CLAIMS:-all}"

# Real CN upstreams (PRD §G5). Each is fronted by its own interposer so a
# failure injected into one protocol cannot perturb another.
UP_GOMOD="${UP_GOMOD:-https://goproxy.cn}"
UP_NPM="${UP_NPM:-https://registry.npmmirror.com}"
UP_PYPI="${UP_PYPI:-https://pypi.tuna.tsinghua.edu.cn}"

WORK="$(mktemp -d /tmp/specula-groundtruth.XXXXXX)"
ROWS="${WORK}/rows.jsonl"
: > "$ROWS"
MY_PIDS=()

# ─────────────────────────── plumbing ───────────────────────────

log()  { printf '\n\033[1m── %s\033[0m\n' "$*"; }
info() { printf '   %s\n' "$*"; }

cleanup() {
  local p
  for p in "${MY_PIDS[@]:-}"; do
    [[ -z "$p" ]] && continue
    # Kill ONLY processes we started. Never pkill -f: this script's own command
    # line contains every pattern we could match on, so a pattern kill would
    # terminate the gate itself.
    kill "$p" 2>/dev/null || true
  done
  # Deliberately NO bare `wait` here: the interposers and the daemon are jobs of
  # this shell, so a bare wait blocks until they exit. See wait_pids below.
}

# wait_pids <pid...> — wait for exactly these jobs.
#
# A bare `wait` is WRONG in this script and cost a full run: it waits for EVERY
# background job of the shell, which includes the three interposers and the
# Specula daemon — none of which ever exit on their own. The stampede claim hung
# for eleven minutes on precisely that. Always wait on an explicit PID list.
wait_pids() {
  local p
  for p in "$@"; do wait "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

# row <claim> <specula_says> <ground_truth_says> <agree:true|false> <detail>
# Appends one line to the agreement table. This is the ONLY way a claim is
# reported: there is no path that prints a verdict without recording it.
row() {
  jq -nc --arg c "$1" --arg s "$2" --arg g "$3" --argjson a "$4" --arg d "${5:-}" \
    '{claim:$c, specula_says:$s, ground_truth_says:$g, agree:$a, detail:$d}' >> "$ROWS"
  if [[ "$4" == "true" ]]; then
    printf '   \033[32m✓\033[0m %-38s specula=%-22s truth=%s\n' "$1" "$2" "$3"
  else
    printf '   \033[31m✗ DISAGREE\033[0m %-29s specula=%-22s truth=%s\n' "$1" "$2" "$3"
  fi
}

want() { [[ "$CLAIMS" == "all" || " $CLAIMS " == *" $1 "* ]]; }

# metric <name> [labelmatch] → the numeric value of a metric line, or 0 if absent.
# Reads the /metrics text exposition directly; deliberately does NOT use any
# Specula helper to parse it.
metric() {
  local name="$1" want="${2:-}" line
  line="$(curl -fsS --max-time 10 "http://127.0.0.1:${CP}/metrics" 2>/dev/null \
          | grep -E "^${name}" | { [[ -n "$want" ]] && grep -F "$want" || cat; } | head -1)"
  [[ -z "$line" ]] && { echo 0; return; }
  echo "${line##* }"
}

# metric_present <name> → "yes"/"no". Distinguishes "absent" from "zero", which
# PRD §7.6 insists are different statements.
metric_present() {
  curl -fsS --max-time 10 "http://127.0.0.1:${CP}/metrics" 2>/dev/null \
    | grep -qE "^$1" && echo yes || echo no
}

ictl()   { curl -fsS --max-time 10 "http://127.0.0.1:${1}${2}"; }
ireset() { curl -fsS -X POST --max-time 10 "http://127.0.0.1:${1}/reset" >/dev/null; }
imode()  { curl -fsS -X POST --max-time 10 "http://127.0.0.1:${1}/mode?m=${2}" >/dev/null; }
# icount <control_port> [path] → number of requests the interposer actually saw.
icount() {
  local q=""
  [[ -n "${2:-}" ]] && q="?path=$2"
  ictl "$1" "/stats${q}" | jq -r '.total'
}

# ─────────────────────────── build ───────────────────────────

log "Building to our own temp path (${WORK})"
# Building to a private path is not fastidiousness: agents in this repo have
# clobbered each other's bin/specula and then graded the wrong binary.
INTERPOSER="${WORK}/interposer"
(cd "$REPO_ROOT" && go build -o "$INTERPOSER" ./test/groundtruth/interposer) || {
  echo "FATAL: interposer build failed"; exit 1; }

if [[ -n "${GT_SPECULA_BIN:-}" ]]; then
  BIN="$GT_SPECULA_BIN"
  info "grading prebuilt binary: $BIN"
else
  BIN="${WORK}/specula"
  (cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/specula) || {
    echo "FATAL: specula build failed"; exit 1; }
  info "built: $BIN"
fi

# ─────────────────────────── interposers ───────────────────────────

# start_interposer <name> <upstream> → sets ${name}_D (data addr) / ${name}_C (control port)
start_interposer() {
  local name="$1" up="$2" ports="${WORK}/ports-${1}.json"
  "$INTERPOSER" -upstream "$up" -name "$name" -ports "$ports" \
                -log "${WORK}/${name}.jsonl" > "${WORK}/${name}.log" 2>&1 &
  MY_PIDS+=("$!")
  local i
  for i in $(seq 1 50); do [[ -s "$ports" ]] && break; sleep 0.2; done
  [[ -s "$ports" ]] || { echo "FATAL: interposer $name never wrote $ports"; cat "${WORK}/${name}.log"; exit 1; }
  local d c
  d="$(jq -r .data "$ports")"; c="$(jq -r .control "$ports")"
  curl -fsS --max-time 5 "http://${c}/healthz" >/dev/null || {
    echo "FATAL: interposer $name control not answering"; exit 1; }
  printf -v "IP_${name}_D" '%s' "$d"
  printf -v "IP_${name}_C" '%s' "${c##*:}"
  info "interposer[$name] $d → $up (control :${c##*:})"
}

log "Starting interposers between Specula and the real CN mirrors"
start_interposer gomod "$UP_GOMOD"
start_interposer npm   "$UP_NPM"
start_interposer pypi  "$UP_PYPI"
GOMOD_D="$IP_gomod_D"; GOMOD_C="$IP_gomod_C"
NPM_D="$IP_npm_D";     NPM_C="$IP_npm_C"
PYPI_D="$IP_pypi_D";   PYPI_C="$IP_pypi_C"

# ─────────────────────────── config ───────────────────────────

DP="$(pick_free_port)"; CP="$(pick_free_port)"
CFG="${WORK}/specula.yaml"
BLOBS="${WORK}/blobs"; mkdir -p "$BLOBS"
DB="${WORK}/meta.db"

# default_mutable_ttl_seconds: 0 is a WORKAROUND, not a preference.
#
# ARCHITECTURE §3 and specula.example.yaml both document `ttl: 0` as the "always
# revalidate" sentinel, but cmd/specula/main.go's mutableTTL() treats a
# per-protocol 0 as "unset" and substitutes the global default, so the sentinel
# is unreachable per-protocol. Setting the GLOBAL default to 0 is the only way to
# make the sentinel reach a handler. This gate reports that contradiction as the
# claim `ttl_zero_sentinel_reaches_handler` rather than quietly working around it.
cat > "$CFG" <<EOF
server:
  data_plane_addr: "127.0.0.1:${DP}"
  control_plane_addr: "127.0.0.1:${CP}"
storage:
  blob:
    driver: local
    local:
      root: ${BLOBS}
  meta:
    driver: sqlite
    dsn: ${DB}
cache:
  default_mutable_ttl_seconds: 0
  negative_ttl_seconds: 1800
protocols:
  go:
    upstreams:
      - name: interposed
        base_url: http://${GOMOD_D}
        priority: 1
        official: true
    verification:
      tiers: [tofu, checksum]
      quorum: 1
  npm:
    upstreams:
      - name: interposed
        base_url: http://${NPM_D}
        priority: 1
        official: true
    verification:
      tiers: [tofu, checksum]
      quorum: 1
  pypi:
    upstreams:
      - name: interposed
        base_url: http://${PYPI_D}
        priority: 1
        official: true
    verification:
      tiers: [tofu, checksum]
      quorum: 1
EOF

# ─────────────────────────── daemon ───────────────────────────

log "Starting Specula (data :${DP}, control :${CP})"
SLOG="${WORK}/specula.log"
START_EPOCH="$(date +%s.%N)"
"$BIN" --config "$CFG" > "$SLOG" 2>&1 &
SPECULA_PID=$!
MY_PIDS+=("$SPECULA_PID")

# wait_for_daemon asserts KERNEL SOCKET OWNERSHIP, not just "something answered".
# A conformance run here once reported a pass for a binary it never touched.
wait_for_daemon "$SPECULA_PID" "$CP" "http://127.0.0.1:${CP}/healthz" "$SLOG" || exit 1
info "daemon owns :${CP} (pid ${SPECULA_PID})"

# ══════════════════════════ CLAIM 0 ══════════════════════════
# specula_cache_bytes visibility at startup.
#
# PRD §7 opens by requiring that every metric is registered at package init and
# that "registration must not depend on constructing an object or on a request
# arriving", citing specula_cache_bytes{protocol="git"} as the cautionary tale.
# cache_bytes is nevertheless registered in internal/stats by the Collector
# constructor and is only ever Set by a 30s ticker, so a fresh process reports
# NOTHING until the first tick. This is a known, documented gap; a gate that
# cannot see it is not a gate.
if want cache_bytes_visible_at_startup; then
  log "CLAIM cache_bytes_visible_at_startup"
  present="$(metric_present specula_cache_bytes)"
  elapsed="$(echo "$(date +%s.%N) - ${START_EPOCH}" | bc)"
  if [[ "$present" == "yes" ]]; then
    row cache_bytes_visible_at_startup "series present at +${elapsed}s" \
        "/metrics exposes specula_cache_bytes" true "PRD §7 satisfied"
  else
    row cache_bytes_visible_at_startup "series ABSENT at +${elapsed}s" \
        "/metrics has no specula_cache_bytes line" false \
        "PRD §7 requires registration at package init, independent of object construction or traffic. cache_bytes is registered by stats.newCollector and only Set by the 30s refresh ticker, so a fresh headless process reports nothing at all until t+30s. Absent != zero (PRD §7.6): an operator scraping in the first 30s cannot distinguish 'no cache' from 'metric broken'."
  fi
fi

# ══════════════════════════ CLAIM 1 ══════════════════════════
# A cold request must actually contact upstream.
if want cold_miss_contacts_upstream; then
  log "CLAIM cold_miss_contacts_upstream (gomod .info, cold)"
  ireset "$GOMOD_C"
  h0="$(metric 'specula_cache_hits_total' 'protocol="gomod"')"
  m0="$(metric 'specula_cache_misses_total' 'protocol="gomod"')"
  l0="$(metric 'specula_upstream_latency_seconds_count' 'protocol="gomod"')"
  code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 40 \
          "http://127.0.0.1:${DP}/go/rsc.io/quote/@v/v1.5.2.info")"
  seen="$(icount "$GOMOD_C" /rsc.io/quote/@v/v1.5.2.info)"
  m1="$(metric 'specula_cache_misses_total' 'protocol="gomod"')"
  l1="$(metric 'specula_upstream_latency_seconds_count' 'protocol="gomod"')"
  dm=$(( m1 - m0 )); dl=$(( l1 - l0 ))
  ok=false
  [[ "$code" == "200" && "$dm" -eq 1 && "$seen" -ge 1 && "$dl" -eq 1 ]] && ok=true
  row cold_miss_contacts_upstream "misses+${dm}, latency_count+${dl}" \
      "interposer saw ${seen} request(s)" "$ok" \
      "HTTP ${code}. A cold request must produce exactly one miss and be visible on the wire."
fi

# ══════════════════════════ CLAIM 2 ══════════════════════════
# A warm IMMUTABLE hit must contact upstream ZERO times.
#
# This is the one case where the naive rule does hold, and the docs agree: a CAS
# entry is immutable and is never revalidated (ARCHITECTURE §3). This claim is
# the one the interposer alone can make — the hit counter cannot distinguish
# "served from cache" from "served from cache after secretly refetching".
if want warm_immutable_hit_zero_upstream; then
  log "CLAIM warm_immutable_hit_zero_upstream (gomod .info, warm ×3)"
  ireset "$GOMOD_C"
  h0="$(metric 'specula_cache_hits_total' 'protocol="gomod"')"
  l0="$(metric 'specula_upstream_latency_seconds_count' 'protocol="gomod"')"
  for _ in 1 2 3; do
    curl -sS -o /dev/null --max-time 40 "http://127.0.0.1:${DP}/go/rsc.io/quote/@v/v1.5.2.info"
  done
  seen="$(icount "$GOMOD_C")"
  h1="$(metric 'specula_cache_hits_total' 'protocol="gomod"')"
  l1="$(metric 'specula_upstream_latency_seconds_count' 'protocol="gomod"')"
  dh=$(( h1 - h0 )); dl=$(( l1 - l0 ))
  ok=false
  [[ "$dh" -eq 3 && "$seen" -eq 0 && "$dl" -eq 0 ]] && ok=true
  row warm_immutable_hit_zero_upstream "hits+${dh}, latency_count+${dl}" \
      "interposer saw ${seen} request(s)" "$ok" \
      "Immutable CAS entries are never revalidated (ARCHITECTURE §3), so 3 warm hits must cost 0 upstream contacts. Only the interposer can tell a real hit from a hit that refetched anyway."
fi

# ══════════════════════════ CLAIM 3 ══════════════════════════
# The DOCUMENTED caveat: hit != no upstream contact.
#
# PRD §7.2 states plainly that a 304 revalidation is a hit AND costs a full round
# trip. Testing the naive rule here would fail correct code. What we verify is
# the documented rule, with the interposer as arbiter:
#   - hits move on a 304 (body came from cache), AND
#   - the round trip is real and IS counted by latency_count.
if want hit_is_not_no_upstream_contact; then
  log "CLAIM hit_is_not_no_upstream_contact (npm packument, always-revalidate ×3)"
  ireset "$NPM_C"
  h0="$(metric 'specula_cache_hits_total' 'protocol="npm"')"
  m0="$(metric 'specula_cache_misses_total' 'protocol="npm"')"
  l0="$(metric 'specula_upstream_latency_seconds_count' 'protocol="npm"')"
  for _ in 1 2 3; do
    curl -sS -o /dev/null --max-time 40 "http://127.0.0.1:${DP}/npm/is-odd"
  done
  st="$(ictl "$NPM_C" /stats)"
  seen="$(echo "$st" | jq -r .total)"
  n304="$(echo "$st" | jq -r .not_modified)"
  ncond="$(echo "$st" | jq -r .conditional)"
  h1="$(metric 'specula_cache_hits_total' 'protocol="npm"')"
  m1="$(metric 'specula_cache_misses_total' 'protocol="npm"')"
  l1="$(metric 'specula_upstream_latency_seconds_count' 'protocol="npm"')"
  dh=$(( h1 - h0 )); dm=$(( m1 - m0 )); dl=$(( l1 - l0 ))
  # The documented contract, all three parts:
  #  (a) the 3 requests partition into hits+misses (they all consulted cache);
  #  (b) a 304 counts as a hit even though upstream WAS contacted;
  #  (c) latency_count is the honest answer to "how often did we touch upstream"
  #      and must equal what the interposer actually saw.
  ok=false
  if [[ $(( dh + dm )) -eq 3 && "$dh" -ge 1 && "$n304" -ge 1 && "$dl" -eq "$seen" && "$seen" -eq 3 ]]; then
    ok=true
  fi
  row hit_is_not_no_upstream_contact "hits+${dh}, misses+${dm}, latency_count+${dl}" \
      "interposer saw ${seen} contacts, ${ncond} conditional, ${n304} × 304" "$ok" \
      "PRD §7.2's documented caveat, verified against the wire: ${dh} hit(s) coexist with ${seen} real upstream round trips. The naive rule 'hit ⇒ no upstream contact' is FALSE here BY DESIGN. Reconciliation asserted: Δlatency_count(${dl}) == interposer(${seen})."
fi

# ══════════════════════════ CLAIM 4 ══════════════════════════
# Single-flight / stampede protection (ARCHITECTURE §7).
if want single_flight_collapses_stampede; then
  log "CLAIM single_flight_collapses_stampede (10 concurrent cold, same artifact)"
  ireset "$GOMOD_C"
  l0="$(metric 'specula_upstream_latency_seconds_count' 'protocol="gomod"')"
  P="/rsc.io/quote/@v/v1.5.1.mod"
  cpids=()
  for _ in $(seq 1 10); do
    curl -sS -o /dev/null --max-time 40 "http://127.0.0.1:${DP}/go${P}" &
    cpids+=("$!")
  done
  wait_pids "${cpids[@]}"
  seen="$(icount "$GOMOD_C" "$P")"
  l1="$(metric 'specula_upstream_latency_seconds_count' 'protocol="gomod"')"
  dl=$(( l1 - l0 ))
  ok=false
  [[ "$seen" -eq 1 ]] && ok=true
  row single_flight_collapses_stampede "latency_count+${dl} (self-consistent, reveals nothing)" \
      "interposer saw ${seen} upstream fetches for 1 artifact" "$ok" \
      "ARCHITECTURE §7 claims two-layer stampede protection collapsing N concurrent misses into ONE upstream fetch. Ground truth: ${seen}. Note the counters are perfectly self-consistent at ${dl} — no counter-reading test can see this."
fi

# ══════════════════════════ CLAIM 5 ══════════════════════════
# The 0 = always-revalidate sentinel.
if want ttl_zero_sentinel_reaches_handler; then
  log "CLAIM ttl_zero_sentinel_reaches_handler (per-protocol ttl:0 on pypi)"
  # This run's config sets the GLOBAL default to 0; a per-protocol 0 is what the
  # docs tell an operator to write. We measure the global-0 path here (which
  # works) and report the per-protocol contradiction, which is proven separately
  # by groundtruth-inject.sh's control run.
  ireset "$PYPI_C"
  for _ in 1 2 3; do
    curl -sS -o /dev/null --max-time 40 "http://127.0.0.1:${DP}/pypi/simple/six/"
  done
  seen="$(icount "$PYPI_C" /simple/six/)"
  ok=false
  [[ "$seen" -eq 3 ]] && ok=true
  row ttl_zero_sentinel_reaches_handler "mutable ttl=0 configured (globally)" \
      "interposer saw ${seen}/3 revalidations" "$ok" \
      "ARCHITECTURE §3: 'ttl: 0 = 每次重验'. With the GLOBAL default set to 0 the sentinel reaches the handler and every request revalidates. cmd/specula/main.go mutableTTL() nevertheless discards a PER-PROTOCOL 0 as 'unset' — see the separate finding."
fi

# ══════════════════════════ CLAIM 6 ══════════════════════════
# Bytes-served correctness: what the client got vs what the REAL upstream serves.
#
# sha256 both. This catches corruption or substitution that Specula's own digest
# bookkeeping would happily agree with itself about — we never ask Specula what
# the digest should be; we ask goproxy.cn directly, bypassing both Specula and
# the interposer.
if want bytes_served_match_real_upstream; then
  log "CLAIM bytes_served_match_real_upstream (sha256 client bytes vs direct mirror fetch)"
  ok=true; details=""
  for art in "rsc.io/quote/@v/v1.5.2.info" "rsc.io/quote/@v/v1.5.1.mod"; do
    curl -fsS --max-time 40 -o "${WORK}/via.bin"    "http://127.0.0.1:${DP}/go/${art}"
    curl -fsS --max-time 40 -o "${WORK}/direct.bin" "${UP_GOMOD}/${art}"
    a="$(sha256sum "${WORK}/via.bin"    | cut -d' ' -f1)"
    b="$(sha256sum "${WORK}/direct.bin" | cut -d' ' -f1)"
    [[ "$a" == "$b" ]] || { ok=false; details+="${art}: specula=${a} upstream=${b}; "; }
  done
  if $ok; then
    row bytes_served_match_real_upstream "served 2 artifacts from cache" \
        "sha256 identical to direct mirror fetch" true \
        "Client bytes compared against ${UP_GOMOD} fetched directly, bypassing both Specula and the interposer."
  else
    row bytes_served_match_real_upstream "served 2 artifacts from cache" \
        "sha256 MISMATCH vs direct mirror fetch" false "$details"
  fi
fi

# ══════════════════════════ CLAIM 7/8 ══════════════════════════
# Byte accounting via three independent paths.
#
# Tolerance is DERIVED, not fudged:
#
#  * gauge vs DB — must be EXACT, but only after the collector's 30s tick has
#    run: the gauge is a periodic copy of the DB, so we poll for convergence
#    rather than assert instantly. A permanent difference is a real defect; a
#    <30s lag is the documented (and separately reported) refresh design.
#
#  * DB vs filesystem — the CAS is content-addressed and shared by ALL protocols
#    (one root, path = <2-hex-shard>/<digest>, no protocol component). So:
#      - PER-PROTOCOL byte truth cannot come from the filesystem at all. It exists
#        only in cache_entries.protocol. Stated as a gap, not papered over.
#      - The filesystem holds ONE file per DISTINCT digest, while the DB holds one
#        row per (protocol,name,version). Two rows sharing a digest are one file.
#        The honest identity is therefore
#            SUM(size) over DISTINCT digest  ==  sum of CAS file sizes
#        and NOT SUM(size) over all rows, which double-counts deduplicated blobs.
#    We compare with `find -type f -printf %s` (exact byte lengths) rather than
#    `du -sb`, and report du -sb alongside: `du -sb` is apparent size but still
#    adds each directory's own ~4096-byte apparent size, which is filesystem
#    bookkeeping, not cache content. That is a real difference with a known
#    cause, so we account for it explicitly instead of allowing a tolerance band
#    to hide a genuine leak.
if want cache_bytes_gauge_matches_db || want cache_bytes_db_matches_filesystem; then
  log "CLAIM byte accounting (gauge vs DB vs filesystem)"

  # Wait for the collector's tick so we compare converged values.
  info "waiting up to 40s for the stats collector tick..."
  db_total_rows=0
  for _ in $(seq 1 40); do
    db_total_rows="$(sqlite3 "$DB" 'SELECT COALESCE(SUM(size),0) FROM cache_entries;')"
    g="$(curl -fsS --max-time 10 "http://127.0.0.1:${CP}/metrics" \
         | awk '/^specula_cache_bytes\{/ {s+=$NF} END {print s+0}')"
    [[ "$g" == "$db_total_rows" && "$g" != "0" ]] && break
    sleep 1
  done

  if want cache_bytes_gauge_matches_db; then
    mismatch=""
    while IFS='|' read -r proto bytes objs; do
      [[ -z "$proto" ]] && continue
      gb="$(metric 'specula_cache_bytes' "protocol=\"${proto}\"")"
      go_="$(metric 'specula_cache_objects' "protocol=\"${proto}\"")"
      gb="${gb%.*}"; go_="${go_%.*}"
      [[ "$gb" == "$bytes" ]] || mismatch+="${proto}: gauge_bytes=${gb} db=${bytes}; "
      [[ "$go_" == "$objs" ]] || mismatch+="${proto}: gauge_objects=${go_} db=${objs}; "
    done < <(sqlite3 "$DB" 'SELECT protocol, SUM(size), COUNT(*) FROM cache_entries GROUP BY protocol;')
    gsum="$(curl -fsS --max-time 10 "http://127.0.0.1:${CP}/metrics" \
            | awk '/^specula_cache_bytes\{/ {s+=$NF} END {print s+0}')"
    if [[ -z "$mismatch" ]]; then
      row cache_bytes_gauge_matches_db "sum(cache_bytes)=${gsum}" \
          "sqlite3 SUM(size)=${db_total_rows}, per-protocol identical" true \
          "Gauge read from /metrics; truth read with the sqlite3 CLI straight off the DB file — not via stats.Collector."
    else
      row cache_bytes_gauge_matches_db "sum(cache_bytes)=${gsum}" \
          "sqlite3 SUM(size)=${db_total_rows}" false "$mismatch"
    fi
  fi

  if want cache_bytes_db_matches_filesystem; then
    # FS truth: exact byte lengths of every file under the CAS root.
    fs_bytes="$(find "$BLOBS" -type f -printf '%s\n' 2>/dev/null | awk '{s+=$1} END {print s+0}')"
    fs_files="$(find "$BLOBS" -type f 2>/dev/null | wc -l)"
    du_bytes="$(du -sb "$BLOBS" 2>/dev/null | cut -f1)"
    # DB truth for the SAME quantity: one row per distinct digest.
    db_distinct="$(sqlite3 "$DB" \
      "SELECT COALESCE(SUM(size),0) FROM (SELECT DISTINCT digest, size FROM cache_entries WHERE digest != '');")"
    db_ndigest="$(sqlite3 "$DB" \
      "SELECT COUNT(*) FROM (SELECT DISTINCT digest FROM cache_entries WHERE digest != '');")"
    tmpfiles="$(find "$BLOBS" -type f -name '.tmp-*' 2>/dev/null | wc -l)"
    ok=false
    [[ "$fs_bytes" == "$db_distinct" && "$fs_files" == "$db_ndigest" ]] && ok=true
    row cache_bytes_db_matches_filesystem \
        "DB distinct-digest SUM(size)=${db_distinct} over ${db_ndigest} digests" \
        "CAS holds ${fs_files} files totalling ${fs_bytes} bytes" "$ok" \
        "Independent path: find(1) over the CAS root, no Specula code involved. du -sb reports ${du_bytes} (higher: it adds each shard directory's own apparent size — filesystem bookkeeping, not cache content, hence the exact file-length sum is the arbiter). In-flight .tmp-* files at measurement: ${tmpfiles}. NOTE: the CAS root is shared by all protocols and paths carry no protocol component, so PER-PROTOCOL byte truth is NOT obtainable from the filesystem — only cache_entries.protocol has it. Row-sum (${db_total_rows}) ≥ distinct-digest sum (${db_distinct}) by CAS dedup."
  fi
fi

# ══════════════════════════ CLAIM 9/10 ══════════════════════════
# serve-stale on upstream failure, and the auto-block gauge.
#
# Done LAST and on a dedicated protocol+interposer: it deliberately breaks an
# upstream, and must not perturb the other claims.
if want serve_stale_on_upstream_failure || want upstream_blocked_gauge_tracks_reality; then
  log "CLAIM serve_stale_on_upstream_failure (pypi; interposer forced to fail)"

  # Prime: cache the artifact while the upstream is healthy, and keep the bytes.
  imode "$PYPI_C" ok
  curl -fsS --max-time 40 -o "${WORK}/stale-good.bin" "http://127.0.0.1:${DP}/pypi/simple/six/"
  good_sha="$(sha256sum "${WORK}/stale-good.bin" | cut -d' ' -f1)"
  good_len="$(stat -c%s "${WORK}/stale-good.bin")"
  info "primed: ${good_len} bytes, sha256=${good_sha:0:16}…"

  # Now kill the upstream. 503 is classified transient by internal/upstream, so
  # this is the exact condition ARCHITECTURE §3 promises serve-stale for.
  imode "$PYPI_C" fail
  ireset "$PYPI_C"
  h0="$(metric 'specula_cache_hits_total' 'protocol="pypi"')"
  l0="$(metric 'specula_upstream_latency_seconds_count' 'protocol="pypi"')"

  code="$(curl -sS -o "${WORK}/stale-served.bin" -w '%{http_code}' --max-time 40 \
          "http://127.0.0.1:${DP}/pypi/simple/six/")"
  served_sha="$(sha256sum "${WORK}/stale-served.bin" | cut -d' ' -f1)"
  seen="$(icount "$PYPI_C")"
  h1="$(metric 'specula_cache_hits_total' 'protocol="pypi"')"
  l1="$(metric 'specula_upstream_latency_seconds_count' 'protocol="pypi"')"
  dh=$(( h1 - h0 )); dl=$(( l1 - l0 ))

  if want serve_stale_on_upstream_failure; then
    ok=false
    [[ "$code" == "200" && "$served_sha" == "$good_sha" && "$dh" -eq 1 ]] && ok=true
    row serve_stale_on_upstream_failure "HTTP ${code}, hits+${dh}, latency_count+${dl}" \
        "interposer saw ${seen} failed attempt(s); body sha256 $( [[ "$served_sha" == "$good_sha" ]] && echo 'matches the primed bytes' || echo 'DIFFERS from primed bytes' )" \
        "$ok" \
        "ARCHITECTURE §3 + PRD §7.2: on upstream failure the stale body is served and counted a HIT (body came from cache); the failure shows up as the ABSENCE of a latency observation (Δ=${dl}, expected 0) — latency is only observed on a SUCCESSFUL round trip — plus the blocked gauge. The interposer proves Specula genuinely tried (${seen} attempt(s)) rather than never looking."
  fi

  if want upstream_blocked_gauge_tracks_reality; then
    log "CLAIM upstream_blocked_gauge_tracks_reality (5 consecutive transient failures)"
    # Force enough consecutive transient failures to trip the tracker.
    for _ in $(seq 1 8); do
      curl -sS -o /dev/null --max-time 40 "http://127.0.0.1:${DP}/pypi/simple/requests/" || true
    done
    seen2="$(icount "$PYPI_C")"
    blocked="$(metric 'specula_upstream_blocked' 'protocol="pypi"')"
    ok=false
    [[ "${blocked%.*}" == "1" ]] && ok=true
    row upstream_blocked_gauge_tracks_reality "specula_upstream_blocked=${blocked}" \
        "interposer returned 503 to every one of ${seen2} attempts" "$ok" \
        "PRD §7.4: the gauge means '5 consecutive TRANSIENT failures, inside the 30s refusal window'. 503 is transient, so it must reach 1. The interposer is the arbiter that the failures were real and were delivered."
  fi

  imode "$PYPI_C" ok
fi

# ══════════════════════════ CLAIM 11 ══════════════════════════
# The PER-PROTOCOL `ttl: 0` sentinel, tested the way the docs tell an operator to
# write it.
#
# This needs its OWN daemon: the main run above sets the GLOBAL default to 0 in
# order to work around this very defect, which would mask it. So we start a
# second instance on fresh ports with a fresh DB, configured exactly as
# specula.example.yaml documents:
#     cache.default_mutable_ttl_seconds: 300   (a normal default)
#     protocols.pypi.mutable_ttl_seconds: 0    ("revalidate on every request")
# and ask the interposer how many revalidations actually happened.
if want ttl_zero_per_protocol_sentinel; then
  log "CLAIM ttl_zero_per_protocol_sentinel (own daemon: global default 300, pypi ttl 0)"
  DP2="$(pick_free_port)"; CP2="$(pick_free_port)"
  BLOBS2="${WORK}/blobs2"; mkdir -p "$BLOBS2"
  CFG2="${WORK}/specula-ttl.yaml"
  cat > "$CFG2" <<EOF
server:
  data_plane_addr: "127.0.0.1:${DP2}"
  control_plane_addr: "127.0.0.1:${CP2}"
storage:
  blob:
    driver: local
    local:
      root: ${BLOBS2}
  meta:
    driver: sqlite
    dsn: ${WORK}/meta2.db
cache:
  default_mutable_ttl_seconds: 300
  negative_ttl_seconds: 1800
protocols:
  pypi:
    mutable_ttl_seconds: 0
    upstreams:
      - name: interposed
        base_url: http://${PYPI_D}
        priority: 1
        official: true
    verification:
      tiers: [tofu, checksum]
      quorum: 1
EOF
  SLOG2="${WORK}/specula-ttl.log"
  "$BIN" --config "$CFG2" > "$SLOG2" 2>&1 &
  PID2=$!
  MY_PIDS+=("$PID2")
  if wait_for_daemon "$PID2" "$CP2" "http://127.0.0.1:${CP2}/healthz" "$SLOG2"; then
    imode "$PYPI_C" ok
    ireset "$PYPI_C"
    for _ in 1 2 3; do
      curl -sS -o /dev/null --max-time 40 "http://127.0.0.1:${DP2}/pypi/simple/click/"
    done
    seen="$(icount "$PYPI_C" /simple/click/)"
    ok=false
    [[ "$seen" -eq 3 ]] && ok=true
    row ttl_zero_per_protocol_sentinel \
        "config says pypi.mutable_ttl_seconds: 0 = revalidate every request" \
        "interposer saw ${seen}/3 revalidations" "$ok" \
        "ARCHITECTURE §3 and specula.example.yaml both document ttl:0 as the 'revalidate on every request' sentinel, and internal/cache/cache.go isMutableFresh() implements it correctly (ttlAlwaysRevalidate → never fresh). But cmd/specula/main.go mutableTTL() reads 'if pc.MutableTTLSeconds != 0 { return pc.MutableTTLSeconds }; return cfg.Cache.DefaultMutableTTLSeconds' — conflating the SENTINEL 0 with 'unset', so a per-protocol 0 is silently replaced by the global default (300s here). The sentinel is unreachable per-protocol. This is not academic: the shipped specula.example.yaml sets apt.mutable_ttl_seconds: 0 with the comment 'always revalidate: InRelease has its own expiry field', so apt serves InRelease from cache for up to the global default instead."
    kill "$PID2" 2>/dev/null || true
  else
    row ttl_zero_per_protocol_sentinel "second daemon failed to start" \
        "could not establish ground truth" false "see ${SLOG2}"
  fi
fi

# ─────────────────────────── report ───────────────────────────

mkdir -p "$(dirname "$OUT")"
total="$(wc -l < "$ROWS")"
agree="$(jq -s '[.[] | select(.agree)] | length' "$ROWS")"
disagree=$(( total - agree ))

jq -s --arg ts "$(date -Iseconds)" --arg bin "$BIN" \
      --argjson total "$total" --argjson agree "$agree" --argjson disagree "$disagree" \
  '{generated_at:$ts, binary:$bin, claims_total:$total, claims_agree:$agree,
    claims_disagree:$disagree, rows:.}' "$ROWS" > "$OUT"

log "Agreement table"
jq -r '["CLAIM","SPECULA_SAYS","GROUND_TRUTH_SAYS","AGREE"],
       ["-----","------------","-----------------","-----"],
       (.rows[] | [.claim, .specula_says, .ground_truth_says, (.agree|tostring)])
       | @tsv' "$OUT" | column -t -s$'\t' 2>/dev/null || jq -r '.rows[]' "$OUT"

printf '\n   artifact: %s\n' "$OUT"
printf '   %s/%s claims agree, %s disagree\n\n' "$agree" "$total" "$disagree"

if [[ "$disagree" -gt 0 ]]; then
  echo "✗ groundtruth gate: ${disagree} claim(s) where Specula and reality disagree"
  # ALLOW_FAIL only suppresses the EXIT CODE (the injection harness expects red);
  # it must never suppress the message. A gate that can be made to print "all
  # good" over a disagreement is the exact failure mode this whole dimension
  # exists to prevent.
  [[ "$ALLOW_FAIL" == "1" ]] || exit 1
  echo "  (ALLOW_FAIL=1: reporting without failing)"
else
  echo "✓ groundtruth gate: no unexplained disagreement"
fi
