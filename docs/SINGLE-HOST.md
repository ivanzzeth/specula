# Single-host install (intranet + systemd)

One VM in the same VPC as the clients, one static binary, systemd. No Kubernetes,
no container runtime, no HA. Ops work after day one is `scp` + one command.

## When this is the right shape

Use this instead of [`specula cluster install`](CLUSTER-INSTALL.md) when:

- One box is enough (a cache miss is slow once, then it is a local read).
- Clients live in the **same VPC / region** as this box. Then client traffic is
  intranet (free, Gbps) and the only public traffic is Specula's own upstream
  fetch — which is *inbound* on most CN clouds, i.e. not billed. Serving a
  cluster over the public internet is the expensive shape; avoid it.
- You accept a single point of failure (see [Fallback mirror](#fallback-mirror)).

`sqlite` + local blobs is correct here. Postgres only buys something under
`server.ha` or with `blob.driver: s3`.

## 1. Install

The binary is pure Go (`modernc.org/sqlite`, `CGO_ENABLED=0`) and the WebUI is
embedded, so there is nothing else to ship — it runs on old glibc too.

```bash
scp specula host:/tmp/specula
ssh host 'sudo /tmp/specula install'
```

`install` creates the `specula` system user, `/etc/specula/specula.yaml` from the
embedded example, `/var/lib/specula`, writes the systemd unit
(`ProtectSystem=strict`, `Restart=on-failure`, `WantedBy=multi-user.target`),
enables and starts it.

## 2. TLS — sign with DNS-01

`hosts.toml` must dial `https://` (plain `http://` against a TLS port returns
HTTP 400, and `http` + `skip_verify` is a documented footgun). A certificate
validates the **domain**, not the IP, so a public-CA cert works fine on an
intranet address.

The box has no public inbound reachability, so **HTTP-01 cannot complete — use
DNS-01**. Any ACME client with your DNS provider's plugin works; renewal is a
cron/timer plus `systemctl reload-or-restart specula`.

```yaml
server:
  data_plane_addr: "0.0.0.0:7732"
  control_plane_addr: "0.0.0.0:7733"
  tls:
    cert_file: /etc/specula/tls/fullchain.pem
    key_file: /etc/specula/tls/privkey.pem
  # Do not omit: left empty the server derives "<host the browser used>:<data port>",
  # which is wrong for every non-loopback deployment.
  registry_public_host: specula.internal.example.com
```

Cert files must be readable by `User=specula` (`chown specula: /etc/specula/tls/*`,
mode `0640` on the key).

## 3. Resolve the domain to the private IP

Cloud private DNS (Aliyun PrivateZone / Tencent Private DNS) with an A record to
the VPC address. Node-local `/etc/hosts` also works but is missed by every node
added later — private DNS is the maintainable choice.

## 4. Wire clients

```bash
sudo specula integrate --addr https://specula.internal.example.com:7732 \
                       --protocols oci,helm,go,npm,pypi,apt,git
sudo systemctl restart containerd     # only when the CRI config changed
specula doctor                        # exits 1 on RISK — run this every time
```

`doctor` is not optional: it catches the containerd 2.2 colon `config_path`
bypass, residual `server =` public fallbacks, k3s certs.d root, apt http-vs-https
mismatches. See [CLAUDE.md](../CLAUDE.md) for the full list of known footguns.

## 5. Upgrade and rollback

Linux refuses to write into a running executable (`ETXTBSY`), so copying straight
onto `/usr/local/bin/specula` fails; and stop-copy-start has no way back when a
build is bad. `specula upgrade` does the rename-swap, restarts, waits for
`/healthz`, and restores the previous binary if the daemon does not come up.

```bash
scp specula host:/tmp/specula
ssh host 'sudo /tmp/specula upgrade'
```

```text
upgrading /usr/local/bin/specula: v0.4.1 → v0.5.0
installed /tmp/specula → /usr/local/bin/specula (rollback copy: /usr/local/bin/specula.prev)
upgrade OK — specula.service healthy
verify wiring: specula doctor
```

A failed health check rolls back automatically and exits non-zero. To revert a
build that started fine but misbehaves later:

```bash
sudo specula rollback
```

| Flag | Meaning |
|------|---------|
| `--binary PATH` | Installed path to replace (default `/usr/local/bin/specula`) |
| `--config PATH` | Config read only to locate the health endpoint (never modified) |
| `--no-restart` | Swap now, restart in a maintenance window |
| `--skip-health` | Restart without the `/healthz` gate (no auto-rollback) |
| `--health-timeout D` | Default `60s` |

The health probe dials loopback with TLS verification off — the cert is minted
for the public domain, so verifying against `127.0.0.1` would always fail. It is
a liveness check, not a trust decision.

Config is never rewritten by `upgrade`. To pull new reference defaults after a
version bump, do it deliberately:

```bash
sudo specula config apply-example --dry-run
```

## 6. Disk

Blobs only grow. Put `/var/lib/specula` on its own disk and set a cap — a full
disk here means every client in the VPC stops pulling.

```yaml
cache:
  max_bytes: 200_000_000_000   # evicts oldest unpinned entries; 0 = unbounded
```

## 7. Security group

Allow `7732`/`7733` **from the VPC CIDR only**. No public inbound. An
internet-exposed data plane is an open proxy, and `7733` is the admin plane.

## Fallback mirror

If this host is down, every client fails to pull. Add a second `mirror` entry
after Specula in `hosts.toml` (e.g. a public CN mirror). Add a *mirror* — do not
restore the `server = "https://…"` public fallback, which is exactly the line CN
nodes must not carry.

## Acceptance

Automated — real systemd in a throwaway container, no cluster required:

```bash
make test-single-host          # or: scripts/single-host-upgrade.sh
# SPECULA_E2E_SINGLE_HOST=0 to skip; SPECULA_SYSTEMD_IMAGE=<image> to reuse one
```

Asserts install → `/healthz`, the ETXTBSY premise (`cp` onto the running binary
must fail), a health-gated `upgrade` of a live daemon, auto-rollback when the
gate fails, explicit `rollback`, `--no-restart` leaving `MainPID` untouched, and
that a binary built without `web/dist` still boots (serving the placeholder page
rather than panicking).

On the real host:

```bash
systemctl is-active specula
curl -fsS https://specula.internal.example.com:7733/healthz
specula doctor                                   # from a client node
crictl pull registry.k8s.io/pause:3.9            # from a client node
sudo specula upgrade --no-restart && sudo specula rollback   # exercise both paths
```
