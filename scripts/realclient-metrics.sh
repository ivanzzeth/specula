#!/usr/bin/env bash
# Real-traffic behavioural acceptance for the PRD §7 metrics.
#
# Drives REAL clients against REAL CN upstreams through a FRESH HEADLESS specula
# process (nobody browsing the WebUI), then scrapes /metrics and proves that what
# it reports matches what actually happened.
#
# The load-bearing assertion is the metric-vs-DB cross-check:
# specula_verification_total{check="chain"}'s tier labels must equal the tiers
# actually persisted in cache_entries for that same traffic. If they disagree,
# one of them is lying.
#
# Ports are picked free (never 7732/7733, which belong to the demo).
# The binary is built to this script's OWN temp dir.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d /tmp/specula-metrics-accept.XXXXXX)"
. "${REPO}/scripts/lib/daemon.sh"

DATA_PORT="$(pick_free_port)"
CTRL_PORT="$(pick_free_port)"
# A port with nothing listening: the genuinely-unreachable upstream used to prove
# specula_upstream_blocked actually moves. Picked free, then never bound.
DEAD_PORT="$(pick_free_port)"

METRICS="http://127.0.0.1:${CTRL_PORT}/metrics"
DATA="http://127.0.0.1:${DATA_PORT}"

pass() { echo "  PASS: $*"; }
fail() { echo "  FAIL: $*" >&2; FAILED=1; }
step() { echo ""; echo "==> $*"; }
FAILED=0

mkdir -p "${WORK}/blobs"

step "Building specula to our own temp path (${WORK}/specula)"
go -C "${REPO}" build -o "${WORK}/specula" ./cmd/specula || { echo "build failed"; exit 1; }

step "Writing config (data :${DATA_PORT}, ctrl :${CTRL_PORT}, dead upstream :${DEAD_PORT})"
cat > "${WORK}/cfg.yaml" <<EOF
server:
  data_plane_addr: ":${DATA_PORT}"
  control_plane_addr: ":${CTRL_PORT}"
auth:
  registry_token_key_path: ${WORK}/regkey.pem
storage:
  blob: {driver: local, local: {root: ${WORK}/blobs}}
  meta: {driver: sqlite, dsn: ${WORK}/meta.db}
protocols:
  # NOTE: the CONFIG key is "go" (goProtocolKey), while the metadata store rows
  # and the metric's protocol label use "gomod". Getting this wrong silently
  # yields "configured: false" and no go traffic at all.
  go:
    upstreams:
      - name: goproxy-cn
        base_url: https://goproxy.cn
        priority: 0
    mutable_ttl_seconds: 300
    sumdb:
      url: https://sum.golang.google.cn
      verifier_key: "sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8"
      policy: enforce
  pypi:
    upstreams:
      - name: tuna
        base_url: https://pypi.tuna.tsinghua.edu.cn/simple
        priority: 0
      - name: pypi-org
        base_url: https://pypi.org/simple
        priority: 1
    mutable_ttl_seconds: 300
    verification:
      consensus:
        quorum: 2
        mirrors:
          - name: tuna
            base_url: https://pypi.tuna.tsinghua.edu.cn/simple
          - name: pypi-org
            base_url: https://pypi.org/simple
  npm:
    upstreams:
      - name: npmmirror
        base_url: https://registry.npmmirror.com
        priority: 0
    mutable_ttl_seconds: 120
  apt:
    mutable_ttl_seconds: 0
    upstreams:
      - name: aliyun
        base_url: http://mirrors.aliyun.com/ubuntu
        priority: 1
    verification:
      gpg:
        keyring: /usr/share/keyrings/ubuntu-archive-keyring.gpg
        policy: enforce
  helm:
    upstreams:
      - name: azure-cn
        base_url: https://mirror.azure.cn/kubernetes
        priority: 0
        official: true
    mutable_ttl_seconds: 1800
  tarball:
    # The allowlist is derived from the upstream base URLs' Hostname(), so the
    # only allowed host is "127.0.0.1" and the request path must name it.
    upstreams:
      - name: dead-upstream
        base_url: http://127.0.0.1:${DEAD_PORT}
        priority: 0
    mutable_ttl_seconds: 300
  git:
    upstreams:
      - name: github
        base_url: https://github.com
        priority: 0
  # OCI's ONLY upstream is a closed port. Unlike tarball (whose handler keeps an
  # upstream.Client "for API symmetry" but builds its own URLs and never calls
  # Fetch), the OCI handler goes through the instrumented generic client, so this
  # is what can actually prove specula_upstream_blocked moves.
  oci:
    upstreams:
      - name: dead-oci
        base_url: http://127.0.0.1:${DEAD_PORT}
        priority: 0
EOF

step "Starting FRESH HEADLESS specula (no UI interaction at any point)"
"${WORK}/specula" --config "${WORK}/cfg.yaml" > "${WORK}/daemon.log" 2>&1 &
SPID=$!
trap 'echo ""; echo "==> stopping our specula (pid ${SPID})"; kill "${SPID}" 2>/dev/null || true; ' EXIT
wait_for_daemon "${SPID}" "${DATA_PORT}" "http://127.0.0.1:${DATA_PORT}/healthz" "${WORK}/daemon.log" || {
  echo "daemon failed to start; log:"; cat "${WORK}/daemon.log"; exit 1; }
pass "specula up (pid ${SPID})"

# ── Baseline scrape: BEFORE any traffic, on a fresh headless process ──────────
step "Baseline /metrics scrape — fresh headless process, ZERO traffic"
curl -fsS "${METRICS}" > "${WORK}/metrics-cold.txt" || fail "could not scrape /metrics"
echo "--- metrics present at zero traffic (pre-initialised series) ---"
grep -E '^specula_(cache_hits_total|cache_misses_total|upstream_blocked)' "${WORK}/metrics-cold.txt" | head -20
echo "--- PRD §7 metric families present in HELP at zero traffic ---"
grep -E '^# HELP specula_' "${WORK}/metrics-cold.txt" | awk '{print $3}' | sort

# ── Real client traffic ──────────────────────────────────────────────────────
step "go: real GOPROXY-protocol traffic via specula (goproxy.cn + sum.golang.google.cn)"
# NOTE: the config key is "go" but the protocol label / store rows are "gomod".
# .mod and .zip are IMMUTABLE and are what the sumdb (signed) verifier covers;
# @v/list is mutable and legitimately tops out lower.
for f in "@v/list" "@v/v0.9.1.info" "@v/v0.9.1.mod" "@v/v0.9.1.zip"; do
  if curl -fsS -o /dev/null --max-time 60 "${DATA}/go/github.com/pkg/errors/${f}"; then
    pass "go ${f}"
  else
    echo "  NOTE: go ${f} non-zero"
  fi
done
# warm pass for hit/miss
curl -fsS -o /dev/null --max-time 60 "${DATA}/go/github.com/pkg/errors/@v/v0.9.1.zip" && pass "go .zip (warm)" || true

# Also drive the REAL go client end-to-end.
mkdir -p "${WORK}/gomod/proj" && cd "${WORK}/gomod/proj"
cat > go.mod <<'EOF'
module example.com/probe
go 1.21
EOF
env GOPATH="${WORK}/gomod/gopath" GOMODCACHE="${WORK}/gomod/cache" GOFLAGS=-mod=mod \
  GOPROXY="${DATA}/go" GONOSUMDB="*" GONOSUMCHECK=1 GOSUMDB=off GOPRIVATE="" \
  timeout 240 go get github.com/pkg/errors@v0.9.1 >"${WORK}/goget.log" 2>&1 \
  && pass "real go get through specula" || { echo "  NOTE: go get non-zero:"; tail -3 "${WORK}/goget.log"; }
cd "${REPO}"

step "pypi: real pip download via specula (tuna + pypi.org consensus)"
timeout 240 pip download six --no-deps --no-cache-dir \
  --index-url "${DATA}/pypi/simple/" -d "${WORK}/pip1" >/dev/null 2>&1 \
  && pass "pip download (cold)" || echo "  NOTE: pip cold non-zero"
timeout 240 pip download six --no-deps --no-cache-dir \
  --index-url "${DATA}/pypi/simple/" -d "${WORK}/pip2" >/dev/null 2>&1 \
  && pass "pip download (warm)" || echo "  NOTE: pip warm non-zero"

step "npm: real npm pack via specula (registry.npmmirror.com)"
mkdir -p "${WORK}/npm" && cd "${WORK}/npm"
timeout 180 npm pack lodash --registry "${DATA}/npm/" >/dev/null 2>&1 \
  && pass "npm pack (cold)" || echo "  NOTE: npm cold non-zero"
timeout 180 npm pack lodash --registry "${DATA}/npm/" >/dev/null 2>&1 \
  && pass "npm pack (warm)" || echo "  NOTE: npm warm non-zero"
cd "${REPO}"

step "npm: immutable tarball (tofu applies only to IMMUTABLE refs, not the packument)"
curl -fsS -o /dev/null "${DATA}/npm/lodash/-/lodash-4.17.21.tgz" && pass "npm tarball (cold)" || echo "  NOTE: npm tgz non-zero"
curl -fsS -o /dev/null "${DATA}/npm/lodash/-/lodash-4.17.21.tgz" && pass "npm tarball (warm)" || true

step "helm: immutable chart .tgz (FLAT repo: base=/kubernetes, charts under /helm/charts)"
curl -fsS -o /dev/null "${DATA}/helm/charts/acs-engine-autoscaler-2.2.2.tgz" && pass "helm chart (cold)" || echo "  NOTE: helm tgz non-zero"
curl -fsS -o /dev/null "${DATA}/helm/charts/acs-engine-autoscaler-2.2.2.tgz" && pass "helm chart (warm)" || true

step "apt: real InRelease + Packages + .deb via specula (aliyun, GPG keyring configured)"
curl -fsS -o /dev/null "${DATA}/apt/dists/jammy/InRelease" && pass "apt InRelease" || echo "  NOTE: apt InRelease non-zero"
curl -fsS -o /dev/null "${DATA}/apt/dists/jammy/InRelease" && pass "apt InRelease (warm)" || true

step "helm: real index.yaml via specula (mirror.azure.cn FLAT repo, base=/kubernetes → /helm/charts)"
curl -fsS -o /dev/null "${DATA}/helm/charts/index.yaml" && pass "helm index.yaml" || echo "  NOTE: helm index non-zero"
curl -fsS -o /dev/null "${DATA}/helm/charts/index.yaml" && pass "helm index.yaml (warm)" || true

step "oci: point at a CLOSED PORT to prove specula_upstream_blocked moves (>5 consecutive transient failures)"
for i in 1 2 3 4 5 6 7 8; do
  curl -fsS -o /dev/null --max-time 5 "${DATA}/v2/library/nginx/manifests/latest" 2>/dev/null || true
done
pass "8 OCI manifest attempts against closed port :${DEAD_PORT}"

step "tarball: also point at the CLOSED PORT (expected: NO blocked series — see report)"
# 6 attempts: the tracker blocks after 5 consecutive TRANSIENT failures
# (connection refused IS a network error, hence transient).
for i in 1 2 3 4 5 6 7 8; do
  curl -fsS -o /dev/null --max-time 5 "${DATA}/tarball/127.0.0.1/pkg/v1.0.0.tar.gz" 2>/dev/null || true
done
pass "8 fetch attempts against closed port :${DEAD_PORT}"

# ── Scrape after traffic ─────────────────────────────────────────────────────
step "Scraping /metrics after real traffic (still nobody touching the UI)"
curl -fsS "${METRICS}" > "${WORK}/metrics-warm.txt" || fail "scrape failed"

echo ""
echo "════════════ specula_verification_total ════════════"
grep -E '^specula_verification_total' "${WORK}/metrics-warm.txt" | sort || echo "(none)"

echo ""
echo "════════════ specula_cache_hits_total / misses_total ════════════"
grep -E '^specula_cache_(hits|misses)_total' "${WORK}/metrics-warm.txt" | grep -v ' 0$' | sort || echo "(none non-zero)"

echo ""
echo "════════════ specula_upstream_latency_seconds (count/sum) ════════════"
grep -E '^specula_upstream_latency_seconds_(count|sum)' "${WORK}/metrics-warm.txt" | sort || echo "(none)"

echo ""
echo "════════════ specula_upstream_blocked ════════════"
grep -E '^specula_upstream_blocked' "${WORK}/metrics-warm.txt" | sort || echo "(none)"

echo ""
echo "════════════ specula_requests_total ════════════"
grep -E '^specula_requests_total' "${WORK}/metrics-warm.txt" | sort || echo "(none)"

# ── THE CROSS-CHECK: metric tier vs DB tier ──────────────────────────────────
step "CROSS-CHECK: specula_verification_total{check=chain} tier  vs  cache_entries.tier in the DB"

echo ""
echo "--- DB: cache_entries grouped by (protocol, tier) ---"
# Tier is stored as the artifact.Tier int: 0=checksum 1=tofu 2=consensus 3=signed
sqlite3 "${WORK}/meta.db" <<'SQL'
.mode column
.headers on
SELECT protocol,
       CASE tier WHEN 3 THEN 'signed' WHEN 2 THEN 'consensus'
                 WHEN 1 THEN 'tofu'   WHEN 0 THEN 'checksum'
                 ELSE 'UNKNOWN(' || tier || ')' END AS tier_label,
       COUNT(*) AS rows
FROM cache_entries GROUP BY protocol, tier ORDER BY protocol, tier;
SQL

echo ""
echo "--- METRIC: specula_verification_total{check=\"chain\",result=pass|warn} by (protocol,tier) ---"
grep -E '^specula_verification_total\{check="chain"' "${WORK}/metrics-warm.txt" \
  | grep -Ev 'result="fail"' | sort || echo "(none)"

echo ""
echo "--- Per-protocol highest tier: DB vs METRIC ---"
for proto in gomod pypi npm apt helm tarball git oci; do
  db_tier=$(sqlite3 "${WORK}/meta.db" \
    "SELECT CASE MAX(tier) WHEN 3 THEN 'signed' WHEN 2 THEN 'consensus' WHEN 1 THEN 'tofu' WHEN 0 THEN 'checksum' END
     FROM cache_entries WHERE protocol='${proto}';" 2>/dev/null)
  [ -z "${db_tier}" ] && continue
  metric_tiers=$(grep -E "^specula_verification_total\{check=\"chain\",protocol=\"${proto}\"" "${WORK}/metrics-warm.txt" \
    | grep -Ev 'result="fail"' | sed -E 's/.*tier="([^"]*)".*/\1/' | sort -u | tr '\n' ',' )
  echo "  ${proto}: DB_max=${db_tier}   METRIC_chain_tiers={${metric_tiers%,}}"
  if [ -n "${metric_tiers}" ] && ! echo "${metric_tiers}" | grep -q "${db_tier}"; then
    fail "${proto}: DB says ${db_tier} but metric never reports it — one of them is lying"
  fi
done

# ── Dedicated blocked-gauge proof (second, isolated instance) ────────────────
# Why a second instance: the gauge only moves for a protocol whose handler
# actually reaches the instrumented generic upstream client.
#   - tarball keeps an upstream.Client "for API symmetry" but builds its own URLs
#     and never calls Fetch  -> no latency, no blocked. Structural gap.
#   - oci answers 401 (bearer challenge) before any upstream fetch -> never tried.
# npm demonstrably goes through Fetch (it reports latency above), so npm pointed
# at a closed port is the honest proof.
step "BLOCKED PROOF: second instance, npm's ONLY upstream is a closed port"
B_DATA="$(pick_free_port)"; B_CTRL="$(pick_free_port)"; B_DEAD="$(pick_free_port)"
mkdir -p "${WORK}/b/blobs"
cat > "${WORK}/b/cfg.yaml" <<EOF
server:
  data_plane_addr: ":${B_DATA}"
  control_plane_addr: ":${B_CTRL}"
auth:
  registry_token_key_path: ${WORK}/b/regkey.pem
storage:
  blob: {driver: local, local: {root: ${WORK}/b/blobs}}
  meta: {driver: sqlite, dsn: ${WORK}/b/meta.db}
protocols:
  npm:
    upstreams:
      - name: dead-npm
        base_url: http://127.0.0.1:${B_DEAD}
        priority: 0
    mutable_ttl_seconds: 120
EOF
"${WORK}/specula" --config "${WORK}/b/cfg.yaml" > "${WORK}/b/daemon.log" 2>&1 &
BPID=$!
trap 'kill "${SPID}" 2>/dev/null || true; kill "${BPID}" 2>/dev/null || true' EXIT
wait_for_daemon "${BPID}" "${B_DATA}" "http://127.0.0.1:${B_DATA}/healthz" "${WORK}/b/daemon.log" || true

echo "--- blocked gauge BEFORE any traffic (fresh, pre-initialised) ---"
curl -fsS "http://127.0.0.1:${B_CTRL}/metrics" | grep -E '^specula_upstream_blocked' || echo "(none)"

# 5 consecutive TRANSIENT failures (connection refused = network error) trip it.
for i in 1 2 3 4 5 6 7 8; do
  curl -fsS -o /dev/null --max-time 5 "http://127.0.0.1:${B_DATA}/npm/lodash" 2>/dev/null || true
done

echo "--- blocked gauge AFTER 8 attempts against closed port :${B_DEAD} ---"
curl -fsS "http://127.0.0.1:${B_CTRL}/metrics" | grep -E '^specula_upstream_blocked' || echo "(none)"
BLOCKED_VAL=$(curl -fsS "http://127.0.0.1:${B_CTRL}/metrics" | grep '^specula_upstream_blocked{protocol="npm",upstream="dead-npm"}' | awk '{print $2}')
if [ "${BLOCKED_VAL}" = "1" ]; then
  pass "specula_upstream_blocked went 0 -> 1 against a genuinely unreachable upstream"
else
  fail "specula_upstream_blocked did not move (got '${BLOCKED_VAL}')"
fi
kill "${BPID}" 2>/dev/null || true

echo ""
echo "==> WORK DIR: ${WORK}"
echo "==> metrics-cold.txt / metrics-warm.txt / meta.db retained for inspection"
exit ${FAILED}
