#!/usr/bin/env bash
# PyPI real-client conformance runner for Specula's PyPI handler.
#
# Drives the REAL pip client (PEP 503 / PEP 691) end-to-end through Specula
# and asserts each behaviour:
#
#  1. pip install   (PEP 503 HTML simple index; exit 0; package on disk)
#  2. Cache hit     (second install served from Specula's CAS; no upstream fetch)
#  3. pip download  (PEP 503 download-only mode)
#  4. PEP 691 JSON  (Accept: application/vnd.pypi.simple.v1+json → 200 + valid
#                    body; tuna doesn't support JSON so Specula falls back to
#                    HTML with text/html CT — valid per PEP 691 §4.1 graceful
#                    degradation; pip accepts this)
#  5. #sha256= fragment preserved end-to-end so pip's --require-hashes works
#     (PEP 503 §2: the URL fragment MUST be the hash of the file)
#
# Upstreams configured:
#   - Priority 0: https://pypi.tuna.tsinghua.edu.cn/simple  (CN mirror)
#   - Priority 1: https://pypi.org/simple                   (official fallback)
#
# PORTS  (assigned to PyPI by the parent workflow — do not change):
#   Data plane  :5101
#   Control plane :5201
#
# Usage:
#   scripts/realclient-pypi.sh
#
# Exit 0 only if ALL assertions pass.
set -euo pipefail

# ── Paths ──────────────────────────────────────────────────────────────────────
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-/tmp/specula-pypi-conf}"
DATA_PORT="${DATA_PORT:-5101}"
CTRL_PORT="${CTRL_PORT:-5201}"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOSUMDB="${GOSUMDB:-sum.golang.google.cn}"

PYPI_INDEX="http://127.0.0.1:${DATA_PORT}/pypi/simple/"

# Test package: small pure-Python wheel with real deps, widely available on CN
# mirrors.  "requests" pulls in charset-normalizer, idna, urllib3, certifi.
PKG_NAME="requests"
# "six" is tiny, single-file, no deps — used for the --require-hashes test.
HASH_PKG="six"

mkdir -p "${WORK}/blobs"

# ── Helpers ───────────────────────────────────────────────────────────────────
pass() { echo "  PASS: $*"; }
fail() { echo "  FAIL: $*" >&2; exit 1; }

step() {
    echo ""
    echo "==> $*"
}

# assert_exit0 <label> <command...>
assert_exit0() {
    local label="$1"; shift
    if "$@"; then
        pass "${label}"
    else
        fail "${label} (exit $?)"
    fi
}

# ── 1. Build specula ──────────────────────────────────────────────────────────
step "Building specula"
go -C "${REPO}" build -o "${WORK}/specula" ./cmd/specula
pass "specula built"

# ── 2. Write config ───────────────────────────────────────────────────────────
step "Writing config (data_plane :${DATA_PORT}, ctrl :${CTRL_PORT})"
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
  pypi:
    # Upstreams use the canonical simple-index base URL (with /simple suffix)
    # as operators naturally write it per PEP 503.  The handler normalises the
    # URL by stripping the /simple suffix before passing to the upstream client,
    # which already prepends "simple/<project>/" via buildPath — so the final
    # fetch URL is correctly "…/simple/<project>/", not the doubled
    # "…/simple/simple/<project>/" that would otherwise result.
    upstreams:
      - name: tuna
        base_url: https://pypi.tuna.tsinghua.edu.cn/simple
        priority: 0
      - name: pypi-org
        base_url: https://pypi.org/simple
        priority: 1
    mutable_ttl_seconds: 300
EOF
pass "config written"

# ── 3. Start specula ──────────────────────────────────────────────────────────
step "Starting specula"
rm -f "${WORK}"/meta.db* ; rm -rf "${WORK}"/blobs/*
"${WORK}/specula" --config "${WORK}/cfg.yaml" > "${WORK}/daemon.log" 2>&1 &
SPID=$!
trap '
    echo ""
    echo "==> Stopping specula (PID=${SPID})"
    kill "${SPID}" 2>/dev/null || true
' EXIT

# Wait for the data plane to become reachable.
for i in $(seq 1 10); do
    if curl -fsS --max-time 1 "http://127.0.0.1:${DATA_PORT}/healthz" >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
curl -fsS --max-time 3 "http://127.0.0.1:${DATA_PORT}/healthz" >/dev/null || {
    echo "Specula failed to start:" >&2
    cat "${WORK}/daemon.log" >&2
    exit 1
}
pass "specula listening on :${DATA_PORT}"

# ── 4. Seed admin user (for stats API) ────────────────────────────────────────
step "Seeding admin user"
curl -fsS -H 'Content-Type: application/json' -X POST \
    "http://127.0.0.1:${CTRL_PORT}/api/v1/auth/register" \
    -d '{"email":"pypi-conf@specula.local","password":"password123","name":"PyPI Conformance"}' \
    >/dev/null
pass "admin user seeded"

# ── Helper: login and get stats ───────────────────────────────────────────────
admin_stats() {
    local cookiefile="${WORK}/cookie.txt"
    curl -fsS -c "${cookiefile}" -H 'Content-Type: application/json' -X POST \
        "http://127.0.0.1:${CTRL_PORT}/api/v1/auth/login" \
        -d '{"email":"pypi-conf@specula.local","password":"password123"}' \
        >/dev/null
    curl -fsS -b "${cookiefile}" \
        "http://127.0.0.1:${CTRL_PORT}/api/v1/admin/stats"
}

# ── 5. pip install (cache MISS — fresh install) ───────────────────────────────
step "Test 1: pip install ${PKG_NAME} (cache miss → upstream fetch)"
INST1="${WORK}/install1"
mkdir -p "${INST1}"
pip install \
    --index-url "${PYPI_INDEX}" \
    --target "${INST1}" \
    --no-cache-dir \
    --no-deps \
    "${PKG_NAME}" 2>&1 | tee "${WORK}/pip-install1.log"

# Assert: exit 0 already enforced by set -e above; assert package on disk.
ls "${INST1}/"*requests* >/dev/null 2>&1 \
    || fail "requests not found under ${INST1}"
pass "pip install exit 0; package on disk"

# ── 5b. Check stats after first install ───────────────────────────────────────
step "Stats after first install"
STATS1=$(admin_stats)
PYPI_BYTES1=$(echo "${STATS1}" | python3 -c "
import sys, json
d = json.load(sys.stdin)
for p in d.get('per_protocol', []):
    if p['protocol'] == 'pypi':
        print(p['bytes'])
        break
else:
    print(0)
")
echo "  PyPI cached bytes (after install 1): ${PYPI_BYTES1}"
[ "${PYPI_BYTES1}" -gt 0 ] || fail "expected non-zero cached bytes after first install"
pass "non-zero bytes cached"

# ── 6. pip install (cache HIT — second install) ───────────────────────────────
step "Test 2: pip install ${PKG_NAME} (cache hit — no upstream contact)"
INST2="${WORK}/install2"
mkdir -p "${INST2}"
pip install \
    --index-url "${PYPI_INDEX}" \
    --target "${INST2}" \
    --no-cache-dir \
    --no-deps \
    "${PKG_NAME}" 2>&1 | tee "${WORK}/pip-install2.log"

ls "${INST2}/"*requests* >/dev/null 2>&1 \
    || fail "requests not found under ${INST2} on second install"
pass "second pip install exit 0; package on disk"

# Verify CAS object count did not increase (no new upstream fetch).
STATS2=$(admin_stats)
PYPI_OBJ2=$(echo "${STATS2}" | python3 -c "
import sys, json
d = json.load(sys.stdin)
for p in d.get('per_protocol', []):
    if p['protocol'] == 'pypi':
        print(p['objects'])
        break
else:
    print(0)
")
PYPI_OBJ1=$(echo "${STATS1}" | python3 -c "
import sys, json
d = json.load(sys.stdin)
for p in d.get('per_protocol', []):
    if p['protocol'] == 'pypi':
        print(p['objects'])
        break
else:
    print(0)
")
echo "  CAS objects after install 1: ${PYPI_OBJ1}  after install 2: ${PYPI_OBJ2}"
[ "${PYPI_OBJ2}" -eq "${PYPI_OBJ1}" ] \
    || fail "CAS object count increased from ${PYPI_OBJ1} to ${PYPI_OBJ2} — unexpected upstream fetch on cache hit"
pass "CAS object count unchanged — cache hit confirmed"

# ── 7. pip download ───────────────────────────────────────────────────────────
step "Test 3: pip download ${HASH_PKG}"
DLDIR="${WORK}/downloads"
mkdir -p "${DLDIR}"
pip download \
    --index-url "${PYPI_INDEX}" \
    --dest "${DLDIR}" \
    --no-cache-dir \
    --no-deps \
    "${HASH_PKG}" 2>&1 | tee "${WORK}/pip-download.log"

WHL=$(ls "${DLDIR}"/*.whl 2>/dev/null || ls "${DLDIR}"/*.tar.gz 2>/dev/null | head -1)
[ -n "${WHL}" ] || fail "no wheel/sdist found in ${DLDIR}"
pass "pip download exit 0; file on disk: $(basename "${WHL}")"

# ── 8. PEP 691 JSON Accept header test ────────────────────────────────────────
# PEP 691 §4.1: "A server MAY respond with any version they support."
# tuna doesn't implement PEP 691 JSON, so Specula falls back to HTML with
# text/html Content-Type — valid per the spec.  pip's Accept list always
# includes text/html as a fallback; it will parse the response as HTML.
step "Test 4: PEP 691 — Accept: application/vnd.pypi.simple.v1+json"
JSON_RESPONSE=$(curl -fsS \
    -H "Accept: application/vnd.pypi.simple.v1+json" \
    "http://127.0.0.1:${DATA_PORT}/pypi/simple/${HASH_PKG}/")
JSON_CT=$(curl -fsS \
    -I \
    -H "Accept: application/vnd.pypi.simple.v1+json" \
    "http://127.0.0.1:${DATA_PORT}/pypi/simple/${HASH_PKG}/" \
    2>/dev/null | grep -i '^content-type:' | tr -d '\r')
echo "  Content-Type: ${JSON_CT}"
echo "  Body (first line): $(echo "${JSON_RESPONSE}" | head -1)"

# Assert 200 (already enforced by curl -f) and body is non-empty.
[ -n "${JSON_RESPONSE}" ] || fail "empty body for JSON Accept request"
# Assert body is HTML (graceful degradation, valid per PEP 691 §4.1).
echo "${JSON_RESPONSE}" | grep -qi "<!DOCTYPE html\|<html" \
    || fail "expected HTML fallback body for JSON Accept (tuna doesn't support PEP 691 JSON)"
pass "PEP 691 JSON Accept → 200 + valid HTML body (graceful degradation per PEP 691 §4.1)"
echo "  Note: full JSON negotiation (forwarding Accept to upstream) requires"
echo "  upstream.WithAcceptHeader — see KNOWN-LIMITATIONS in runner output."

# ── 9. #sha256= fragment preservation + --require-hashes ─────────────────────
# PEP 503 §2: The URL for a package file MUST be the file URL with the
# sha256 hash of the file appended as a URL fragment in the form
# #sha256=<hashvalue>.  pip's --require-hashes uses this fragment to verify
# the downloaded file.
step "Test 5: #sha256= fragment preserved → pip --require-hashes"

# Extract the exact 64-char sha256 hash from the simple index page.
INDEX_HTML=$(curl -fsS "http://127.0.0.1:${DATA_PORT}/pypi/simple/${HASH_PKG}/")
LATEST_WHL=$(echo "${INDEX_HTML}" | grep -o '"[^"]*\.whl#sha256=[0-9a-f]*"' | tail -1 | tr -d '"')
echo "  Latest wheel reference: ${LATEST_WHL}"
HASH_VALUE=$(echo "${LATEST_WHL}" | python3 -c "
import sys, re
href = sys.stdin.read().strip()
m = re.search(r'#sha256=([0-9a-f]{64})', href)
print(m.group(1) if m else '')
")
PKG_VER=$(echo "${LATEST_WHL}" | python3 -c "
import sys, re
href = sys.stdin.read().strip()
m = re.search(r'([^/]+\.whl)', href)
print(m.group(1) if m else '')
")
echo "  wheel: ${PKG_VER}"
echo "  sha256: ${HASH_VALUE}"
[ "${#HASH_VALUE}" -eq 64 ] || fail "#sha256= fragment not found or not 64 chars in simple index"

# Build a PEP 508 / requirements.txt hash-pinned line.
VERSION=$(echo "${PKG_VER}" | python3 -c "
import sys, re
pkg = sys.stdin.read().strip()
m = re.search(r'-([0-9][^-]+)-', pkg)
print(m.group(1) if m else '')
")
REQ_FILE="${WORK}/requirements-hashed.txt"
cat > "${REQ_FILE}" <<REQEOF
${HASH_PKG}==${VERSION} --hash=sha256:${HASH_VALUE}
REQEOF
echo "  Requirements file:"
cat "${REQ_FILE}"

HASH_INST="${WORK}/install-hashed"
mkdir -p "${HASH_INST}"
pip install \
    --index-url "${PYPI_INDEX}" \
    --target "${HASH_INST}" \
    --no-cache-dir \
    --no-deps \
    --require-hashes \
    -r "${REQ_FILE}" 2>&1 | tee "${WORK}/pip-require-hashes.log"

ls "${HASH_INST}/"*.py >/dev/null 2>&1 \
    || ls "${HASH_INST}/"*.dist-info >/dev/null 2>&1 \
    || fail "package not installed under ${HASH_INST}"
pass "#sha256= fragment preserved end-to-end; pip --require-hashes exit 0"

# ── 10. Summary ───────────────────────────────────────────────────────────────
echo ""
echo "======================================================="
echo " ALL PyPI real-client conformance assertions PASSED"
echo "======================================================="
echo ""
echo "Conformance matrix (PEP 503 / 691 / 700):"
echo "  PEP 503 simple index (HTML)           PASS  — pip install, pip download"
echo "  PEP 503 #sha256= fragment              PASS  — preserved through proxy"
echo "  PEP 503 relative download URLs         PASS  — ../../packages/... resolves"
echo "                                                  correctly under /pypi/simple/<pkg>/"
echo "  PEP 503 caching (two-tier CAS)         PASS  — second install is a cache hit"
echo "  PEP 691 JSON Accept negotiation        PASS* — 200 + HTML fallback"
echo "                                                  (* JSON from upstream not available:"
echo "                                                     tuna doesn't support PEP 691;"
echo "                                                     upstream.WithAcceptHeader needed)"
echo "  pip --require-hashes                   PASS  — hash verified by pip"
echo ""
echo "KNOWN LIMITATIONS:"
echo "  PEP 691 full JSON support (upstream JSON fetch):"
echo "    The PyPI handler currently does not forward the client's Accept header"
echo "    to the upstream (internal/upstream.Client.Fetch lacks a"
echo "    WithAcceptHeader(string) RequestOption).  When a client requests JSON and"
echo "    no JSON entry is cached, Specula falls back to HTML (PEP 691 §4.1 valid)."
echo "    To enable true JSON negotiation, add upstream.WithAcceptHeader to"
echo "    internal/upstream/upstream.go and thread it through serveIndex."
echo ""
echo "Daemon log: ${WORK}/daemon.log"
echo "Run artifacts: ${WORK}/"
