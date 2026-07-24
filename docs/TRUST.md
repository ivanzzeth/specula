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

## 1. OCI — cosign keyed (CN-safe)

Keyless/Fulcio/Rekor are **unsupported** (CN-blocked). Use long-lived keys,
`tlog: false`.

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

Verify: `make test-trust-oracle-signed` (builds a real signed image locally).

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
    verification:
      dependency_confusion:
        private_scopes: ["@myorg"]
        private_unscoped: ["internal-svc"]
        private_upstream: https://npm.internal.example.com
        on_private_down: fail_closed
```

When the private upstream is down: **5xx / refuse**, never a public hit for a
private name. That is the whole point.

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

```yaml
protocols:
  npm:   # same shape for pypi / cargo
    verification:
      tiers: [consensus, tofu, checksum]
      maturity:
        min_age: 72h          # Go duration (e.g. 24h, 168h)
        policy: warn          # warn | enforce (enforce → verify FAIL → not cached)
```

- Uses registry-advertised publish/upload time when known; else `Last-Modified`.
- If neither is available → **skip** (honest: do not invent an age).
- Does **not** raise the trust tier; Events/UI show it as a policy outcome.
- Does **not** claim to stop XZ-style long-horizon signed poisoning.

---

## 6. What “PASS” does and does not mean

- `test-trust-oracle` PASS ≠ cosign or helm `.prov` on public CN mirrors.
- `test-trust-oracle-signed` PASS ≠ any public mirror serves signatures; it proves
  Specula’s verifier + pull-through reach `signed` against real cosign/helm output.
- WebUI / Prometheus tiers are **self-reported**; the oracles are the external check.
- Maturity `PASS` ≠ malware-free — only “older than min_age”.

Authoritative schema: [`specula.example.yaml`](../specula.example.yaml) and
[`internal/config/config.go`](../internal/config/config.go). Product claims:
[`docs/PRD.md`](PRD.md) §G2 / §6 / milestone v0.10.
