#!/usr/bin/env bash
# Offline mode real-client gate: warm cache online, restart offline, hit + miss.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$(mktemp -d /tmp/specula-offline.XXXXXX)}"
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/daemon.sh"
DATA_PORT="${DATA_PORT:-$(pick_free_port)}"
CTRL_PORT="${CTRL_PORT:-$(pick_free_port)}"
command -v docker >/dev/null || { echo "SKIP: docker not installed"; exit 0; }

export NO_PROXY="${NO_PROXY:+$NO_PROXY,}127.0.0.1,localhost"
export no_proxy="${no_proxy:+$no_proxy,}127.0.0.1,localhost"

REMOTE_REF="registry.k8s.io/pause:3.9"
LOCAL_REF="127.0.0.1:${DATA_PORT}/${REMOTE_REF}"

step() { echo; echo "==> $*"; }
step "building specula"
mkdir -p "${WORK}/blobs"
go -C "${REPO}" build -o "${WORK}/specula" ./cmd/specula

write_cfg() {
  local mode="$1"
  cat > "${WORK}/cfg.yaml" <<YAML
server:
  data_plane_addr: ":${DATA_PORT}"
  control_plane_addr: ":${CTRL_PORT}"
  mode: ${mode}
auth:
  registry_token_key_path: ${WORK}/regkey.pem
storage:
  blob: {driver: local, local: {root: ${WORK}/blobs}}
  meta: {driver: sqlite, dsn: ${WORK}/meta.db}
protocols:
  oci:
    mutable_ttl_seconds: 300
    upstreams:
      - name: daocloud
        base_url: https://docker.m.daocloud.io
        priority: 1
        official: false
    oci:
      remote_registries:
        - host: registry.k8s.io
YAML
}

start_daemon() {
  "${WORK}/specula" --config "${WORK}/cfg.yaml" > "${WORK}/daemon.log" 2>&1 &
  SPID=$!
  wait_for_daemon "$SPID" "$DATA_PORT" "http://127.0.0.1:${DATA_PORT}/healthz" "${WORK}/daemon.log"
}

stop_daemon() {
  if [[ -n "${SPID:-}" ]]; then
    kill "$SPID" 2>/dev/null || true
    wait "$SPID" 2>/dev/null || true
    SPID=""
  fi
}
trap 'stop_daemon' EXIT

step "online warm pull ${LOCAL_REF}"
write_cfg online
start_daemon
docker pull "${LOCAL_REF}" 2>&1 | tee "${WORK}/pull-warm.log"
stop_daemon

step "offline restart — cached pull must succeed"
write_cfg offline
start_daemon
docker pull "${LOCAL_REF}" 2>&1 | tee "${WORK}/pull-hit.log"
grep -qiE 'up to date|Downloaded newer|Pull complete|Status:' "${WORK}/pull-hit.log"

step "offline miss — uncached tag must fail"
MISS_REF="127.0.0.1:${DATA_PORT}/registry.k8s.io/pause:this-tag-does-not-exist-offline"
set +e
docker pull "${MISS_REF}" >"${WORK}/pull-miss.out" 2>"${WORK}/pull-miss.log"
miss_rc=$?
set -e
if [[ "$miss_rc" -eq 0 ]]; then
  echo "FATAL: offline miss should fail" >&2
  cat "${WORK}/pull-miss.log" >&2
  exit 1
fi
code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json' \
  "http://127.0.0.1:${DATA_PORT}/v2/registry.k8s.io/pause/manifests/this-tag-does-not-exist-offline" || true)
if [[ "$code" != "404" && "$code" != "401" ]]; then
  echo "FATAL: expected manifest probe 404/401, got ${code}" >&2
  exit 1
fi

echo "PASS: offline mode realclient"
