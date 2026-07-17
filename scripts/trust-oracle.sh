#!/usr/bin/env bash
# scripts/trust-oracle.sh — the INDEPENDENT ORACLE acceptance gate for PRD §G2.
#
# ─────────────────────────────────────────────────────────────────────────────
# What this gate is for
#
# Specula's entire value proposition is that it reports HONESTLY what it actually
# verified: `signed` > `consensus` > `tofu` > `checksum` (PRD §G2). Everything
# backing that claim is SELF-REPORTED. `cache_entries.tier` and
# `specula_verification_total{tier}` are both written by our own code, so they are
# not orthogonal: one bug upstream of both satisfies both, and a cross-check
# between them agrees row-for-row while the claim is false.
#
# We have shipped exactly this class of bug, repeatedly, and caught each by hand:
#   * apt claimed the "end-to-end gold standard" while recording tofu x6 (a literal
#     path lookup missed every by-hash request);
#   * go's sumdb verifier never ran at all in the documented CN config;
#   * git recorded no tier while TOFU was live;
#   * every verifier encoded "I skipped this" as StatusPass @ TierChecksum —
#     byte-identical to "I checked it and it passed".
#
# Humans poking at it does not scale, and it is exactly what a customer cannot do.
# This gate re-derives the truth from OUTSIDE and fails loudly on disagreement.
#
# How independence is enforced STRUCTURALLY (not by promise)
#
# The oracle is scripts/oracle/trust_oracle.py — a Python program that shells out
# to gpg(1), the `go` toolchain and the real mirrors. It CANNOT import
# internal/verify even by accident: there is no import path from Python to Go.
# An oracle sharing code with the thing it grades is a mirror, and a mirror agrees
# with a lie. This gate asserts that property below rather than assuming it.
#
# What is graded, and by whose reference tooling
#
#   apt   signed    gpg(1) + the real Ubuntu keyring -> InRelease -> Packages -> .deb
#   go    signed    the `go` toolchain's own sumdb verification vs sum.golang.google.cn
#   pypi  consensus independent PEP 503 re-fetch from the real mirrors + quorum check
#   helm  tofu      upstream .prov probe: proves the ABSENCE of a stronger anchor
#
# What is NOT graded — stated explicitly, because a silent gap is how three of
# these bugs shipped:
#
#   cosign keyed / OCI `signed`  NOT ORACLED. The cosign CLI is not installed on
#                                this machine, and no keyed signing setup is
#                                obtainable here. This gate provides ZERO evidence
#                                about OCI signed claims. Do not read its PASS as
#                                covering cosign.
#   helm .prov `signed`          NOT ORACLED against a real signature: the CN helm
#                                mirror (mirror.azure.cn/kubernetes/charts) publishes
#                                NO .prov files at all (verified: every .prov 404s).
#                                Only the ABSENCE case is graded. `helm verify` has
#                                never been exercised here.
#   npm / git / tarball / oci    no oracle implemented; rows are reported UNGRADED.
#
# Usage:  scripts/trust-oracle.sh          # full gate, needs network
#         KEEP_WORK=1 scripts/trust-oracle.sh
# Exit 0 only if every graded artifact's recorded tier is the tier it deserves.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$(mktemp -d /tmp/specula-trust-oracle.XXXXXX)}"
OUT_JSON="${OUT_JSON:-$REPO/results/trust-oracle.json}"
# The metrics snapshot always lands NEXT TO the verdict it belongs to, never in a
# fixed directory. The mutation meta-gate redirects OUT_JSON to a scratch path when
# it runs this gate against deliberately-lying binaries; a hard-coded results dir
# would let those runs overwrite the real artifact with a mutant's metrics and
# quietly corrupt the evidence this gate exists to produce.
RESULTS="$(dirname "$OUT_JSON")"

# Free ports + a socket-ownership assertion at startup. A gate that grades the
# wrong process is worse than no gate, and that has literally happened here —
# see scripts/lib/daemon.sh for why liveness+health checks alone cannot detect it.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/daemon.sh"
DATA_PORT="${DATA_PORT:-$(pick_free_port)}"
CTRL_PORT="${CTRL_PORT:-$(pick_free_port)}"

KEYRING="/usr/share/keyrings/ubuntu-archive-keyring.gpg"

# Real CN upstreams (PRD §G5). Note aliyun has been measured at 27 kB/s on one
# link and the upstream client has a hard-coded 30s whole-request timeout, so the
# artifacts below are deliberately SMALL (hello .deb 26 kB, pkg/errors 17 kB,
# six sdist 34 kB, mysql chart 11 kB). Large indexes may not fetch on a bad link.
APT_BASE="${APT_BASE:-https://mirrors.aliyun.com/ubuntu}"
GO_PROXY_UP="${GO_PROXY_UP:-https://goproxy.cn}"
GO_SUMDB="${GO_SUMDB:-sum.golang.google.cn}"
PYPI_TUNA="${PYPI_TUNA:-https://pypi.tuna.tsinghua.edu.cn}"
PYPI_ALIYUN="${PYPI_ALIYUN:-https://mirrors.aliyun.com/pypi}"
PYPI_ORG="${PYPI_ORG:-https://pypi.org}"
# FLAT repo: the base IS /helm — there is no /helm/charts. An agent already burned
# a cycle "finding a bug" that was really this operator error.
HELM_BASE="${HELM_BASE:-https://mirror.azure.cn/kubernetes/charts}"

die() { echo "FAIL: $*" >&2; exit 1; }

# SPID holds the PID of the daemon WE started, captured from $!.
#
# NEVER discover it by matching a command line: any process whose argv contains
# the pattern matches too — INCLUDING THIS SCRIPT — so `pkill -f` self-kills.
# Six agents have hit this. Holding the PID from $! is exact and cannot select a
# bystander. We also never touch foreign specula processes (e.g. the demo on
# 7732/7733), which is why nothing here greps for them.
SPID=""

stop_specula() {
    [[ -n "$SPID" ]] || return 0
    kill "$SPID" 2>/dev/null || true
    local i=0
    while kill -0 "$SPID" 2>/dev/null && (( i < 50 )); do
        sleep 0.1
        i=$(( i + 1 ))  # not (( i++ )): that evaluates to 0 when i=0 and aborts under set -e
    done
}

cleanup() {
    local rc=$?
    stop_specula
    if [[ -z "${KEEP_WORK:-}" ]]; then
        # The Go module cache is written read-only, so a plain `rm -rf` fails and —
        # because this runs as an EXIT trap — its status would REPLACE the gate's.
        # A gate whose exit code reports its own tidying rather than its verdict is
        # worse than useless: it can turn a real tier disagreement green. Make the
        # tree writable first, and never let cleanup speak for the result.
        chmod -R u+w "$WORK" 2>/dev/null || true
        rm -rf "$WORK" 2>/dev/null || echo "note: could not fully remove $WORK (harmless)" >&2
    else
        echo "==> work dir kept: $WORK"
    fi
    return $rc
}

# ── Step 0: prerequisites ────────────────────────────────────────────────────
echo "==> checking prerequisites"
[[ -x "$(command -v gpg)" ]]      || die "gpg not found — the apt oracle's trust anchor"
[[ -x "$(command -v go)" ]]       || die "go not found — the go oracle's sumdb verifier"
[[ -x "$(command -v apt-get)" ]]  || die "apt-get not found"
[[ -x "$(command -v curl)" ]]     || die "curl not found"
[[ -x "$(command -v python3)" ]]  || die "python3 not found"
[[ -x "$(command -v sqlite3)" ]]  || echo "    note: sqlite3 CLI absent (only used for diagnostics)"
[[ -r "$KEYRING" ]]               || die "Ubuntu archive keyring not found at $KEYRING"

# ── Step 0b: assert the oracle is structurally independent ───────────────────
# The non-negotiable design constraint, checked rather than promised. If the
# oracle ever grows a dependency on the code it grades, this gate is void and
# must fail here — loudly — rather than quietly becoming a mirror.
ORACLE="$REPO/scripts/oracle/trust_oracle.py"
[[ -r "$ORACLE" ]] || die "oracle not found at $ORACLE"
# Parse the oracle's AST rather than grepping its text: the file DISCUSSES
# internal/verify at length (explaining why it must not touch it), so a plain grep
# flags its own documentation. What actually matters is (a) nothing imported
# resolves to Specula, and (b) no string the program can EXECUTE names Specula's
# packages — that is the only way Python could smuggle the graded code back in.
python3 - "$ORACLE" << 'PYEOF' || die "ORACLE INDEPENDENCE VIOLATED — refusing to run.
     An oracle that shares code with the thing it grades is a mirror, and a
     mirror agrees with a lie."
import ast, sys
src = open(sys.argv[1]).read()
tree = ast.parse(src)
bad = []
for node in ast.walk(tree):
    if isinstance(node, ast.Import):
        for a in node.names:
            if "specula" in a.name.lower():
                bad.append(f"import {a.name}")
    elif isinstance(node, ast.ImportFrom):
        if node.module and "specula" in node.module.lower():
            bad.append(f"from {node.module} import ...")
    elif isinstance(node, ast.Constant) and isinstance(node.value, str):
        # Docstrings are documentation, not executable coupling: skip them.
        if node.value is not ast.get_docstring(tree) and (
            "internal/verify" in node.value or "specula/internal" in node.value
        ):
            bad.append(f"executable string references {node.value[:60]!r}")
docstrings = {ast.get_docstring(n) for n in ast.walk(tree)
              if isinstance(n, (ast.Module, ast.ClassDef, ast.FunctionDef))}
bad = [b for b in bad if not any(b[30:60] in (d or "") for d in docstrings)]
if bad:
    print("ORACLE INDEPENDENCE VIOLATED:", file=sys.stderr)
    for b in bad:
        print("  -", b, file=sys.stderr)
    sys.exit(1)
print("    oracle independence: OK — no import of, and no executable reference to,")
print("    Specula's verify code (AST-checked, not grep-checked)")
PYEOF

# ── Step 1: build specula to OUR OWN temp path ───────────────────────────────
# Never bin/specula: concurrent agents have clobbered each other's binaries and
# then graded someone else's build.
echo "==> building specula -> $WORK/specula"
mkdir -p "$WORK"
go -C "$REPO" build -o "$WORK/specula" ./cmd/specula || die "build failed"

# ── Step 2: config ───────────────────────────────────────────────────────────
mkdir -p "$WORK/blobs" "$WORK/lists/partial" "$WORK/cache/archives/partial" \
         "$WORK/state" "$WORK/log" "$WORK/debs"

# Note on pypi upstreams: an upstream marked `official: true` becomes the origin
# WITNESS, not a mirror, so quorum=2 needs TWO non-official mirrors. Specula
# fail-closes at startup otherwise ("quorum exceeds available independent
# mirrors") — a good guard, and the reason aliyun is here alongside tuna.
cat > "$WORK/cfg.yaml" << EOF
server:
  data_plane_addr: ":${DATA_PORT}"
  control_plane_addr: ":${CTRL_PORT}"
storage:
  blob:
    driver: local
    local:
      root: ${WORK}/blobs
  meta:
    driver: sqlite
    dsn: ${WORK}/meta.db
protocols:
  apt:
    mutable_ttl_seconds: 0
    upstreams:
      - { name: aliyun, base_url: ${APT_BASE}, priority: 1, official: false }
    verification:
      tiers: [signed, tofu, checksum]
      quorum: 1
      tofu: enforce
      gpg:
        policy: enforce
        keyring: ${KEYRING}
  go:
    upstreams:
      - { name: goproxycn, base_url: ${GO_PROXY_UP}, priority: 1, official: false }
    verification:
      tiers: [signed, tofu, checksum]
      quorum: 1
      tofu: enforce
    sumdb:
      url: https://${GO_SUMDB}
      policy: enforce
  pypi:
    upstreams:
      - { name: tuna, base_url: ${PYPI_TUNA}, priority: 1, official: false }
      - { name: aliyunpypi, base_url: ${PYPI_ALIYUN}, priority: 2, official: false }
      - { name: pypi, base_url: ${PYPI_ORG}, priority: 3, official: true }
    verification:
      tiers: [consensus, tofu, checksum]
      quorum: 2
      tofu: enforce
  helm:
    mutable_ttl_seconds: 1800
    upstreams:
      - { name: azurecn, base_url: ${HELM_BASE}, priority: 1, official: false }
    verification:
      tiers: [tofu, checksum]
      quorum: 1
      tofu: enforce
EOF

# ── Step 3: start the daemon ─────────────────────────────────────────────────
echo "==> starting specula (data=:${DATA_PORT} ctrl=:${CTRL_PORT})"
"$WORK/specula" --config "$WORK/cfg.yaml" > "$WORK/daemon.log" 2>&1 &
SPID=$!
trap cleanup EXIT

# Blocks until the KERNEL confirms :$DATA_PORT is owned by OUR pid. Liveness and
# health cannot tell our server from a squatter; only socket ownership can.
wait_for_daemon "$SPID" "$DATA_PORT" "http://127.0.0.1:${DATA_PORT}/healthz" "$WORK/daemon.log" \
    || die "specula did not start correctly (check $WORK/daemon.log)"
echo "==> specula up (pid $SPID)"

BASE="http://127.0.0.1:${DATA_PORT}"

# ── Step 4: drive REAL traffic through Specula, multi-protocol ───────────────
# Everything below goes THROUGH Specula so that Specula records a tier for it.
# The oracle later grades the bytes Specula stored — never a fresh copy of its own.

echo "==> [apt] apt-get update + download hello (real apt-get, isolated state)"
cat > "$WORK/sources.list" << EOF
deb [arch=amd64 signed-by=${KEYRING}] ${BASE}/apt/ubuntu jammy main
EOF
APT_OPTS=(
    -o "Dir::Etc::sourcelist=${WORK}/sources.list"
    -o "Dir::Etc::sourceparts=/dev/null"
    -o "Dir::State::Lists=${WORK}/lists"
    -o "Dir::Cache=${WORK}/cache"
    -o "Dir::State=${WORK}/state"
    -o "Dir::Log=${WORK}/log"
    -o "APT::Get::Assume-Yes=1"
)
apt-get "${APT_OPTS[@]}" update > "$WORK/apt-update.log" 2>&1 \
    || { tail -20 "$WORK/apt-update.log"; die "apt-get update failed"; }
( cd "$WORK/debs" && apt-get "${APT_OPTS[@]}" download hello > "$WORK/apt-dl.log" 2>&1 ) \
    || { tail -20 "$WORK/apt-dl.log"; die "apt-get download hello failed"; }

echo "==> [go] go mod download through Specula (real go toolchain)"
mkdir -p "$WORK/gomod"
printf 'module oracleprobe\n\ngo 1.21\n' > "$WORK/gomod/go.mod"
(
    cd "$WORK/gomod"
    GOMODCACHE="$WORK/gomodcache" GOPATH="$WORK/gopath" GOFLAGS=-mod=mod \
    GOPROXY="${BASE}/go" GOSUMDB="$GO_SUMDB" GONOSUMDB="" GOPRIVATE="" GOINSECURE="" \
        go mod download github.com/pkg/errors@v0.9.1
) > "$WORK/go-dl.log" 2>&1 || { tail -20 "$WORK/go-dl.log"; die "go mod download failed"; }

echo "==> [pypi] simple index + sdist through Specula"
curl -fsS --max-time 60 "${BASE}/pypi/simple/six/" -o "$WORK/six-index.html" \
    || die "pypi simple index fetch failed"
curl -fsS --max-time 60 -L "${BASE}/pypi/packages/source/s/six/six-1.16.0.tar.gz" \
    -o "$WORK/six.tar.gz" || die "pypi sdist fetch failed"

echo "==> [helm] chart through Specula (FLAT repo: base is /helm, not /helm/charts)"
curl -fsS --max-time 60 "${BASE}/helm/mysql-1.6.9.tgz" -o "$WORK/mysql.tgz" \
    || die "helm chart fetch failed"

# Snapshot Specula's self-reported metrics next to the verdict, so a reader can
# see BOTH self-reports (DB tier + counter) alongside the independent verdict.
curl -fsS --max-time 10 "http://127.0.0.1:${CTRL_PORT}/metrics" 2>/dev/null \
    | grep "^specula_verification_total" | sort > "$WORK/metrics.txt" || true
mkdir -p "$RESULTS"
cp "$WORK/metrics.txt" "$RESULTS/trust-oracle-metrics.txt" 2>/dev/null || true

# ── Step 5: run the INDEPENDENT oracle ───────────────────────────────────────
# From here on nothing consults Specula: the oracle reads the sqlite claim and the
# CAS bytes, and re-derives the deserved tier using each ecosystem's own tooling.
echo
echo "==> running the independent oracle (gpg / go toolchain / real mirrors)"
set +e
python3 "$ORACLE" \
    --db "$WORK/meta.db" \
    --blobs "$WORK/blobs" \
    --keyring "$KEYRING" \
    --apt-base "$APT_BASE" \
    --go-proxy "${GO_PROXY_UP},direct" \
    --go-sumdb "$GO_SUMDB" \
    --pypi-mirror "tuna=$PYPI_TUNA" \
    --pypi-mirror "aliyun=$PYPI_ALIYUN" \
    --pypi-mirror "pypi.org=$PYPI_ORG" \
    --pypi-quorum 2 \
    --helm-base "$HELM_BASE" \
    --out "$OUT_JSON"
RC=$?
set -e

echo
echo "==> Specula's own self-reports (for contrast — NOT evidence of anything):"
sed 's/^/    /' "$WORK/metrics.txt" 2>/dev/null || true

echo
if [[ $RC -ne 0 ]]; then
    echo "TRUST ORACLE GATE: FAIL — see $OUT_JSON"
    exit $RC
fi
cat << 'EOF'
TRUST ORACLE GATE: PASS

COVERAGE — read this before quoting the PASS:
  GRADED independently:
    apt   signed     gpg(1) + real Ubuntu keyring, full InRelease->Packages->.deb chain
    go    signed     the go toolchain's own sumdb verification (traffic asserted)
    pypi  consensus  independent PEP 503 re-fetch + quorum
    helm  tofu       proven ABSENCE of a .prov anchor upstream
  NOT GRADED — this gate says NOTHING about these:
    cosign keyed / OCI signed   cosign CLI unavailable here; ZERO evidence.
    helm .prov signed           CN mirror publishes no .prov at all; only the
                                absence case is exercised. `helm verify` unproven.
    npm / git / tarball / oci   no oracle implemented; reported UNGRADED.
EOF
exit 0
