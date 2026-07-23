#!/usr/bin/env bash
# conda channel real-client gate for Specula.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$(mktemp -d /tmp/specula-conda-conf.XXXXXX)}"
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/daemon.sh"
DATA_PORT="${DATA_PORT:-$(pick_free_port)}"
CTRL_PORT="${CTRL_PORT:-$(pick_free_port)}"
CLI=""
command -v micromamba >/dev/null && CLI=micromamba
command -v conda >/dev/null && CLI=${CLI:-conda}
if [ -z "$CLI" ]; then echo "SKIP: micromamba/conda not installed"; exit 0; fi

# Ambient HTTP(S)_PROXY must not intercept Specula or healthz.
export NO_PROXY="${NO_PROXY:+$NO_PROXY,}127.0.0.1,localhost"
export no_proxy="${no_proxy:+$no_proxy,}127.0.0.1,localhost"

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
  conda:
    mutable_ttl_seconds: 300
    upstreams:
      - name: tuna-cloud
        base_url: https://mirrors.tuna.tsinghua.edu.cn/anaconda/cloud
        priority: 1
        official: false
      - name: anaconda-cloud
        base_url: https://conda.anaconda.org
        priority: 2
        official: true
    conda:
      channels:
        - name: conda-forge
          base_url: https://mirrors.tuna.tsinghua.edu.cn/anaconda/cloud/conda-forge
YAML

step "starting specula"
"${WORK}/specula" --config "${WORK}/cfg.yaml" > "${WORK}/daemon.log" 2>&1 &
SPID=$!
trap 'kill "$SPID" 2>/dev/null || true; wait "$SPID" 2>/dev/null || true' EXIT
wait_for_daemon "$SPID" "$DATA_PORT" "http://127.0.0.1:${DATA_PORT}/healthz" "${WORK}/daemon.log"

CHAN="http://127.0.0.1:${DATA_PORT}/conda/conda-forge"
step "fetch repodata via Specula"
code=$(curl -sS -o "${WORK}/repodata.json" -w '%{http_code}' "${CHAN}/noarch/repodata.json" || true)
test "$code" = "200"
test -s "${WORK}/repodata.json"
grep -q '"packages"' "${WORK}/repodata.json" || grep -q 'packages.conda' "${WORK}/repodata.json"

step "${CLI} search via Specula channel"
if [ "$CLI" = micromamba ]; then
  micromamba search ca-certificates -c "$CHAN" --override-channels -r "${WORK}/root" 2>&1 | tee "${WORK}/search.log" || true
else
  conda search ca-certificates -c "$CHAN" --override-channels 2>&1 | tee "${WORK}/search.log" || true
fi
echo "PASS: conda realclient (repodata OK)"
