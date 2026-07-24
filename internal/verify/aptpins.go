package verify

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrIndexRollback is returned when ReplaceIndexPins is asked to accept an
// InRelease whose Date is strictly older than the persisted high-water mark for
// that (scope, repo, suite). This is the TUF-style anti-rollback gate (PRD §G2 /
// DESIGN-REVIEW H2): an old-but-still-validly-signed index must not replace a
// newer one Specula has already accepted.
var ErrIndexRollback = errors.New("apt pins: anti-rollback: signed InRelease Date is older than persisted high-water")

// AptPinStore persists the apt trust chain's pinned hashes.
//
// # Why this is a store and not a cache
//
// The hashes an InRelease commits to are REQUIRED CHAIN STATE: without them a
// pool .deb cannot reach TierSigned at all — it can only fail closed. Holding
// them in one process's heap contradicts PRD §G3 ("Specula 实例无状态": shared
// state lives only in the blob store + metadata DB, with no gossip and no leader
// election). Behind a load balancer with >=2 replicas, replica A serving
// `apt-get update` while replica B receives the `.deb` is the NORMAL path, and a
// heap-local chain breaks it. The same applies to any restart or redeploy while
// a client's apt list is still valid.
//
// # What is pinned, and keyed by what
//
// Two distinct kinds of pin, with deliberately different keys and lifetimes:
//
//	index pins:  (scope, repo, suite) → {suite-relative path → sha256hex}
//	pool pins:   (scope, repo, pool path) → sha256hex
//
// `scope` is the identity of the TRUST ANCHOR that vouched for the pin — a
// digest over the keyring's primary key fingerprints (see keyringScope). A pin
// means "the holder of these keys signed an InRelease committing path P to hash
// H", so the anchor belongs in the key: pins made under one keyring must never
// be readable by a verifier anchored on a different one. It is deliberately NOT
// the upstream host: mirrors (aliyun, tuna) are interchangeable views of one
// repo, so keying by the serving mirror would break the chain on mirror
// failover — InRelease from one mirror could not vouch for a .deb from another,
// which is a 502 for a perfectly valid chain. It is also stable across restarts
// and identical on every replica, and it self-invalidates when the operator
// rotates the anchor, which is exactly when old pins should stop being trusted.
//
// `repo` is the URL repository prefix (`/apt/<repo>/dists/...`).
//
// `suite` is in the index key but deliberately NOT in the pool key. In a Debian
// repository the pool is shared across suites by design and pool filenames embed
// the version and architecture, so one pool path denotes exactly one immutable
// object regardless of which suite's Packages index happens to reference it.
// Putting the suite in the pool key would be actively wrong: a .deb request
// carries no suite (see PoolPin), so the lookup could not be performed at all.
type AptPinStore interface {
	// ReplaceIndexPins atomically makes pins the COMPLETE pin set for
	// (scope, repo, suite), removing any pins a previous InRelease established
	// for that suite, and advances the InRelease Date high-water mark.
	//
	// Replace — not merge — because InRelease is the mutable-tier root of its
	// suite (ARCHITECTURE §3). A path the newest signed InRelease no longer
	// lists must stop being servable at `signed`; merging would let a
	// superseded signed index be served indefinitely.
	//
	// Anti-rollback: if a high-water Date is already persisted and indexDate is
	// strictly earlier, the call returns ErrIndexRollback and leaves pins
	// unchanged. Equal or newer Dates are accepted (idempotent re-fetch OK).
	// indexDate must be non-zero.
	ReplaceIndexPins(ctx context.Context, scope, repo, suite string, indexDate time.Time, pins map[string]string) error

	// IndexPins returns the pins established by the most recent verified
	// InRelease for (scope, repo, suite). An empty map means no InRelease has
	// been verified for that suite — callers MUST treat that as "cannot chain
	// verify", never as "nothing is pinned, so anything goes".
	IndexPins(ctx context.Context, scope, repo, suite string) (map[string]string, error)

	// PutPoolPins upserts pool-path → sha256 pins learned from a Packages index
	// that has itself been verified against a signed InRelease.
	//
	// Upsert — not replace — because pool pins are immutable-tier facts
	// (ARCHITECTURE §3/§6), not a view of the current index. See PoolPin for why
	// they outlive the InRelease that produced them. Within one (scope, repo,
	// pool path) the newest signed statement wins.
	PutPoolPins(ctx context.Context, scope, repo string, pins map[string]string) error

	// PoolPin returns the sha256hex pinned for poolPath anywhere in scope, or ""
	// when no verified Packages index has ever pinned it.
	//
	// The lookup cannot be narrowed by repo: an immutable pool ref carries no
	// repository prefix (the apt handler drops it — see servePool — because the
	// upstream fetch path for a pool file is repo-independent). It therefore
	// fails closed on ambiguity: if two repositories under the same anchor pin
	// the same pool path to DIFFERENT hashes, there is no basis to choose, and
	// choosing would let one repo's InRelease vouch for another's bytes. That is
	// a trust bug; a 502 is not.
	//
	// It deliberately returns the hash rather than a bool: a store that could
	// only answer "is this path known?" would turn every pin into a
	// path-existence check and launder unverified bytes.
	PoolPin(ctx context.Context, scope, poolPath string) (string, error)
}

// ErrAmbiguousPoolPin is returned by PoolPin when two repositories under the
// same trust anchor pin one pool path to different hashes. Fail closed.
var ErrAmbiguousPoolPin = fmt.Errorf("apt pins: pool path pinned to conflicting hashes by two repositories under the same trust anchor")

// ─────────────────────────────────────────────────────────────────────────────
// In-memory implementation
// ─────────────────────────────────────────────────────────────────────────────

// memAptPinStore is the default AptPinStore: correct, process-local, and lost on
// restart. It exists so the verifier has exactly ONE chain-state code path
// regardless of wiring — a nil-store fallback branch inside the verifier would
// be a second, less-tested path through the trust chain.
//
// It is NOT suitable for the PRD §G3 topology; cmd/specula wires the metadata
// store instead and fails fast if it cannot.
type memAptPinStore struct {
	mu        sync.RWMutex
	index     map[string]map[string]string // scope\x00repo\x00suite → relpath → sha256
	highwater map[string]time.Time         // scope\x00repo\x00suite → InRelease Date
	pool      map[string]map[string]string // scope\x00poolPath → repo → sha256
}

// NewMemAptPinStore returns an in-memory AptPinStore.
func NewMemAptPinStore() AptPinStore {
	return &memAptPinStore{
		index:     make(map[string]map[string]string),
		highwater: make(map[string]time.Time),
		pool:      make(map[string]map[string]string),
	}
}

func memKey(parts ...string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\x00"
		}
		out += p
	}
	return out
}

func (m *memAptPinStore) ReplaceIndexPins(_ context.Context, scope, repo, suite string, indexDate time.Time, pins map[string]string) error {
	if indexDate.IsZero() {
		return fmt.Errorf("apt pins: ReplaceIndexPins requires a non-zero InRelease Date")
	}
	cp := make(map[string]string, len(pins))
	for k, v := range pins {
		cp[k] = v
	}
	k := memKey(scope, repo, suite)
	m.mu.Lock()
	defer m.mu.Unlock()
	if hw, ok := m.highwater[k]; ok && indexDate.Before(hw) {
		return fmt.Errorf("%w: got %s, high-water %s", ErrIndexRollback, indexDate.UTC().Format(time.RFC3339), hw.UTC().Format(time.RFC3339))
	}
	m.index[k] = cp
	m.highwater[k] = indexDate.UTC()
	return nil
}

func (m *memAptPinStore) IndexPins(_ context.Context, scope, repo, suite string) (map[string]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.index[memKey(scope, repo, suite)]
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out, nil
}

func (m *memAptPinStore) PutPoolPins(_ context.Context, scope, repo string, pins map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for poolPath, sum := range pins {
		k := memKey(scope, poolPath)
		if m.pool[k] == nil {
			m.pool[k] = make(map[string]string, 1)
		}
		m.pool[k][repo] = sum
	}
	return nil
}

func (m *memAptPinStore) PoolPin(_ context.Context, scope, poolPath string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	byRepo := m.pool[memKey(scope, poolPath)]
	found := ""
	for _, sum := range byRepo {
		if found != "" && sum != found {
			return "", ErrAmbiguousPoolPin
		}
		found = sum
	}
	return found, nil
}
