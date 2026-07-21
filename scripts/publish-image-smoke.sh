#!/usr/bin/env bash
# Build (or reuse) the Specula product image, push it into an ephemeral Specula
# hosted registry, pull it back, and assert digests match — dogfoods our own
# OCI push path while proving the container image is pushable.
#
# Usage:
#   make image-smoke
#   IMAGE=specula:v0.4.0 bash scripts/publish-image-smoke.sh
#
# Environment:
#   IMAGE         local image ref to publish (required unless built via make)
#   HOSTED_TAG    tag used on the hosted repo (default: IMAGE's tag, else "smoke")
#   WORK          scratch dir (default: mktemp)
#   SKIP_BUILD_DAEMON  if set, reuse $WORK/specula binary instead of go build
#
# Prerequisites: docker CLI + daemon; go toolchain (to start the ephemeral registry).
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$(mktemp -d /tmp/specula-image-smoke.XXXXXX)}"

. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/daemon.sh"
DATA_PORT="${DATA_PORT:-$(pick_free_port)}"
CTRL_PORT="${CTRL_PORT:-$(pick_free_port)}"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" GOSUMDB="${GOSUMDB:-sum.golang.google.cn}"

REGISTRY="127.0.0.1:${DATA_PORT}"
EMAIL="image-smoke@specula.local"
PASSWORD="password123"

IMAGE="${IMAGE:-}"
if [ -z "$IMAGE" ]; then
  echo "ERROR: IMAGE is required (e.g. IMAGE=specula:local)" >&2
  exit 1
fi

# Derive hosted tag from IMAGE if not set (specula:v1.2.3 → v1.2.3).
HOSTED_TAG="${HOSTED_TAG:-}"
if [ -z "$HOSTED_TAG" ]; then
  HOSTED_TAG="${IMAGE##*:}"
  if [ "$HOSTED_TAG" = "$IMAGE" ] || [ -z "$HOSTED_TAG" ]; then
    HOSTED_TAG="smoke"
  fi
fi
HOSTED_IMG="${REGISTRY}/default/specula:${HOSTED_TAG}"

pass() { printf '\033[0;32mPASS\033[0m %s\n' "$*"; }
fail() { printf '\033[0;31mFAIL\033[0m %s\n' "$*"; exit 1; }
step() { echo; printf '==> %s\n' "$*"; }

cleanup_docker() {
  docker rmi "$HOSTED_IMG" 2>/dev/null || true
  docker logout "$REGISTRY" 2>/dev/null || true
}

SPID=""
kill_specula() {
  [ -n "$SPID" ] || return 0
  kill "$SPID" 2>/dev/null || true
  local i=0
  while kill -0 "$SPID" 2>/dev/null && [ $i -lt 10 ]; do
    sleep 0.2; i=$((i+1))
  done
}

trap 'kill_specula; cleanup_docker' EXIT

# ── 0. Assert product image exists ───────────────────────────────────────────

step "inspect product image ${IMAGE}"
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  fail "image ${IMAGE} not found — run: make image"
fi
PRODUCT_ID=$(docker image inspect --format='{{.Id}}' "$IMAGE")
echo "  image id: ${PRODUCT_ID}"

# Optional: container reports a version string.
if docker run --rm --entrypoint /specula "$IMAGE" version >/tmp/specula-image-ver.$$ 2>&1; then
  echo "  version: $(cat /tmp/specula-image-ver.$$)"
  pass "product image runs (version)"
else
  echo "  version probe failed:" >&2
  cat /tmp/specula-image-ver.$$ >&2 || true
  fail "product image failed to run version"
fi
rm -f /tmp/specula-image-ver.$$

# ── 1. Build ephemeral Specula (registry under test) ─────────────────────────

step "building ephemeral Specula daemon"
mkdir -p "${WORK}/blobs"
if [ -z "${SKIP_BUILD_DAEMON:-}" ]; then
  go -C "$REPO" build -o "${WORK}/specula" ./cmd/specula
fi
[ -x "${WORK}/specula" ] || fail "missing ${WORK}/specula"

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
  oci:
    mutable_ttl_seconds: 300
    upstreams:
      - name: dockerhub
        base_url: https://registry-1.docker.io
        priority: 1
        official: true
EOF

step "starting Specula on :${DATA_PORT} (data) / :${CTRL_PORT} (control)"
rm -f "${WORK}"/meta.db* ; rm -rf "${WORK}"/blobs/*
"${WORK}/specula" --config "${WORK}/cfg.yaml" > "${WORK}/daemon.log" 2>&1 &
SPID=$!

wait_for_daemon "$SPID" "$DATA_PORT" "http://127.0.0.1:${DATA_PORT}/healthz" "${WORK}/daemon.log" \
  || exit 1
wait_for_daemon "$SPID" "$CTRL_PORT" "http://127.0.0.1:${CTRL_PORT}/healthz" "${WORK}/daemon.log" \
  || exit 1
pass "ephemeral Specula is up (pid ${SPID})"

# ── 2. Seed user + API key ───────────────────────────────────────────────────

step "register first user + create API key"
curl -fsS -H 'Content-Type:application/json' -X POST \
  "http://127.0.0.1:${CTRL_PORT}/api/v1/auth/register" \
  -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\",\"name\":\"Image Smoke\"}" >/dev/null

curl -c "${WORK}/cookies.txt" -fsS \
  -H 'Content-Type:application/json' -X POST \
  "http://127.0.0.1:${CTRL_PORT}/api/v1/auth/login" \
  -d "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}" >/dev/null

API_KEY=$(curl -b "${WORK}/cookies.txt" -fsS \
  -H 'Content-Type:application/json' -X POST \
  "http://127.0.0.1:${CTRL_PORT}/api/v1/keys" \
  -d '{"label":"image-smoke"}' \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["raw_key"])')
echo "  API key prefix: ${API_KEY:0:10}…"

# ── 3. Push product image into hosted OCI ────────────────────────────────────

step "docker login ${REGISTRY}"
echo "$API_KEY" | docker login "$REGISTRY" -u "$EMAIL" --password-stdin
pass "docker login succeeded"

step "tag + push ${HOSTED_IMG}"
docker tag "$IMAGE" "$HOSTED_IMG"
PUSH_OUTPUT=$(docker push "$HOSTED_IMG" 2>&1)
echo "$PUSH_OUTPUT"
PUSHED_DIGEST=$(echo "$PUSH_OUTPUT" | grep -oE 'sha256:[a-f0-9]{64}' | head -1)
[ -n "$PUSHED_DIGEST" ] || fail "could not extract pushed digest"
echo "  pushed digest: ${PUSHED_DIGEST}"
pass "docker push to Specula hosted OCI succeeded"

step "rmi + pull back"
docker rmi "$HOSTED_IMG" >/dev/null 2>&1 || true
docker pull "$HOSTED_IMG"
PULLED_DIGEST=$(docker inspect --format='{{index .RepoDigests 0}}' "$HOSTED_IMG" 2>/dev/null | sed 's/.*@//')
echo "  pushed digest: ${PUSHED_DIGEST}"
echo "  pulled digest: ${PULLED_DIGEST}"
if [ "$PUSHED_DIGEST" = "$PULLED_DIGEST" ]; then
  pass "pulled digest matches pushed digest"
else
  fail "digest mismatch: pushed=${PUSHED_DIGEST} pulled=${PULLED_DIGEST}"
fi

step "run pulled image version"
docker run --rm --entrypoint /specula "$HOSTED_IMG" version
pass "pulled image runs"

echo
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
printf '\033[0;32m IMAGE SMOKE PASSED\033[0m  %s → %s\n' "$IMAGE" "$HOSTED_IMG"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
