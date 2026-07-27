#!/usr/bin/env bash
# Single-host deployment gate: `specula install` + `specula upgrade` + rollback
# on REAL systemd, in a throwaway container.
#
# This is the acceptance test for docs/SINGLE-HOST.md — the intranet one-VM
# deployment where ops work is `scp` + one command. The upgrade path cannot be
# covered by Go unit tests: it needs root, a live systemd unit, and a running
# daemon whose executable is being replaced underneath it. So it runs here.
#
# Asserts:
#   1. install    → unit active, /healthz 200, data plane answering
#   2. ETXTBSY    → cp onto the running binary FAILS (why upgrade exists at all)
#   3. upgrade    → version swapped, .prev holds the old one, no .new residue, healthy
#   4. rollback   → a failed health gate restores the previous binary and restarts
#   5. rollback   → the explicit command flips back to .prev
#   6. no-restart → binary swapped on disk, MainPID untouched, old process serving
#   7. webui      → a binary built WITHOUT web/dist boots and serves the placeholder
#                   (regression: it used to panic on boot and take the data plane down)
#
# Requires: docker, go. Set SPECULA_E2E_SINGLE_HOST=0 to skip.
# Override the systemd base image with SPECULA_SYSTEMD_IMAGE=<image>.
# Usage: scripts/single-host-upgrade.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CONTAINER="${SPECULA_SH_CONTAINER:-specula-single-host-gate}"
IMAGE="${SPECULA_SYSTEMD_IMAGE:-specula-systemd-gate:local}"
BASE_IMAGE="${SPECULA_SYSTEMD_BASE:-debian:12}"
WORK="$(mktemp -d)"
CTRL_PORT=7733
DATA_PORT=7732

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "single-host-upgrade: required command not found: $1" >&2
    exit 1
  }
}
need docker
need go

STEP=0
step() {
  STEP=$((STEP + 1))
  echo
  echo "==> [${STEP}] $*"
}
fail() {
  echo "single-host-upgrade: FAIL — $*" >&2
  exit 1
}
# dexec runs in the container; every assertion goes through it.
dexec() { docker exec "${CONTAINER}" sh -c "$1"; }

cleanup() {
  docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
  rm -rf "${WORK}"
  # web/dist/index.html is moved aside for the no-dist build (step 7).
  if [ -f "${ROOT}/web/dist/.index.html.gate-bak" ]; then
    mv "${ROOT}/web/dist/.index.html.gate-bak" "${ROOT}/web/dist/index.html"
  fi
}
trap cleanup EXIT

# ── binaries ─────────────────────────────────────────────────────────────────
# Container arch must match the host's docker arch, not the host's GOARCH name.
case "$(uname -m)" in
  arm64 | aarch64) GOARCH=arm64 ;;
  x86_64 | amd64) GOARCH=amd64 ;;
  *) fail "unsupported arch $(uname -m)" ;;
esac

build_binary() {
  local version="$1" out="$2"
  GOOS=linux GOARCH="${GOARCH}" CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/ivanzzeth/specula/internal/version.Version=${version}" \
    -o "${out}" "${ROOT}/cmd/specula" || fail "build ${version}"
}

step "cross-compiling linux/${GOARCH} binaries (old, new)"
(cd "${ROOT}" && build_binary sh-old "${WORK}/specula-old")
(cd "${ROOT}" && build_binary sh-new "${WORK}/specula-new")

step "cross-compiling a binary WITHOUT web/dist (step 7 regression)"
if [ -f "${ROOT}/web/dist/index.html" ]; then
  mv "${ROOT}/web/dist/index.html" "${ROOT}/web/dist/.index.html.gate-bak"
fi
(cd "${ROOT}" && build_binary sh-nodist "${WORK}/specula-nodist")
if [ -f "${ROOT}/web/dist/.index.html.gate-bak" ]; then
  mv "${ROOT}/web/dist/.index.html.gate-bak" "${ROOT}/web/dist/index.html"
fi

# ── systemd container ────────────────────────────────────────────────────────
if ! docker image inspect "${IMAGE}" >/dev/null 2>&1; then
  step "building systemd test image ${IMAGE} from ${BASE_IMAGE}"
  # systemd as PID 1 + curl for the health assertions. Nothing Specula-specific:
  # the binary is side-loaded so the image stays reusable across runs.
  docker build -t "${IMAGE}" -f - "${WORK}" <<EOF || fail "docker build ${IMAGE} (network?)"
FROM ${BASE_IMAGE}
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*
STOPSIGNAL SIGRTMIN+3
CMD ["/lib/systemd/systemd"]
EOF
else
  echo "==> reusing systemd image ${IMAGE}"
fi

step "starting container ${CONTAINER} with systemd as PID 1"
docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
docker run -d --name "${CONTAINER}" --privileged \
  --tmpfs /run --tmpfs /run/lock \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
  -v "${WORK}:/mnt:ro" \
  "${IMAGE}" >/dev/null

for _ in $(seq 1 30); do
  if dexec 'systemctl is-system-running 2>/dev/null | grep -qE "running|degraded"'; then break; fi
  sleep 1
done
dexec 'systemctl is-system-running 2>/dev/null | grep -qE "running|degraded"' \
  || fail "systemd did not come up inside the container"

# ── 1. install ───────────────────────────────────────────────────────────────
step "1. specula install (systemd unit, system user, embedded config)"
dexec 'cp /mnt/specula-old /tmp/specula-old && chmod +x /tmp/specula-old && /tmp/specula-old install' \
  || fail "specula install"

wait_healthy() {
  # -s not -fsS: a restart in progress makes curl shout "Failed to connect" on
  # stderr every second, which reads like a failure in the middle of a passing run.
  for _ in $(seq 1 30); do
    if dexec "curl -fs -o /dev/null http://127.0.0.1:${CTRL_PORT}/healthz 2>/dev/null"; then return 0; fi
    sleep 1
  done
  return 1
}
wait_healthy || fail "/healthz never turned 200 after install"
dexec 'test "$(systemctl is-active specula.service)" = active' || fail "unit not active"
# The data plane must answer too: a 401 registry challenge means it is alive.
dexec "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:${DATA_PORT}/v2/ | grep -qE '^(200|401)$'" \
  || fail "data plane /v2/ not answering"
dexec 'test "$(systemctl show -p NRestarts --value specula.service)" = 0' \
  || fail "daemon restarted after install — it should have come up clean"
echo "    OK: active, /healthz 200, /v2/ answering, NRestarts=0"

# ── 2. ETXTBSY ───────────────────────────────────────────────────────────────
step "2. cp onto the running binary must fail (ETXTBSY — the reason upgrade exists)"
if dexec 'cp /mnt/specula-new /usr/local/bin/specula 2>/dev/null'; then
  fail "cp onto a running executable SUCCEEDED — the ETXTBSY premise no longer holds"
fi
dexec '/usr/local/bin/specula version | grep -q sh-old' || fail "binary changed despite failed cp"
echo "    OK: 'Text file busy', binary untouched"

# ── 3. upgrade ───────────────────────────────────────────────────────────────
step "3. specula upgrade (old → new) while the daemon is live"
dexec 'cp /mnt/specula-new /tmp/specula-new && chmod +x /tmp/specula-new && /tmp/specula-new upgrade' \
  || fail "upgrade exited non-zero"
dexec '/usr/local/bin/specula version | grep -q sh-new' || fail "installed binary is not the new one"
dexec '/usr/local/bin/specula.prev version | grep -q sh-old' || fail ".prev does not hold the old binary"
dexec 'test ! -e /usr/local/bin/specula.new' || fail "staging file .new survived"
dexec 'test "$(systemctl is-active specula.service)" = active' || fail "unit not active after upgrade"
wait_healthy || fail "/healthz not 200 after upgrade"
echo "    OK: version swapped, .prev kept, no .new residue, healthy"

# ── 4. auto-rollback on a failed health gate ─────────────────────────────────
step "4. failed health gate must auto-rollback"
# The daemon is fine; the probe is pointed at a dead port to force the gate to
# fail deterministically. What is under test is the rollback machinery — restore
# .prev, restart, exit non-zero — not the daemon's own health.
dexec "sed 's/control_plane_addr: \"0.0.0.0:${CTRL_PORT}\"/control_plane_addr: \"0.0.0.0:19999\"/' \
        /etc/specula/specula.yaml > /tmp/deadhealth.yaml" || fail "could not write probe-only config"
dexec 'grep -q "0.0.0.0:19999" /tmp/deadhealth.yaml' || fail "probe config not rewritten"

if dexec '/tmp/specula-old upgrade --config /tmp/deadhealth.yaml --health-timeout 5s 2>/tmp/upg.log'; then
  fail "upgrade with an unreachable health endpoint exited 0 — it must fail"
fi
dexec 'grep -q "rolled back" /tmp/upg.log' || fail "output does not report a rollback"
dexec '/usr/local/bin/specula version | grep -q sh-new' \
  || fail "binary was not restored to the pre-upgrade version"
wait_healthy || fail "/healthz not 200 after rollback — the host was left broken"
echo "    OK: exit≠0, 'rolled back', previous binary restored, service healthy"

# ── 5. explicit rollback ─────────────────────────────────────────────────────
step "5. specula rollback flips back to .prev"
# After step 4 .prev holds sh-new, so install sh-old cleanly first to make the
# flip observable.
dexec '/tmp/specula-old upgrade' || fail "downgrade to old failed"
dexec '/usr/local/bin/specula version | grep -q sh-old' || fail "downgrade did not take"
dexec '/usr/local/bin/specula rollback' || fail "rollback exited non-zero"
dexec '/usr/local/bin/specula version | grep -q sh-new' || fail "rollback did not restore sh-new"
wait_healthy || fail "/healthz not 200 after explicit rollback"
echo "    OK: rollback restored sh-new, healthy"

# ── 6. --no-restart ──────────────────────────────────────────────────────────
step "6. --no-restart swaps the binary without touching the running process"
PID_BEFORE="$(dexec 'systemctl show -p MainPID --value specula.service')"
dexec '/tmp/specula-old upgrade --no-restart' || fail "--no-restart exited non-zero"
PID_AFTER="$(dexec 'systemctl show -p MainPID --value specula.service')"
[ "${PID_BEFORE}" = "${PID_AFTER}" ] || fail "MainPID changed (${PID_BEFORE} → ${PID_AFTER}) despite --no-restart"
dexec '/usr/local/bin/specula version | grep -q sh-old' || fail "binary on disk was not swapped"
dexec "curl -fsS -o /dev/null http://127.0.0.1:${CTRL_PORT}/healthz" \
  || fail "the still-running old process stopped serving"
echo "    OK: disk binary swapped, MainPID ${PID_AFTER} unchanged, still serving"

# ── 7. WebUI-less binary must boot ───────────────────────────────────────────
step "7. a binary built without web/dist must boot and serve the placeholder"
# Regression: this used to panic in web/embed.go on startup ("read dist/index.html"),
# killing the data plane too — in CN that means no node can pull, including the
# images needed to fix it.
dexec 'cp /mnt/specula-nodist /tmp/specula-nodist && chmod +x /tmp/specula-nodist && /tmp/specula-nodist upgrade' \
  || fail "upgrade to the no-dist binary failed — it should boot and pass the health gate"
dexec '/usr/local/bin/specula version | grep -q sh-nodist' || fail "no-dist binary not installed"
dexec "curl -fsS http://127.0.0.1:${CTRL_PORT}/ | grep -q 'WebUI not built'" \
  || fail "control plane did not serve the placeholder page"
dexec "curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1:${CTRL_PORT}/cache/oci | grep -q 200" \
  || fail "SPA deep link did not fall back to the placeholder"
dexec 'journalctl -u specula.service --no-pager | grep -q "WebUI bundle is NOT embedded"' \
  || fail "startup WARN about the missing bundle is absent from the journal"
echo "    OK: boots, placeholder served on / and deep links, WARN logged"

echo
echo "single-host-upgrade: ALL ${STEP} STEPS PASSED"
