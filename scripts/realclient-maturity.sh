#!/usr/bin/env bash
# scripts/realclient-maturity.sh — maturity cool-down enforce smoke (hermetic).
#
# Spins a fake npm registry + Specula with verification.maturity.policy=enforce,
# then asserts:
#   * a "young" package (PublishedAt = now) is rejected on tarball fetch
#   * an "old" package (PublishedAt = 2020) is allowed
#   * Admin Events carry kind=maturity for the young rejection
#
# Reuses scripts/lib/daemon.sh (free ports + socket ownership). No external
# network required after the Go build.
#
# Usage: scripts/realclient-maturity.sh
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$(mktemp -d /tmp/specula-maturity.XXXXXX)}"
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/daemon.sh"
DATA_PORT="${DATA_PORT:-$(pick_free_port)}"
CTRL_PORT="${CTRL_PORT:-$(pick_free_port)}"
UP_PORT="${UP_PORT:-$(pick_free_port)}"

mkdir -p "${WORK}/blobs" "${WORK}/up"

YOUNG_ISO="$(date -u +"%Y-%m-%dT%H:%M:%S.000Z")"
OLD_ISO="2020-01-15T12:00:00.000Z"
# Minimal gzip members — content is irrelevant; maturity gates on PublishedAt.
TGZ_BYTES="$(printf '\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\x03\x03\x00\x00\x00\x00\x00\x00\x00\x00' | base64 -w0 2>/dev/null || printf '\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\x03\x03\x00\x00\x00\x00\x00\x00\x00\x00' | base64)"

cat > "${WORK}/up/server.py" <<'PY'
import base64, json, os
from http.server import BaseHTTPRequestHandler, HTTPServer

YOUNG = os.environ["YOUNG_ISO"]
OLD = os.environ["OLD_ISO"]
TGZ = base64.b64decode(os.environ["TGZ_B64"])

def packument(name: str, ver: str, when: str) -> bytes:
    return json.dumps({
        "name": name,
        "versions": {ver: {"name": name, "version": ver,
            "dist": {"tarball": f"http://ignored/{name}/-/{name}-{ver}.tgz"}}},
        "time": {ver: when, "created": when, "modified": when},
    }).encode()

class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        p = self.path.split("?", 1)[0]
        if p == "/young-pkg":
            body = packument("young-pkg", "1.0.0", YOUNG)
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if p == "/old-pkg":
            body = packument("old-pkg", "1.0.0", OLD)
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if p.endswith(".tgz"):
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(TGZ)))
            self.end_headers()
            self.wfile.write(TGZ)
            return
        self.send_response(404)
        self.end_headers()

HTTPServer(("127.0.0.1", int(os.environ["UP_PORT"])), H).serve_forever()
PY

YOUNG_ISO="$YOUNG_ISO" OLD_ISO="$OLD_ISO" TGZ_B64="$TGZ_BYTES" UP_PORT="$UP_PORT" \
  python3 "${WORK}/up/server.py" &
UP_PID=$!
trap 'kill "$UP_PID" "$SPID" 2>/dev/null || true' EXIT
sleep 0.2

echo "==> building specula"
go -C "$REPO" build -o "${WORK}/specula" ./cmd/specula

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
  npm:
    mutable_ttl_seconds: 60
    upstreams:
      - name: fake-npm
        base_url: http://127.0.0.1:${UP_PORT}
        priority: 1
        official: true
    verification:
      tiers: [tofu, checksum]
      maturity:
        min_age: 72h
        policy: enforce
YAML

echo "==> starting specula"
"${WORK}/specula" --config "${WORK}/cfg.yaml" > "${WORK}/daemon.log" 2>&1 &
SPID=$!
wait_for_daemon "$SPID" "$DATA_PORT" "http://127.0.0.1:${CTRL_PORT}/healthz" "${WORK}/daemon.log"

pass() { echo "PASS $*"; }
fail() { echo "FAIL $*" >&2; exit 1; }

# Seed admin (first register → admin) for Events API.
curl -fsS -H 'Content-Type: application/json' -X POST \
  "http://127.0.0.1:${CTRL_PORT}/api/v1/auth/register" \
  -d '{"email":"maturity@specula.local","password":"password123","name":"Maturity"}' \
  >/dev/null
COOKIE="${WORK}/cookie.txt"
curl -fsS -c "$COOKIE" -H 'Content-Type: application/json' -X POST \
  "http://127.0.0.1:${CTRL_PORT}/api/v1/auth/login" \
  -d '{"email":"maturity@specula.local","password":"password123"}' >/dev/null

# Warm packuments so enrichPublishTime can read PublishedAt from cache.
curl -fsS "http://127.0.0.1:${DATA_PORT}/npm/young-pkg" >/dev/null
curl -fsS "http://127.0.0.1:${DATA_PORT}/npm/old-pkg" >/dev/null

echo "==> young tarball must be rejected (maturity enforce)"
CODE=$(curl -sS -o /tmp/maturity-young.out -w '%{http_code}' \
  "http://127.0.0.1:${DATA_PORT}/npm/young-pkg/-/young-pkg-1.0.0.tgz" || true)
# Verify failure surfaces as 502 from the npm handler.
[[ "$CODE" == "502" || "$CODE" == "403" || "$CODE" == "451" ]] \
  || fail "young package expected blocked status, got HTTP $CODE (body: $(head -c 200 /tmp/maturity-young.out))"
pass "young package blocked (HTTP $CODE)"

echo "==> old tarball must succeed"
curl -fsS -o /tmp/maturity-old.tgz \
  "http://127.0.0.1:${DATA_PORT}/npm/old-pkg/-/old-pkg-1.0.0.tgz" \
  || fail "old package fetch failed"
[[ -s /tmp/maturity-old.tgz ]] || fail "old package empty body"
pass "old package allowed"

echo "==> Events feed must include maturity kind for the young reject"
EVENTS=$(curl -fsS -b "$COOKIE" "http://127.0.0.1:${CTRL_PORT}/api/v1/admin/events?limit=50")
echo "$EVENTS" | python3 -c '
import json,sys
d=json.load(sys.stdin)
evs=d.get("events") or []
mat=[e for e in evs if e.get("kind")=="maturity" or "maturity:" in (e.get("detail") or "")]
fail=[e for e in mat if e.get("result")=="fail"]
if not fail:
    print("events:", json.dumps(evs, indent=2)[:2000])
    raise SystemExit("no maturity fail event found")
print("maturity fail detail:", fail[0].get("detail","")[:160])
' || fail "Events API missing maturity fail"
pass "Events kind=maturity fail recorded"

echo "PASS: maturity realclient"
