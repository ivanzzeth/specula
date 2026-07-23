#!/usr/bin/env bash
# scripts/realclient-multisource.sh — path-strip smoke for apt/helm/conda allowlists.
#
# Spins a tiny upstream HTTP server + Specula with named sources, then asserts
# that /apt|helm|conda/<name>/… strips <name> before contacting the upstream.
# Unknown names must 404.
#
# Usage: scripts/realclient-multisource.sh
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$(mktemp -d /tmp/specula-multisource.XXXXXX)}"
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/daemon.sh"
DATA_PORT="${DATA_PORT:-$(pick_free_port)}"
CTRL_PORT="${CTRL_PORT:-$(pick_free_port)}"
UP_PORT="${UP_PORT:-$(pick_free_port)}"

mkdir -p "${WORK}/blobs" "${WORK}/up"

# Upstream records every request path into hits.log and serves tiny fixtures.
cat > "${WORK}/up/server.py" <<'PY'
import http.server, os, pathlib
ROOT = pathlib.Path(os.environ["UP_ROOT"])
LOG = ROOT / "hits.log"
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        LOG.write_text(LOG.read_text() + self.path + "\n" if LOG.exists() else self.path + "\n")
        body = b"ok\n"
        if self.path.endswith("index.yaml"):
            body = b"apiVersion: v1\nentries: {}\n"
        elif self.path.endswith("repodata.json"):
            body = b'{"packages":{}}\n'
        elif "InRelease" in self.path:
            body = b"Origin: Test\n"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
http.server.HTTPServer(("127.0.0.1", int(os.environ["UP_PORT"])), H).serve_forever()
PY
: > "${WORK}/up/hits.log"
UP_ROOT="${WORK}/up" UP_PORT="$UP_PORT" python3 "${WORK}/up/server.py" &
UP_PID=$!
trap 'kill "$UP_PID" "$SPID" 2>/dev/null || true' EXIT
sleep 0.2

echo "==> building specula"
go -C "$REPO" build -o "${WORK}/specula" ./cmd/specula

cat > "${WORK}/cfg.yaml" <<YAML
server:
  data_plane_addr: ":${DATA_PORT}"
  control_plane_addr: ":${CTRL_PORT}"
storage:
  blob: {driver: local, local: {root: ${WORK}/blobs}}
  meta: {driver: sqlite, dsn: ${WORK}/meta.db}
protocols:
  apt:
    mutable_ttl_seconds: 0
    upstreams:
      - name: unused
        base_url: http://127.0.0.1:1
        priority: 1
    apt:
      repositories:
        - name: ubuntu
          base_url: http://127.0.0.1:${UP_PORT}
    verification: {tiers: [tofu], quorum: 1, tofu: enforce}
  helm:
    mutable_ttl_seconds: 1800
    upstreams:
      - name: unused
        base_url: http://127.0.0.1:1
        priority: 1
    helm:
      repositories:
        - name: bitnami
          base_url: http://127.0.0.1:${UP_PORT}
    verification: {tiers: [tofu], quorum: 1, tofu: enforce}
  conda:
    mutable_ttl_seconds: 300
    upstreams:
      - name: unused
        base_url: http://127.0.0.1:1
        priority: 1
    conda:
      channels:
        - name: conda-forge
          base_url: http://127.0.0.1:${UP_PORT}
    verification: {tiers: [tofu], quorum: 1, tofu: enforce}
YAML

echo "==> starting specula"
"${WORK}/specula" --config "${WORK}/cfg.yaml" > "${WORK}/daemon.log" 2>&1 &
SPID=$!
wait_for_daemon "$SPID" "$DATA_PORT" "http://127.0.0.1:${DATA_PORT}/healthz" "${WORK}/daemon.log"

assert_200_strip() {
  local label="$1" url="$2" want_path="$3"
  code=$(curl -sS -o /dev/null -w '%{http_code}' "$url" || true)
  test "$code" = "200" || { echo "FAIL $label status=$code"; exit 1; }
  grep -qxF "$want_path" "${WORK}/up/hits.log" || {
    echo "FAIL $label expected upstream path $want_path; hits:"; cat "${WORK}/up/hits.log"; exit 1
  }
  echo "PASS $label → $want_path"
}

: > "${WORK}/up/hits.log"
assert_200_strip "apt strip" \
  "http://127.0.0.1:${DATA_PORT}/apt/ubuntu/dists/jammy/InRelease" \
  "/dists/jammy/InRelease"

: > "${WORK}/up/hits.log"
assert_200_strip "helm strip" \
  "http://127.0.0.1:${DATA_PORT}/helm/bitnami/index.yaml" \
  "/index.yaml"

: > "${WORK}/up/hits.log"
assert_200_strip "conda strip" \
  "http://127.0.0.1:${DATA_PORT}/conda/conda-forge/linux-64/repodata.json" \
  "/linux-64/repodata.json"

for url in \
  "http://127.0.0.1:${DATA_PORT}/apt/evil/dists/jammy/InRelease" \
  "http://127.0.0.1:${DATA_PORT}/helm/evil/index.yaml" \
  "http://127.0.0.1:${DATA_PORT}/conda/evil/linux-64/repodata.json"
do
  code=$(curl -sS -o /dev/null -w '%{http_code}' "$url" || true)
  test "$code" = "404" || { echo "FAIL unknown source should 404: $url → $code"; exit 1; }
done
echo "PASS unknown sources → 404"

echo "PASS: multisource realclient"
