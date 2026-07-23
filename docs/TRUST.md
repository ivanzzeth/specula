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

---

## 5. What “PASS” does and does not mean

- `test-trust-oracle` PASS ≠ cosign or helm `.prov` on public CN mirrors.
- `test-trust-oracle-signed` PASS ≠ any public mirror serves signatures; it proves
  Specula’s verifier + pull-through reach `signed` against real cosign/helm output.
- WebUI / Prometheus tiers are **self-reported**; the oracles are the external check.

Authoritative schema: [`specula.example.yaml`](../specula.example.yaml) and
[`internal/config/config.go`](../internal/config/config.go). Product claims:
[`docs/PRD.md`](PRD.md) §G2 / §6.
