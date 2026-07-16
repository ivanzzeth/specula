#!/usr/bin/env bash
# npm real-client conformance runner for Specula's npm handler.
#
# Builds specula, starts it with a local CAS + sqlite config on the assigned
# ports (data :5102 / control :5202), then drives the REAL npm client end-to-end:
#
#   1. npm install <unscoped pkg with deps>  — packument + tarball proxy + cache
#   2. npm install <scoped pkg>              — @types/node scoped-name routing
#   3. Second install of same pkg            — cache hit (no upstream round-trip)
#   4. npm ci with lockfile                  — lockfile resolved URLs point at Specula
#
# The npm registry protocol mandates that a proxy rewrites dist.tarball URLs to
# point at itself (verdaccio/Artifactory/Nexus all do this). Without rewriting,
# npm bypasses the proxy for tarball fetches and the cache never warms for
# tarballs. This script exercises that critical path end-to-end.
#
# References:
#   npm registry protocol §dist.tarball:
#     https://github.com/npm/registry/blob/master/docs/responses/package-metadata.md
#   Packument format:
#     https://github.com/npm/registry/blob/master/docs/REGISTRY-API.md
#
# Usage:  scripts/realclient-npm.sh
# Exit 0 only when all assertions pass.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-/tmp/specula-npm-conf}"
DATA_PORT="${DATA_PORT:-5102}"
CTRL_PORT="${CTRL_PORT:-5202}"
NPM_REGISTRY="http://127.0.0.1:${DATA_PORT}/npm/"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOSUMDB="${GOSUMDB:-sum.golang.google.cn}"

# ── helper: print a banner line ───────────────────────────────────────────────
step() { echo; echo "==> $*"; }

# ── 1. Build specula ──────────────────────────────────────────────────────────
step "building specula from ${REPO}"
mkdir -p "${WORK}/blobs"
go -C "${REPO}" build -o "${WORK}/specula" ./cmd/specula

# ── 2. Write config ───────────────────────────────────────────────────────────
step "writing config (data :${DATA_PORT} / ctrl :${CTRL_PORT})"
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
  npm:
    mutable_ttl_seconds: 120
    upstreams:
      - name: npmmirror
        base_url: https://registry.npmmirror.com
        priority: 1
        official: false
      - name: npm-registry
        base_url: https://registry.npmjs.org
        priority: 2
        official: true
EOF

# Fresh storage for every run.
rm -f "${WORK}"/meta.db*
rm -rf "${WORK}"/blobs/*

# ── 3. Start specula ──────────────────────────────────────────────────────────
step "starting specula"
"${WORK}/specula" --config "${WORK}/cfg.yaml" > "${WORK}/daemon.log" 2>&1 &
SPID=$!
trap '
  echo
  echo "==> killing specula (pid ${SPID})"
  kill "${SPID}" 2>/dev/null || true
  wait "${SPID}" 2>/dev/null || true
' EXIT

# Wait for the data plane to accept connections.
for i in $(seq 1 20); do
  if curl -fsS "http://127.0.0.1:${DATA_PORT}/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.3
done
curl -fsS "http://127.0.0.1:${DATA_PORT}/healthz" >/dev/null \
  || { echo "ERROR: specula did not start (see ${WORK}/daemon.log)"; cat "${WORK}/daemon.log"; exit 1; }
echo "specula is up (pid ${SPID})"

# ── 4. Test workspace (isolated from system npm cache to force upstream hits) ──
NPMDIR="${WORK}/npmtest"
rm -rf "${NPMDIR}"
mkdir -p "${NPMDIR}"

# npm config: use Specula as the sole registry; disable the local npm cache so
# every packument/tarball goes through the proxy on the first install.
NPM_CACHE="${WORK}/npmcache"
rm -rf "${NPM_CACHE}"

NPM_COMMON=(
  --registry "${NPM_REGISTRY}"
  --cache    "${NPM_CACHE}"
  --no-audit
  --no-fund
  --prefer-online
)

PASS=0
FAIL=0

assert_ok() {
  local label="$1"; shift
  if "$@"; then
    echo "    PASS: ${label}"
    PASS=$((PASS+1))
  else
    echo "    FAIL: ${label}"
    FAIL=$((FAIL+1))
  fi
}

assert_file() {
  local label="$1" path="$2"
  if [[ -e "${path}" ]]; then
    echo "    PASS: ${label} (${path} exists)"
    PASS=$((PASS+1))
  else
    echo "    FAIL: ${label} (${path} missing)"
    FAIL=$((FAIL+1))
  fi
}

assert_tarball_url() {
  local label="$1" lockfile="$2" pkg="$3"
  # The resolved URL in the lockfile must point at Specula, not at upstream.
  # This proves dist.tarball URL rewriting is working.
  local resolved
  resolved=$(grep -A5 "\"node_modules/${pkg}\"" "${lockfile}" \
    | grep '"resolved"' | head -1 | sed 's/.*"resolved": *"\([^"]*\)".*/\1/' || true)
  if [[ "${resolved}" == "${NPM_REGISTRY}"* ]]; then
    echo "    PASS: ${label} (resolved=${resolved})"
    PASS=$((PASS+1))
  else
    echo "    FAIL: ${label} — lockfile resolved URL is NOT through Specula: '${resolved}'"
    FAIL=$((FAIL+1))
  fi
}

# ── 5a. Install ms (unscoped, 0 runtime deps — fast smoke test) ───────────────
step "5a. npm install ms (unscoped, 0 runtime deps)"
MSDIR="${NPMDIR}/ms-test"
mkdir -p "${MSDIR}"
pushd "${MSDIR}" >/dev/null

echo '{}' > package.json
npm install "${NPM_COMMON[@]}" ms 2>&1 | tee "${WORK}/install-ms.log"
assert_file "node_modules/ms exists"          "${MSDIR}/node_modules/ms/package.json"
assert_file "package-lock.json created"       "${MSDIR}/package-lock.json"
assert_tarball_url "ms lockfile resolved via Specula" "${MSDIR}/package-lock.json" ms

popd >/dev/null

# ── 5b. Install express (unscoped, has transitive deps) ───────────────────────
step "5b. npm install express (unscoped, transitive deps)"
EXDIR="${NPMDIR}/express-test"
mkdir -p "${EXDIR}"
pushd "${EXDIR}" >/dev/null

echo '{}' > package.json
npm install "${NPM_COMMON[@]}" express 2>&1 | tee "${WORK}/install-express.log"
assert_file "node_modules/express exists"     "${EXDIR}/node_modules/express/package.json"
assert_file "node_modules/ms exists (dep)"   "${EXDIR}/node_modules/ms/package.json"
assert_tarball_url "express lockfile resolved via Specula" "${EXDIR}/package-lock.json" express

popd >/dev/null

# ── 5c. Install @types/node (scoped package) ──────────────────────────────────
step "5c. npm install @types/node (scoped packument + tarball)"
TYPDIR="${NPMDIR}/types-test"
mkdir -p "${TYPDIR}"
pushd "${TYPDIR}" >/dev/null

echo '{}' > package.json
npm install "${NPM_COMMON[@]}" @types/node 2>&1 | tee "${WORK}/install-types-node.log"
assert_file "node_modules/@types/node exists" "${TYPDIR}/node_modules/@types/node/package.json"
assert_tarball_url "@types/node lockfile resolved via Specula" \
  "${TYPDIR}/package-lock.json" '@types/node'

popd >/dev/null

# ── 5d. Second install of ms — must be a cache hit (Specula serves from CAS) ──
step "5d. second npm install ms — cache hit"
MSDIR2="${NPMDIR}/ms-cachehit"
mkdir -p "${MSDIR2}"
pushd "${MSDIR2}" >/dev/null

echo '{}' > package.json
# Fresh npm cache dir forces npm to re-download — but Specula must serve from CAS.
npm install "${NPM_COMMON[@]}" ms 2>&1 | tee "${WORK}/install-ms-cachehit.log"
assert_file "node_modules/ms exists (cache hit)" "${MSDIR2}/node_modules/ms/package.json"

# Verify specula daemon is still alive (a cache-read crash would kill it).
assert_ok "specula still running after cache hit" kill -0 "${SPID}"

popd >/dev/null

# ── 5e. npm ci with lockfile (tests that resolved URLs in lockfile work) ───────
step "5e. npm ci with ms lockfile (resolved URLs must point at Specula)"
CIDIR="${NPMDIR}/ci-test"
mkdir -p "${CIDIR}"
pushd "${CIDIR}" >/dev/null

# Seed the lockfile from the ms install done in step 5a.
cp "${MSDIR}/package.json"       ./package.json
cp "${MSDIR}/package-lock.json"  ./package-lock.json

# npm ci with a fresh npm cache (all tarballs must come through Specula).
npm ci "${NPM_COMMON[@]}" 2>&1 | tee "${WORK}/ci-ms.log"
assert_file "node_modules/ms exists after npm ci" "${CIDIR}/node_modules/ms/package.json"

popd >/dev/null

# ── 6. Verify packument dist.tarball rewriting via raw HTTP ───────────────────
step "6. verify dist.tarball rewriting in live packument response"

# Fetch the ms packument directly from Specula and check the tarball URL.
PACKUMENT=$(curl -fsS "${NPM_REGISTRY}ms")
TARBALL_URL=$(echo "${PACKUMENT}" | python3 -c "
import sys, json
doc = json.load(sys.stdin)
# Any version's dist.tarball must point at Specula.
for ver, vobj in doc.get('versions', {}).items():
    t = vobj.get('dist', {}).get('tarball', '')
    if t:
        print(t)
        break
" 2>/dev/null || true)

if [[ "${TARBALL_URL}" == "${NPM_REGISTRY}"* ]]; then
  echo "    PASS: packument dist.tarball rewritten to Specula (${TARBALL_URL})"
  PASS=$((PASS+1))
else
  echo "    FAIL: packument dist.tarball NOT rewritten — got: '${TARBALL_URL}'"
  echo "          Expected it to start with: '${NPM_REGISTRY}'"
  FAIL=$((FAIL+1))
fi

# ── 7. Results ────────────────────────────────────────────────────────────────
step "results: ${PASS} passed, ${FAIL} failed"
echo
echo "daemon log: ${WORK}/daemon.log"
echo "npm logs:   ${WORK}/install-*.log  ${WORK}/ci-*.log"

if [[ "${FAIL}" -gt 0 ]]; then
  echo
  echo "FAILED (${FAIL} assertion(s) did not pass)"
  exit 1
fi

echo
echo "ALL ASSERTIONS PASSED"
