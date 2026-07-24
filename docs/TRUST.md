# Trust & dep-confusion — operator cookbook

Specula’s value is **honest tiers** (`signed` > `consensus` > `tofu` > `checksum`).
This page is the one-pager to turn engines that already exist into config you can
copy, gates you can run, and fail-closed private namespaces.

Independent gates (do not trust Specula’s own counters alone):

| Gate | Command | Covers |
|------|---------|--------|
| CN-mirror oracle | `make test-trust-oracle` | apt GPG, Go sumdb, PyPI consensus, helm tofu (no `.prov` on CN mirrors) |
| Signed lab oracle | `make test-trust-oracle-signed` | OCI cosign keyed + Helm `.prov` (hermetic; needs docker/cosign/helm/gpg) |
| Meta-gate | `make test-trust-oracle-mutations` | Proves the oracle catches planted lies |

CI runs the first two on `main` / PRs (see `.github/workflows/ci.yml`).

---

## 1. OCI — cosign signed (CN-safe / air-gap)

Online Rekor (`tlog: true`) is **unsupported**. Two authenticity anchors
(either or both):

1. **Keys** — long-lived publisher public keys (classic keyed cosign).
2. **trusted_root** — Sigstore `trusted_root.json` Fulcio CAs; signatures that
   carry a leaf certificate (`dev.sigstore.cosign/certificate`) are verified
   offline against those CAs. This is the self-hosted / air-gap “keyless-style”
   path: you trust whoever your Fulcio issues to. Specula does **not** consult
   Rekor/CT or enforce OIDC identity policies while `tlog: false`.

### Keyed (recommended for CN mirrors)

```yaml
protocols:
  oci:
    verification:
      tiers: [signed, tofu, checksum]
      quorum: 1
      tofu: enforce
      cosign:
        keys: [/etc/specula/keys/cosign.pub]
        tlog: false
```

Generate / distribute the **public** key out-of-band (never via the mirror).
Unsigned images then fail closed under `signed` in the tier list when a key is
configured. Layer blobs are not cosign-gated (only manifests/indexes).

### Air-gap Fulcio (trusted_root only or with keys)

```yaml
      cosign:
        # keys: [/etc/specula/keys/cosign.pub]   # optional alongside trusted_root
        tlog: false
        trusted_root: /etc/specula/sigstore/trusted_root.json
```

Point `trusted_root` at your **self-hosted** Fulcio CA material (or a pinned
export). Public Sigstore production roots alone do not help if Rekor/Fulcio are
unreachable for signing; this path verifies the certificate chain + payload
signature already attached to the image.

Honesty: without Rekor and without identity subject/issuer matching, any leaf
chaining to a trusted Fulcio CA passes. Prefer keyed cosign when you can
distribute publisher keys; use trusted_root when you operate (and trust) Fulcio.

Verify keyed path: `make test-trust-oracle-signed` (builds a real signed image locally).

---

## 1b. Cache inventory SBOM (CycloneDX)

Admin API (auth required):

```http
GET /api/v1/admin/sbom
GET /api/v1/admin/sbom?protocol=npm
GET /api/v1/admin/sbom?limit=500
```

Returns CycloneDX 1.5 JSON of **immutable CAS entries** Specula has verified and
cached (name, version, purl, sha256, trust tier). This is an audit export of the
proxy inventory — **not** a recursive dependency graph of package contents.
Mutable rows (packuments, indexes) are omitted. `x-specula-truncated: true` when
the export hit the component ceiling.

---

## 2. apt — distro keyring (gold standard)

```yaml
protocols:
  apt:
    verification:
      tiers: [signed, tofu, checksum]
      gpg:
        policy: enforce
        # Ubuntu/Debian host — ship the same file into the Specula container/VM:
        keyring: /usr/share/keyrings/ubuntu-archive-keyring.gpg
```

Copy the keyring into the Specula host/image if the daemon does not share the
client’s `/usr/share/keyrings`. A mirror cannot forge this file.

Verify: `make test-trust-oracle` (gpg(1) + real Ubuntu keyring).

---

## 3. Helm — `.prov` GPG

Public CN chart mirrors usually **publish no `.prov`**. Keep `policy: warn` so
unsigned charts degrade to tofu; point `keyring` at your publishers’ keys when
you control the repo.

```yaml
protocols:
  helm:
    verification:
      tiers: [signed, tofu, checksum]
      provenance:
        policy: warn          # enforce only if every chart has .prov
        keyring: /etc/specula/keyrings/helm-signing.gpg
```

Verify signed path: `make test-trust-oracle-signed` (`helm package --sign`).

---

## 4. Dep-confusion — fail-closed private names

**Clients must use Specula as the sole index** (`--index-url` only; never
`--extra-index-url` / dual registry). Prefix “conventions” are theatre on PyPI’s
flat namespace — list exact private names.

### PyPI

```yaml
protocols:
  pypi:
    verification:
      dependency_confusion:
        private_names: ["corp-internal", "corp-lib"]
        private_upstream: https://pypi.internal.example.com/simple
        on_private_down: fail_closed   # never fall back to tuna/pypi.org
```

### npm

```yaml
protocols:
  npm:
    upstreams:
      - name: npmmirror
        base_url: https://registry.npmmirror.com
        official: false
      - name: huawei-npm
        base_url: https://repo.huaweicloud.com/repository/npm
        official: false
      - name: npm-registry
        base_url: https://registry.npmjs.org
        official: true          # origin witness (not a quorum voter)
    verification:
      # Content-ID consensus: packument dist.integrity (sha512 SSRI) quorum +
      # body SSRI bind. Never equates integrity with CAS sha256.
      tiers: [consensus, tofu, checksum]
      quorum: 2
      tofu: enforce
      maturity:
        min_age: 72h
        policy: enforce         # without this, hijack publish is a known window
      dependency_confusion:
        private_scopes: ["@myorg"]
        private_unscoped: ["internal-svc"]
        private_upstream: https://npm.internal.example.com
        on_private_down: fail_closed
```

Hermetic integrity quorum: Go tests in `internal/verify` (`TestConsensusVerifier_NPMContentID_*`).
Maturity gate: `bash scripts/realclient-maturity.sh`.

When the private upstream is down: **5xx / refuse**, never a public hit for a
private name. That is the whole point.

### cargo

```yaml
protocols:
  cargo:
    verification:
      tiers: [consensus, tofu, checksum]
      quorum: 1                 # raise when ≥2 independent sparse indexes vote
      tofu: enforce
      maturity:
        min_age: 72h
        policy: enforce
```

Consensus polls sparse-index `cksum` (indexes without a comparable checksum abstain).
`.crate` body is bound to the winning cksum before CAS sha256 promotion.

### tarball

No metadata digest → **do not** enable `consensus` (would be a lie or a fail-closed forever). Floor:

```yaml
protocols:
  tarball:
    verification:
      tiers: [tofu, checksum]
      tofu: enforce
```

Callers that need content identity pin `?digest=sha256:…` (mismatch → reject).
Do **not** default to multi-source full-blob downloads to invent a consensus tier.


### Client anti-patterns (must fix)

| Bad | Why | Specula help |
|-----|-----|--------------|
| `pip` `--extra-index-url` + public PyPI | Highest version wins across indexes → classic confusion | `integrate` sets **sole** `index-url`; strips public extras; `integrate status` audits leftovers |
| Dual npm registries without scope mapping | Unscoped private names resolve publicly | `integrate` sets sole `registry=`; status warns on leftover dual config |
| Artifactory/Nexus “virtual” merge of private+public | Same highest-version algorithm | Keep Specula as the only client-facing index; put private upstream behind Specula’s `dependency_confusion` |

```bash
specula integrate --protocols pypi,npm   # sole-index wiring
specula integrate status                 # includes risk audit rows
```

---

## 5. Maturity / cool-down (policy gate, not a trust tier)

Checksum and TOFU do **not** stop a compromised maintainer who publishes a new
malicious version of a real package (npm worm / hijack window). Industry answer
(JFrog Curation, Socket cool-down): hold versions younger than N until the
community has had time to yank them.

The shipped example enables maturity for **pypi / npm / cargo** (`min_age: 72h`,
`policy: enforce`). Turn it off only if you knowingly accept that window.

```yaml
protocols:
  npm:   # same shape for pypi / cargo
    verification:
      tiers: [consensus, tofu, checksum]
      maturity:
        min_age: 72h          # Go duration (e.g. 24h, 168h)
        policy: enforce       # warn | enforce — prefer enforce for harden
```

**npm without maturity = known hijack window** even with integrity consensus (consensus
only stops mirrors disagreeing; it does not wait out a malicious new version).

- Uses registry-advertised publish/upload time when known
  (npm `packument.time[version]`, PyPI PEP 691 `upload-time` / Warehouse
  `upload_time_iso_8601`, crates.io API `created_at`); else `Last-Modified`.
- If neither is available → **skip** (honest: do not invent an age).
- Does **not** raise the trust tier; Events/UI show it as a policy outcome
  (`kind=maturity`, distinct from `kind=tofu` digest drift).
- Does **not** claim to stop XZ-style long-horizon signed poisoning.
- Hermetic gate: `bash scripts/realclient-maturity.sh` (young reject / old
  allow + Events `kind=maturity`).

---

## 6. Anti-rollback (signed index high-water)

Distinct from maturity: this rejects an **older signed index**, not a young package.

| Ecosystem | Monotonic identity | Status |
|-----------|-------------------|--------|
| Go sumdb | signed tree size | ✅ (with CDN lag tolerance) |
| apt | InRelease `Date` | ✅ (`apt_index_highwater` + GPG verifier) |
| helm / OCI | — | not yet (charts/tags use tofu / cosign) |

Hermetic apt gate: `TestGPGVerifier_AntiRollback_*` / `TestReplaceIndexPins_AntiRollback`.

---

## 7. What “PASS” does and does not mean

- `test-trust-oracle` PASS ≠ cosign or helm `.prov` on public CN mirrors.
- `test-trust-oracle-signed` PASS ≠ any public mirror serves signatures; it proves
  Specula’s verifier + pull-through reach `signed` against real cosign/helm output.
- WebUI / Prometheus tiers are **self-reported**; the oracles are the external check.
- Maturity `PASS` ≠ malware-free — only “older than min_age”.
- Anti-rollback `PASS` ≠ content is safe — only “this signed index is not older than one we already accepted”.

Authoritative schema: [`specula.example.yaml`](../specula.example.yaml) and
[`internal/config/config.go`](../internal/config/config.go). Product claims:
[`docs/PRD.md`](PRD.md) §G2 / §6 / milestone v0.12.
