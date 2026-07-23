#!/usr/bin/env bash
# HuggingFace Hub real-client gate for Specula.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$(mktemp -d /tmp/specula-hf-conf.XXXXXX)}"
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/daemon.sh"
DATA_PORT="${DATA_PORT:-$(pick_free_port)}"
CTRL_PORT="${CTRL_PORT:-$(pick_free_port)}"

if ! python3 -c 'import huggingface_hub' 2>/dev/null; then
  echo "SKIP: python package huggingface_hub not installed"
  exit 0
fi

# Ambient HTTP(S)_PROXY must not intercept Specula or healthz.
export NO_PROXY="${NO_PROXY:+$NO_PROXY,}127.0.0.1,localhost"
export no_proxy="${no_proxy:+$no_proxy,}127.0.0.1,localhost"
export HF_HUB_DISABLE_TELEMETRY=1
# Force HTTP downloads through Specula (Xet CDN would bypass the proxy).
export HF_HUB_DISABLE_XET=1
unset HF_DEBUG

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
  hf:
    mutable_ttl_seconds: 120
    upstreams:
      - name: hf-mirror
        base_url: https://hf-mirror.com
        priority: 1
        official: false
      - name: huggingface
        base_url: https://huggingface.co
        priority: 2
        official: true
YAML

step "starting specula"
"${WORK}/specula" --config "${WORK}/cfg.yaml" > "${WORK}/daemon.log" 2>&1 &
SPID=$!
trap 'kill "$SPID" 2>/dev/null || true; wait "$SPID" 2>/dev/null || true' EXIT
wait_for_daemon "$SPID" "$DATA_PORT" "http://127.0.0.1:${DATA_PORT}/healthz" "${WORK}/daemon.log"

export HF_ENDPOINT="http://127.0.0.1:${DATA_PORT}/hf"
TINY="hf-internal-testing/tiny-random-bert"
OUT="${WORK}/model"

step "download tiny repo files via HF_ENDPOINT"
python3 - <<PY
from huggingface_hub import hf_hub_download
repo = "$TINY"
out = "$OUT"
for name in ("config.json", "tokenizer_config.json", "vocab.txt"):
    path = hf_hub_download(repo, name, local_dir=out)
    print("got", path)
PY
test -f "${OUT}/config.json"

step "second download (cache warm)"
python3 - <<PY
from huggingface_hub import hf_hub_download
repo = "$TINY"
out = "${OUT}-2"
path = hf_hub_download(repo, "config.json", local_dir=out)
print("got", path)
PY
test -f "${OUT}-2/config.json"
echo "PASS: hf realclient"
