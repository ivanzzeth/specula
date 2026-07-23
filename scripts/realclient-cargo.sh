#!/usr/bin/env bash
# cargo sparse-registry real-client gate for Specula.
set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$(mktemp -d /tmp/specula-cargo-conf.XXXXXX)}"
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/daemon.sh"
DATA_PORT="${DATA_PORT:-$(pick_free_port)}"
CTRL_PORT="${CTRL_PORT:-$(pick_free_port)}"
command -v cargo >/dev/null || { echo "SKIP: cargo not installed"; exit 0; }

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
  cargo:
    mutable_ttl_seconds: 300
    upstreams:
      - name: index-crates-io
        base_url: https://index.crates.io
        priority: 1
        official: true
    cargo:
      dl_upstreams:
        - name: static-crates-io
          base_url: https://static.crates.io
          priority: 1
          official: true
YAML

step "starting specula"
"${WORK}/specula" --config "${WORK}/cfg.yaml" > "${WORK}/daemon.log" 2>&1 &
SPID=$!
trap 'kill "$SPID" 2>/dev/null || true; wait "$SPID" 2>/dev/null || true' EXIT
wait_for_daemon "$SPID" "$DATA_PORT" "http://127.0.0.1:${DATA_PORT}/healthz" "${WORK}/daemon.log"

step "cargo fetch via Specula sparse index"
mkdir -p "${WORK}/proj/src"
cat > "${WORK}/proj/Cargo.toml" <<'TOM'
[package]
name = "specula-rc"
version = "0.1.0"
edition = "2021"
[dependencies]
cfg-if = "1.0"
TOM
echo 'fn main() {}' > "${WORK}/proj/src/main.rs"
mkdir -p "${WORK}/cargo-home"
cat > "${WORK}/cargo-home/config.toml" <<TOM
[source.crates-io]
replace-with = "specula"
[source.specula]
registry = "sparse+http://127.0.0.1:${DATA_PORT}/cargo/index/"
TOM
export CARGO_HOME="${WORK}/cargo-home"
(cd "${WORK}/proj" && cargo fetch -v) 2>&1 | tee "${WORK}/cargo1.log"
# Prove the crate landed in the local registry cache (verbose log text varies by cargo version).
crate="$(find "${CARGO_HOME}" -type f -name 'cfg-if-*.crate' -print -quit 2>/dev/null || true)"
test -n "$crate"

step "second fetch (cache warm)"
(cd "${WORK}/proj" && cargo fetch -v) 2>&1 | tee "${WORK}/cargo2.log"
echo "PASS: cargo realclient"
