#!/usr/bin/env bash
# OCI Distribution Spec conformance runner for Specula's writable registry.
#
# Builds specula, starts it with a local CAS + sqlite config, seeds the first
# user (= admin + owner of org "default"), then runs the official
# opencontainers/distribution-spec conformance suite against /v2/.
#
# The conformance binary is built once from the module cache (fetched via
# goproxy.cn) into $CONF_BIN. Set OCI_CONFORMANCE_BIN to reuse an existing one.
#
# Usage:  scripts/oci-conformance.sh
# Exit 0 only if the conformance suite passes.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-/tmp/specula-oci-conf}"
CONF_BIN="${OCI_CONFORMANCE_BIN:-$WORK/conformance.test}"
# Ports default to FREE ones, never 5000/8080.
#
# This is not tidiness. When these defaulted to 5000/8080 and something else
# already held them (a demo instance), our daemon lost the bind and exited — but
# it is started in the background, so `set -e` never saw it, and the whole
# conformance suite silently ran against THE OTHER SERVER. It reported a pass for
# a binary it never touched, and registered its first user into someone else's
# database. A gate that quietly grades the wrong process is worse than no gate.
#
# pick_free_port and wait_for_daemon now live in lib/daemon.sh and are shared by every
# script that starts a daemon. See that file for why the liveness+health check this script
# originally used is NOT sufficient on its own (it was demonstrated to pass while a
# different server answered) and why socket ownership is asserted instead.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/daemon.sh"
DATA_PORT="${DATA_PORT:-$(pick_free_port)}"
CTRL_PORT="${CTRL_PORT:-$(pick_free_port)}"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" GOSUMDB="${GOSUMDB:-sum.golang.google.cn}"

mkdir -p "$WORK/blobs"

# 1. Build the conformance binary from the official module (once).
if [[ ! -x "$CONF_BIN" ]]; then
  echo "==> building OCI conformance.test via $GOPROXY"
  tmp="$WORK/confsrc"; mkdir -p "$tmp"; ( cd "$tmp"
    [[ -f go.mod ]] || printf 'module ociconfrunner\ngo 1.24\n' > go.mod
    GOFLAGS=-mod=mod go get github.com/opencontainers/distribution-spec/conformance@latest )
  CONF_DIR="$(cd "$tmp" && go list -m -f '{{.Dir}}' github.com/opencontainers/distribution-spec/conformance)"
  ( cd "$CONF_DIR" && go test -c -o "$CONF_BIN" . )
fi

# 2. Build & start specula.
echo "==> building specula"
go -C "$REPO" build -o "$WORK/specula" ./cmd/specula
cat > "$WORK/cfg.yaml" <<EOF
server:
  data_plane_addr: ":$DATA_PORT"
  control_plane_addr: ":$CTRL_PORT"
auth:
  registry_token_key_path: $WORK/regkey.pem
storage:
  blob: {driver: local, local: {root: $WORK/blobs}}
  meta: {driver: sqlite, dsn: $WORK/meta.db}
EOF
rm -f "$WORK"/meta.db* ; rm -rf "$WORK"/blobs/*
"$WORK/specula" --config "$WORK/cfg.yaml" > "$WORK/daemon.log" 2>&1 &
SPID=$!
trap 'kill $SPID 2>/dev/null || true' EXIT

# Prove OUR daemon is the one answering before trusting a single result: the kernel must
# confirm both sockets belong to $SPID. The suite drives the data plane and seeds its user
# through the control plane, so ownership of both is asserted.
wait_for_daemon "$SPID" "$CTRL_PORT" "http://127.0.0.1:$CTRL_PORT/healthz" "$WORK/daemon.log" || exit 1
wait_for_daemon "$SPID" "$DATA_PORT" "http://127.0.0.1:$DATA_PORT/healthz" "$WORK/daemon.log" || exit 1
echo "==> specula up (pid $SPID, data :$DATA_PORT, control :$CTRL_PORT)"

# 3. Seed the first user (admin + owner of org "default").
curl -fsS -H Content-Type:application/json -X POST \
  "http://127.0.0.1:$CTRL_PORT/api/v1/auth/register" \
  -d '{"email":"conf@specula.local","password":"password123","name":"Conformance"}' >/dev/null
echo "==> seeded first user (owner of org 'default')"

# 4. Run the conformance suite.
export OCI_ROOT_URL="http://127.0.0.1:$DATA_PORT"
export OCI_NAMESPACE="default/conformance"
export OCI_CROSSMOUNT_NAMESPACE="default/conformance2"
export OCI_USERNAME="conf@specula.local"
export OCI_PASSWORD="password123"
export OCI_TEST_PULL=1 OCI_TEST_PUSH=1 OCI_TEST_CONTENT_DISCOVERY=1 OCI_TEST_CONTENT_MANAGEMENT=1
echo "==> running OCI distribution-spec conformance against $OCI_ROOT_URL"
# Run from $WORK: the suite writes its report artifacts (junit.xml, report.html,
# results.yaml) into a results/ directory relative to the working directory, and
# those are throwaway run output that must not land in the repo tree.
mkdir -p "$WORK/run"
( cd "$WORK/run" && "$CONF_BIN" )
echo "==> conformance report artifacts in $WORK/run/results/"
