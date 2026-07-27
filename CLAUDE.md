# Specula — Claude / agent development guide

This file is the **gated development workflow** for Specula. Follow it for every feature, bugfix, and resilience change. Do not skip a stage because “the next layer will catch it.”

## Surfaces (in order)

| Stage | What it is | Primary paths |
|-------|------------|---------------|
| **1. API** | Internal packages: handlers, upstream, cache, verify, config, bootstrap | `internal/**`, protocol handlers under `internal/handler/**` |
| **2. SDK** | Public Go library façade | `pkg/specula`, `pkg/embed`, `pkg/artifact`, `pkg/upstream`, `pkg/handler`, `docs/LIBRARY.md` |
| **3. CLI** | Daemon + operator commands | `cmd/specula/**` (`serve`, `integrate`, `install`, `config`, `bootstrap-*`, …) |
| **4. WebUI** | Control-plane SPA embedded in the binary | `web/**` → embedded via `web/embed.go` |

Pipeline:

```text
API  →  SDK  →  CLI  →  WebUI
 ↑       ↑       ↑       ↑
 tests   tests   tests   tests   (each gate must be green before advancing)
```

## Hard rule: test gate before advancing

1. Implement and **fully test** the current stage.
2. Only then expose or wire the next stage.
3. A regression found later means **go back** to the earliest broken stage — do not paper over it in CLI/WebUI.

Minimum bar per stage (all required when that stage changes):

| Stage | Must pass before next stage |
|-------|-----------------------------|
| API | `go test ./internal/...` (unit). For protocol/upstream/cache paths that have integration tags: `go test -tags=integration ./test/e2e/...` for the affected scenarios. Prefer TDD: RED → GREEN → IMPROVE. Coverage target **≥ 80%** on touched packages. |
| SDK | `go test ./pkg/...` plus a smoke that `pkg/specula` / `pkg/embed` still compile against the new API. Update `docs/LIBRARY.md` if the public contract changed. |
| CLI | `go test ./cmd/specula/...` and manual/scripted smoke for changed commands (`integrate`, `install`, `config apply-example`, …). Prefer hermetic tests over live cluster when possible. |
| WebUI | `cd web && npm test` / `npm run build` as applicable; exercise control-plane API the UI calls. Never ship UI that assumes an untested API. |

Orchestration shortcuts (from repo root):

```bash
make test-unit          # Go unit tests
make test-integration   # integration-tagged tests
make test-e2e           # broader e2e when available
make test-ui            # WebUI
make test-all           # full gate when cutting a release-quality change
```

## Upstream / CN resilience (API-first)

Changes to `internal/upstream` (failover, resume, circuit breaker, budgets) **must**:

1. Land as API unit tests first (`internal/upstream/*_test.go`).
2. Then hermetic integration/E2E (`test/e2e/*failover*`, OCI/Helm) before CLI bootstrap/integrate behaviour is considered done.
3. Metrics (`specula_upstream_*`) updated with bounded labels only.
4. Never wrap streaming response bodies in failsafe Timeout / `http.Client.Timeout`.

Known production constraints (do not regress):

- Non-final mirrors: short dial + fail-fast on dial/TLS/header timeout; soft retry only for 5xx/429.
- Mid-stream Range resume with cross-upstream fallthrough without closing the caller body.
- containerd `hosts.toml` must **not** keep `server = "https://…"` public fallbacks on CN nodes.
- containerd **2.2** CRI `config_path` must be a **single** directory (not `certs.d:/etc/docker/certs.d`). The colon default is ignored by the transfer service — `crictl`/kubelet bypass Specula while `ctr --hosts-dir` still works ([containerd#12808](https://github.com/containerd/containerd/issues/12808)). `integrate --protocols oci` rewrites this **and** sets `plugins.'io.containerd.transfer.v1.local'.config_path` to the same certs.d (default `''` skips hosts.toml for transfer pulls).
- `integrate` default `--addr` is `https://127.0.0.1:7732`. Passing `http://` against a TLS-only port auto-upgrades to `https://` (avoids hosts.toml HTTP 400). Use `--skip-scheme-probe` only for offline/tests.
- After OCI wire-up on a node: `specula doctor` (or `integrate doctor`) — catches colon/empty CRI+transfer `config_path`, stale `containerd config dump`, residual `server=`, k3s wrong certs.d root, missing `registry.k8s.io` hosts, Specula `/v2/` down, **apt https without system CA**, **apt http list vs https addr**. Exit 1 on RISK.
- Upstream HTTP clients must send `Specula/upstream` User-Agent (`upstream.WrapUserAgent`); CN mirrors (tuna) 403 Go's default UA.
- Gate: `make test-cri` / `scripts/realclient-cri-k8s.sh` (hermetic colon-bypass + single-path Specula + live pause/etcd via CRI). Set `SPECULA_E2E_CRI=0` to skip.
- OCI install overwrite only when config is still single-`base_url` / one-upstream (or `SPECULA_FORCE_CN_OCI=1`).

## Agent working rules

- **Run anything slow in the background.** CI polling (`gh run watch`, status
  loops), `docker buildx` builds, `make test-cluster-install`, `make
  test-single-host`, cluster installs, `minikube start` — all of it goes to a
  background task, and you report from its output when it finishes. Never chain
  sleeps or poll in the foreground: a blocked session cannot be redirected while a
  15-minute build runs. Single quick checks (`gh run view`, a file read) are fine.
- **Backgrounding is not handing off.** After launching a background task, keep
  working in the same turn: chain the follow-up steps into that task, or do the
  next independent piece. Never end a turn with "waiting for the notification" —
  the human should never have to ask "进度如何". Prefer ONE background chain that
  runs the whole remaining sequence (build → install → verify → clean up) over
  several tasks that each need a prompt to continue.
- **Never mutate a live cluster to make a test easier.** Do not delete images the
  cluster depends on (`crictl rmi …/pause`), restart node components, or drain
  nodes to force a cache miss. Pull something the node does not have instead, and
  read `specula stats` / `/metrics` for the evidence.
- **Verify a value before wiring it into CI or a cluster.** Registry hostnames,
  endpoints and credentials: probe first (`curl https://<registry>/v2/` must answer
  401), then set the secret. Guessing a hostname costs a full CI round trip.
- **Keep prose short.** Lead with the result or the blocker. Enumerating options,
  restating context, or narrating what you are about to do reads as stalling.

## Commit & review

- Conventional commits: `feat|fix|refactor|docs|test|chore|perf|ci: …`
- No secrets in tree; validate at system boundaries.
- After substantive code: address CRITICAL/HIGH review findings before moving to the next surface.
- Do not push or open PRs unless the human asks.

## Quick checklist (copy into PRs)

- [ ] API tests green for touched `internal/**`
- [ ] SDK tests green / docs updated if public API changed
- [ ] CLI tests / smoke for changed commands
- [ ] WebUI build/tests if control-plane UI touched
- [ ] No advance to next stage with failing tests on the previous
