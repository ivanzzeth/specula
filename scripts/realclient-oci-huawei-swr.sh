#!/usr/bin/env bash
# scripts/realclient-oci-huawei-swr.sh — live Huawei SWR nested path_prefix gate.
#
# Proves the production CN layout Specula must speak:
#
#   client:  127.0.0.1:PORT/registry.k8s.io/pause:3.9
#   SWR:     /v2/ddn-k8s/registry.k8s.io/pause/…   (layout: huawei-ddn)
#   NOT:     /v2/pause/…                            (transparent — 404 on SWR)
#
# A local recording reverse proxy sits as the only CN "mirror". Specula is
# configured with layout: huawei-ddn against that proxy. After docker pull we
# assert the proxy saw the nested path — so a missing path_prefix cannot pass
# by silently falling through to registry.k8s.io origin.
#
# Needs: docker, network, python3. Skips cleanly when SWR is unreachable.
#
# Usage:  scripts/realclient-oci-huawei-swr.sh
# Env:    SPECULA_E2E_HUAWEI_SWR=0  → skip
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$(mktemp -d /tmp/specula-oci-huawei-swr.XXXXXX)}"
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/daemon.sh"
DATA_PORT="${DATA_PORT:-$(pick_free_port)}"
CTRL_PORT="${CTRL_PORT:-$(pick_free_port)}"
PROXY_PORT="${PROXY_PORT:-$(pick_free_port)}"
MISS_PORT="${MISS_PORT:-$(pick_free_port)}"

if [[ "${SPECULA_E2E_HUAWEI_SWR:-}" == "0" || "${SPECULA_E2E_HUAWEI_SWR:-}" == "false" ]]; then
  echo "SKIP: SPECULA_E2E_HUAWEI_SWR=0"
  exit 0
fi

command -v docker >/dev/null || { echo "SKIP: docker not installed"; exit 0; }
command -v python3 >/dev/null || { echo "SKIP: python3 not installed"; exit 0; }
command -v curl >/dev/null || { echo "SKIP: curl not installed"; exit 0; }

export NO_PROXY="${NO_PROXY:+$NO_PROXY,}127.0.0.1,localhost"
export no_proxy="${no_proxy:+$no_proxy,}127.0.0.1,localhost"

SWR_ORIGIN="https://swr.cn-north-4.myhuaweicloud.com"
REMOTE_REF="registry.k8s.io/pause:3.9"
ACCEPT='application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json'

step() { echo; echo "==> $*"; }

step "probe live SWR nested vs transparent paths"
NESTED_CODE=$(curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 8 --max-time 30 \
  -H "Accept: ${ACCEPT}" \
  "${SWR_ORIGIN}/v2/ddn-k8s/registry.k8s.io/pause/manifests/3.9" || echo "000")
FLAT_CODE=$(curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 8 --max-time 30 \
  -H "Accept: ${ACCEPT}" \
  "${SWR_ORIGIN}/v2/pause/manifests/3.9" || echo "000")
echo "  nested /v2/ddn-k8s/registry.k8s.io/pause/… → HTTP ${NESTED_CODE}"
echo "  flat   /v2/pause/…                         → HTTP ${FLAT_CODE}"

# 200 = anonymous ok; 401 = path exists (bearer challenge). Anything else → skip.
case "${NESTED_CODE}" in
  200|401) ;;
  *)
    echo "SKIP: Huawei SWR nested path unreachable (HTTP ${NESTED_CODE})"
    exit 0
    ;;
esac
if [[ "${FLAT_CODE}" != "404" ]]; then
  echo "WARN: expected flat SWR path to 404 (got ${FLAT_CODE}); continuing — nested assert still decisive"
fi

mkdir -p "${WORK}/blobs" "${WORK}/proxy"

# Transparent mirror that always 404s — forces failover onto the SWR proxy.
cat > "${WORK}/proxy/miss.py" <<'PY'
import http.server, os
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        self.send_response(404)
        self.end_headers()
    def do_HEAD(self):
        self.send_response(404)
        self.end_headers()
http.server.HTTPServer(("127.0.0.1", int(os.environ["MISS_PORT"])), H).serve_forever()
PY

# Recording reverse proxy → real SWR. Logs every request path for the assert.
cat > "${WORK}/proxy/swr_proxy.py" <<'PY'
import http.server, os, pathlib, urllib.error, urllib.request

UPSTREAM = os.environ["SWR_ORIGIN"].rstrip("/")
LOG = pathlib.Path(os.environ["HITS_LOG"])
SKIP_REQ = {"host", "transfer-encoding", "connection", "content-length"}

class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def do_GET(self):
        self._proxy()

    def do_HEAD(self):
        self._proxy()

    def _proxy(self):
        with LOG.open("a", encoding="utf-8") as f:
            f.write(self.path + "\n")
        url = UPSTREAM + self.path
        req = urllib.request.Request(url, method=self.command)
        for k, v in self.headers.items():
            if k.lower() in SKIP_REQ:
                continue
            req.add_header(k, v)
        try:
            resp = urllib.request.urlopen(req, timeout=180)
            status = resp.status
            headers = resp.headers
            body = resp.read() if self.command != "HEAD" else b""
        except urllib.error.HTTPError as e:
            status = e.code
            headers = e.headers
            body = e.read() if self.command != "HEAD" else b""
        self.send_response(status)
        for k, v in headers.items():
            lk = k.lower()
            if lk in ("transfer-encoding", "connection", "content-encoding"):
                continue
            if self.command == "HEAD" and lk == "content-length":
                continue
            self.send_header(k, v)
        if self.command != "HEAD":
            self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if self.command != "HEAD" and body:
            self.wfile.write(body)

http.server.HTTPServer(("127.0.0.1", int(os.environ["PROXY_PORT"])), H).serve_forever()
PY

: > "${WORK}/proxy/hits.log"
MISS_PORT="${MISS_PORT}" python3 "${WORK}/proxy/miss.py" &
MISS_PID=$!
SWR_ORIGIN="${SWR_ORIGIN}" HITS_LOG="${WORK}/proxy/hits.log" PROXY_PORT="${PROXY_PORT}" \
  python3 "${WORK}/proxy/swr_proxy.py" &
PROXY_PID=$!

SPID=""
cleanup() {
  kill "${SPID:-}" "${PROXY_PID:-}" "${MISS_PID:-}" 2>/dev/null || true
  wait "${SPID:-}" "${PROXY_PID:-}" "${MISS_PID:-}" 2>/dev/null || true
}
trap cleanup EXIT
sleep 0.3

step "building specula"
go -C "${REPO}" build -o "${WORK}/specula" ./cmd/specula

step "writing config (transparent 404 → Huawei layout via recording proxy)"
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
      - name: unused-hub
        base_url: http://127.0.0.1:1
        priority: 1
        official: false
    oci:
      remote_registries:
        - host: registry.k8s.io
          upstreams:
            # Always miss — proves failover into the nested SWR layout.
            - name: transparent-miss
              base_url: http://127.0.0.1:${MISS_PORT}
              priority: 1
            - name: huawei-swr
              base_url: http://127.0.0.1:${PROXY_PORT}
              layout: huawei-ddn
              priority: 2
YAML

step "starting specula"
"${WORK}/specula" --config "${WORK}/cfg.yaml" > "${WORK}/daemon.log" 2>&1 &
SPID=$!
wait_for_daemon "$SPID" "$DATA_PORT" "http://127.0.0.1:${DATA_PORT}/healthz" "${WORK}/daemon.log"

LOCAL_REF="127.0.0.1:${DATA_PORT}/${REMOTE_REF}"
: > "${WORK}/proxy/hits.log"

step "path-style pull ${LOCAL_REF} (must use nested SWR path)"
if ! docker pull "${LOCAL_REF}" 2>&1 | tee "${WORK}/pull.log"; then
  echo "FAIL: docker pull failed"
  echo "--- proxy hits ---"; cat "${WORK}/proxy/hits.log" || true
  echo "--- daemon.log (tail) ---"; tail -n 80 "${WORK}/daemon.log" || true
  exit 1
fi

step "assert recording proxy saw nested path_prefix"
if ! grep -q '/v2/ddn-k8s/registry.k8s.io/pause/' "${WORK}/proxy/hits.log"; then
  echo "FAIL: proxy never saw /v2/ddn-k8s/registry.k8s.io/pause/… — path_prefix not applied?"
  echo "--- proxy hits ---"; cat "${WORK}/proxy/hits.log" || true
  echo "--- daemon.log (tail) ---"; tail -n 80 "${WORK}/daemon.log" || true
  exit 1
fi
# Flat /v2/pause/… on this proxy would mean layout expand failed (SWR 404s that
# path). Nested success above is the gate; flag flat hits as a soft warning.
if grep -E '^/v2/pause/' "${WORK}/proxy/hits.log" >/dev/null 2>&1; then
  echo "WARN: proxy also saw transparent /v2/pause/… (unexpected with layout: huawei-ddn)"
  grep -E '^/v2/pause/' "${WORK}/proxy/hits.log" | sed 's/^/    /' || true
fi
echo "  proxy hits (unique):"
sort -u "${WORK}/proxy/hits.log" | sed 's/^/    /'

step "second pull (cache warm)"
docker pull "${LOCAL_REF}" 2>&1 | tee "${WORK}/pull2.log"
grep -qiE 'up to date|Downloaded newer|Pull complete|Status:' "${WORK}/pull2.log"

echo
echo "PASS: oci Huawei SWR path_prefix realclient (nested ddn-k8s/registry.k8s.io)"
