// Package git — bare-mirror store (ported from ai-sandbox gitproxy/mirror.go).
//
// Each bare mirror lives under <mirrorDir>/<host>/<project>.git and is
// protected by a per-path mutex so concurrent clone requests coalesce into
// a single git remote update (stampede protection, ARCHITECTURE §7).
package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// mirrorRefspecs is what a cache mirrors: branches and tags. Nothing else.
//
// # Why not +refs/*:refs/* (what `git clone --mirror` configures)
//
// A forge's ref namespace is far larger than the repository. GitHub synthesises
// refs/pull/<n>/head for every pull request ever opened; GitLab does the same
// under refs/merge-requests/*. On github.com/octocat/Hello-World that is 3289
// PR refs against 3 branches and 0 tags — 99.9% of the refs, and with them the
// entire object graph of every fork that ever opened a PR. Measured on this
// machine, cold:
//
//	git clone --mirror  →  94.4s, 10.3 MB   (3293 refs)
//	git clone --bare    →   5.3s, 29.6 KB   (3 refs)
//
// The --mirror clone exceeded the client's patience even when run directly by
// hand, which made the first clone of any popular repo unusable — the exact case
// US-7 exists to accelerate.
//
// refs/heads/* + refs/tags/* is not a compromise, it is the correct definition:
// it is precisely what an unproxied `git clone <upstream>` fetches. Mirroring
// refs/pull/* would make the proxy advertise MORE refs than the upstream's own
// clone does, and those refs are forge-specific to boot (they are not part of
// any git protocol spec — gitprotocol-v2(5) has no notion of them).
//
// # The trade-off, stated plainly
//
// A client explicitly asking for a forge-generated ref by name
// (`git fetch <proxy>/github.com/o/r.git refs/pull/1/head`) now gets "couldn't
// find remote ref" where the full mirror would have served it. That is a real
// behaviour change for a use case outside `git clone`/`git fetch` defaults. It
// is the deliberate price of US-7's headline case working at all; if PR-ref
// fetching is ever required, the right shape is an explicit per-repo opt-in
// refspec in config, not making every clone pay 94 seconds.
var mirrorRefspecs = []string{
	"+refs/heads/*:refs/heads/*",
	"+refs/tags/*:refs/tags/*",
}

// mirrorMarkerFile marks a bare mirror as COMPLETE. It is written inside the
// staging directory before the atomic rename into place, so its presence at the
// final path means "a clone we ran to completion produced this tree".
//
// # Why a marker and not a git-level health check
//
// A clone killed by SIGKILL (which is exactly what exec.CommandContext does to
// it when the request context is cancelled) leaves a directory that git itself
// considers a valid bare repository: verified against a real kill -9 of an
// in-flight clone of octocat/Hello-World, the corpse answers
// `git rev-parse --is-bare-repository` = true while refs/heads is empty and an
// orphaned objects/pack/tmp_pack_* remains. So "is this a git repo?" cannot
// distinguish a mirror from a corpse, and neither can "does it have refs?" — a
// genuinely empty upstream repository has none either. Only the writer knows
// whether it finished, so the writer records it.
const mirrorMarkerFile = "specula-mirror-complete"

// tempMirrorPrefix names the staging directories a clone builds in before the
// atomic rename. The leading dot keeps them out of the way of the mirror tree's
// <host>/<project>.git layout.
const tempMirrorPrefix = ".specula-tmp-"

// mirrorStore manages on-disk bare git mirrors.
type mirrorStore struct {
	root       string
	staleAfter time.Duration
	// syncTimeout bounds a single clone/fetch. It is applied to a context
	// DETACHED from the request, so a client hang-up cannot kill a sync.
	syncTimeout time.Duration

	// mu guards lastSync and syncMu maps (metadata-level lock, not path-level).
	mu       sync.Mutex
	lastSync map[string]time.Time
	syncMu   map[string]*sync.Mutex
}

func newMirrorStore(root string, staleAfter, syncTimeout time.Duration) *mirrorStore {
	if syncTimeout <= 0 {
		syncTimeout = defaultUpstreamTimeout
	}
	return &mirrorStore{
		root:        root,
		staleAfter:  staleAfter,
		syncTimeout: syncTimeout,
		lastSync:    map[string]time.Time{},
		syncMu:      map[string]*sync.Mutex{},
	}
}

// mirrorPath returns the absolute path for the bare mirror of ref.
func (m *mirrorStore) mirrorPath(ref repoRef) string {
	return filepath.Join(m.root, ref.mirrorRelPath())
}

// lockForKey returns the per-path mutex, creating it if absent.
func (m *mirrorStore) lockForKey(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.syncMu[key]; ok {
		return l
	}
	l := &sync.Mutex{}
	m.syncMu[key] = l
	return l
}

// isUsableMirror reports whether path holds a mirror that a clone ran to
// completion. It is the answer to "is this a usable mirror?", which is a
// strictly stronger claim than the "os.Stat succeeded" it replaces: a killed
// clone leaves a directory that stats fine, passes git's own bare-repo check,
// and serves an empty repository to every client forever.
func isUsableMirror(path string) bool {
	st, err := os.Stat(filepath.Join(path, mirrorMarkerFile))
	return err == nil && st.Mode().IsRegular()
}

// syncContext derives the context a clone/fetch runs under: detached from the
// caller's cancellation, bounded by syncTimeout.
//
// The bare mirror is SHARED cache state, not per-request scratch. Binding a
// clone to the request context meant a client that pressed Ctrl-C — or simply
// gave up on the 94-second --mirror clone — SIGKILLed the clone mid-flight and
// left a corpse behind that was then served forever. A hang-up cancels a
// response; it must not corrupt what other clients read. context.WithoutCancel
// keeps the context's values (trace IDs, deadlines we set ourselves) while
// dropping the caller's cancellation.
func (m *mirrorStore) syncContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), m.syncTimeout)
}

// EnsureSynced creates or refreshes the bare mirror for ref, using
// upstreamURL as the remote source. Concurrent callers for the same
// mirror path block on a per-path mutex; only one clone/fetch runs.
//
// If the mirror exists, is usable, and was last synced within staleAfter, the
// call returns immediately (cache hit). A path that exists but is NOT a usable
// mirror — a corpse from a killed clone, or a full --mirror clone from before
// the refspec fix — is removed and re-cloned.
//
// contactedUpstream reports whether this call ran a clone or a fetch against the
// upstream, and is what lets the caller record an honest cache outcome: false
// means the mirror was already present and the response is built entirely from
// disk (a hit), true means objects were requested from the upstream (a miss).
// It is reported for failed syncs too, since those still cost an upstream round
// trip and fall through to a passthrough whose body comes from the upstream.
func (m *mirrorStore) EnsureSynced(ctx context.Context, ref repoRef, upstreamURL string) (contactedUpstream bool, err error) {
	key := ref.mirrorRelPath()
	l := m.lockForKey(key)
	l.Lock()
	defer l.Unlock()

	path := m.mirrorPath(ref)

	if isUsableMirror(path) {
		m.mu.Lock()
		last := m.lastSync[key]
		m.mu.Unlock()
		if !last.IsZero() && time.Since(last) < m.staleAfter {
			return false, nil // fresh mirror — serve from disk, no upstream contact
		}
		return true, m.fetch(ctx, path, key)
	}

	// Anything else at the path is not a mirror, whatever os.Stat says about it.
	if _, err := os.Stat(path); err == nil {
		if rmErr := os.RemoveAll(path); rmErr != nil {
			return false, fmt.Errorf("git mirror: remove unusable mirror %s: %w", path, rmErr)
		}
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("git mirror: stat %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("git mirror: mkdirall %s: %w", filepath.Dir(path), err)
	}
	return true, m.clone(ctx, upstreamURL, path, key)
}

// clone builds a complete bare mirror in a staging directory and moves it into
// place with a single atomic rename.
//
// Staging is what makes "the path exists" mean "the mirror is complete" for
// every other reader — a concurrent clone, the stats du walk, the cache browser,
// git http-backend. A mirror is never observable half-built, and a sync that
// dies (killed, timed out, upstream refused) leaves nothing at the final path
// for a later EnsureSynced to mistake for synced.
func (m *mirrorStore) clone(ctx context.Context, src, path, key string) error {
	ctx, cancel := m.syncContext(ctx)
	defer cancel()

	parent := filepath.Dir(path)
	// Sweep staging dirs orphaned by an earlier hard kill (SIGKILL leaves no
	// chance to clean up). Safe: the per-key mutex means no live clone for this
	// key is using one, and staging names are derived from the mirror's basename.
	m.sweepStaging(parent, filepath.Base(path))

	staging, err := os.MkdirTemp(parent, tempMirrorPrefix+filepath.Base(path)+"-")
	if err != nil {
		return fmt.Errorf("git mirror: staging dir in %s: %w", parent, err)
	}
	// MkdirTemp creates the directory; git clone wants to create it itself.
	stagingRepo := filepath.Join(staging, "mirror.git")
	defer os.RemoveAll(staging) // no-op once the rename has moved the repo out

	// --bare (not --mirror): --mirror configures fetch = +refs/*:refs/* and
	// refs/pull/* comes down with it. --bare fetches branches and tags, and
	// sets HEAD from the upstream's default branch, which is what clients need
	// to resolve a default branch on clone.
	cmd := exec.CommandContext(ctx, "git", "clone", "--bare", "--", src, stagingRepo)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone --bare: %w: %s", err, trimGitOutput(out))
	}

	// `git clone --bare` deliberately leaves remote.origin.fetch UNSET, so
	// without this the mirror would never refresh: a later `git fetch origin`
	// would update FETCH_HEAD and nothing else, and the mirror would silently
	// serve the clone-time refs forever. --mirror used to configure the refspec
	// (as +refs/*:refs/*); we configure the one we actually want.
	if err := m.configureRefspecs(ctx, stagingRepo); err != nil {
		return err
	}
	m.configurePartialClone(ctx, stagingRepo)

	// The marker goes in before the rename: the tree is complete as of here.
	if err := os.WriteFile(filepath.Join(stagingRepo, mirrorMarkerFile),
		[]byte("complete\n"), 0o644); err != nil {
		return fmt.Errorf("git mirror: write completion marker: %w", err)
	}

	if err := os.Rename(stagingRepo, path); err != nil {
		// A racing writer (another process sharing this mirror root) may have
		// landed a complete mirror first. Its work is as good as ours.
		if isUsableMirror(path) {
			m.markSynced(key)
			return nil
		}
		return fmt.Errorf("git mirror: publish %s: %w", path, err)
	}

	m.markSynced(key)
	return nil
}

// sweepStaging removes staging directories left by a previously killed clone of
// base under parent.
func (m *mirrorStore) sweepStaging(parent, base string) {
	matches, err := filepath.Glob(filepath.Join(parent, tempMirrorPrefix+base+"-*"))
	if err != nil {
		return
	}
	for _, p := range matches {
		_ = os.RemoveAll(p)
	}
}

// configureRefspecs installs mirrorRefspecs as remote.origin's fetch refspecs.
func (m *mirrorStore) configureRefspecs(ctx context.Context, path string) error {
	for i, spec := range mirrorRefspecs {
		args := []string{"-C", path, "config", "--add", "remote.origin.fetch", spec}
		if i == 0 {
			// Replace whatever clone left (nothing, today) rather than appending
			// to it, so re-running can never accumulate duplicate refspecs.
			args = []string{"-C", path, "config", "--replace-all", "remote.origin.fetch", spec}
		}
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git config remote.origin.fetch %q: %w: %s",
				spec, err, trimGitOutput(out))
		}
	}
	return nil
}

// configurePartialClone applies partial-clone server-side knobs to the bare
// mirror at path.  These are best-effort: a failure degrades partial-clone
// support for this mirror but does not break full clones.
//
// Refs: gitprotocol-capabilities §filter (uploadpack.allowFilter),
//
//	git-config(1) uploadpack.allowAnySHA1InWant.
func (m *mirrorStore) configurePartialClone(ctx context.Context, path string) {
	for _, args := range [][]string{
		{"-C", path, "config", "uploadpack.allowFilter", "true"},
		{"-C", path, "config", "uploadpack.allowAnySHA1InWant", "true"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		_ = cmd.Run() // best-effort; failure is non-fatal
	}
}

// fetch refreshes an existing mirror against mirrorRefspecs.
//
// --prune drops branches deleted upstream; --prune-tags does the same for tags
// (a deleted or moved tag is otherwise sticky, since the tag refspec would only
// ever add). Like clone, it runs detached from the request context.
func (m *mirrorStore) fetch(ctx context.Context, path, key string) error {
	ctx, cancel := m.syncContext(ctx)
	defer cancel()

	// Re-assert the refspecs before fetching so a mirror created by an older
	// build (or edited by hand) converges on the current policy instead of
	// quietly keeping +refs/*:refs/* and dragging refs/pull/* down on refresh.
	if err := m.configureRefspecs(ctx, path); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "git", "-C", path,
		"fetch", "--prune", "--prune-tags", "--quiet", "origin")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git fetch --prune: %w: %s", err, trimGitOutput(out))
	}
	m.markSynced(key)
	return nil
}

// markSynced records a successful sync time for key.
func (m *mirrorStore) markSynced(key string) {
	m.mu.Lock()
	m.lastSync[key] = time.Now()
	m.mu.Unlock()
}

// trimGitOutput caps the output snippet included in error messages.
func trimGitOutput(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 512 {
		return s[:512] + "…"
	}
	return s
}
