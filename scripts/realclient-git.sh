#!/usr/bin/env bash
# realclient-git.sh — Real git client end-to-end runner for Specula's git handler.
#
# Builds specula, starts it with a local bare git repo as the upstream
# (served via a minimal Python git-http-backend CGI wrapper), then drives
# the REAL git client through the proxy and asserts every operation.
#
# Tested operations:
#   git ls-remote          (ref advertisement, mutable tier)
#   git clone              (cold mirror creation + warm cache hit)
#   git clone --depth 1    (shallow clone — gitprotocol-http §shallow)
#   git clone --filter=blob:none  (partial/blobless — gitprotocol-capabilities §filter)
#   GIT_PROTOCOL=version=2 git clone  (protocol v2 — gitprotocol-v2 §HTTP)
#   force-push detection   (TOFU non-fast-forward alert — ARCHITECTURE §5)
#
# Usage:  scripts/realclient-git.sh
# Exit 0 only if all assertions pass.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$(mktemp -d /tmp/specula-git-realclient.XXXXXX)}"

# Free ports + a socket-ownership assertion at startup; see scripts/lib/daemon.sh for why
# both are required and why liveness/health checks alone are not enough.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/daemon.sh"
DATA_PORT="${DATA_PORT:-$(pick_free_port)}"
CTRL_PORT="${CTRL_PORT:-$(pick_free_port)}"
# Fake upstream git HTTP server (plain CGI wrapper around git-http-backend).
GIT_SRV_PORT="${GIT_SRV_PORT:-$(pick_free_port)}"

export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOSUMDB="${GOSUMDB:-sum.golang.google.cn}"

# ── Global state ────────────────────────────────────────────────────────────
SPID=""          # specula PID (may be replaced for the force-push sub-test)
GIT_SRV_PID=""   # fake upstream HTTP server PID
PASS=0
FAIL=0
ERRORS=""

# ── Cleanup ─────────────────────────────────────────────────────────────────
cleanup() {
    local spid="$SPID" gpid="$GIT_SRV_PID"
    if [[ -n "$spid" ]]; then
        kill "$spid" 2>/dev/null || true
        wait "$spid" 2>/dev/null || true
    fi
    if [[ -n "$gpid" ]]; then
        kill "$gpid" 2>/dev/null || true
        wait "$gpid" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# ── Assertion helpers ────────────────────────────────────────────────────────

pass() { echo "  PASS: $1"; ((PASS++)) || true; }
fail() {
    echo "  FAIL: $1"
    ERRORS="$ERRORS\n  - $1"
    ((FAIL++)) || true
}

assert_exit0() {
    local desc="$1"; shift
    local out rc=0
    out=$("$@" 2>&1) || rc=$?
    if [[ $rc -eq 0 ]]; then
        pass "$desc (exit 0)"
    else
        fail "$desc: exit $rc — $out"
    fi
    echo "$out"
}

assert_contains() {
    local desc="$1" text="$2" pat="$3"
    if echo "$text" | grep -q "$pat"; then
        pass "$desc (contains: $pat)"
    else
        fail "$desc: pattern '$pat' not found"
    fi
}

assert_file_exists() {
    local desc="$1" path="$2"
    if [[ -e "$path" ]]; then
        pass "$desc (exists: $path)"
    else
        fail "$desc: not found: $path"
    fi
}

assert_equal() {
    local desc="$1" got="$2" want="$3"
    if [[ "$got" == "$want" ]]; then
        pass "$desc (= $want)"
    else
        fail "$desc: got=$got want=$want"
    fi
}

req_count() {
    # Count upstream requests logged to the request log file.
    if [[ -f "$WORK/upstream-requests.log" ]]; then
        wc -l < "$WORK/upstream-requests.log" | tr -d ' '
    else
        echo "0"
    fi
}

stop_specula() {
    if [[ -n "$SPID" ]]; then
        kill "$SPID" 2>/dev/null || true
        wait "$SPID" 2>/dev/null || true
        SPID=""
    fi
}

start_specula() {
    local cfg="$1" log="$2"
    # SPECULA_GIT_UPSTREAM_SCHEME=http: tell the git handler to use plain HTTP
    # for its upstream git clone --mirror operations.  The fake upstream in this
    # test is a plain HTTP server (git-http-backend wrapped by Python).
    # In production this env var is unset and the default HTTPS is used.
    SPECULA_GIT_UPSTREAM_SCHEME=http \
        "$WORK/specula" --config "$cfg" > "$log" 2>&1 &
    SPID=$!
    # Replaces `sleep 2` + a single `kill -0`: a fixed sleep is both slower than needed and
    # unsound — it can elapse while the daemon is still binding, and a live process does not
    # prove it is the one serving our port. wait_for_daemon asserts socket ownership.
    wait_for_daemon "$SPID" "$DATA_PORT" "http://127.0.0.1:${CTRL_PORT}/healthz" "$log" || return 1
}

# ── 0. Workspace ─────────────────────────────────────────────────────────────

echo "==> workspace: $WORK"
rm -rf "$WORK"
mkdir -p "$WORK"/{blobs,mirror,clones}

# ── 1. Build specula ─────────────────────────────────────────────────────────

echo "==> building specula"
go -C "$REPO" build -o "$WORK/specula" ./cmd/specula

# ── 2. Create local bare git upstream and populate it ────────────────────────

echo "==> creating local bare git upstream"
UPSTREAM_ROOT="$WORK/upstream-root"
mkdir -p "$UPSTREAM_ROOT"

BARE_REPO="$UPSTREAM_ROOT/testrepo.git"
git init --bare "$BARE_REPO" --quiet
# Allow force-push for the force-push detection test.
git -C "$BARE_REPO" config receive.denyNonFastForwards false
# Enable partial-clone capability on the upstream too (needed for the initial
# git clone --mirror if the client requests filter capability discovery).
git -C "$BARE_REPO" config uploadpack.allowFilter true
git -C "$BARE_REPO" config uploadpack.allowAnySHA1InWant true

WORKCOPY="$WORK/workcopy"
git clone --quiet "$BARE_REPO" "$WORKCOPY"
echo "# testrepo" > "$WORKCOPY/README.md"
printf 'data line 1\ndata line 2\n' > "$WORKCOPY/data.txt"
mkdir -p "$WORKCOPY/subdir"
echo "nested content" > "$WORKCOPY/subdir/nested.txt"
git -C "$WORKCOPY" \
    -c user.email="test@specula.local" \
    -c user.name="Specula Test" \
    add .
git -C "$WORKCOPY" \
    -c user.email="test@specula.local" \
    -c user.name="Specula Test" \
    commit --quiet -m "initial commit"

DEFAULT_BRANCH=$(git -C "$WORKCOPY" rev-parse --abbrev-ref HEAD)
git -C "$WORKCOPY" push --quiet origin "HEAD:$DEFAULT_BRANCH"
INITIAL_SHA=$(git -C "$WORKCOPY" rev-parse HEAD)
echo "==> upstream ready: branch=$DEFAULT_BRANCH SHA=$INITIAL_SHA"

# ── 3. Start fake git HTTP upstream server ───────────────────────────────────
# Wraps git-http-backend in a minimal Python CGI server.  Each request is
# logged to upstream-requests.log for cache-hit counting.

echo "==> starting fake git HTTP upstream on :$GIT_SRV_PORT"
cat > "$WORK/git-http-server.py" << 'PYEOF'
#!/usr/bin/env python3
"""Minimal git Smart HTTP server wrapping git-http-backend."""
import http.server
import os
import subprocess
import sys

GIT_ROOT = os.environ["GIT_PROJECT_ROOT"]
REQUEST_LOG = os.environ["REQUEST_LOG"]


class GitHTTPHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self._handle()

    def do_POST(self):
        self._handle()

    def _handle(self):
        # Log the request for upstream-hit counting.
        with open(REQUEST_LOG, "a") as fh:
            fh.write(f"{self.command} {self.path}\n")

        path = self.path
        query = ""
        if "?" in path:
            path, query = path.split("?", 1)

        env = dict(
            os.environ,
            GIT_HTTP_EXPORT_ALL="1",
            GIT_PROJECT_ROOT=GIT_ROOT,
            PATH_INFO=path,
            QUERY_STRING=query,
            REQUEST_METHOD=self.command,
            SERVER_PROTOCOL="HTTP/1.1",
            CONTENT_TYPE=self.headers.get("Content-Type", ""),
            CONTENT_LENGTH=self.headers.get("Content-Length", ""),
        )
        # Propagate Git-Protocol for protocol v2 (gitprotocol-v2 §HTTP).
        git_proto = self.headers.get("Git-Protocol", "")
        if git_proto:
            env["GIT_PROTOCOL"] = git_proto

        body_len = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(body_len) if body_len else b""

        proc = subprocess.run(
            ["git", "http-backend"],
            input=body,
            capture_output=True,
            env=env,
        )

        raw = proc.stdout
        sep = raw.find(b"\r\n\r\n")
        sep_len = 4
        if sep < 0:
            sep = raw.find(b"\n\n")
            sep_len = 2

        if sep < 0:
            self.send_response(200)
            self.end_headers()
            self.wfile.write(raw)
            return

        header_block = raw[:sep].decode("utf-8", errors="replace")
        body_out = raw[sep + sep_len:]

        status = 200
        headers = []
        for line in header_block.splitlines():
            line = line.strip()
            if not line:
                continue
            if line.lower().startswith("status:"):
                status = int(line.split()[1])
                continue
            if ":" in line:
                k, v = line.split(":", 1)
                k = k.strip()
                # Let us control Content-Length precisely (we buffer the body).
                if k.lower() == "content-length":
                    continue
                headers.append((k, v.strip()))

        self.send_response(status)
        for k, v in headers:
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(body_out)))
        self.end_headers()
        self.wfile.write(body_out)

    def log_message(self, fmt, *args):
        pass  # suppress default access log noise


if __name__ == "__main__":
    port = int(sys.argv[1])
    server = http.server.HTTPServer(("127.0.0.1", port), GitHTTPHandler)
    server.serve_forever()
PYEOF
chmod +x "$WORK/git-http-server.py"

touch "$WORK/upstream-requests.log"
GIT_PROJECT_ROOT="$UPSTREAM_ROOT" REQUEST_LOG="$WORK/upstream-requests.log" \
    python3 "$WORK/git-http-server.py" "$GIT_SRV_PORT" &
GIT_SRV_PID=$!
sleep 1

# Verify the fake upstream is reachable.
echo "==> verifying fake upstream"
LS_CHECK=$(git ls-remote "http://127.0.0.1:$GIT_SRV_PORT/testrepo.git" 2>&1) || {
    echo "FATAL: fake upstream not responding: $LS_CHECK"
    exit 1
}
echo "  upstream ls-remote: OK ($( echo "$LS_CHECK" | wc -l | tr -d ' ') refs)"

# ── 4. Write specula config (standard, sync_stale_after=30s) ─────────────────

UPSTREAM_HOST="127.0.0.1:$GIT_SRV_PORT"
CLONE_BASE="http://127.0.0.1:$DATA_PORT/git/$UPSTREAM_HOST/testrepo.git"
echo "==> Specula proxy URL: $CLONE_BASE"

write_cfg() {
    local stale="$1" out="$2"
    cat > "$out" << EOF
server:
  data_plane_addr: ":$DATA_PORT"
  control_plane_addr: ":$CTRL_PORT"
auth:
  registry_token_key_path: $WORK/regkey.pem
storage:
  blob: {driver: local, local: {root: $WORK/blobs}}
  meta: {driver: sqlite, dsn: $WORK/meta.db}
protocols:
  git:
    # The generic upstreams list is required by the validator (internal/config/validate.go §Protocols)
    # even though the git handler routes via git.allowed_upstreams, not this list.
    upstreams:
      - name: local-upstream
        base_url: "http://$UPSTREAM_HOST"
        priority: 1
        official: true
    git:
      allowed_upstreams: ["$UPSTREAM_HOST"]
      mirror_dir: $WORK/mirror
      sync_stale_after: "$stale"
      public_only: false
      fail_closed: false
EOF
}

write_cfg "30s" "$WORK/cfg-main.yaml"

echo "==> starting specula (sync_stale_after=30s)"
start_specula "$WORK/cfg-main.yaml" "$WORK/daemon-main.log"

# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "══ Test 1: git ls-remote (also creates the bare mirror) ══"
# ls-remote is the first request, so it triggers EnsureSynced →
# git clone --mirror from the upstream.  Record req count before/after to
# confirm the cold-mirror creation contacted the upstream server.
REQS_BEFORE_LS=$(req_count)
LS_OUT=$(GIT_TERMINAL_PROMPT=0 git -c http.followRedirects=true \
    ls-remote "$CLONE_BASE" 2>&1) && LS_RC=0 || LS_RC=$?
REQS_AFTER_LS=$(req_count)
echo "  exit=$LS_RC  upstream_reqs=$((REQS_AFTER_LS - REQS_BEFORE_LS))"
echo "$LS_OUT" | sed 's/^/    /'
if [[ $LS_RC -eq 0 ]]; then pass "ls-remote exit 0"; else fail "ls-remote exit $LS_RC"; fi
assert_contains "ls-remote HEAD"   "$LS_OUT" "HEAD"
assert_contains "ls-remote branch" "$LS_OUT" "refs/heads/$DEFAULT_BRANCH"
assert_contains "ls-remote SHA"    "$LS_OUT" "$INITIAL_SHA"
# ls-remote triggers the bare-mirror clone (cold); upstream should be hit.
if [[ $((REQS_AFTER_LS - REQS_BEFORE_LS)) -gt 0 ]]; then
    pass "ls-remote created cold mirror (contacted upstream ${REQS_AFTER_LS} times)"
else
    fail "ls-remote should have contacted upstream for cold mirror (got 0 requests)"
fi
assert_file_exists "mirror bare repo created by ls-remote" "$WORK/mirror/$UPSTREAM_HOST/testrepo.git"

# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "══ Test 2: git clone (warm mirror — served from cache, 0 upstream) ══"
# Mirror was created by ls-remote above; within the 30s stale window, the
# clone is served from the mirror with zero upstream requests.
CLONE1="$WORK/clones/clone1"
REQS_BEFORE=$(req_count)
CLONE1_OUT=$(GIT_TERMINAL_PROMPT=0 git clone --quiet "$CLONE_BASE" "$CLONE1" 2>&1) && C1_RC=0 || C1_RC=$?
REQS_AFTER=$(req_count)
DELTA1=$((REQS_AFTER - REQS_BEFORE))
echo "  exit=$C1_RC  upstream_reqs=$DELTA1"
if [[ $C1_RC -eq 0 ]]; then pass "clone1 exit 0"; else fail "clone1 exit $C1_RC — $CLONE1_OUT"; fi
assert_file_exists "clone1 has README.md" "$CLONE1/README.md"
LOG1=$(git -C "$CLONE1" log --oneline 2>&1)
assert_contains "clone1 has initial commit" "$LOG1" "initial commit"
SHA1=$(git -C "$CLONE1" rev-parse HEAD 2>/dev/null)
assert_equal "clone1 HEAD SHA" "$SHA1" "$INITIAL_SHA"
if [[ $DELTA1 -eq 0 ]]; then
    pass "warm mirror clone — 0 upstream requests (served from mirror)"
else
    fail "warm mirror clone should not have contacted upstream (got $DELTA1 requests)"
fi

# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "══ Test 3: git clone (second warm clone — still from cache) ══"
CLONE2="$WORK/clones/clone2"
REQS_BEFORE=$(req_count)
CLONE2_OUT=$(GIT_TERMINAL_PROMPT=0 git clone --quiet "$CLONE_BASE" "$CLONE2" 2>&1) && C2_RC=0 || C2_RC=$?
REQS_AFTER=$(req_count)
DELTA2=$((REQS_AFTER - REQS_BEFORE))
echo "  exit=$C2_RC  upstream_reqs=$DELTA2"
if [[ $C2_RC -eq 0 ]]; then pass "clone2 exit 0"; else fail "clone2 exit $C2_RC — $CLONE2_OUT"; fi
SHA2=$(git -C "$CLONE2" rev-parse HEAD 2>/dev/null)
assert_equal "clone2 HEAD SHA matches" "$SHA2" "$INITIAL_SHA"
if [[ $DELTA2 -eq 0 ]]; then
    pass "second clone served from mirror (0 upstream requests)"
else
    fail "second clone should not have contacted upstream (got $DELTA2 requests)"
fi

# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "══ Test 4: git clone --depth 1 (shallow) ══"
CLONE_SHALLOW="$WORK/clones/shallow"
SHALLOW_OUT=$(GIT_TERMINAL_PROMPT=0 git clone --quiet --depth 1 "$CLONE_BASE" "$CLONE_SHALLOW" 2>&1) && SH_RC=0 || SH_RC=$?
echo "  exit=$SH_RC"
if [[ $SH_RC -eq 0 ]]; then pass "shallow clone exit 0"; else fail "shallow clone exit $SH_RC — $SHALLOW_OUT"; fi
assert_file_exists "shallow clone has .git/shallow" "$CLONE_SHALLOW/.git/shallow"
COMMIT_COUNT=$(git -C "$CLONE_SHALLOW" log --oneline 2>/dev/null | wc -l | tr -d ' ')
if [[ "$COMMIT_COUNT" -eq 1 ]]; then
    pass "shallow clone has exactly 1 commit (depth=1)"
else
    fail "shallow clone should have 1 commit, got $COMMIT_COUNT"
fi
assert_file_exists "shallow clone has README.md" "$CLONE_SHALLOW/README.md"

# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "══ Test 5: git clone --filter=blob:none (partial/blobless) ══"
# Requires uploadpack.allowFilter=true in the mirror (fixed in mirror.go).
CLONE_PARTIAL="$WORK/clones/partial"
PARTIAL_OUT=$(GIT_TERMINAL_PROMPT=0 git clone --quiet \
    --filter=blob:none "$CLONE_BASE" "$CLONE_PARTIAL" 2>&1) && P_RC=0 || P_RC=$?
echo "  exit=$P_RC"
if [[ $P_RC -ne 0 ]]; then
    fail "blobless clone exit $P_RC — $PARTIAL_OUT"
    echo "  [INFO] Likely cause: uploadpack.allowFilter not enabled in mirror (see mirror.go fix)"
else
    pass "blobless clone exit 0"
    # Verify blobs are lazily fetchable (the remote is promisor-configured).
    README_CONTENT=$(git -C "$CLONE_PARTIAL" cat-file -p HEAD:README.md 2>&1) && RF_RC=0 || RF_RC=$?
    if [[ $RF_RC -eq 0 ]]; then
        pass "blobless: blob lazily fetchable from promisor remote"
        assert_contains "blobless README content" "$README_CONTENT" "testrepo"
    else
        fail "blobless: could not read blob from promisor remote: $README_CONTENT"
    fi
fi

# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "══ Test 6: git clone with GIT_PROTOCOL=version=2 (protocol v2) ══"
# Requires GIT_PROTOCOL env-var propagation in serve.go.
# Use GIT_TRACE_PACKET to confirm v2 wire protocol was negotiated.
CLONE_V2="$WORK/clones/v2"
V2_TRACE="$WORK/v2-trace.log"
V2_OUT=$(GIT_TERMINAL_PROMPT=0 GIT_PROTOCOL=version=2 GIT_TRACE_PACKET="$V2_TRACE" \
    git clone --quiet "$CLONE_BASE" "$CLONE_V2" 2>&1) && V2_RC=0 || V2_RC=$?
echo "  exit=$V2_RC"
if [[ $V2_RC -eq 0 ]]; then pass "protocol-v2 clone exit 0"; else fail "protocol-v2 clone exit $V2_RC — $V2_OUT"; fi
assert_file_exists "protocol-v2 clone has README.md" "$CLONE_V2/README.md"
SHA_V2=$(git -C "$CLONE_V2" rev-parse HEAD 2>/dev/null)
assert_equal "protocol-v2 clone HEAD SHA" "$SHA_V2" "$INITIAL_SHA"
# Confirm protocol v2 was actually negotiated on the wire.
if [[ -f "$V2_TRACE" ]] && grep -q "version 2" "$V2_TRACE"; then
    pass "protocol v2 negotiated (trace confirms 'version 2')"
else
    fail "protocol v2 not confirmed in trace (serve.go GIT_PROTOCOL propagation may be missing)"
    echo "  [INFO] Trace file: $V2_TRACE"
    if [[ -f "$V2_TRACE" ]]; then head -20 "$V2_TRACE" | sed 's/^/    /'; fi
fi

# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "══ Test 7: force-push / history-rewrite detection (TOFU) ══"
# Restart specula with a very short stale window so the next clone triggers
# a mirror re-sync even though we just synced above.
echo "  stopping main specula"
stop_specula
# Reuse the same meta.db (preserving the initial TOFU pins) but shorten
# sync_stale_after to 1s so the force-pushed clone always re-syncs.
write_cfg "1s" "$WORK/cfg-fast.yaml"
echo "  starting specula-fast (sync_stale_after=1s, same meta.db)"
start_specula "$WORK/cfg-fast.yaml" "$WORK/daemon-fast.log"

# Force-push: amend the initial commit (changes its SHA).
echo "  amending upstream commit (force-push)"
echo "amended line" >> "$WORKCOPY/data.txt"
git -C "$WORKCOPY" \
    -c user.email="test@specula.local" \
    -c user.name="Specula Test" \
    add data.txt
git -C "$WORKCOPY" \
    -c user.email="test@specula.local" \
    -c user.name="Specula Test" \
    commit --quiet --amend --no-edit
git -C "$WORKCOPY" push --quiet --force origin "HEAD:$DEFAULT_BRANCH"
FORCED_SHA=$(git -C "$WORKCOPY" rev-parse HEAD)
echo "  force-push complete: old=$INITIAL_SHA new=$FORCED_SHA"
if [[ "$FORCED_SHA" != "$INITIAL_SHA" ]]; then
    pass "force-push produced a new SHA (!=initial)"
else
    fail "force-push SHA unchanged — test cannot proceed"
fi

# Sleep to expire the 1s stale window.
sleep 2

# Clone through specula-fast: triggers mirror re-sync + TOFU pin comparison.
CLONE_FP="$WORK/clones/forcepush"
FP_OUT=$(GIT_TERMINAL_PROMPT=0 git clone --quiet "$CLONE_BASE" "$CLONE_FP" 2>&1) && FP_RC=0 || FP_RC=$?
echo "  post-force-push clone exit=$FP_RC"
if [[ $FP_RC -eq 0 ]]; then pass "post-force-push clone exit 0"; else fail "post-force-push clone exit $FP_RC — $FP_OUT"; fi
SHA_FP=$(git -C "$CLONE_FP" rev-parse HEAD 2>/dev/null)
assert_equal "post-force-push clone HEAD = forced SHA" "$SHA_FP" "$FORCED_SHA"

# Check daemon log for the TOFU non-fast-forward alert.
# updateTOFUPins emits: "git tofu: NON-FAST-FORWARD update on <ref> in <repo>:..."
# serveMirror logs each alert at WARN level: {"msg":"git tofu: NON-FAST-FORWARD ..."}
if grep -q "NON-FAST-FORWARD\|non-fast-forward ref update detected" "$WORK/daemon-fast.log"; then
    pass "TOFU non-fast-forward alert found in daemon log"
else
    fail "TOFU non-fast-forward alert NOT found in daemon log"
    echo "  [DEBUG] daemon-fast.log tail:"
    tail -20 "$WORK/daemon-fast.log" | sed 's/^/    /'
fi

stop_specula

# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "══ Summary ══"
echo "  PASS: $PASS  FAIL: $FAIL"
if [[ $FAIL -gt 0 ]]; then
    printf "Failures:%b\n" "$ERRORS"
    exit 1
fi
echo "All git real-client assertions passed."
exit 0
