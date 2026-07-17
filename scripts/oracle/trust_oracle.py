#!/usr/bin/env python3
"""trust_oracle.py — an INDEPENDENT oracle for Specula's honest-tier claims (PRD §G2).

WHAT THIS IS
------------
Specula records, for every artifact it serves, the trust tier it claims to have
reached: `signed` > `consensus` > `tofu` > `checksum` (PRD §G2, DESIGN-REVIEW §1.2).
Both places that tier appears — `cache_entries.tier` and the Prometheus counter
`specula_verification_total{tier}` — are written by Specula's own code. They are
not orthogonal: one bug upstream of both satisfies both. Cross-checking them
proves nothing about whether the tier is DESERVED.

This oracle answers a different question, from outside:

    "What tier does this artifact ACTUALLY deserve?"

It re-derives that answer from the bytes Specula actually stored, using each
ecosystem's OWN reference tooling, and then asserts equality with Specula's claim.
A disagreement means either Specula is lying or the oracle is wrong — both are
findings, and neither can be summarised away, because the verdict lands in a
machine-readable JSON table.

WHY IT IS PYTHON THAT SHELLS OUT
--------------------------------
NON-NEGOTIABLE DESIGN CONSTRAINT: the oracle must not import Specula's verify
packages. An oracle that shares code with the thing it grades is a mirror, and a
mirror agrees with a lie. Every bug in the honest-tier model that shipped here
(apt claiming signed while recording tofu x6; go's sumdb verifier never running
in the documented CN config; every verifier encoding "I skipped this" as
StatusPass @ TierChecksum) would have been invisible to a checker built out of
the same code.

Being a Python program that shells out to `gpg(1)`, the `go` toolchain and
`curl(1)` makes that constraint STRUCTURAL rather than a promise in a comment:
this file cannot import `internal/verify` even by accident. It is not merely that
we chose not to — there is no import path from here to there.

THE TRUST ANCHOR IS ALWAYS OUT-OF-BAND
--------------------------------------
Each oracle below is anchored in something a malicious mirror — or a lying
Specula — cannot forge:

  apt   the distro keyring shipped with the OS (/usr/share/keyrings), never
        fetched from a mirror. gpg(1), not our openpgp code, checks the signature.
  go    the sumdb Ed25519-signed Merkle tree head, checked by the `go` toolchain
        itself against sum.golang.google.cn — not our internal/verify/sumdb*.
  pypi  independently re-fetched PEP 503 indexes from the real mirrors. No
        signature exists in this ecosystem (PEP 740 needs Rekor, blocked in CN),
        so the strongest honest claim is cross-source consensus.

CRITICALLY: every oracle grades THE BYTES SPECULA STORED, read straight out of
the CAS on disk — not a fresh copy it fetched for itself. Grading a fresh copy
would prove the upstream is honest while saying nothing about what Specula served.

Exit status: 0 iff every artifact's Specula tier == the oracle's tier.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import lzma
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import urllib.request
from dataclasses import dataclass, field, asdict
from typing import Optional

# Specula's artifact.Tier enum (internal/artifact/types.go). Mirrored as data, not
# imported: these four names ARE the product's public vocabulary (PRD §G2), and an
# int->name mapping is not verification logic. Nothing else crosses the boundary.
TIER_NAMES = {0: "checksum", 1: "tofu", 2: "consensus", 3: "signed"}

HTTP_TIMEOUT = 30  # aliyun measured ~27 kB/s on one link; keep artifacts small.


@dataclass
class Verdict:
    """One row of the agreement table: what Specula claimed vs what it deserves."""
    protocol: str
    artifact: str
    specula_tier: str
    oracle_tier: str
    agree: bool
    method: str          # which reference tool decided, and how
    evidence: list = field(default_factory=list)
    note: str = ""


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def cas_path(blobs_root: str, digest: str) -> Optional[str]:
    """Resolve a CAS blob. Layout: <root>/<first 2 hex>/<full hex>
    (internal/store/local/local.go)."""
    hexd = digest.split(":", 1)[-1]
    if len(hexd) < 2:
        return None
    p = os.path.join(blobs_root, hexd[:2], hexd)
    return p if os.path.exists(p) else None


def http_get(url: str, timeout: int = HTTP_TIMEOUT) -> Optional[bytes]:
    try:
        req = urllib.request.Request(url, headers={"User-Agent": "specula-trust-oracle/1"})
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.read()
    except Exception:
        return None


def http_status(url: str, timeout: int = HTTP_TIMEOUT) -> int:
    """Return the HTTP status for url; 0 on transport failure. Used to prove the
    ABSENCE of a stronger anchor (e.g. no .prov => helm cannot exceed tofu)."""
    try:
        req = urllib.request.Request(url, method="HEAD",
                                     headers={"User-Agent": "specula-trust-oracle/1"})
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code
    except Exception:
        return 0


# ───────────────────────────── apt: the gold standard ─────────────────────────
#
# Chain, per DESIGN-REVIEW §1.1 — every link checked here, none taken on trust:
#
#   out-of-band distro keyring
#     -> InRelease            clear-signature checked by gpg(1)
#     -> Packages.xz          SHA256 pinned inside the GPG-VERIFIED InRelease text
#     -> pool/*.deb           SHA256 pinned inside that Packages index
#
# The pins are parsed ONLY out of gpg-verified plaintext. That is the entire point:
# `gpg --output` writes plaintext only after the signature checks out, so a forged
# InRelease yields no pins at all rather than attacker-chosen ones. Parsing the
# signed file's text before verifying it would re-introduce exactly the circular
# verification the trust model exists to prevent.

class AptOracle:
    def __init__(self, keyring: str, blobs: str):
        self.keyring = keyring
        self.blobs = blobs
        self._inrelease_pins: dict[str, str] = {}   # sha256 -> path (from InRelease)
        self._deb_pins: dict[str, str] = {}         # pool filename -> sha256
        self._inrelease_ok = False
        self._gpg_signer = ""

    def gpg_verify_bytes(self, path: str) -> tuple[bool, str, str]:
        """gpg --decrypt the clear-signed file at `path` against the out-of-band
        keyring. Returns (ok, verified_plaintext, signer). We use --decrypt (not
        bare --verify) because we need the plaintext gpg VOUCHES FOR; --verify
        would tell us the signature is good but hand us nothing safe to parse."""
        with tempfile.TemporaryDirectory() as td:
            out = os.path.join(td, "plain")
            proc = subprocess.run(
                ["gpg", "--batch", "--no-default-keyring", "--keyring", self.keyring,
                 "--status-fd", "1", "--output", out, "--decrypt", path],
                capture_output=True, text=True,
            )
            status = proc.stdout
            good = "[GNUPG:] GOODSIG" in status and "[GNUPG:] VALIDSIG" in status
            bad = "[GNUPG:] BADSIG" in status or "[GNUPG:] ERRSIG" in status
            if proc.returncode != 0 or not good or bad:
                return False, "", ""
            signer = ""
            m = re.search(r"\[GNUPG:\] GOODSIG \w+ (.+)", status)
            if m:
                signer = m.group(1).strip()
            with open(out, "r", encoding="utf-8", errors="replace") as f:
                return True, f.read(), signer

    def load_inrelease(self, cas_file: str) -> tuple[bool, str]:
        """Verify the InRelease bytes SPECULA STORED and pin every SHA256 it commits
        to. Note we verify the CAS copy, not a fresh upstream fetch: a fresh fetch
        would grade the mirror, not Specula, and InRelease is re-signed periodically
        so a fresh copy would race anyway."""
        ok, plain, signer = self.gpg_verify_bytes(cas_file)
        if not ok:
            return False, "gpg(1) refused the InRelease signature"
        self._gpg_signer = signer
        # Parse the SHA256: section — "<hash> <size> <path>" lines.
        in_sha256 = False
        for line in plain.splitlines():
            if re.match(r"^SHA256:\s*$", line):
                in_sha256 = True
                continue
            if in_sha256:
                if line.startswith(" "):
                    parts = line.split()
                    if len(parts) >= 3:
                        self._inrelease_pins[parts[0]] = parts[2]
                    continue
                in_sha256 = False
        self._inrelease_ok = bool(self._inrelease_pins)
        return self._inrelease_ok, signer

    def load_packages(self, blobs: str) -> int:
        """Find every Packages index that the GPG-verified InRelease pins, read it
        from the CAS, and pin the SHA256 of each .deb it lists. Only indexes whose
        own bytes hash to an InRelease pin are parsed — an unpinned index is
        attacker-controlled and must contribute no pins."""
        count = 0
        for pinned_hash, rel_path in self._inrelease_pins.items():
            if "binary-" not in rel_path or "Packages" not in rel_path:
                continue
            p = cas_path(blobs, pinned_hash)
            if not p:
                continue
            if sha256_file(p) != pinned_hash:
                continue  # CAS bytes do not match the signed pin: contribute nothing.
            try:
                if rel_path.endswith(".xz"):
                    data = lzma.open(p, "rt", encoding="utf-8", errors="replace").read()
                elif rel_path.endswith(".gz"):
                    import gzip
                    data = gzip.open(p, "rt", encoding="utf-8", errors="replace").read()
                else:
                    data = open(p, "rt", encoding="utf-8", errors="replace").read()
            except Exception:
                continue
            for stanza in data.split("\n\n"):
                fn = re.search(r"^Filename:\s*(\S+)", stanza, re.M)
                sh = re.search(r"^SHA256:\s*([a-f0-9]{64})", stanza, re.M)
                if fn and sh:
                    self._deb_pins[os.path.basename(fn.group(1))] = sh.group(1)
                    count += 1
        return count

    def tier_for(self, name: str, version: str, cas_file: str) -> tuple[str, list, str]:
        """Decide the tier this apt artifact deserves. Returns (tier, evidence, method)."""
        actual = sha256_file(cas_file)

        # (1) InRelease itself: signed iff gpg(1) accepts the stored bytes.
        if version.endswith("/InRelease"):
            if self._inrelease_ok:
                return "signed", [
                    f"gpg(1) GOODSIG+VALIDSIG from {self._gpg_signer}",
                    f"keyring={self.keyring} (out-of-band, ships with the OS)",
                    f"pins {len(self._inrelease_pins)} index hashes",
                ], "gpg(1) clear-signature over the stored bytes"
            return "checksum", ["gpg(1) rejected the stored InRelease"], "gpg(1)"

        # (2) by-hash index: the hash is IN the path, so this doubles as a check that
        #     the by-hash lookup resolved to the right bytes. The literal-path-lookup
        #     bug that shipped here made apt record tofu for exactly these requests.
        m = re.search(r"/by-hash/SHA256/([a-f0-9]{64})$", version)
        if m:
            path_hash = m.group(1)
            if actual != path_hash:
                return "checksum", [
                    f"stored bytes hash {actual[:16]}… != by-hash path {path_hash[:16]}…"
                ], "sha256 of stored bytes vs by-hash path"
            if path_hash in self._inrelease_pins:
                return "signed", [
                    f"sha256={path_hash[:16]}… matches the by-hash path",
                    f"pinned by GPG-verified InRelease as {self._inrelease_pins[path_hash]}",
                ], "GPG-verified InRelease pin"
            return "tofu", [
                f"sha256={path_hash[:16]}… is NOT pinned by the GPG-verified InRelease"
            ], "GPG-verified InRelease pin (absent)"

        # (3) pool/*.deb: pinned by a Packages index that InRelease pinned.
        base = os.path.basename(version)
        if base.endswith(".deb"):
            pin = self._deb_pins.get(base)
            if pin is None:
                return "tofu", [
                    f"{base} not pinned by any GPG-verified Packages index"
                ], "GPG-verified Packages pin (absent)"
            if pin != actual:
                return "checksum", [
                    f"stored bytes {actual[:16]}… != signed pin {pin[:16]}…"
                ], "GPG-verified Packages pin (MISMATCH)"
            return "signed", [
                f"keyring -> InRelease (gpg GOODSIG) -> Packages -> {base}",
                f"signed pin {pin[:16]}… == stored bytes {actual[:16]}…",
            ], "full apt chain via gpg(1) + Packages pin"

        return "checksum", ["no apt trust anchor applies to this path"], "n/a"


# ───────────────────────────── go: sumdb via the real toolchain ────────────────
#
# The `go` command verifies a module against the checksum database itself: it
# fetches the Ed25519-signed tree head plus inclusion/consistency proofs, and
# REFUSES the download on mismatch. We drive that, in a throwaway module cache,
# straight at the upstream proxy — never through Specula — and then compare the
# bytes it accepted against the bytes Specula stored.
#
# Two properties make this a real oracle rather than theatre:
#   * go.sum starts EMPTY, so the toolchain is forced to consult the sumdb; it
#     cannot satisfy the download from a pre-seeded hash.
#   * we assert we OBSERVED sumdb traffic (`go mod download -x` logs it). Without
#     that assertion a silently-disabled sumdb (GOFLAGS=-insecure, GONOSUMDB, a
#     GONOSUMCHECK leak from the caller's shell) would make every module look
#     "verified" — which is exactly the bug that shipped: go's sumdb verifier
#     never ran in the documented CN config and nothing noticed.

class GoOracle:
    def __init__(self, proxy: str, sumdb: str):
        self.proxy = proxy
        self.sumdb = sumdb
        self._cache: dict[str, dict] = {}

    def resolve(self, module: str, version: str) -> dict:
        key = f"{module}@{version}"
        if key in self._cache:
            return self._cache[key]

        res: dict = {"ok": False, "sumdb_observed": False, "err": "", "zip": "", "mod": ""}
        td = tempfile.mkdtemp(prefix="specula-oracle-go.")
        try:
            mod_dir = os.path.join(td, "m")
            os.makedirs(mod_dir)
            with open(os.path.join(mod_dir, "go.mod"), "w") as f:
                f.write("module oracleprobe\n\ngo 1.21\n")

            env = dict(os.environ)
            env.update({
                "GOMODCACHE": os.path.join(td, "modcache"),
                "GOPATH": os.path.join(td, "gopath"),
                "GOPROXY": self.proxy,
                "GOSUMDB": self.sumdb,
                "GOFLAGS": "-mod=mod",
                # Scrub every knob that could silently disable sumdb verification.
                # If any of these leak in from the caller's shell the oracle would
                # certify unverified modules as `signed`.
                "GONOSUMDB": "", "GONOSUMCHECK": "", "GOPRIVATE": "", "GONOSUMDB_": "",
                "GONOSUMDBPATTERNS": "", "GOINSECURE": "",
            })
            proc = subprocess.run(
                ["go", "mod", "download", "-x", "-json", f"{module}@{version}"],
                cwd=mod_dir, env=env, capture_output=True, text=True, timeout=300,
            )
            # `-x` logs every fetch to stderr; sumdb tile/lookup traffic proves the
            # checksum database was actually consulted for this module.
            sumdb_host = self.sumdb.split("/")[0]
            res["sumdb_observed"] = bool(
                re.search(rf"https://{re.escape(sumdb_host)}/(lookup|tile)/", proc.stderr)
            )
            if proc.returncode != 0:
                res["err"] = (proc.stderr or proc.stdout)[-400:]
                self._cache[key] = res
                return res
            try:
                info = json.loads(proc.stdout)
            except Exception:
                # -x can interleave; take the last JSON object.
                objs = re.findall(r"\{.*?\n\}", proc.stdout, re.S)
                info = json.loads(objs[-1]) if objs else {}
            zip_path, mod_path = info.get("Zip", ""), info.get("GoMod", "")
            if not zip_path or not os.path.exists(zip_path):
                res["err"] = "go mod download returned no zip"
                self._cache[key] = res
                return res
            # Copy out before the temp cache dies.
            keep = tempfile.mkdtemp(prefix="specula-oracle-gokeep.")
            res["zip"] = shutil.copy(zip_path, os.path.join(keep, "m.zip"))
            if mod_path and os.path.exists(mod_path):
                res["mod"] = shutil.copy(mod_path, os.path.join(keep, "m.mod"))
            res["sum"], res["gomodsum"] = info.get("Sum", ""), info.get("GoModSum", "")
            res["ok"] = True
        except Exception as e:
            res["err"] = str(e)
        finally:
            shutil.rmtree(td, ignore_errors=True)
        self._cache[key] = res
        return res

    def tier_for(self, module: str, version_file: str, cas_file: str) -> tuple[str, list, str]:
        # v0.9.1.zip / v0.9.1.mod / v0.9.1.info
        m = re.match(r"^(v.+?)\.(zip|mod|info)$", version_file)
        if not m:
            return "checksum", ["unrecognised go artifact suffix"], "n/a"
        ver, kind = m.group(1), m.group(2)

        # .info is a metadata blob. The sumdb commits to exactly two things per
        # module version — the zip dirhash and the go.mod hash — and NOTHING that
        # covers .info. So .info cannot exceed tofu, by construction. Asserting this
        # is the UNDER-claim guard: it catches Specula over-claiming `signed` on a
        # file the checksum database provably says nothing about.
        if kind == "info":
            return "tofu", [
                "the sumdb commits to <mod>@<ver> (zip) and <mod>@<ver>/go.mod only",
                ".info has no sumdb entry => no cryptographic anchor exists for it",
            ], "sumdb coverage (structural: no entry can exist)"

        r = self.resolve(module, ver)
        if not r["ok"]:
            return "checksum", [f"go mod download failed: {r['err'][:160]}"], "go toolchain (failed)"
        if not r["sumdb_observed"]:
            # Refuse to certify `signed` when we cannot prove the check ran. "It
            # didn't fail" is not "it ran" — that conflation is precisely the
            # skip-as-pass bug this oracle exists to catch.
            return "checksum", [
                f"NO sumdb traffic to {self.sumdb} observed — cannot certify signed"
            ], "go toolchain (sumdb not consulted)"

        ref = r["zip"] if kind == "zip" else r["mod"]
        if not ref or not os.path.exists(ref):
            return "checksum", [f"go toolchain produced no {kind}"], "go toolchain"
        ref_hash, actual = sha256_file(ref), sha256_file(cas_file)
        if ref_hash != actual:
            return "checksum", [
                f"stored bytes {actual[:16]}… != sumdb-verified upstream {ref_hash[:16]}…"
            ], "go toolchain sumdb (MISMATCH)"
        sumval = r.get("sum") if kind == "zip" else r.get("gomodsum")
        return "signed", [
            f"go toolchain verified {module}@{ver} against {self.sumdb}",
            f"sumdb traffic observed (Ed25519 tree head + inclusion proof)",
            f"go.sum {sumval}",
            f"sumdb-verified bytes == stored bytes ({actual[:16]}…)",
        ], "go toolchain sumdb verification"


# ───────────────────────────── pypi: cross-source consensus ────────────────────
#
# PyPI has no usable signature anchor in CN: PEP 740 attestations need Rekor
# (blocked) and cover ~5% of artifacts anyway (DESIGN-REVIEW §1.1). So the
# strongest honest claim is consensus — and consensus is only real if the mirrors
# are INDEPENDENTLY re-queried. We fetch each PEP 503 index ourselves, parse the
# `#sha256=` fragment, and require >= quorum mirrors to agree AND the agreed hash
# to equal the bytes Specula stored.
#
# This is what makes "quorum was met" falsifiable: Specula claiming consensus
# after talking to one mirror looks identical, from the inside, to real quorum.

class PyPIOracle:
    def __init__(self, mirrors: list[tuple[str, str]], quorum: int):
        self.mirrors = mirrors  # [(name, simple_base_url)]
        self.quorum = quorum
        self._idx: dict[str, dict[str, str]] = {}

    def hashes_for(self, project: str, filename: str) -> dict[str, str]:
        key = f"{project}/{filename}"
        if key in self._idx:
            return self._idx[key]
        found: dict[str, str] = {}
        for name, base in self.mirrors:
            body = http_get(f"{base.rstrip('/')}/simple/{project}/")
            if not body:
                continue
            text = body.decode("utf-8", errors="replace")
            m = re.search(re.escape(filename) + r"#sha256=([a-f0-9]{64})", text)
            if m:
                found[name] = m.group(1)
        self._idx[key] = found
        return found

    def tier_for(self, project: str, filename: str, cas_file: str) -> tuple[str, list, str]:
        actual = sha256_file(cas_file)
        found = self.hashes_for(project, filename)
        if not found:
            return "checksum", [
                f"no mirror served a #sha256 for {filename} (index unreachable?)"
            ], "PEP 503 re-fetch (no data)"

        agreeing = [n for n, h in found.items() if h == actual]
        ev = [f"{n}: {h[:16]}…{' MATCH' if h == actual else ' DIFFERS'}"
              for n, h in sorted(found.items())]
        ev.append(f"stored bytes: {actual[:16]}…")
        ev.append(f"{len(agreeing)}/{len(found)} independently re-fetched mirrors agree "
                  f"(quorum={self.quorum})")
        if len(agreeing) >= self.quorum:
            return "consensus", ev, f"independent PEP 503 re-fetch from {len(found)} mirrors"
        if len(agreeing) >= 1:
            return "tofu", ev, "independent PEP 503 re-fetch (quorum NOT met)"
        return "checksum", ev, "independent PEP 503 re-fetch (no mirror matches stored bytes)"


# ───────────────────────────── helm: absence of an anchor ─────────────────────
#
# Helm reaches `signed` only via a .prov GPG signature + keyring. If the upstream
# publishes no .prov, `tofu` is the honest CEILING and claiming more would be a
# lie. Proving the ABSENCE of the stronger anchor is what makes this an
# under-claim check too: it pins down what the correct answer IS, so we would
# notice Specula recording either more OR less than it earned.

class HelmOracle:
    """Grades a helm chart's deserved tier.

    Two modes, chosen by whether the operator gave us a keyring:

      * NO keyring (the CN-mirror reality, and the original behaviour): probe the
        upstream for a .prov. If none exists, `tofu` is the honest CEILING and
        claiming more is a lie. This proves the ABSENCE of a stronger anchor and
        is what the standing trust-oracle gate exercises (mirror.azure.cn serves
        no .prov at all).

      * WITH a keyring AND a .prov (the lab where we generated a real
        helm-signed chart): INDEPENDENTLY verify the .prov with gpg(1) against the
        out-of-band keyring — never our openpgp code — and confirm the sha256 the
        signed .prov commits to equals the sha256 of the chart bytes SPECULA
        STORED. Only then is `signed` deserved. This grades the bytes on disk, not
        a fresh copy, and the trust anchor (the keyring) is out-of-band.
    """

    def __init__(self, base: str, keyring: str = "", prov_dir: str = ""):
        self.base = base.rstrip("/")
        self.keyring = keyring
        # prov_dir: a local directory that may hold <chart>.tgz.prov (the lab's
        # upstream). When set we read the .prov from there instead of the network,
        # so the oracle works fully offline against a local registry/repo.
        self.prov_dir = prov_dir

    def _prov_bytes(self, chart_file: str) -> tuple[bytes | None, str]:
        if self.prov_dir:
            p = os.path.join(self.prov_dir, chart_file + ".prov")
            if os.path.exists(p):
                return open(p, "rb").read(), p
            return None, p
        url = f"{self.base}/{chart_file}.prov"
        return http_get(url), url

    def _gpg_verify_prov(self, prov_bytes: bytes) -> tuple[bool, str, str]:
        """gpg --decrypt the clear-signed .prov against the out-of-band keyring.
        Returns (ok, verified_plaintext, signer). Uses --decrypt so we only ever
        parse plaintext gpg VOUCHES FOR (same discipline as AptOracle)."""
        with tempfile.TemporaryDirectory() as td:
            src = os.path.join(td, "chart.prov")
            out = os.path.join(td, "plain")
            open(src, "wb").write(prov_bytes)
            proc = subprocess.run(
                ["gpg", "--batch", "--no-default-keyring", "--keyring", self.keyring,
                 "--status-fd", "1", "--output", out, "--decrypt", src],
                capture_output=True, text=True,
            )
            status = proc.stdout
            good = "[GNUPG:] GOODSIG" in status and "[GNUPG:] VALIDSIG" in status
            bad = "[GNUPG:] BADSIG" in status or "[GNUPG:] ERRSIG" in status
            if proc.returncode != 0 or not good or bad:
                return False, "", ""
            signer = ""
            m = re.search(r"\[GNUPG:\] GOODSIG \w+ (.+)", status)
            if m:
                signer = m.group(1).strip()
            with open(out, "r", encoding="utf-8", errors="replace") as f:
                return True, f.read(), signer

    @staticmethod
    def _digest_from_prov(plaintext: str, chart_file: str) -> Optional[str]:
        """Extract the sha256 the .prov `files:` block commits to for chart_file.
        Parsed ONLY from gpg-verified plaintext (never the raw signed file)."""
        in_files = False
        for raw in plaintext.splitlines():
            line = raw.rstrip("\r")
            if line.strip() == "files:":
                in_files = True
                continue
            if not in_files:
                continue
            if line and not line[0].isspace():
                break
            entry = line.strip()
            if ": " not in entry:
                continue
            name, digest = entry.split(": ", 1)
            if name.strip() == chart_file and digest.strip().startswith("sha256:"):
                return digest.strip().split(":", 1)[1]
        return None

    def tier_for(self, chart_file: str, cas_file: str) -> tuple[str, list, str]:
        # Absence probe path (no keyring configured): tofu is the ceiling.
        if not self.keyring:
            prov_url = f"{self.base}/{chart_file}.prov"
            code = http_status(prov_url)
            if code == 200:
                return "unknown", [
                    f"{prov_url} EXISTS (HTTP 200) — a .prov anchor is available",
                    "oracle cannot grade it: no helm keyring configured for this repo",
                ], "helm .prov probe (UNGRADED — see report)"
            return "tofu", [
                f"{prov_url} -> HTTP {code}: upstream publishes NO .prov signature",
                "no keyring anchor can exist for this chart => tofu is the honest ceiling",
                f"stored bytes: {sha256_file(cas_file)[:16]}…",
            ], "helm .prov probe (absent => tofu is the ceiling)"

        # Keyring configured: independently grade the .prov signature.
        prov_bytes, prov_src = self._prov_bytes(chart_file)
        if not prov_bytes:
            return "tofu", [
                f"no .prov available at {prov_src} — nothing to verify",
                "absent .prov => tofu is the honest ceiling",
            ], "helm .prov gpg(1) (absent => tofu)"
        ok, plain, signer = self._gpg_verify_prov(prov_bytes)
        if not ok:
            return "tofu", [
                f".prov at {prov_src} did NOT verify under {self.keyring}",
                "gpg(1) rejected the signature => not signed; tofu is the ceiling",
            ], "helm .prov gpg(1) (signature REJECTED)"
        prov_digest = self._digest_from_prov(plain, chart_file)
        actual = sha256_file(cas_file)
        if prov_digest is None:
            return "tofu", [
                "gpg(1) accepted the .prov but it commits to no sha256 for this chart",
            ], "helm .prov gpg(1) (no digest binding)"
        if prov_digest != actual:
            return "checksum", [
                f"gpg-verified .prov commits to sha256 {prov_digest[:16]}…",
                f"but the chart SPECULA STORED hashes to {actual[:16]}… — BINDING BROKEN",
            ], "helm .prov gpg(1) (digest mismatch)"
        return "signed", [
            f"gpg(1) GOODSIG+VALIDSIG from {signer}",
            f"keyring={self.keyring} (out-of-band)",
            f"signed .prov digest {prov_digest[:16]}… == stored chart {actual[:16]}…",
        ], "helm .prov gpg(1) clear-signature + digest binding"


class CosignOracle:
    """Independently grades an OCI artifact's deserved tier using the REAL cosign
    CLI — never Specula's verify code.

    For each oci row Specula recorded, we run `cosign verify --key <pub>
    --insecure-ignore-tlog` against the UPSTREAM registry at the digest Specula
    stored. cosign fetches the sha256-<hex>.sig companion tag and checks the
    signature against the out-of-band public key:

      * verifies      => the artifact really is signed => deserved tier `signed`.
      * does not      => it is NOT cosign-signed (a config/layer blob, or an
                         unsigned manifest) => the honest ceiling under an
                         [signed, tofu] policy is `tofu` (first-fetch pin).

    This is a TWO-SIDED check: it catches Specula recording `signed` for something
    cosign cannot verify (over-claim) AND recording less than `signed` for the
    manifest cosign CAN verify (under-claim). The public key is the out-of-band
    anchor a lying mirror or a lying Specula cannot forge.
    """

    def __init__(self, cosign_bin: str, registry: str, pubkey: str,
                 unsigned_ceiling: str = "tofu"):
        self.cosign = cosign_bin
        self.registry = registry.rstrip("/")
        self.pubkey = pubkey
        self.unsigned_ceiling = unsigned_ceiling

    def tier_for(self, name: str, digest: str) -> tuple[str, list, str]:
        ref = f"{self.registry}/{name}@{digest}"
        proc = subprocess.run(
            [self.cosign, "verify", "--key", self.pubkey,
             "--insecure-ignore-tlog=true", "--allow-http-registry=true",
             "--allow-insecure-registry=true", ref],
            capture_output=True, text=True,
        )
        if proc.returncode == 0:
            return "signed", [
                f"cosign verify --key {os.path.basename(self.pubkey)} ACCEPTED {ref}",
                "keyed verification, transparency log ignored (CN-offline mode)",
                "public key is the out-of-band anchor a mirror cannot forge",
            ], "cosign verify --key (real cosign CLI)"
        tail = (proc.stderr or proc.stdout).strip().splitlines()
        last = tail[-1] if tail else "no output"
        return self.unsigned_ceiling, [
            f"cosign verify --key REJECTED {ref} (exit {proc.returncode})",
            f"  cosign: {last[:100]}",
            f"not cosign-signed => {self.unsigned_ceiling} is the honest ceiling",
        ], "cosign verify --key (real cosign CLI; not signed)"


# ───────────────────────────────── driver ─────────────────────────────────────

def main() -> int:
    ap = argparse.ArgumentParser(description="Independent oracle for Specula tier claims")
    ap.add_argument("--db", required=True, help="path to Specula's sqlite meta.db")
    ap.add_argument("--blobs", required=True, help="path to the CAS blob root")
    ap.add_argument("--keyring", default="/usr/share/keyrings/ubuntu-archive-keyring.gpg")
    ap.add_argument("--apt-base", default="https://mirrors.aliyun.com/ubuntu")
    ap.add_argument("--go-proxy", default="https://goproxy.cn,direct")
    ap.add_argument("--go-sumdb", default="sum.golang.google.cn")
    ap.add_argument("--pypi-mirror", action="append", default=[],
                    help="name=base_url (repeatable)")
    ap.add_argument("--pypi-quorum", type=int, default=2)
    ap.add_argument("--helm-base", default="https://mirror.azure.cn/kubernetes/charts")
    # Optional signed-tier grading (lab harness). When unset the oracle behaves
    # exactly as before: oci rows are UNGRADED and helm uses the .prov absence
    # probe. This keeps the standing CN-mirror gate backward compatible.
    ap.add_argument("--helm-keyring", default="",
                    help="out-of-band GPG keyring to grade helm .prov signatures")
    ap.add_argument("--helm-prov-dir", default="",
                    help="local dir holding <chart>.tgz.prov (lab upstream)")
    ap.add_argument("--cosign-bin", default="",
                    help="path to the real cosign CLI for grading oci `signed`")
    ap.add_argument("--oci-registry", default="",
                    help="upstream OCI registry host that holds the signed images")
    ap.add_argument("--cosign-key", default="",
                    help="out-of-band cosign PUBLIC key (PEM) to grade oci `signed`")
    ap.add_argument("--oci-unsigned-ceiling", default="tofu",
                    help="deserved tier for a non-cosign-signed oci artifact (matches config)")
    ap.add_argument("--out", required=True, help="where to write the JSON verdict table")
    args = ap.parse_args()

    for tool in ("gpg", "go"):
        if not shutil.which(tool):
            print(f"FATAL: {tool} not found — the oracle cannot run without it", file=sys.stderr)
            return 2

    con = sqlite3.connect(args.db)
    rows = con.execute(
        "SELECT protocol, name, version, digest, tier FROM cache_entries "
        "ORDER BY protocol, name, version"
    ).fetchall()
    con.close()
    if not rows:
        print("FATAL: cache_entries is empty — nothing to grade", file=sys.stderr)
        return 2

    apt = AptOracle(args.keyring, args.blobs)
    go = GoOracle(args.go_proxy, args.go_sumdb)
    mirrors = []
    for spec in args.pypi_mirror:
        if "=" in spec:
            n, u = spec.split("=", 1)
            mirrors.append((n, u))
    pypi = PyPIOracle(mirrors, args.pypi_quorum)
    helm = HelmOracle(args.helm_base, keyring=args.helm_keyring, prov_dir=args.helm_prov_dir)
    cosign = None
    if args.cosign_bin and args.oci_registry and args.cosign_key:
        cosign = CosignOracle(args.cosign_bin, args.oci_registry, args.cosign_key,
                              unsigned_ceiling=args.oci_unsigned_ceiling)

    # Bootstrap the apt chain: verify InRelease first so its pins are available to
    # every other apt row. Order matters — the chain is a chain.
    for protocol, name, version, digest, tier in rows:
        if protocol == "apt" and version.endswith("/InRelease"):
            p = cas_path(args.blobs, digest)
            if p:
                ok, signer = apt.load_inrelease(p)
                print(f"==> apt InRelease: gpg(1) {'ACCEPTED' if ok else 'REJECTED'}"
                      f"{' — ' + signer if ok else ''}")
    n_pins = apt.load_packages(args.blobs)
    if n_pins:
        print(f"==> apt: {n_pins} .deb hashes pinned via GPG-verified Packages indexes")

    verdicts: list[Verdict] = []
    ungraded: list[Verdict] = []

    for protocol, name, version, digest, tier in rows:
        claimed = TIER_NAMES.get(tier, f"unknown({tier})")
        artifact_id = f"{name}/{version}" if name else version
        p = cas_path(args.blobs, digest)
        if not p:
            ungraded.append(Verdict(protocol, artifact_id, claimed, "n/a", True,
                                    "CAS blob missing", note="blob absent; not graded"))
            continue

        if protocol == "apt":
            t, ev, method = apt.tier_for(name, version, p)
        elif protocol == "gomod":
            t, ev, method = go.tier_for(name, version, p)
        elif protocol == "pypi":
            if version == "simple" or version.endswith("/simple"):
                # A mutable PEP 503 index. No signature exists, and it is not a
                # content-addressed artifact, so `checksum` is the honest floor.
                t, ev, method = "checksum", [
                    "mutable PEP 503 index: no signature anchor exists in this ecosystem",
                ], "structural (mutable metadata, no anchor)"
            else:
                project = name.split("/")[-1]
                t, ev, method = pypi.tier_for(project, os.path.basename(version), p)
        elif protocol == "helm":
            t, ev, method = helm.tier_for(os.path.basename(version), p)
        elif protocol == "oci":
            if cosign is None:
                ungraded.append(Verdict(protocol, artifact_id, claimed, "n/a", True,
                                        "no cosign key/registry configured",
                                        note="oci NOT graded (no --cosign-key/--oci-registry)"))
                continue
            # The oci digest is the CAS key; name is the repository. cosign checks
            # the signature attached to that digest on the upstream registry.
            t, ev, method = cosign.tier_for(name, digest)
        else:
            ungraded.append(Verdict(protocol, artifact_id, claimed, "n/a", True,
                                    "no oracle implemented",
                                    note=f"protocol {protocol} NOT independently oracled"))
            continue

        if t == "unknown":
            ungraded.append(Verdict(protocol, artifact_id, claimed, "unknown", True,
                                    method, ev, note="oracle could not grade"))
            continue
        verdicts.append(Verdict(protocol, artifact_id, claimed, t, claimed == t, method, ev))

    disagreements = [v for v in verdicts if not v.agree]

    out = {
        "schema": "specula.trust-oracle/v1",
        "independent": True,
        "oracle_imports_specula_verify_code": False,
        "reference_tools": {
            "apt": "gpg(1) + /usr/share/keyrings/ubuntu-archive-keyring.gpg",
            "gomod": f"go toolchain sumdb verification vs {args.go_sumdb}",
            "pypi": f"independent PEP 503 re-fetch, quorum={args.pypi_quorum}",
            "helm": ("gpg(1) .prov clear-signature + digest binding"
                     if args.helm_keyring else "upstream .prov probe (absence of anchor)"),
            "oci": ("cosign verify --key (real cosign CLI)"
                    if cosign is not None else "UNGRADED (no cosign key/registry)"),
        },
        "graded": len(verdicts),
        "agree": len(verdicts) - len(disagreements),
        "disagree": len(disagreements),
        "verdicts": [asdict(v) for v in verdicts],
        "ungraded": [asdict(v) for v in ungraded],
    }
    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
    with open(args.out, "w") as f:
        json.dump(out, f, indent=2)

    # ── the agreement table ──
    print()
    print(f"{'PROTOCOL':<9} {'ARTIFACT':<52} {'SPECULA':<10} {'ORACLE':<10} {'AGREE':<6}")
    print("-" * 92)
    for v in verdicts:
        a = v.artifact if len(v.artifact) <= 52 else "…" + v.artifact[-51:]
        print(f"{v.protocol:<9} {a:<52} {v.specula_tier:<10} {v.oracle_tier:<10} "
              f"{'YES' if v.agree else 'NO  <<<':<6}")
    for v in ungraded:
        a = v.artifact if len(v.artifact) <= 52 else "…" + v.artifact[-51:]
        print(f"{v.protocol:<9} {a:<52} {v.specula_tier:<10} {'UNGRADED':<10} {'-':<6}")
    print("-" * 92)
    print(f"graded={len(verdicts)} agree={len(verdicts)-len(disagreements)} "
          f"disagree={len(disagreements)} ungraded={len(ungraded)}")
    print(f"verdict table written to {args.out}")

    if disagreements:
        print()
        print("!!! TIER DISAGREEMENT — Specula's claim is not what the artifact deserves.")
        print("!!! Either Specula is lying, or the oracle is wrong. Both are findings.")
        for v in disagreements:
            print(f"\n  {v.protocol} {v.artifact}")
            print(f"    specula claims : {v.specula_tier}")
            print(f"    oracle derives : {v.oracle_tier}   (via {v.method})")
            for e in v.evidence:
                print(f"      - {e}")
        return 1

    print("\nPASS: every graded artifact's recorded tier is the tier it deserves.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
