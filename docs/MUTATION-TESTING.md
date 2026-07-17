# Mutation testing — do our tests TEST, or do they merely EXECUTE?

> Gate: `make test-mutation` → `scripts/mutation-gate.sh`
> Artifacts: `results/mutation/summary.json`, `results/mutation/survivors.tsv`
> Status: **report-only**. Proposed threshold: **85% efficacy**.

## Why this dimension exists

`scripts/coverage-gate.sh` asks *"was this line executed?"*. Mutation testing asks the only
question that matters: *"if this line were **wrong**, would any test notice?"*

Those are different questions, and this codebase's dominant failure mode lives in the gap
between them. Repeatedly, a test double answered **whatever the code asked** rather than
what the real dependency does, so the test could not fail no matter what production did:

| Shipped defect | The lie |
|---|---|
| `3ccd5ad` — a caller's `?digest=` pin ignored on every cache hit | `fakeMetaStore.Get` keyed on `ref.Digest` — the **exact opposite** of production, which ignores digest. A wrong pin looked like a clean cache miss. Passed unit tests, the OCI conformance suite **and** the coverage gate. |
| admin stats | `fakeStatsCollector.AddOpaquePath` was `{}`; `ByProtocol` returned a preset map. The suite **structurally could not fail** for the bug it existed to cover. |
| `tier=""` beside 6 real pins | `fakeMetaStore.GetMutable` always returned `(nil, nil)` — a TOFU pin could never be observed. |
| `02186f7` — sumdb URL shape | `sumdb_test.go` keyed its double on `responses[r.URL.Path]`, answering whatever path the handler built. **Any** URL shape passed, including the broken one it enshrined as expected. |

A lying double means the tests never exercise production behaviour — so mutants in that
production code **survive**, in clusters. Mutation testing attacks this mechanically.

This also answers a specific weakness in `make test-trust-oracle-mutations`: those mutation
proofs are hand-picked **by the same agent that wrote the fix**, i.e. it chooses mutations it
already knows its tests catch. That is self-certification, not evidence. A tool enumerates
them and cannot be cherry-picked. The two gates are complements: that one proves the
**oracle** catches lies; this one proves the **tests** do.

## Tool choice: `go-gremlins/gremlins` v0.6.0

Per the standing rule (align to a mature implementation; do not hand-roll), the Go mutation
landscape was surveyed before writing anything.

| Candidate | Verdict | Evidence |
|---|---|---|
| **`go-gremlins/gremlins`** | **CHOSEN** | Apache-2.0, 373★. `go.mod` declares `go 1.25.0` — matches our toolchain. Releases through **v0.6.0 (2025-12-06)**, commits into 2026-03, issues triaged within days. CLI takes a **positional package path** (scoping), `-o` writes a **JSON report**, `--threshold-efficacy` / `--threshold-mcover` are built-in gates, `-E` excludes by regexp. 11 operators (4 on by default). Installs cleanly via `goproxy.cn` in ~6s. |
| `zimmski/go-mutesting` | **REJECTED — dead** | Last commit **2019-10-21**; last release **v1.2 (2021-06-10)**; 43 open issues untouched. 673★ is legacy popularity, not maintenance. Predates modules-as-default and our toolchain by six years. Gremlins' own README points at a *fork* (`avito-tech`) rather than this repo, which is itself a signal. |
| `gtramontina/ooze` | **REJECTED — wrong shape** | Alive-ish but only dependency bumps since 2026-04; **no releases at all**. Fatal for us: it is a *library*, not a CLI — you embed a runner inside a test file (`ooze.Release(t)`) and it mutates the whole module from there. That means editing the test tree to add a gate, and no clean way to scope to a package subset or to keep the gate out of `go test ./...`. It would also collide with the `-short`/`-race` default loop. |
| Go's own toolchain | **Nothing relevant** | `go test` has coverage and fuzzing; no mutation support, and no proposal near acceptance. |

### Known upstream sharp edges (verified here, not taken on faith)

- **#267 — "Everything times out when running twice in a row" (open since 2026-01-04).**
  We hit this immediately and it is the single most dangerous thing about the tool. See
  below; `scripts/mutation-gate.sh` neutralises it with `GOFLAGS=-count=1`.
- **#295** — 0% mutator coverage when a comment precedes the `module` directive in `go.mod`.
  Not applicable: our `go.mod` opens with `module github.com/ivanzzeth/specula`.
- **#272** — integration mode (`-i`) misreports LIVED. We do **not** use `-i`.
- **#252** — generics unsupported. Minor for the scoped packages.

## The trap that would have made this gate a liar

Gremlins derives its per-mutant timeout from the wall-clock of **one coverage run**
(`internal/coverage/coverage.go` → `executeCoverage()`; `internal/engine/executor.go` →
`testExecutionTime = elapsed * coefficient`). That `go test` carries **no `-count=1`**, so
Go's test cache replays it.

Measured on `internal/verify` (288 mutants):

| | coverage elapsed | timeout | result |
|---|---|---|---|
| cache warm (gremlins as-shipped) | **0.66s** (replay) | 6.6s | 48 KILLED, 0 LIVED, **240 TIMED OUT** → **"Test efficacy: 100.00%"** |
| `GOFLAGS=-count=1` | **11.8s** (real) | 94s | 256 KILLED, 30 LIVED, 2 TIMED OUT → **89.51%** |

Read the first row again. Gremlins printed a **perfect 100%** while silently discarding
**240 of 288 mutants**. Efficacy is `KILLED/(KILLED+LIVED)`, so a TIMED OUT mutant leaves
the denominator entirely — **a too-short timeout does not fail the gate, it flatters it.**
That is exactly the hidden-exclusion dishonesty this repo exists to fight, arriving through
the tool instead of through us.

`GOFLAGS=-count=1` is therefore exported by the script. It reaches the coverage run *and*
every mutant run, so the measurement is honest and no mutant can be served from cache — the
same "*a cached pass is not a pass*" rule the rest of the test matrix already follows.

A single timeout coefficient cannot serve all packages, because the budget is derived from
each package's own test time: `internal/verify` takes 11.8s (coefficient 5 → 59s, ample),
but `internal/metrics` takes 0.036s (coefficient 5 → 0.18s, **shorter than the mutant's own
compile step**, so all 18 mutants time out and the score reads a fraudulent 100%). The
script sets the coefficient **per package** (`coefficient_for`).

### Timeouts are two different things, and the gate must not conflate them

The remaining 2 timeouts are **not** a budget artifact, and the honest handling of that
distinction matters more than the number:

- **GENUINE** — the mutation really hangs the suite. Both survivors sit on
  `consensus.go:191`, `for i := 0; i < total; i++ { r := <-resultCh; ... }`. Mutating `<` →
  `<=` (BOUNDARY) or `i++` → `i--` (INCREMENT_DECREMENT) makes the loop receive **more values
  than were ever sent** on `resultCh`, so it blocks forever. Deadlock by construction. Per
  gremlins' own docs this means "the mutation actually made the tests fail, but not
  explicitly" — i.e. the mutant **was caught**, just not creditably. The score is therefore
  *conservative*, not inflated.
- **ARTIFACT** — the budget ran out. The mutant was caught by nothing; the score is inflated.

The distinguishing **evidence** is that a genuine timeout persists when the budget rises and
an artifact disappears. Verified: these two are identical at coefficient 5 **and** 8, across
runs whose coverage elapsed differed (11.8s vs 7.4s — the measurement is load-dependent).

So the gate does **not** guess by ratio — a ratio rule is precisely the fudge this gate
exists to refuse. It pins the sites verified genuine (`KNOWN_GENUINE_TIMEOUTS`, one entry:
`consensus.go:191`, with the proof above) and treats a timeout **anywhere else** as a
**measurement failure** that exits non-zero *even in report-only mode*. Report-only is a
statement about thresholds, never a licence to publish a number computed over a silently
shrunk denominator. That list carries the same contract as the equivalent-mutant list below:
an entry without a proof is a bug, not a waiver.

## Scope, and why

Full-repo mutation testing is not useful: cost is O(mutants × package test time), each mutant
being a full compile + test of its package. We scope to the **trust-bearing** code, where a
survivor is a statement about supply-chain safety rather than about plumbing:

| Package | Why | Wall-clock |
|---|---|---|
| `internal/verify` | The four-tier trust model itself (PRD §G2). Per PRD §7.5, tagging a checksum-only artifact `tier="signed"` is *"the most serious error this codebase can make"*. | ~4 min (288 mutants) |
| `internal/cache` | Digest pinning, freshness, serve-stale, Range serving. Source of the `3ccd5ad` pin bug and the `45674be` serve-stale bug — **both shipped green**. | ~15s (47) |
| `internal/metrics` | The tier counter. PRD §7.5 makes `/metrics` the operator-facing evidence for G2; a mislabelled series is a false claim about trust. | ~13s (18) |

`internal/artifact` was in the original target list and is **deliberately excluded**: it is
197 lines of type/interface declarations, and a dry-run finds **zero mutants** in it. Listing
it would be cargo-cult — an empty target that always passes while implying coverage it cannot
provide.

## Equivalent mutants — nothing is excluded

Some survivors are semantically identical to the original and can **never** be killed by any
test. The honest move is to **name** them, not to filter them out of the denominator: a score
propped up by hidden exclusions is worse than a lower honest one.

**Every mutant below remains in `summary.json`, in the survivor table, and in the
denominator. They cost us efficacy points on purpose.** If this list ever grows without a
proof beside each entry, distrust it.

| Site | Mutation | Proof of equivalence |
|---|---|---|
| `verify.go:113:15` | `res.Tier > highestTier` → `>=` | At `res.Tier == highestTier` the body executes `highestTier = res.Tier` — a **self-assignment**. No reachable state differs. |
| `cache.go:428:12` | `offset < 0` → `<=` | At `offset == 0` the body executes `offset = 0` — self-assignment. |
| `cache.go:431:12` | `offset > total` → `>=` | At `offset == total` the body executes `offset = total` — self-assignment. |
| `cache.go:435:27` | `length < int64(len(data))` → `<=` | At `length == len(data)` the body executes `data = data[:len(data)]` — self-assignment. |
| `aptpins.go:125:8` | `i > 0` → `>=` (BOUNDARY) | Prepends one `\x00` to **every** key uniformly. `memKey` is a pure, **process-local** map-key derivation (`memAptPinStore` is documented "process-local, lost on restart") used symmetrically for all reads and writes; a uniform prefix preserves injectivity, so no observable behaviour changes. Equivalent **by symmetry**, not by identity. |

**Not equivalent, though it looks it** — `cache.go:177:34`, `isMutableFresh`'s
`time.Since(e.FetchedAt) < ttl` → `<=`. This *is* semantically different, but only when
elapsed time equals the TTL to the nanosecond against a real clock. It is **untestable
without an injected clock**, not equivalent. It stays in the denominator, labelled honestly.
Killing it would mean injecting a clock into `internal/cache` — a real (if unglamorous)
improvement, not a scoring trick.

## Triage of the top survivors

Ranked. Each is a lead, not a verdict-free number.

### 1. `gpg.go:702:33`, `gpg.go:706:12` — **lying fixture**, apt `signed` tier

```go
el, armErr := openpgp.ReadArmoredKeyRing(f)
if armErr == nil { return el, nil }
if _, err := f.Seek(0, 0); err != nil { return nil, err }   // 702 — survives
el, binErr := openpgp.ReadKeyRing(f)
if binErr != nil { return nil, fmt.Errorf(...) }            // 706 — survives
```

**Every keyring fixture in the tree is armored** — `gpg_test.go:newAptTestKey`,
`helmprov_test.go:newHelmTestKey`, and even `test/e2e/apt_e2e_test.go` (`test-keyring.asc`)
all build them with `armor.Encode`. **Every real distro keyring is binary**; verified on this
host:

```
$ file /usr/share/keyrings/ubuntu-archive-keyring.gpg
OpenPGP Public Key Version 4, Created Fri May 11 2012, RSA 4096 bits    # magic 0x99, not "-----BEGIN PGP"
```

So the binary fallback — **the only path production takes** — is never once exercised to
success by a Go test at any level. The lines are "covered" solely by the *negative* tests
(`TestNewGPGVerifier_InvalidKeyring`, `..._EmptyKeyring`), and those assert only
`require.Error` — **any** error satisfies them, including the wrong error produced by a
broken path. That is why both mutants live: mutate `err != nil` → `err == nil` and
`loadKeyring` returns `(nil, nil)`; `NewGPGVerifier` then fails with "contains no keys"
instead, and `require.Error` is still satisfied. The test cannot tell correct from broken.

This is the `fakeMetaStore.Get` species exactly: **a fixture whose shape contradicts
production**, on the tier PRD §G2 calls the *端到端金标准* (end-to-end gold standard).

*Mitigating, stated plainly:* the binary path **is** exercised by `make test-trust-oracle`,
which feeds Specula the real `/usr/share/keyrings/ubuntu-archive-keyring.gpg`
(`scripts/trust-oracle.sh:77,223`). So this is not "nothing tests it" — it is "nothing in the
default loop tests it, and the tests that claim to, structurally cannot".

**Verdict: missing test + lying fixture. Not a live bug** (`loadKeyring` is correct).
**Fix:** add a binary-keyring fixture (`entity.Serialize` without `armor.Encode`) and assert
on the *specific* error in the negative tests.

### 2. `aptpins.go:174:25` — **the tested implementation is not the default one**

```go
byRepo := m.pool[memKey(scope, poolPath)]
for _, sum := range byRepo {
    if found != "" && sum != found { return "", ErrAmbiguousPoolPin }   // survives: sum != found → sum == found
    found = sum
}
```

`TestGPGVerifier_AmbiguousPoolPin_FailsClosed` and `..._AgreeingPoolPins_AcrossRepos_Resolve`
exist and look like exactly the right tests — but they build their store via
`newSharedPinStore(t)`, which returns `aptpins.New(st.DB(), aptpins.SQLite)`: **the
SQLite-backed implementation in a different package**. They therefore exercise *that* copy of
the ambiguity guard. `memAptPinStore.PoolPin` — the **default** store, used whenever
`WithAptPinStore` is not passed — carries its own copy, and **no test exercises its ambiguity
branch**. Inverting the condition (fail-closed becomes fail-*open*: it errors when repos
*agree* and stays silent when they *conflict*) changes nothing observable.

Two implementations of one security guard; only one is tested. The comment above
`memAptPinStore` says it exists so "the verifier has exactly ONE chain-state code path" — the
mutation shows there are two, and the second is unverified.

*Mitigating:* `cmd/specula` wires the metadata store and fails fast otherwise, so the
untested copy is not the production path.

**Verdict: missing test.** Medium. **Fix:** run the ambiguity tests against both
implementations (table-driven over the `AptPinStore` constructors).

### 3. `metrics.go:285:53`, `285:56`, `273:12`, `273:28` — **tests that cannot distinguish**

```go
if status < 100 || status > 599 { return "invalid" }                                    // 273
out[i] = string(rune('0'+n/100)) + string(rune('0'+(n/10)%10)) + string(rune('0'+n%10)) // 285
```

`statusStrings` is a hand-rolled itoa in the hot path. The whole test is:

```go
assert.Equal(t, "200", httpStatusLabel(200))
assert.Equal(t, "404", httpStatusLabel(404))
assert.Equal(t, "502", httpStatusLabel(502))
```

**All three have tens digit 0.** So the middle-digit arithmetic is never validated: mutate
`'0'+(n/10)%10` → `'0'-(n/10)%10`, or `(n/10)` → `(n*10)` (making the term identically 0), and
200/404/502 still render perfectly. Statuses with a non-zero tens digit — **429** (rate
limited) and **503**-adjacent codes this proxy really emits — would silently mislabel. 100%
coverage; zero discriminating power. Same species as the lying doubles: the test
*structurally cannot fail*.

`273:12/28` are the twin: the boundaries **100** and **599** are the only interesting inputs
and neither is tested (the suite tests 99, 0, 600, −1 — all *outside*). `status < 100` → `<=`
makes `100` render "invalid" and nothing notices.

**Verdict: missing test. Not a live bug.** **Fix — one line, kills the whole class:**
`for i := 100; i <= 599; i++ { assert.Equal(t, strconv.Itoa(i), httpStatusLabel(i)) }`.

### 4. `cache.go:428:12` (NEGATION), `435:12`, `435:27` (NEGATION) — untested Range arithmetic

`serveMutablePayload` implements Range semantics for payload-backed mutable entries (PyPI
simple index, npm packument). Its only test, `TestServeMutablePayloadEntry`, calls
`Serve(ctx, ref, 0, -1)` — the trivial whole-body case. `TestServeEntryRangeRead` *does* test
ranges, but on the **blob-backed** path (`m.blobs.Get`), which never enters this function.

So the clamping logic executes on every run and is never tested. Survivors prove it:
`offset < 0` → `offset >= 0` clamps **every** offset to 0 (all Range requests would serve from
byte 0); `length >= 0` → `length > 0` makes a zero-length range serve the **whole** body;
`length < len(data)` → `>=` disables truncation. All three change real behaviour; none is
caught.

Coverage reports these lines green because `(0, -1)` executes them. **This is the thesis of
this document in four lines of code.**

**Verdict: missing test. Not a live bug** — the arithmetic is correct on inspection.
**Fix:** table-drive `serveMutablePayload` over `(offset, length)` pairs.

### Not pursued (reported, ranked below the above)

`sumdb_client.go:{123,174,207,216,247}`, `consensus.go:{144,205}`, `consensus_http.go:{76,98,142,173,244}`,
`gpg.go:{236,242,276,287,401,528}`, `gitsigned.go:54`, `sumdb.go:351`, `depconfusion.go:164`,
`helmprov.go:{220,230}`, `cosign_fetcher.go:176`. Predominantly off-by-one boundary mutants on
retry counts, buffer sizes and index bounds. Worth a pass, but none carries the
"the-test-cannot-fail" signature of the four above.

## Did mutation testing find a real bug?

**No — and that is worth stating plainly rather than dressing up.** Every triaged survivor is
a *missing or non-discriminating test*, not live broken behaviour. The scoped code is correct
on inspection.

What it *did* find is the thing this repo actually cares about: **three places where tests
that look like they cover a security property structurally cannot fail** — an armored-only
keyring fixture standing in for a binary production format; an ambiguity guard whose tests
exercise a different implementation than the default; and a status-label test whose three
inputs cannot distinguish correct arithmetic from broken. Those are the same species as the
four defects in the table at the top of this file, caught **before** shipping, by a tool that
cannot cherry-pick what it looks for.

That is the dimension paying for itself. An 89.5% efficacy headline would have told you none
of it — which is why `survivors.tsv` is the deliverable and the score is not.

## Threshold

**Proposed: 85% efficacy, report-only.** Measured baseline at `5d5ec3a` is ~89–90% on
`internal/verify` and ~85% on `internal/cache`, so 85% is a real floor with a little slack —
not a number chosen to be already-green everywhere.

**Report-only to start, deliberately:**

- The timeout budget is derived from **wall-clock** — machine- and load-dependent, and we
  watched it swing 11.8s → 7.4s between two runs on one idle machine. A budget that is too
  tight does not redden the gate, it *inflates* it (see above), so the failure mode of a
  flaky budget here is a false **green**, not a false red. That needs a few runs on other
  machines before a number derived from it is allowed to block.
- Equivalent mutants are in the denominator by design. Blocking on a score that includes
  unkillable mutants creates pressure to "fix" the score by excluding them — the exact
  dishonesty this gate exists to prevent. Report-only removes that incentive while the list
  is built honestly.

This mirrors `scripts/coverage-gate.sh`'s own GATED/WATCH precedent: prove the number is
stable and fair before letting it block, and never let a gate's convenience shape what it
reports. Promote to blocking (`MUTATION_BLOCKING=1`) once the coefficients are shown to hold
across machines.

**Non-negotiable even in report-only mode:** an **unexplained** timeout (one not in
`KNOWN_GENUINE_TIMEOUTS`, each entry proof-carrying) exits non-zero. That is not a score
failure, it is a **measurement** failure, and an invalid measurement must never be reported as
a result. Known-genuine hangs do not fail the gate — they make the score conservative, not
inflated — but they are still printed, per site, every run.
