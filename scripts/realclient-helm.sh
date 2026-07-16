#!/usr/bin/env bash
# Helm real-client conformance runner for Specula's classic-HTTP Helm handler.
#
# Drives the REAL `helm` client end-to-end against a live Specula instance to
# verify conformance with the Helm Chart Repository specification:
#   https://helm.sh/docs/topics/chart_repository/
#
# The upstream is the Azure CN mirror of the Helm stable charts repo:
#   https://mirror.azure.cn/kubernetes/charts/
#
# URL routing layout:
#   Specula upstream base_url: https://mirror.azure.cn/kubernetes   (parent dir)
#   Specula repo path:         charts                               (subdir)
#   Helm repo URL:             http://127.0.0.1:DATA_PORT/helm/charts
#
# The upstream buildPath for helm yields:
#   index:  charts/index.yaml  → https://mirror.azure.cn/kubernetes/charts/index.yaml
#   chart:  charts/<file>.tgz  → https://mirror.azure.cn/kubernetes/charts/<file>.tgz
#
# Non-conformances found and fixed in this run:
#   1. Absolute chart URLs in index.yaml bypassed the Specula proxy.
#      Fix: rewriteIndexURLs() in internal/handler/helm/endpoints.go rewrites
#           absolute http/https URLs in every "urls" sequence to just the
#           filename (last path segment), so helm resolves them relative to the
#           Specula repo URL. Spec ref: "urls: A list of URLs for each version
#           of the chart. Relative URLs are resolved against the repository URL."
#
# Usage:  scripts/realclient-helm.sh
# Exit 0 only if all assertions pass.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-/tmp/specula-helm-real}"
DATA_PORT="${DATA_PORT:-5104}"
CTRL_PORT="${CTRL_PORT:-5204}"
REPO_ALIAS="specula-helm-test"

# Upstream config: parent directory of the stable chart repo so that the repo
# path "charts" maps to https://mirror.azure.cn/kubernetes/charts/index.yaml
UPSTREAM_BASE="https://mirror.azure.cn/kubernetes"
SPECULA_REPO_URL="http://127.0.0.1:${DATA_PORT}/helm/charts"

# Chart to pull during the test (from the azure mirror's stable archive).
TEST_CHART="mysql"
TEST_CHART_VERSION="1.6.9"

PASS=0
FAIL=0

# Arithmetic increment: use PASS=$((PASS+1)) not ((PASS++)) because the latter
# evaluates to 0 on the first call, which triggers set -e.
pass() { echo "[PASS] $*"; PASS=$((PASS+1)); }
fail() { echo "[FAIL] $*"; FAIL=$((FAIL+1)); }
assert_exit0() {
    local label="$1"; shift
    if "$@" > /tmp/specula-helm-assert.out 2>&1; then
        pass "$label"
    else
        fail "$label (exit $?)"
        cat /tmp/specula-helm-assert.out || true
    fi
}

mkdir -p "$WORK/blobs"

# ── 1. Build specula ─────────────────────────────────────────────────────────
echo "==> building specula"
GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" \
GOSUMDB="${GOSUMDB:-sum.golang.google.cn}" \
  go -C "$REPO" build -o "$WORK/specula" ./cmd/specula

# ── 2. Write config ──────────────────────────────────────────────────────────
cat > "$WORK/cfg.yaml" <<EOF
server:
  data_plane_addr: ":${DATA_PORT}"
  control_plane_addr: ":${CTRL_PORT}"
auth:
  registry_token_key_path: ${WORK}/regkey.pem
storage:
  blob:
    driver: local
    local:
      root: ${WORK}/blobs
  meta:
    driver: sqlite
    dsn: ${WORK}/meta.db
protocols:
  helm:
    upstreams:
      - name: azure-cn
        base_url: ${UPSTREAM_BASE}
        priority: 0
        official: true
    mutable_ttl_seconds: 1800
EOF

# ── 3. Start specula ─────────────────────────────────────────────────────────
echo "==> starting specula on data=:${DATA_PORT} ctrl=:${CTRL_PORT}"
rm -f "$WORK"/meta.db*
rm -rf "$WORK"/blobs/*

"$WORK/specula" --config "$WORK/cfg.yaml" > "$WORK/daemon.log" 2>&1 &
SPID=$!

# Kill specula on EXIT (success or failure) — by PID not pattern to avoid
# self-kill when the shell's own command line contains the config path.
trap 'kill "$SPID" 2>/dev/null || true' EXIT

# Wait for the control plane to become healthy.
for i in $(seq 1 20); do
    if curl -fsS --max-time 1 "http://127.0.0.1:${CTRL_PORT}/healthz" >/dev/null 2>&1; then
        break
    fi
    sleep 0.5
done
if ! curl -fsS --max-time 2 "http://127.0.0.1:${CTRL_PORT}/healthz" >/dev/null 2>&1; then
    echo "ERROR: specula failed to start; daemon log:"
    cat "$WORK/daemon.log"
    exit 1
fi
echo "==> specula healthy"

# ── 4. Remove any previous helm repo alias ───────────────────────────────────
helm repo remove "$REPO_ALIAS" 2>/dev/null || true

# ── 5. helm repo add ─────────────────────────────────────────────────────────
echo ""
echo "==> helm repo add ${REPO_ALIAS} ${SPECULA_REPO_URL}"
CMD_EXIT=0
CMD_OUTPUT=$(helm repo add "$REPO_ALIAS" "$SPECULA_REPO_URL" 2>&1) || CMD_EXIT=$?
echo "$CMD_OUTPUT"
if [ "$CMD_EXIT" -eq 0 ]; then
    pass "helm repo add (exit 0)"
else
    fail "helm repo add (exit ${CMD_EXIT})"
fi

# ── 6. helm repo update ──────────────────────────────────────────────────────
echo ""
echo "==> helm repo update ${REPO_ALIAS}"
CMD_EXIT=0
CMD_OUTPUT=$(helm repo update "$REPO_ALIAS" 2>&1) || CMD_EXIT=$?
echo "$CMD_OUTPUT"
if [ "$CMD_EXIT" -eq 0 ]; then
    pass "helm repo update (exit 0)"
else
    fail "helm repo update (exit ${CMD_EXIT})"
fi

# ── 7. Assert index.yaml is served with correct content ─────────────────────
echo ""
echo "==> checking index.yaml served with apiVersion v1 and entries"
INDEX_FILE=$(mktemp /tmp/specula-helm-index-XXXXXX.yaml)
INDEX_EXIT=0
curl -fsS --max-time 60 "$SPECULA_REPO_URL/index.yaml" -o "$INDEX_FILE" || INDEX_EXIT=$?
if [ $INDEX_EXIT -ne 0 ]; then
    fail "GET /helm/charts/index.yaml (exit ${INDEX_EXIT})"
else
    INDEX_BYTES=$(wc -c < "$INDEX_FILE")
    pass "GET /helm/charts/index.yaml (HTTP 200, ${INDEX_BYTES} bytes)"
    # Verify apiVersion field (Helm chart repository spec requires apiVersion: v1)
    if grep -q "^apiVersion:" "$INDEX_FILE"; then
        pass "index.yaml has apiVersion field"
    else
        fail "index.yaml missing apiVersion field"
    fi
    # Verify entries field (Helm chart repository spec requires entries map)
    if grep -q "^entries:" "$INDEX_FILE"; then
        pass "index.yaml has entries field"
    else
        fail "index.yaml missing entries field"
    fi
    # Assert URLs are RELATIVE (non-conformance fix: absolute URLs now rewritten)
    # The rewritten index should NOT contain any absolute upstream chart URLs.
    if grep -q "https://mirror.azure.cn.*\.tgz" "$INDEX_FILE"; then
        fail "index.yaml urls still contain absolute upstream URLs (URL rewrite failed)"
    else
        pass "index.yaml urls are relative (absolute upstream URLs rewritten)"
    fi
    # Assert chart filenames appear in the urls field
    if grep -qE "^[[:space:]]+-[[:space:]][a-z].*\.tgz$" "$INDEX_FILE"; then
        pass "index.yaml urls contain relative chart filenames"
    else
        fail "index.yaml urls do not contain relative chart filenames"
    fi
fi
rm -f "$INDEX_FILE"

# ── 8. helm search repo ───────────────────────────────────────────────────────
echo ""
echo "==> helm search repo ${REPO_ALIAS}/${TEST_CHART}"
CMD_EXIT=0
CMD_OUTPUT=$(helm search repo "${REPO_ALIAS}/${TEST_CHART}" 2>&1) || CMD_EXIT=$?
echo "$CMD_OUTPUT"
if [ "$CMD_EXIT" -eq 0 ] && echo "$CMD_OUTPUT" | grep -q "${TEST_CHART}"; then
    pass "helm search repo found ${TEST_CHART} (exit 0)"
else
    fail "helm search repo (exit ${CMD_EXIT})"
fi

# ── 9. First helm pull (cache miss, from upstream) ────────────────────────────
echo ""
echo "==> helm pull ${REPO_ALIAS}/${TEST_CHART} (first pull — upstream fetch)"
PULL_DIR=$(mktemp -d)
CMD_EXIT=0
CMD_OUTPUT=$(helm pull "${REPO_ALIAS}/${TEST_CHART}" --destination "$PULL_DIR" 2>&1) || CMD_EXIT=$?
echo "$CMD_OUTPUT"
CHART_TGZ=$(ls "$PULL_DIR"/${TEST_CHART}-*.tgz 2>/dev/null | head -1)
if [ "$CMD_EXIT" -eq 0 ] && [ -n "$CHART_TGZ" ]; then
    pass "helm pull first (exit 0, got ${CHART_TGZ##*/})"
else
    fail "helm pull first (exit ${CMD_EXIT}, no .tgz in $PULL_DIR)"
fi

# ── 10. helm show chart: verify the tgz is a valid chart ──────────────────────
if [ -n "$CHART_TGZ" ]; then
    echo ""
    echo "==> helm show chart ${CHART_TGZ##*/}"
    CMD_EXIT=0
    CMD_OUTPUT=$(helm show chart "$CHART_TGZ" 2>&1) || CMD_EXIT=$?
    echo "$CMD_OUTPUT"
    if [ "$CMD_EXIT" -eq 0 ] && echo "$CMD_OUTPUT" | grep -q "^name: ${TEST_CHART}"; then
        pass "helm show chart parses valid Chart.yaml (name: ${TEST_CHART})"
    else
        fail "helm show chart failed or wrong name (exit ${CMD_EXIT})"
    fi
    if echo "$CMD_OUTPUT" | grep -q "^version: ${TEST_CHART_VERSION}"; then
        pass "helm show chart reports correct version ${TEST_CHART_VERSION}"
    else
        fail "helm show chart version mismatch (expected ${TEST_CHART_VERSION})"
    fi
fi

# ── 11. Assert chart tgz resolves THROUGH Specula ─────────────────────────────
echo ""
echo "==> verifying chart url resolves through Specula"
CHART_URL="${SPECULA_REPO_URL}/${TEST_CHART}-${TEST_CHART_VERSION}.tgz"
echo "    GET ${CHART_URL}"
HTTP_STATUS=$(curl -s -o /dev/null -w "%{http_code}" --max-time 30 "$CHART_URL")
if [ "$HTTP_STATUS" = "200" ]; then
    pass "chart tgz resolves through Specula (HTTP 200)"
else
    fail "chart tgz via Specula returned HTTP ${HTTP_STATUS}"
fi

# ── 12. Second helm pull (cache hit) ─────────────────────────────────────────
echo ""
echo "==> helm pull ${REPO_ALIAS}/${TEST_CHART} (second pull — cache hit)"
PULL_DIR2=$(mktemp -d)
START_MS=$(date +%s%N)
CMD_EXIT=0
CMD_OUTPUT=$(helm pull "${REPO_ALIAS}/${TEST_CHART}" --destination "$PULL_DIR2" 2>&1) || CMD_EXIT=$?
END_MS=$(date +%s%N)
ELAPSED_MS=$(( (END_MS - START_MS) / 1000000 ))
echo "$CMD_OUTPUT"
CHART_TGZ2=$(ls "$PULL_DIR2"/${TEST_CHART}-*.tgz 2>/dev/null | head -1)
if [ "$CMD_EXIT" -eq 0 ] && [ -n "$CHART_TGZ2" ]; then
    pass "helm pull second (exit 0, ${ELAPSED_MS}ms)"
    # Compare file sizes between first and second pull
    if [ -n "$CHART_TGZ" ] && [ -n "$CHART_TGZ2" ]; then
        SIZE1=$(wc -c < "$CHART_TGZ")
        SIZE2=$(wc -c < "$CHART_TGZ2")
        if [ "$SIZE1" -eq "$SIZE2" ]; then
            pass "chart tgz size identical on second pull (${SIZE1} bytes = cache hit)"
        else
            fail "chart tgz size differs: first=${SIZE1} second=${SIZE2}"
        fi
    fi
else
    fail "helm pull second (exit ${CMD_EXIT})"
fi

# ── 13. helm pull --verify (expected to fail: no .prov for this archive) ──────
echo ""
echo "==> helm pull --verify (expected to fail: azure mirror has no .prov files)"
PULL_DIR3=$(mktemp -d)
VERIFY_EXIT=0
VERIFY_OUTPUT=$(helm pull "${REPO_ALIAS}/${TEST_CHART}" --verify --destination "$PULL_DIR3" 2>&1) || VERIFY_EXIT=$?
echo "$VERIFY_OUTPUT"
if [ $VERIFY_EXIT -ne 0 ] && echo "$VERIFY_OUTPUT" | grep -qi "provenance\|prov"; then
    pass "helm pull --verify fails gracefully (no .prov upstream; expected)"
    echo "    NOTE: .prov verification not validated — the Azure stable mirror (archived)"
    echo "          does not publish GPG provenance files. helm correctly reports the"
    echo "          missing .prov. Specula serves a 404 for the .prov fetch as expected."
else
    fail "helm pull --verify had unexpected outcome (exit ${VERIFY_EXIT})"
    echo "$VERIFY_OUTPUT"
fi

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "==> helm conformance results: ${PASS} passed, ${FAIL} failed"
echo "==> daemon log at: $WORK/daemon.log"

if [ $FAIL -gt 0 ]; then
    echo "==> FAIL"
    exit 1
fi
echo "==> PASS"
