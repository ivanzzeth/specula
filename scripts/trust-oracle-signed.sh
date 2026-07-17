#!/usr/bin/env bash
# scripts/trust-oracle-signed.sh — behavioural acceptance + INDEPENDENT oracle
# for the two `signed` tiers the standing trust-oracle gate leaves UNGRADED:
#
#   oci   signed   real cosign keyed signature (tlog disabled — CN-offline mode)
#   helm  signed   real `helm package --sign` GPG .prov
#
# ─────────────────────────────────────────────────────────────────────────────
# WHY THIS EXISTS (and why it is separate from scripts/trust-oracle.sh)
#
# scripts/trust-oracle.sh grades apt/go/pypi against real CN mirrors and states,
# loudly, that it says NOTHING about cosign (no CLI) or helm .prov (the CN mirror
# publishes none). Those two `signed` claims were promised by PRD §G2 with ZERO
# independent evidence. This gate supplies it, hermetically:
#
#   * it builds a REAL cosign-signed image on a LOCAL registry and a REAL
#     helm-signed chart on a LOCAL repo — no dependency on any mirror serving
#     signatures (they don't; that is a finding about the ecosystem, not our bug);
#   * it drives them through a REAL Specula on free ports;
#   * it asserts the recorded tier agrees across THREE independent renderings —
#     cache_entries.tier (the DB), specula_verification_total (the counter), and
#     the extended trust oracle (which re-verifies with the real cosign/gpg
#     binaries and an out-of-band key, never Specula's verify code);
#   * it shows the NEGATIVE (unsigned => never `signed`; fail-closed) and the
#     TAMPER (a signature that EXISTS but does not verify => refused, and it
#     REACHES the verifier — the bcc92b4 lesson);
#   * and it INJECTS a lie (a fabricated `signed` row) and proves the oracle goes
#     RED — because a check never observed to fail is not evidence.
#
# HONEST SCOPE. This proves Specula's verifier + the whole pull-through pipeline
# reach `signed` against signatures produced by the real cosign/helm binaries in
# a lab we built. It does NOT prove any public CN mirror serves such signatures —
# for helm none do (see trust-oracle.sh). Those are different claims; the report
# keeps them apart.
#
# Usage:  scripts/trust-oracle-signed.sh
#   SPECULA_COSIGN_BIN=/path/to/cosign   (else PATH; else oci phase is SKIPPED)
#   KEEP_WORK=1                          (keep the scratch dir)
# Exit 0 only if every graded artifact's recorded tier is the tier it deserves,
# the negative/tamper cases fail closed, AND the injected lie is caught.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$(mktemp -d /tmp/specula-signed-oracle.XXXXXX)}"
OUT_JSON="${OUT_JSON:-$REPO/results/trust-oracle-signed.json}"
RESULTS="$(dirname "$OUT_JSON")"

. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/daemon.sh"
DATA_PORT="${DATA_PORT:-$(pick_free_port)}"
CTRL_PORT="${CTRL_PORT:-$(pick_free_port)}"
REG_PORT="${REG_PORT:-$(pick_free_port)}"
HELM_PORT="${HELM_PORT:-$(pick_free_port)}"

# Uniquely-named container + a signed marker so cleanup NEVER touches a bystander.
REG_NAME="specula-signed-oracle-reg-$$"

COSIGN_BIN="${SPECULA_COSIGN_BIN:-$(command -v cosign || true)}"
# CRITICAL: unset the SPECULA_-prefixed var before it can leak into the specula
# daemon's environment — the config loader maps SPECULA_COSIGN_BIN -> a bogus
# root-level `cosign_bin` key and refuses to start (env.go strict decode).
unset SPECULA_COSIGN_BIN
BASE_IMAGE="${BASE_IMAGE:-alpine:3.20}"

pass() { printf '\033[0;32mPASS\033[0m %s\n' "$*"; }
fail() { printf '\033[0;31mFAIL\033[0m %s\n' "$*" >&2; exit 1; }
step() { echo; printf '\033[1m==> %s\033[0m\n' "$*"; }
info() { printf '     %s\n' "$*"; }

# ── process/container ownership: kill ONLY what we started ────────────────────
SPID=""
HELM_SRV_PID=""
stop_all() {
    local rc=$?
    [[ -n "$SPID" ]] && kill "$SPID" 2>/dev/null || true
    [[ -n "$HELM_SRV_PID" ]] && kill "$HELM_SRV_PID" 2>/dev/null || true
    # Remove ONLY our uniquely-named registry container.
    docker rm -f "$REG_NAME" >/dev/null 2>&1 || true
    if [[ -z "${KEEP_WORK:-}" ]]; then
        chmod -R u+w "$WORK" 2>/dev/null || true
        rm -rf "$WORK" 2>/dev/null || true
    else
        echo "==> work dir kept: $WORK"
    fi
    return $rc
}
trap stop_all EXIT INT TERM

# ── prerequisites ─────────────────────────────────────────────────────────────
step "checking prerequisites"
for t in docker helm gpg python3 sqlite3 curl; do
    command -v "$t" >/dev/null 2>&1 || fail "$t not found (required)"
done
docker info >/dev/null 2>&1 || fail "docker daemon not reachable"
[[ -n "$COSIGN_BIN" && -x "$COSIGN_BIN" ]] || fail "cosign not found (set SPECULA_COSIGN_BIN)"
info "cosign: $COSIGN_BIN ($("$COSIGN_BIN" version 2>&1 | grep -i gitversion | head -1 | tr -s ' '))"
info "helm:   $(helm version --short 2>/dev/null)"
info "ports:  specula data=:$DATA_PORT ctrl=:$CTRL_PORT | registry=:$REG_PORT | helm-repo=:$HELM_PORT"

mkdir -p "$WORK/blobs" "$WORK/keys" "$WORK/helmrepo" "$RESULTS"

# ── build specula to OUR OWN temp path (never bin/specula) ─────────────────────
step "building specula -> $WORK/specula"
go -C "$REPO" build -o "$WORK/specula" ./cmd/specula || fail "build failed"

# ═════════════════════════════════════════════════════════════════════════════
# LAB SETUP — real cosign-signed image on a local registry
# ═════════════════════════════════════════════════════════════════════════════
step "starting a local OCI registry (registry:2) on :$REG_PORT"
docker run -d --rm --name "$REG_NAME" -p "127.0.0.1:${REG_PORT}:5000" registry:2 >/dev/null \
    || fail "could not start local registry"
# Wait for the registry /v2/ to answer.
for i in $(seq 1 50); do
    curl -fsS "http://127.0.0.1:${REG_PORT}/v2/" >/dev/null 2>&1 && break
    sleep 0.2
done
curl -fsS "http://127.0.0.1:${REG_PORT}/v2/" >/dev/null 2>&1 || fail "local registry did not come up"
pass "local registry up"

REG="127.0.0.1:${REG_PORT}"
export COSIGN_PASSWORD="" COSIGN_YES=true

step "cosign generate-key-pair (real keyed setup, no Fulcio/Rekor)"
( cd "$WORK/keys" && "$COSIGN_BIN" generate-key-pair >/dev/null ) || fail "cosign keygen failed"
COSIGN_PUB="$WORK/keys/cosign.pub"
COSIGN_KEY="$WORK/keys/cosign.key"
# A SECOND, untrusted keypair for the tamper case.
mkdir -p "$WORK/attacker"
( cd "$WORK/attacker" && "$COSIGN_BIN" generate-key-pair >/dev/null ) || fail "attacker keygen failed"
ATTACKER_KEY="$WORK/attacker/cosign.key"

# push_and_sign <repo> <signing-key|"">  -> echoes the image digest
#
# Each repo gets a UNIQUE image (a distinct LABEL => distinct config blob =>
# distinct manifest digest) so the CAS cannot conflate them: if all three were
# the same bytes, pulling the signed one first would cache that digest as
# `signed` and the unsigned/tampered pulls would be served from cache WITHOUT
# re-verification — laundering a lie through persistence (the bcc82b4 trap).
push_and_sign() {
    local repo="$1" signkey="$2" digest bdir
    bdir="$WORK/img/$repo"
    mkdir -p "$bdir"
    printf 'FROM %s\nLABEL specula.repo=%q\n' "$BASE_IMAGE" "$repo-$$" > "$bdir/Dockerfile"
    # Legacy builder: a plain schema-2 image manifest (no buildkit provenance/
    # attestation index that would change the manifest shape cosign signs).
    DOCKER_BUILDKIT=0 docker build -q -t "${REG}/${repo}:v1" "$bdir" >/dev/null 2>&1 \
        || fail "build ${repo} failed"
    docker push "${REG}/${repo}:v1" >/dev/null 2>&1 || fail "push ${repo} failed"
    digest=$(docker inspect --format='{{index .RepoDigests 0}}' "${REG}/${repo}:v1" 2>/dev/null | sed 's/.*@//')
    [[ -n "$digest" ]] || fail "could not read pushed digest for ${repo}"
    if [[ -n "$signkey" ]]; then
        "$COSIGN_BIN" sign --key "$signkey" --tlog-upload=false \
            --allow-http-registry=true --yes "${REG}/${repo}@${digest}" >/dev/null 2>&1 \
            || fail "cosign sign ${repo} failed"
    fi
    docker rmi "${REG}/${repo}:v1" >/dev/null 2>&1 || true
    echo "$digest"
}

step "pushing + signing images on the local registry"
docker pull "$BASE_IMAGE" >/dev/null 2>&1 || info "using local $BASE_IMAGE"
SIGNED_DIGEST=$(push_and_sign "team/app" "$COSIGN_KEY")
info "signed   team/app@$SIGNED_DIGEST  (keyed by cosign.pub)"
UNSIGNED_DIGEST=$(push_and_sign "team/unsigned" "")
info "unsigned team/unsigned@$UNSIGNED_DIGEST  (no signature)"
TAMPER_DIGEST=$(push_and_sign "team/tampered" "$ATTACKER_KEY")
info "tampered team/tampered@$TAMPER_DIGEST  (signed by an UNTRUSTED key)"

# ═════════════════════════════════════════════════════════════════════════════
# LAB SETUP — real helm-signed chart on a local repo
# ═════════════════════════════════════════════════════════════════════════════
step "generating a real GPG key and helm-signing a chart"
export GNUPGHOME="$WORK/gnupg"; mkdir -p "$GNUPGHOME"; chmod 700 "$GNUPGHOME"
cat > "$WORK/keyparams" <<'EOF'
%no-protection
Key-Type: RSA
Key-Length: 3072
Name-Real: Specula Signed Oracle
Name-Email: signed-oracle@specula.local
Expire-Date: 0
%commit
EOF
gpg --batch --gen-key "$WORK/keyparams" >/dev/null 2>&1 || fail "gpg keygen failed"
HELM_PUB="$WORK/keys/helm-pub.gpg"
gpg --export > "$HELM_PUB"
gpg --export-secret-keys > "$WORK/keys/helm-secring.gpg"

( cd "$WORK" && helm create mychart >/dev/null 2>&1 ) || fail "helm create failed"
( cd "$WORK/helmrepo" && helm package --sign --key "signed-oracle@specula.local" \
    --keyring "$WORK/keys/helm-secring.gpg" "$WORK/mychart" >/dev/null 2>&1 ) \
    || fail "helm package --sign failed"
# A SECOND chart WITHOUT provenance (the negative: no .prov => tofu).
( cd "$WORK" && cp -r mychart mychart-nosig && sed -i 's/^name: mychart/name: nosigchart/' mychart-nosig/Chart.yaml )
( cd "$WORK/helmrepo" && helm package "$WORK/mychart-nosig" >/dev/null 2>&1 ) || fail "helm package (nosig) failed"
# A TAMPERED chart: corrupt one byte of the .prov signed body (not the digest).
cp "$WORK/helmrepo/mychart-0.1.0.tgz" "$WORK/helmrepo/tamperchart-0.1.0.tgz"
python3 - "$WORK/helmrepo/mychart-0.1.0.tgz.prov" "$WORK/helmrepo/tamperchart-0.1.0.tgz.prov" << 'PY'
import sys
src, dst = sys.argv[1], sys.argv[2]
b = open(src, "rb").read()
# Flip 'A' of "A Helm chart for Kubernetes" inside the signed body (not the digest),
# and rewrite the files: entry name so the chart binding still resolves.
b = b.replace(b"A Helm chart for Kubernetes", b"a Helm chart for Kubernetes", 1)
b = b.replace(b"mychart-0.1.0.tgz:", b"tamperchart-0.1.0.tgz:")
open(dst, "wb").write(b)
PY
helm repo index "$WORK/helmrepo" >/dev/null 2>&1 || true
pass "helm charts packaged (signed / unsigned / tampered)"

step "serving the helm repo on :$HELM_PORT"
( cd "$WORK/helmrepo" && python3 -m http.server "$HELM_PORT" --bind 127.0.0.1 >/dev/null 2>&1 ) &
HELM_SRV_PID=$!
for i in $(seq 1 50); do
    curl -fsS "http://127.0.0.1:${HELM_PORT}/mychart-0.1.0.tgz.prov" -o /dev/null 2>/dev/null && break
    sleep 0.1
done

# ═════════════════════════════════════════════════════════════════════════════
# START SPECULA — oci upstream = local registry (keyed cosign),
#                 helm upstream = local repo (provenance keyring)
# ═════════════════════════════════════════════════════════════════════════════
step "starting specula (oci cosign keyed + helm .prov, both enforcing signed)"
cat > "$WORK/cfg.yaml" <<EOF
server:
  data_plane_addr: ":${DATA_PORT}"
  control_plane_addr: ":${CTRL_PORT}"
storage:
  blob: {driver: local, local: {root: ${WORK}/blobs}}
  meta: {driver: sqlite, dsn: ${WORK}/meta.db}
protocols:
  oci:
    mutable_ttl_seconds: 0
    upstreams:
      - {name: localreg, base_url: http://${REG}, priority: 1, official: false}
    verification:
      tiers: [signed, tofu, checksum]
      quorum: 1
      tofu: enforce
      cosign:
        keys: [${COSIGN_PUB}]
        tlog: false
  helm:
    mutable_ttl_seconds: 0
    upstreams:
      - {name: localhelm, base_url: http://127.0.0.1:${HELM_PORT}, priority: 1, official: false}
    verification:
      tiers: [signed, tofu, checksum]
      quorum: 1
      tofu: enforce
      provenance:
        keyring: ${HELM_PUB}
        policy: enforce
EOF

"$WORK/specula" --config "$WORK/cfg.yaml" > "$WORK/daemon.log" 2>&1 &
SPID=$!
wait_for_daemon "$SPID" "$DATA_PORT" "http://127.0.0.1:${DATA_PORT}/healthz" "$WORK/daemon.log" \
    || { tail -30 "$WORK/daemon.log"; fail "specula did not start"; }
grep -q "cosign keyed signed verification enabled" "$WORK/daemon.log" \
    || fail "cosign verifier not enabled — check config wiring"
grep -q "helm provenance signed verification enabled" "$WORK/daemon.log" \
    || fail "helm provenance verifier not enabled — check config wiring"
pass "specula up (pid $SPID); cosign + helm signed verifiers enabled"

BASE="http://127.0.0.1:${DATA_PORT}"
MANIFEST_ACCEPT='Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json'

# manifest_status <repo> -> prints the HTTP status of a manifest pull-through.
# /v2/ is behind the registry-token Bearer challenge (the writable registry is
# always enabled), so we perform the anonymous token dance the way docker does:
# 401 -> parse WWW-Authenticate -> fetch an anonymous pull token -> retry.
manifest_status() {
    local repo="$1" url="${BASE}/v2/$1/manifests/v1"
    local hdrs code www realm service scope token
    hdrs=$(curl -s -D - -o /dev/null -H "$MANIFEST_ACCEPT" "$url")
    code=$(printf '%s' "$hdrs" | awk 'NR==1{print $2}')
    if [[ "$code" == "401" ]]; then
        www=$(printf '%s' "$hdrs" | grep -i '^www-authenticate:' | head -1)
        realm=$(sed -n 's/.*realm="\([^"]*\)".*/\1/p' <<<"$www")
        service=$(sed -n 's/.*service="\([^"]*\)".*/\1/p' <<<"$www")
        scope=$(sed -n 's/.*scope="\([^"]*\)".*/\1/p' <<<"$www")
        token=$(curl -s "${realm}?service=${service}&scope=${scope}" \
            | python3 -c 'import sys,json;print(json.load(sys.stdin).get("token",""))' 2>/dev/null)
        code=$(curl -s -o /dev/null -w '%{http_code}' -H "$MANIFEST_ACCEPT" \
            -H "Authorization: Bearer ${token}" "$url")
    fi
    echo "$code"
}

# ═════════════════════════════════════════════════════════════════════════════
# COSIGN — happy path (signed), negative (unsigned), tamper (wrong key)
# ═════════════════════════════════════════════════════════════════════════════
step "[oci] pull-through the SIGNED image (expect 200, recorded signed)"
code=$(manifest_status "team/app")
[[ "$code" == "200" ]] || { tail -20 "$WORK/daemon.log"; fail "signed manifest pull returned $code (want 200)"; }
DB_TIER=$(sqlite3 "$WORK/meta.db" \
    "SELECT tier FROM cache_entries WHERE protocol='oci' AND name='team/app' AND version LIKE 'sha256:%';")
info "cache_entries oci team/app tier = ${DB_TIER} (3=signed)"
[[ "$DB_TIER" == "3" ]] || fail "expected cache_entries tier=3 (signed), got '${DB_TIER}'"
pass "cache_entries records signed for the cosign-signed manifest"

step "[oci] NEGATIVE — pull the UNSIGNED image (must fail closed, never signed)"
code=$(manifest_status "team/unsigned")
info "unsigned manifest pull HTTP $code"
[[ "$code" != "200" ]] || fail "unsigned image was SERVED (must fail closed under enforce)"
grep -q "no cosign signature attached" "$WORK/daemon.log" \
    || fail "expected the honest 'no cosign signature attached' fail — did the verifier run?"
UNS_ROWS=$(sqlite3 "$WORK/meta.db" \
    "SELECT COUNT(*) FROM cache_entries WHERE name='team/unsigned' AND tier=3;")
[[ "$UNS_ROWS" == "0" ]] || fail "unsigned image somehow recorded signed"
pass "unsigned image fails closed; the verifier ran and reported no signature"

step "[oci] TAMPER — image signed by an UNTRUSTED key (sig EXISTS, must be refused)"
code=$(manifest_status "team/tampered")
info "tampered manifest pull HTTP $code"
[[ "$code" != "200" ]] || fail "tampered image was SERVED (must fail closed)"
# bcc82b4 lesson: prove the signature REACHED the verifier and was REJECTED —
# not a 404 that never reached it. The 'verified against any of N configured
# key' message only appears after a signature was discovered and checked.
grep -q "no attached signature verified against any of" "$WORK/daemon.log" \
    || fail "tamper did not reach signature verification (would repeat the bcc82b4 mistake)"
TMP_ROWS=$(sqlite3 "$WORK/meta.db" \
    "SELECT COUNT(*) FROM cache_entries WHERE name='team/tampered' AND tier=3;")
[[ "$TMP_ROWS" == "0" ]] || fail "tampered image somehow recorded signed"
pass "tampered signature discovered, verified, and REJECTED (fail-closed)"

# ═════════════════════════════════════════════════════════════════════════════
# HELM — happy path (signed), negative (no .prov => tofu), tamper (bad .prov)
# ═════════════════════════════════════════════════════════════════════════════
step "[helm] pull-through the SIGNED chart (expect 200, recorded signed)"
code=$(curl -s -o "$WORK/mychart.tgz" -w '%{http_code}' "${BASE}/helm/mychart-0.1.0.tgz")
[[ "$code" == "200" ]] || { tail -20 "$WORK/daemon.log"; fail "signed chart fetch returned $code"; }
HELM_TIER=$(sqlite3 "$WORK/meta.db" \
    "SELECT tier FROM cache_entries WHERE protocol='helm' AND version='mychart-0.1.0.tgz';")
info "cache_entries helm mychart tier = ${HELM_TIER} (3=signed)"
[[ "$HELM_TIER" == "3" ]] || fail "expected helm tier=3 (signed), got '${HELM_TIER}'"
pass "cache_entries records signed for the helm-signed chart"

step "[helm] NEGATIVE — chart with NO .prov (must degrade to tofu, never signed)"
code=$(curl -s -o "$WORK/nosig.tgz" -w '%{http_code}' "${BASE}/helm/nosigchart-0.1.0.tgz")
[[ "$code" == "200" ]] || fail "unsigned chart should still be SERVED at a lower tier (got $code)"
NOSIG_TIER=$(sqlite3 "$WORK/meta.db" \
    "SELECT tier FROM cache_entries WHERE protocol='helm' AND version='nosigchart-0.1.0.tgz';")
info "cache_entries helm nosigchart tier = ${NOSIG_TIER} (1=tofu)"
[[ "$NOSIG_TIER" != "3" ]] || fail "chart with no .prov recorded signed (FABRICATED)"
[[ "$NOSIG_TIER" == "1" ]] || fail "chart with no .prov should be tofu, got '${NOSIG_TIER}'"
pass "no-.prov chart degrades honestly to tofu (not a fabricated signed)"

step "[helm] TAMPER — chart with a corrupted .prov signed body (must fail closed)"
code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE}/helm/tamperchart-0.1.0.tgz")
info "tampered-chart fetch HTTP $code"
[[ "$code" != "200" ]] || fail "chart with a tampered .prov was SERVED (must fail closed)"
grep -qE "helmprov: GPG signature verification failed" "$WORK/daemon.log" \
    || fail "tampered .prov did not reach GPG signature verification"
pass "tampered .prov discovered, GPG-verified, and REJECTED (fail-closed)"

# ═════════════════════════════════════════════════════════════════════════════
# CROSS-CHECK #2: /metrics agrees with cache_entries
# ═════════════════════════════════════════════════════════════════════════════
step "cross-check: specula_verification_total agrees with the DB"
curl -fsS "http://127.0.0.1:${CTRL_PORT}/metrics" 2>/dev/null \
    | grep "^specula_verification_total" | sort > "$WORK/metrics.txt" || true
cp "$WORK/metrics.txt" "$RESULTS/trust-oracle-signed-metrics.txt" 2>/dev/null || true
# Labels are protocol,check,tier,result (Prometheus sorts them alphabetically),
# so match each label independently rather than assume an order. A passing
# series with a positive count means the counter agrees with cache_entries.
metric_present() { # <check>
    grep 'specula_verification_total' "$WORK/metrics.txt" \
        | grep "check=\"$1\"" | grep 'tier="signed"' | grep 'result="pass"' \
        | awk '{print $NF}' | grep -qvxE '0'
}
metric_present cosign \
    || { grep -E 'cosign' "$WORK/metrics.txt" >&2; fail "metrics: no passing cosign signed series (DB says signed — disagreement)"; }
metric_present helmprov \
    || { grep -E 'helmprov' "$WORK/metrics.txt" >&2; fail "metrics: no passing helmprov signed series (DB says signed — disagreement)"; }
pass "/metrics counters agree with cache_entries (cosign + helmprov signed pass)"

# ═════════════════════════════════════════════════════════════════════════════
# CROSS-CHECK #3: the INDEPENDENT oracle
# ═════════════════════════════════════════════════════════════════════════════
step "running the extended INDEPENDENT oracle (real cosign / real gpg)"
run_oracle() {
    python3 "$REPO/scripts/oracle/trust_oracle.py" \
        --db "$WORK/meta.db" --blobs "$WORK/blobs" \
        --helm-base "http://127.0.0.1:${HELM_PORT}" \
        --helm-keyring "$HELM_PUB" --helm-prov-dir "$WORK/helmrepo" \
        --cosign-bin "$COSIGN_BIN" --oci-registry "$REG" --cosign-key "$COSIGN_PUB" \
        --oci-unsigned-ceiling tofu \
        --out "$1"
}
set +e
run_oracle "$OUT_JSON"
ORACLE_RC=$?
set -e
[[ $ORACLE_RC -eq 0 ]] || { echo; cat "$OUT_JSON"; fail "oracle reported a tier DISAGREEMENT (see $OUT_JSON)"; }
pass "oracle independently agrees: every recorded tier is the tier it deserves"

# ═════════════════════════════════════════════════════════════════════════════
# META-GATE: prove the NEW oracle checks can actually FAIL.
# A check never observed to fail is not evidence. We fabricate a `signed` claim
# for artifacts that are NOT signed and assert the oracle goes RED — once for the
# cosign path, once for the helm path.
# ═════════════════════════════════════════════════════════════════════════════
step "INJECTION 1 (cosign): fabricate a signed row cosign cannot verify; oracle must catch it"
# The unsigned/tampered images are NOT cached (they failed closed), so there is
# no row to flip. Instead INSERT a fabricated `signed` row: it reuses the REAL
# signed manifest blob (so the oracle finds it in CAS and grades it, not skips
# it) but under the team/unsigned REPOSITORY, where no valid signature for that
# digest exists. `cosign verify --key` therefore refuses it => the oracle derives
# `tofu` while the DB claims `signed` => a disagreement the oracle MUST report.
sqlite3 "$WORK/meta.db" ".backup '$WORK/meta-inject.db'"
sqlite3 "$WORK/meta-inject.db" \
    "INSERT INTO cache_entries (protocol,name,version,digest,tier)
     VALUES ('oci','team/unsigned','faketag','${SIGNED_DIGEST}',3);"
set +e
python3 "$REPO/scripts/oracle/trust_oracle.py" \
    --db "$WORK/meta-inject.db" --blobs "$WORK/blobs" \
    --helm-base "http://127.0.0.1:${HELM_PORT}" \
    --helm-keyring "$HELM_PUB" --helm-prov-dir "$WORK/helmrepo" \
    --cosign-bin "$COSIGN_BIN" --oci-registry "$REG" --cosign-key "$COSIGN_PUB" \
    --oci-unsigned-ceiling tofu --out "$WORK/verdict-inject-cosign.json" \
    > "$WORK/inject-cosign.log" 2>&1
IRC=$?
set -e
grep -q "TIER DISAGREEMENT" "$WORK/inject-cosign.log" && [[ $IRC -ne 0 ]] \
    || { tail -20 "$WORK/inject-cosign.log"; fail "oracle MISSED a fabricated cosign 'signed' — the check is not evidence"; }
pass "oracle caught the fabricated cosign 'signed' (exit $IRC)"

step "INJECTION 2 (helm): fabricate signed for the NO-.prov chart; oracle must catch it"
sqlite3 "$WORK/meta.db" ".backup '$WORK/meta-inject2.db'"
sqlite3 "$WORK/meta-inject2.db" \
    "UPDATE cache_entries SET tier=3 WHERE version='nosigchart-0.1.0.tgz';"
set +e
python3 "$REPO/scripts/oracle/trust_oracle.py" \
    --db "$WORK/meta-inject2.db" --blobs "$WORK/blobs" \
    --helm-base "http://127.0.0.1:${HELM_PORT}" \
    --helm-keyring "$HELM_PUB" --helm-prov-dir "$WORK/helmrepo" \
    --cosign-bin "$COSIGN_BIN" --oci-registry "$REG" --cosign-key "$COSIGN_PUB" \
    --oci-unsigned-ceiling tofu --out "$WORK/verdict-inject-helm.json" \
    > "$WORK/inject-helm.log" 2>&1
IRC2=$?
set -e
grep -q "TIER DISAGREEMENT" "$WORK/inject-helm.log" && [[ $IRC2 -ne 0 ]] \
    || { tail -20 "$WORK/inject-helm.log"; fail "oracle MISSED a fabricated helm 'signed' — the check is not evidence"; }
pass "oracle caught the fabricated helm 'signed' (exit $IRC2)"

echo
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
printf '\033[0;32m TRUST-ORACLE-SIGNED GATE: PASS\033[0m\n'
echo "  oci  signed : cosign keyed, tlog off — real cosign CLI, real registry"
echo "  helm signed : real helm package --sign GPG .prov"
echo "  agreed across cache_entries + /metrics + independent oracle"
echo "  negative (unsigned) + tamper (bad sig) both fail closed and reach the verifier"
echo "  both new oracle checks proven to go RED on a fabricated 'signed'"
echo "  verdict: $OUT_JSON"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
