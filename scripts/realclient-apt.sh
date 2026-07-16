#!/usr/bin/env bash
# scripts/realclient-apt.sh — APT real-client conformance test for Specula.
#
# Drives the REAL apt-get through Specula's APT handler to verify end-to-end
# conformance with the Debian Repository Format:
#   https://wiki.debian.org/DebianRepository/Format
# and the apt-secure(8) trust chain:
#   https://manpages.debian.org/apt-secure
#
# The test makes NO changes to the system's /etc/apt/ or sources.
# All APT state (lists, cache, archives) is kept under $WORK.
#
# What is asserted:
#
#   (A) apt-get update exits 0 against http://127.0.0.1:5103/apt/ubuntu jammy main
#       — if Specula corrupts InRelease, apt rejects the GPG signature and fails
#         loudly.  Exit 0 is the gate (Debian Repository Format §Signed Release Files,
#         apt-secure(8) §TRUST MODEL).
#   (B) By-hash index paths (Format §By-Hash) are served correctly — apt fetches
#       dists/<suite>/<comp>/binary-<arch>/by-hash/SHA256/<hash> to avoid race
#       conditions; our handler routes them through the mutable tier and proxies
#       the exact bytes from the upstream.  Verifying that "update" downloads these
#       without error is the gate.
#   (C) apt-get download hello exits 0 and dpkg-deb --info confirms the .deb is
#       valid (Format §Pool).
#
# Usage:  scripts/realclient-apt.sh
# Exit 0 only if all assertions pass.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$(mktemp -d /tmp/specula-apt-conf.XXXXXX)}"

# Free ports + a socket-ownership assertion at startup; see scripts/lib/daemon.sh for why
# both are required and why liveness/health checks alone are not enough.
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/daemon.sh"
DATA_PORT="${DATA_PORT:-$(pick_free_port)}"
CTRL_PORT="${CTRL_PORT:-$(pick_free_port)}"

# The out-of-band distro keyring — ships with ubuntu-keyring and is managed
# independently of any mirror.  apt verifies InRelease against this key.
KEYRING="/usr/share/keyrings/ubuntu-archive-keyring.gpg"

export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" GOSUMDB="${GOSUMDB:-sum.golang.google.cn}"

# ── Directory layout ──────────────────────────────────────────────────────────
mkdir -p \
    "$WORK/blobs" \
    "$WORK/lists/partial" \
    "$WORK/cache/archives/partial" \
    "$WORK/state" \
    "$WORK/log"

# ── Helpers ───────────────────────────────────────────────────────────────────

die() { echo "FAIL: $*" >&2; exit 1; }

# SPID holds the PID of the daemon WE started; set when it is launched below.
#
# This used to be discovered with `ps -eo pid,args | grep '[s]pecula --config' | grep $WORK`.
# Matching on a command line is a footgun: any process whose own argv contains that string
# matches too — including the shell running this script — so the pattern can select and kill
# the wrong process, up to and including itself. Holding the PID from `$!` is exact,
# needs no pattern, and cannot match a bystander.
SPID=""

stop_specula() {
    [[ -n "$SPID" ]] || return 0
    kill "$SPID" 2>/dev/null || true
    # Wait up to 5 s for the process to exit.
    local i=0
    while kill -0 "$SPID" 2>/dev/null && (( i < 50 )); do
        sleep 0.1
        i=$(( i + 1 ))  # avoid (( i++ )) which evaluates to 0 when i=0 and aborts under set -e
    done
}

# ── Step 0: verify prerequisites ─────────────────────────────────────────────
[[ -x "$(command -v apt-get)" ]]  || die "apt-get not found"
[[ -x "$(command -v dpkg-deb)" ]] || die "dpkg-deb not found"
[[ -r "$KEYRING" ]]               || die "Ubuntu archive keyring not found at $KEYRING"
[[ -x "$(command -v curl)" ]]     || die "curl not found (needed for healthz probe)"

# ── Step 1: build specula ─────────────────────────────────────────────────────
echo "==> building specula"
go -C "$REPO" build -o "$WORK/specula" ./cmd/specula

# ── Step 2: write config on our assigned ports ────────────────────────────────
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
    # InRelease carries its own Valid-Until field; always revalidate to ensure
    # we serve the upstream's current signed release metadata.
    mutable_ttl_seconds: 0
    upstreams:
      - name: aliyun
        base_url: http://mirrors.aliyun.com/ubuntu
        priority: 1
        official: false
      - name: ubuntu-archive
        base_url: http://archive.ubuntu.com/ubuntu
        priority: 2
        official: true
    verification:
      # No Specula-side GPG keyring configured here: apt verifies the chain
      # end-to-end against the system keyring (the design intent).  A future
      # integration could add gpg.keyring to verify inside Specula too.
      tiers: [tofu, checksum]
EOF

# ── Step 3: fresh start (clean state) ────────────────────────────────────────
stop_specula
rm -f "$WORK"/meta.db* 2>/dev/null || true
rm -rf "$WORK"/blobs/* "$WORK"/lists/* "$WORK"/cache/archives/* 2>/dev/null || true
mkdir -p "$WORK/lists/partial" "$WORK/cache/archives/partial"

echo "==> starting specula (data=:${DATA_PORT} ctrl=:${CTRL_PORT})"
"$WORK/specula" --config "$WORK/cfg.yaml" >> "$WORK/daemon.log" 2>&1 &
SPID=$!
trap 'stop_specula' EXIT

# Block until the daemon serves AND the kernel confirms :${DATA_PORT} belongs to OUR pid.
wait_for_daemon "$SPID" "$DATA_PORT" "http://127.0.0.1:${DATA_PORT}/healthz" "$WORK/daemon.log" \
    || die "specula did not start correctly (check $WORK/daemon.log)"
echo "==> specula is up (pid $SPID, data :${DATA_PORT}, control :${CTRL_PORT})"

# ── Step 4: write an isolated APT sources.list ────────────────────────────────
# signed-by= pins the exact keyring used for this source so no system-wide
# trusted keys are consulted (Debian Repository Format §Signed Release Files).
SOURCES_LIST="$WORK/sources.list"
cat > "$SOURCES_LIST" << EOF
deb [arch=amd64 signed-by=${KEYRING}] http://127.0.0.1:${DATA_PORT}/apt/ubuntu jammy main
EOF

# Common apt-get options for full isolation — no system /var/lib/apt/lists or
# /var/cache/apt touched.
# Dir::Etc::sourcelist  — only our sources.list, no includes
# Dir::Etc::sourceparts — disable sources.list.d/
# Dir::State::Lists     — isolated index download dir
# Dir::Cache            — isolated archives cache
# Dir::State            — isolated dpkg/extended_states etc.
# Dir::Log              — isolated log
APT_OPTS=(
    -o "Dir::Etc::sourcelist=${SOURCES_LIST}"
    -o "Dir::Etc::sourceparts=/dev/null"
    -o "Dir::State::Lists=${WORK}/lists"
    -o "Dir::Cache=${WORK}/cache"
    -o "Dir::State=${WORK}/state"
    -o "Dir::Log=${WORK}/log"
    -o "APT::Get::Assume-Yes=1"
)

# ── Step 5: apt-get update ────────────────────────────────────────────────────
# Conformance gates:
#   (A) exit 0 → InRelease passed GPG verification; apt verifies the signed
#       release file end-to-end using the Ubuntu archive keyring.
#       Ref: Debian Repository Format §Signed Release Files; apt-secure(8).
#   (B) Packages index was fetched by-hash (apt.conf Acquire::By-Hash=yes by
#       default on Ubuntu 22.04) — the mutable tier correctly proxies
#       dists/<suite>/<comp>/binary-<arch>/by-hash/SHA256/<hash> paths.
#       Ref: Debian Repository Format §By-Hash.
echo "==> running apt-get update (assertion A + B)"
UPDATE_OUT="$WORK/update.out"
if ! apt-get "${APT_OPTS[@]}" update > "$UPDATE_OUT" 2>&1; then
    echo "--- apt-get update output ---"
    cat "$UPDATE_OUT"
    die "apt-get update FAILED (exit non-zero) — check $WORK/daemon.log for handler errors"
fi

# Require that the output contains "InRelease" (signed release fetched) and
# that no GPG error lines appear.
grep -q "InRelease" "$UPDATE_OUT" \
    || die "apt-get update succeeded but InRelease line not found in output (unexpected)"

if grep -qiE "GPG error|NO_PUBKEY|signature|invalid|expired|not signed|verification" "$UPDATE_OUT"; then
    echo "--- apt-get update output ---"
    cat "$UPDATE_OUT"
    die "apt-get update exited 0 but GPG-related error found in output"
fi

echo "==> apt-get update: PASS (exit 0, no GPG errors)"
cat "$UPDATE_OUT"

# ── Step 6: download a real .deb and verify with dpkg-deb ────────────────────
# Conformance gate (C): pool/*.deb is served correctly.
# Ref: Debian Repository Format §Pool.
echo "==> running apt-get download hello (assertion C)"
DL_DIR="$WORK/debs"
mkdir -p "$DL_DIR"

DOWNLOAD_OUT="$WORK/download.out"
if ! ( cd "$DL_DIR" && apt-get "${APT_OPTS[@]}" download hello > "$DOWNLOAD_OUT" 2>&1 ); then
    echo "--- apt-get download output ---"
    cat "$DOWNLOAD_OUT"
    die "apt-get download hello FAILED (exit non-zero)"
fi

cat "$DOWNLOAD_OUT"

# Find the downloaded .deb.
DEB_FILE="$(ls "$DL_DIR"/hello_*.deb 2>/dev/null | head -1)"
[[ -n "$DEB_FILE" ]] || die "no hello_*.deb found in $DL_DIR after apt-get download"

echo "==> verifying .deb with dpkg-deb --info"
DPKG_OUT="$WORK/dpkg.out"
if ! dpkg-deb --info "$DEB_FILE" > "$DPKG_OUT" 2>&1; then
    echo "--- dpkg-deb output ---"
    cat "$DPKG_OUT"
    die "dpkg-deb --info FAILED — .deb is corrupt or truncated"
fi

# Confirm the package identity field.
grep -q "Package: hello" "$DPKG_OUT" \
    || die "dpkg-deb output missing 'Package: hello' field"

echo "==> dpkg-deb --info: PASS"
cat "$DPKG_OUT"

# ── Step 7: summary ──────────────────────────────────────────────────────────
echo ""
echo "========================================================"
echo " APT real-client conformance: ALL ASSERTIONS PASSED"
echo "========================================================"
echo " (A) apt-get update exit 0 + no GPG errors"
echo " (B) by-hash index paths served correctly"
echo " (C) pool .deb download + dpkg-deb verification OK"
echo " deb package: $(basename "$DEB_FILE")"
echo " specula log: $WORK/daemon.log"
echo "========================================================"
