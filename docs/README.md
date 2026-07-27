# Specula documentation

Start with the [project README](../README.md) for what Specula is and a local
quick start. This page is the map.

## Deploy — pick your shape first

The choice drives everything else (TLS, disk, failure domain), so make it before
reading further.

| Shape | Doc | When |
|-------|-----|------|
| **Hosted, one for many** | [deploy/HOSTED.md](deploy/HOSTED.md) | ONE stateless instance (Postgres + object storage, HPA) serving every other cluster. One profile file, one command. Includes the ten pitfalls hit deploying it for real. |
| **One VM, systemd** | [deploy/SINGLE-HOST.md](deploy/SINGLE-HOST.md) | One intranet box serves the whole VPC. Single static binary, `scp` + `specula upgrade` to update. No Kubernetes. |
| **Into a cluster** | [deploy/CLUSTER.md](deploy/CLUSTER.md) | `specula cluster install --cn` — one command with a kubeconfig. Node `hosts.toml` + CRI wiring handled by a DaemonSet. |
| **Air-gapped / offline** | [deploy/OFFLINE.md](deploy/OFFLINE.md) | Warm the cache — or seed it with `cache import` from a machine that can reach the registry — then serve with no upstream at all. |

Charts, if you drive Helm yourself rather than the CLI:

- [`deploy/helm/specula-bootstrap`](../deploy/helm/specula-bootstrap/README.md) —
  zero-dependency single replica (SQLite + local blobs). What `cluster install`
  uses; the thing that breaks the chicken-and-egg in a censored network.
- [`deploy/helm/specula`](../deploy/helm/specula/README.md) — HA (Postgres +
  Redis + shared CAS). Promote to this *after* a bootstrap Specula can serve its
  dependency images.

**In China, read the registry section of [RELEASE.md](RELEASE.md) first.** The
image has to come from a cloud-region registry (ACR/SWR); Docker Hub and every
public CN mirror measured were unusable, and every other layer depends on that
first pull.

## Operate

| Doc | Contents |
|-----|----------|
| [RELEASE.md](RELEASE.md) | Cutting a release: repo secrets, multi-registry publish, gates, chart/binary version skew |
| [OFFLINE.md](deploy/OFFLINE.md) | Air-gap cookbook (warm → offline → prefetch) |
| [TRUST.md](TRUST.md) | Cosign / apt GPG / Helm `.prov` / dependency-confusion cookbook + independent oracles |

`specula doctor` (node) and `specula cluster doctor` (cluster) are the first thing
to run when something is wrong — both exit non-zero on RISK.

## Build on it

| Doc | Contents |
|-----|----------|
| [LIBRARY.md](LIBRARY.md) | Public `pkg/**` API, stability guarantees, error contract |
| [ARCHITECTURE.md](ARCHITECTURE.md) | Two-plane design, cache tiers, verification pipeline, HA matrix |
| [REGISTRY-DESIGN.md](REGISTRY-DESIGN.md) | The hosted OCI registry (push path, org namespaces, token auth) |

## Project

| Doc | Contents |
|-----|----------|
| [PRD.md](PRD.md) | Product requirements and milestones |
| [DESIGN-REVIEW.md](DESIGN-REVIEW.md) | Recorded design decisions and their trade-offs |
| [MUTATION-TESTING.md](MUTATION-TESTING.md) | Mutation gate: how the trust claims are held honest |
| [../CHANGELOG.md](../CHANGELOG.md) | Release notes |
| [../CLAUDE.md](../CLAUDE.md) | Gated development workflow (API → SDK → CLI → WebUI) and the list of known production footguns |
