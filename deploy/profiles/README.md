# Deployment profiles

One file, one command:

```bash
cp deploy/profiles/hosted-aliyun.example.yaml specula-deploy.yaml
$EDITOR specula-deploy.yaml                      # fill in the <> placeholders
specula cluster install --values specula-deploy.yaml --wait
```

There is no second step. The chart creates the credential Secret from the values in
the profile, so `kubectl create secret` is not part of the flow.

## Keep the filled-in file out of git

It contains a database password and an object-store secret key. `.gitignore` covers
`specula-deploy*.yaml`, `*.deploy.local.yaml` and `/tmp/`. Only the `*.example.yaml`
files here are tracked.

(The values also end up in helm's own release Secret, as with any chart — so a
profile file is no more exposed than `--set`, and no less in need of protection.)

## Flags still win

`--values` files are applied before flag-derived `--set`, and helm gives `--set`
precedence regardless of order. So with a profile in play, `cluster install` emits
`--set` **only for flags you actually typed** — otherwise its defaults (image,
persistence, mirror) would silently override the profile and a single config file
could never work.

## Available profiles

| File | Shape |
|------|-------|
| `client-mirror.example.yaml` | A client cluster pointing at a hosted Specula over the VPC. Mirror DaemonSet only — no Deployment, Service, PVC or HPA. Auto-covers new workers. |
| `hosted-aliyun.example.yaml` | Centrally hosted, **stateless**: Postgres metadata + OSS blobs, `ha=true` with Postgres advisory locks (no Redis), HPA, no PVC, no node pin. One instance serves many clusters. |

For a per-cluster mirror instead — SQLite + local disk, pinned to one node, with the
mirror/integrate DaemonSets rewriting node `hosts.toml` — pass no profile at all;
that is what `cluster install --cn` does by default. See
[docs/deploy/CLUSTER.md](../../docs/deploy/CLUSTER.md).
