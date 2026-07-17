#!/usr/bin/env bash
# scripts/trust-oracle-mutations.sh — the META-GATE for scripts/trust-oracle.sh.
#
# ─────────────────────────────────────────────────────────────────────────────
# A CHECK THAT HAS NEVER BEEN OBSERVED TO FAIL IS NOT EVIDENCE.
#
# scripts/trust-oracle.sh passing tells you nothing on its own. An oracle that
# silently grades nothing, or that grades a fresh upstream copy instead of the
# bytes Specula stored, or whose comparison is inverted, passes exactly the same
# way — green, fast, and worthless. The ONLY thing that makes a green oracle mean
# anything is a demonstration that it goes red when Specula lies.
#
# So this script injects real lies into Specula's verify code, one at a time,
# rebuilds, and asserts the oracle CATCHES each one. Every injection is a bug we
# have actually shipped:
#
#   M1  tier upgrade         record `signed` where only a checksum was compared.
#   M2  by-hash literal      the literal path lookup that made apt record tofu x6
#                            while the docs called it the end-to-end gold standard.
#   M3  skip-as-pass         a check reporting StatusPass at its own tier without
#                            ever running — "I skipped this" made byte-identical
#                            to "I checked it and it passed".
#
# A MUTATION THAT FAILS TO COMPILE PROVES NOTHING: it would make the gate go red
# for a build error, not for a caught lie, and would be indistinguishable from a
# working oracle. So every mutation is compiled BEFORE it is graded, and a
# mutation that does not build is a hard failure of this meta-gate.
#
# Safety: the tree is mutated IN PLACE and restored by an EXIT trap that fires on
# every path including SIGINT. Restore and verification both go through git, and
# the targets are asserted clean before anything is touched — see the precondition
# below for why a self-checked backup was not good enough (it once reported a
# successful restore while an injected lie sat in the tree).
#
# Usage:  scripts/trust-oracle-mutations.sh
# Exit 0 only if EVERY injected lie was caught by the oracle.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BACKUP="$(mktemp -d /tmp/specula-mutation-backup.XXXXXX)"
MUT_RESULTS="${MUT_RESULTS:-$REPO/results/trust-oracle-mutations.json}"

# Files this script is allowed to touch. Each is asserted clean vs git, backed up
# before any mutation, and restored afterwards. Keep this list EXACTLY in sync with
# the injections below: a file mutated but absent here is never backed up and never
# restored — which is how an injected lie escaped into the working tree once.
TARGETS=(
    "internal/verify/tofu.go"   # M1
    "internal/verify/gpg.go"    # M2
    "internal/verify/sumdb.go"  # M3
)

# PRECONDITION: every mutation target must be CLEAN vs git before we start.
#
# This is not fussiness — it is what makes the restore verifiable. If a target
# already carries uncommitted work we cannot distinguish OUR injected lie from
# YOUR edit, and "restore" becomes a guess that could either destroy your work or
# leave a lie in the tree.
#
# Learned the hard way, in this very script: the first design backed each file up
# and verified the restore by comparing the file to that backup. When an earlier
# run died mid-flight and left a mutation in the tree, the next run backed up the
# ALREADY-MUTATED file and then cheerfully reported "restored and verified
# byte-identical" — comparing the lie to itself. A backup cannot validate itself.
# git is an independent reference that a mutated working tree cannot forge, which
# is the same reason the oracle it guards uses gpg instead of our own openpgp code.
echo "==> asserting mutation targets are clean vs git (required for a verifiable restore)"
if ! git -C "$REPO" diff --quiet -- "${TARGETS[@]}"; then
    echo "FATAL: one or more mutation targets have uncommitted changes:" >&2
    git -C "$REPO" status --short -- "${TARGETS[@]}" >&2
    echo "       Refusing to run: an injected mutation could not then be told apart" >&2
    echo "       from your work, and the restore could not be verified." >&2
    exit 1
fi
echo "    clean — restore is verifiable against git"

echo "==> backing up mutation targets to $BACKUP (belt and braces; git is the authority)"
for f in "${TARGETS[@]}"; do
    mkdir -p "$BACKUP/$(dirname "$f")"
    cp "$REPO/$f" "$BACKUP/$f"
done

restore_all() {
    local rc=$?
    # This trap must NEVER abort part-way: leaving an injected lie in the tree is
    # catastrophically worse than any failure it could report. set +e locally, and
    # never let a failed echo (e.g. EPIPE when stdout is a closed `| head`) skip
    # the restore below — that is exactly how a mutation escaped into the tree once.
    set +e
    echo >&2
    echo "==> restoring pristine sources (git checkout, not the backup)" >&2
    git -C "$REPO" checkout -- "${TARGETS[@]}" 2>/dev/null

    # Verify against git, which the working tree cannot forge.
    if git -C "$REPO" diff --quiet -- "${TARGETS[@]}"; then
        echo "    all mutation targets verified clean vs git" >&2
        rm -rf "$BACKUP"
    else
        echo "FATAL: mutation targets DID NOT RESTORE. Still dirty:" >&2
        git -C "$REPO" status --short -- "${TARGETS[@]}" >&2
        echo "       Pristine copies kept at $BACKUP" >&2
        echo "       Run: git -C $REPO checkout -- ${TARGETS[*]}" >&2
        exit 1
    fi
    return $rc
}
trap restore_all EXIT INT TERM

# apply_mutation <file> <python-expression-file> — rewrites $file via python.
# Uses exact-string replacement and FAILS LOUDLY if the anchor is not found: a
# silently no-op mutation would make the oracle "pass" and be misread as the
# oracle failing to catch a lie that was never actually injected.
apply_mutation() {
    local file="$1" old="$2" new="$3"
    python3 - "$REPO/$file" "$old" "$new" << 'PYEOF'
import sys
path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
src = open(path).read()
if old not in src:
    print(f"FATAL: mutation anchor not found in {path}", file=sys.stderr)
    print(f"  looked for: {old[:120]!r}", file=sys.stderr)
    print("  the mutation would be a silent no-op; refusing.", file=sys.stderr)
    sys.exit(1)
if src.count(old) != 1:
    print(f"FATAL: anchor matches {src.count(old)}x in {path}; need exactly 1", file=sys.stderr)
    sys.exit(1)
open(path, "w").write(src.replace(old, new))
PYEOF
}

RESULTS_JSON="["
CAUGHT=0
MISSED=0

# run_injection <id> <description> — build, run the oracle, expect it to FAIL.
run_injection() {
    local id="$1" desc="$2"

    echo
    echo "─────────────────────────────────────────────────────────────────────"
    echo "INJECTION $id: $desc"
    echo "─────────────────────────────────────────────────────────────────────"

    # A mutation that does not compile proves nothing about the oracle.
    echo "==> compiling the mutated tree (a mutation that won't build proves nothing)"
    if ! go -C "$REPO" build -o /dev/null ./... 2>"$BACKUP/build-$id.log"; then
        echo "FATAL: mutation $id DOES NOT COMPILE — it proves nothing. Build log:" >&2
        cat "$BACKUP/build-$id.log" >&2
        exit 1
    fi
    echo "    compiles cleanly"

    echo "==> running the oracle against the LYING binary (expecting it to go RED)"
    local out="$BACKUP/oracle-$id.log"
    local rc=0
    OUT_JSON="$BACKUP/verdict-$id.json" bash "$REPO/scripts/trust-oracle.sh" > "$out" 2>&1 || rc=$?

    # A gate that dies BEFORE grading (upstream flake, fail-closed 502) exits
    # non-zero too. Counting that as "caught" would be the worst possible error
    # here: the meta-gate would certify an oracle that never looked at anything.
    # Require positive proof the oracle ran AND reported a tier disagreement.
    if ! grep -q "verdict table written to" "$out" && ! grep -q "TIER DISAGREEMENT" "$out"; then
        echo "    !!! GATE ABORTED before the oracle graded anything — NOT a catch."
        echo "    --- output tail ---"
        tail -20 "$out" | sed 's/^/      /'
        MISSED=$(( MISSED + 1 ))
        RESULTS_JSON+="{\"injection\":\"$id\",\"description\":\"$desc\",\"caught\":false,"
        RESULTS_JSON+="\"reason\":\"gate aborted before grading\",\"oracle_exit\":$rc},"
        return
    fi

    if [[ $rc -ne 0 ]] && grep -q "TIER DISAGREEMENT" "$out"; then
        echo "    ORACLE CAUGHT IT (exit $rc)"
        echo "    disagreements the oracle reported:"
        grep -A 4 "^  \(apt\|gomod\|pypi\|helm\) " "$out" | sed 's/^/      /' | head -30
        CAUGHT=$(( CAUGHT + 1 ))
        RESULTS_JSON+="{\"injection\":\"$id\",\"description\":\"$desc\",\"caught\":true,\"oracle_exit\":$rc},"
    else
        echo "    !!! ORACLE MISSED IT (exit $rc) — the oracle does not detect this lie."
        echo "    --- oracle output tail ---"
        tail -30 "$out" | sed 's/^/      /'
        MISSED=$(( MISSED + 1 ))
        RESULTS_JSON+="{\"injection\":\"$id\",\"description\":\"$desc\",\"caught\":false,\"oracle_exit\":$rc},"
    fi
}

# ═════════════════════════════════════════════════════════════════════════════
# M1 — TIER UPGRADE: claim `signed` for a bare digest pin.
#
# The most direct lie the product can tell. TOFU compares a digest against one we
# ourselves pinned on first sight — it detects later tampering, but it is NOT a
# cryptographic anchor and proves nothing about origin (PRD §G2). Reporting it as
# `signed` is precisely the integrity/authenticity conflation that DESIGN-REVIEW
# §1 exists to forbid: it lets the mirror grade its own homework and calls the
# result publisher authenticity.
#
# Injected at the TOFU verifier rather than the ChecksumVerifier deliberately.
# ChecksumVerifier IS wired (cmd/specula/main.go:166), but its PASS branch needs a
# reference digest, and on every artifact this gate fetches ref.Digest is empty,
# so it self-gates to StatusSkip and reaches no tier. Mutating it is a silent
# no-op — VERIFIED by trying it: the oracle stayed green because no recorded tier
# moved. An injection that changes nothing tests nothing.
# ═════════════════════════════════════════════════════════════════════════════
echo "==> injecting M1 (tier upgrade: a bare TOFU digest pin reported as signed)"
apply_mutation "internal/verify/tofu.go" \
'			Status:  artifact.StatusWarn,
			Tier:    artifact.TierTofu,
			Message: fmt.Sprintf("tofu: first-lock pinned %s → %s", key, art.Digest),' \
'			Status:  artifact.StatusWarn,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("tofu: first-lock pinned %s → %s", key, art.Digest),'
run_injection "M1" "tier upgrade — report a bare TOFU digest pin as 'signed'"
git -C "$REPO" checkout -- "internal/verify/tofu.go"

# ═════════════════════════════════════════════════════════════════════════════
# M2 — apt UNDER-CLAIM: record tofu for indexes actually verified to signed.
#
# The shipped defect looked up InRelease pins by the LITERAL request path. Under
# `Acquire-By-Hash: yes` apt asks for <dir>/by-hash/SHA256/<hex> while InRelease
# pins canonical paths (main/binary-amd64/Packages.xz) and lists zero by-hash
# paths, so every by-hash index missed and got recorded tofu x6 while the docs
# advertised the end-to-end gold standard.
#
# NOTE — the original mutation (making resolveByHashRef a no-op) is NO LONGER
# REPRODUCIBLE, and that is a real result worth recording: the code has since been
# hardened to FAIL CLOSED. Unresolvable by-hash pins now mean the Packages index
# is never chain-verified, so the .deb has no pin and apt gets a 502 rather than a
# quietly-downgraded artifact. The gate aborts at the traffic step before the
# oracle ever runs. Fail-closed is the correct behaviour, so we reproduce the
# SYMPTOM instead — the recorded tier — at the dists-file result: the index is
# genuinely chain-verified to signed, but records tofu. This is the under-claim
# direction: Specula earns `signed` and reports less. A lesser bug than
# over-claiming, but still a bug, and still a lie. It lands on exactly the 6
# dists files this gate fetches (dep11 x4, i18n, cnf) — the historical "tofu x6".
# ═════════════════════════════════════════════════════════════════════════════
echo
echo "==> injecting M2 (apt under-claim: dists indexes verified to signed, recorded tofu)"
apply_mutation "internal/verify/gpg.go" \
'	return artifact.Result{
		Status:  artifact.StatusPass,
		Tier:    artifact.TierSigned,
		Message: fmt.Sprintf("gpg: dists file %q SHA256 chain-verified via InRelease (TierSigned)", relPath),
	}, nil' \
'	// INJECTED BUG (M2): the chain verified this index to signed — then records
	// tofu, reproducing the tier the by-hash literal-lookup defect reported.
	return artifact.Result{
		Status:  artifact.StatusPass,
		Tier:    artifact.TierTofu,
		Message: fmt.Sprintf("gpg: dists file %q SHA256 chain-verified via InRelease (TierSigned)", relPath),
	}, nil'
run_injection "M2" "apt under-claim — dists indexes verified to signed but recorded tofu"
git -C "$REPO" checkout -- "internal/verify/gpg.go"

# ═════════════════════════════════════════════════════════════════════════════
# M3 — SKIP-AS-PASS: a check claiming it ran when it did not.
#
# The sumdb has entries for exactly two things per module version: the zip dirhash
# and the go.mod hash. It commits to NOTHING about .info, so the verifier correctly
# self-gates out with StatusSkip. This injection makes that skip return
# StatusPass @ TierSigned instead — the verifier now reports a cryptographic
# anchor for a file the checksum database has never heard of.
#
# This is the exact shape of the shipped bug ("I skipped this" encoded identically
# to "I checked it and it passed"), rendered in its tier-visible form.
# ═════════════════════════════════════════════════════════════════════════════
echo
echo "==> injecting M3 (skip-as-pass: sumdb reports 'signed' for .info without looking)"
apply_mutation "internal/verify/sumdb.go" \
'	// .info files have no sumdb entry (they carry timestamps, not content hashes).
	if ext == sumdbExtInfo {
		return artifact.Result{
			Status:  artifact.StatusSkip,
			Tier:    artifact.TierChecksum,' \
'	// .info files have no sumdb entry (they carry timestamps, not content hashes).
	if ext == sumdbExtInfo {
		// INJECTED BUG (M3): encode the skip as a PASS at this verifier'"'"'s own
		// tier — claim a signed anchor for a file the sumdb never covered.
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierSigned,'
run_injection "M3" "skip-as-pass — sumdb claims 'signed' for .info it never verified"
git -C "$REPO" checkout -- "internal/verify/sumdb.go"

# ── verdict ──────────────────────────────────────────────────────────────────
RESULTS_JSON="${RESULTS_JSON%,}]"
mkdir -p "$(dirname "$MUT_RESULTS")"
python3 -c "
import json,sys
data = json.loads(sys.argv[1])
out = {
  'schema': 'specula.trust-oracle-mutations/v1',
  'meta_gate': 'proves the oracle goes red when Specula lies',
  'injections': len(data),
  'caught': sum(1 for d in data if d['caught']),
  'missed': sum(1 for d in data if not d['caught']),
  'results': data,
}
json.dump(out, open('$MUT_RESULTS','w'), indent=2)
" "$RESULTS_JSON"

echo
echo "═════════════════════════════════════════════════════════════════════════"
echo "META-GATE RESULT: ${CAUGHT} caught, ${MISSED} missed  (written to $MUT_RESULTS)"
echo "═════════════════════════════════════════════════════════════════════════"
if [[ $MISSED -gt 0 ]]; then
    echo "FAIL: the oracle missed ${MISSED} injected lie(s). A check that does not go"
    echo "      red when the product lies is not evidence — fix the oracle."
    exit 1
fi
echo "PASS: every injected lie was caught. The oracle's green means something."
exit 0
