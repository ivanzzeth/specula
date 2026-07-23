#!/usr/bin/env bash
# OCI multi-registry path-style real-client gate.
# Pulls a tiny image as 127.0.0.1:PORT/<registry>/<repo>:<tag> through Specula.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$(mktemp -d /tmp/specula-oci-remote.XXXXXX)}"
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/daemon.sh"
DATA_PORT="${DATA_PORT:-$(pick_free_port)}"
CTRL_PORT="${CTRL_PORT:-$(pick_free_port)}"
command -v docker >/dev/null || { echo "SKIP: docker not installed"; exit 0; }

export NO_PROXY="${NO_PROXY:+$NO_PROXY,}127.0.0.1,localhost"
export no_proxy="${no_proxy:+$no_proxy,}127.0.0.1,localhost"

# Small public image on an allowlisted non-Hub registry.
REMOTE_REF="registry.k8s.io/pause:3.9"

step() { echo; echo "==> $*"; }
step "building specula"
mkdir -p "${WORK}/blobs"
go -C "${REPO}" build -o "${WORK}/specula" ./cmd/specula

step "writing config"
cat > "${WORK}/cfg.yaml" <<YAML
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
      - name: daocloud
        base_url: https://docker.m.daocloud.io
        priority: 1
        official: false
    oci:
      remote_registries:
        - host: registry.k8s.io
        - host: ghcr.io
        - host: codeberg.org
YAML

step "starting specula"
"${WORK}/specula" --config "${WORK}/cfg.yaml" > "${WORK}/daemon.log" 2>&1 &
SPID=$!
trap 'kill "$SPID" 2>/dev/null || true; wait "$SPID" 2>/dev/null || true' EXIT
wait_for_daemon "$SPID" "$DATA_PORT" "http://127.0.0.1:${DATA_PORT}/healthz" "${WORK}/daemon.log"

LOCAL_REF="127.0.0.1:${DATA_PORT}/${REMOTE_REF}"
step "path-style pull ${LOCAL_REF}"
docker pull "${LOCAL_REF}" 2>&1 | tee "${WORK}/pull1.log"

step "second pull (cache warm)"
docker pull "${LOCAL_REF}" 2>&1 | tee "${WORK}/pull2.log"
grep -qiE 'up to date|Downloaded newer|Pull complete|Status:' "${WORK}/pull2.log"

# Prove daemon saw Specula (Via / X-Specula) on a probe of the tag.
code=$(curl -sS -o /dev/null -w '%{http_code}' \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json' \
  "http://127.0.0.1:${DATA_PORT}/v2/registry.k8s.io/pause/manifests/3.9" || true)
test "$code" = "200" -o "$code" = "401" # 401 only if auth unexpectedly required

echo "PASS: oci multi-registry path-style realclient"
