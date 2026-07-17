package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── real-git test fixtures ──────────────────────────────────────────────────
//
// These tests drive the REAL git binary against REAL local repositories. There
// is no mirrorStore test double: the defects under test (which refs a clone
// drags down, and what a killed clone leaves on disk) live entirely in git's
// behaviour, so a double that "answers whatever the code asks" would reproduce
// nothing. See the package doc for the trust boundary.

// gitCmd runs git with args in dir and fails the test on error.
func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// newUpstreamRepo builds a real local repository that mimics the shape of a
// popular forge repo: a couple of branches, a tag, and a large pile of
// forge-generated PR refs under refs/pull/* (github.com/octocat/Hello-World has
// 3289 of them against 3 branches — the US-7 headline case).
//
// It returns a path usable as a git remote URL.
func newUpstreamRepo(t *testing.T, pullRefs int) string {
	t.Helper()
	dir := t.TempDir()
	gitCmd(t, dir, "init", "--quiet", "--initial-branch=master", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README"), []byte("hello\n"), 0o644))
	gitCmd(t, dir, "add", "README")
	gitCmd(t, dir, "commit", "--quiet", "-m", "initial")
	gitCmd(t, dir, "tag", "v1.0.0")
	gitCmd(t, dir, "branch", "test")
	head := gitCmd(t, dir, "rev-parse", "HEAD")

	// Forge-generated PR refs. These are NOT part of what a direct `git clone`
	// of the upstream fetches; they exist only in the forge's ref namespace.
	for i := 1; i <= pullRefs; i++ {
		gitCmd(t, dir, "update-ref", "refs/pull/"+strconv.Itoa(i)+"/head", head)
	}
	return dir
}

// mirrorRefs returns the refnames present in the bare mirror at path.
func mirrorRefs(t *testing.T, path string) []string {
	t.Helper()
	cmd := exec.Command("git", "-C", path, "for-each-ref", "--format=%(refname)")
	out, err := cmd.Output()
	require.NoError(t, err)
	var refs []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			refs = append(refs, l)
		}
	}
	return refs
}

func testRef() repoRef {
	return repoRef{Host: "github.com", ProjectPath: "octocat/Hello-World", Tail: "/info/refs"}
}

// ─── BUG 1(a): the refspec ───────────────────────────────────────────────────

// TestEnsureSynced_DoesNotMirrorForgePullRefs is the RED test for the refspec
// defect: `git clone --mirror` uses fetch = +refs/*:refs/*, which drags down the
// forge's entire refs/pull/* namespace. On octocat/Hello-World that is 3289 refs
// against 3 branches, and the clone exceeds 120s even run by hand.
//
// A cache must mirror what a direct clone of the upstream would see: refs/heads/*
// and refs/tags/*. Nothing else.
func TestEnsureSynced_DoesNotMirrorForgePullRefs(t *testing.T) {
	up := newUpstreamRepo(t, 25)
	root := t.TempDir()
	m := newMirrorStore(root, time.Minute, time.Minute)

	_, err := m.EnsureSynced(context.Background(), testRef(), up)
	require.NoError(t, err)

	refs := mirrorRefs(t, m.mirrorPath(testRef()))

	var pull []string
	for _, r := range refs {
		if strings.HasPrefix(r, "refs/pull/") {
			pull = append(pull, r)
		}
	}
	assert.Emptyf(t, pull,
		"mirror dragged down %d refs/pull/* refs; a cache must mirror refs/heads/* + "+
			"refs/tags/* only — the forge's PR namespace is unbounded (3289 refs on "+
			"octocat/Hello-World) and is not part of what `git clone` of the upstream fetches",
		len(pull))

	assert.Contains(t, refs, "refs/heads/master", "branches must be mirrored")
	assert.Contains(t, refs, "refs/heads/test", "branches must be mirrored")
	assert.Contains(t, refs, "refs/tags/v1.0.0", "tags must be mirrored")
}

// TestEnsureSynced_FetchPrunesDeletedBranches pins the refresh path: a branch
// deleted upstream must disappear from the mirror, and the explicit refspec must
// still keep refs/heads and refs/tags up to date (a --bare clone leaves
// remote.origin.fetch unset, so a naive `git remote update` would silently
// refresh nothing).
func TestEnsureSynced_FetchPrunesDeletedBranches(t *testing.T) {
	up := newUpstreamRepo(t, 2)
	root := t.TempDir()
	m := newMirrorStore(root, 0, time.Minute) // staleAfter=0 → always refetch
	ref := testRef()

	_, err := m.EnsureSynced(context.Background(), ref, up)
	require.NoError(t, err)
	require.Contains(t, mirrorRefs(t, m.mirrorPath(ref)), "refs/heads/test")

	// Upstream: delete a branch, add a new one and a new tag.
	gitCmd(t, up, "branch", "-D", "test")
	gitCmd(t, up, "branch", "feature")
	gitCmd(t, up, "tag", "v2.0.0")

	_, err = m.EnsureSynced(context.Background(), ref, up)
	require.NoError(t, err)

	refs := mirrorRefs(t, m.mirrorPath(ref))
	assert.NotContains(t, refs, "refs/heads/test", "deleted branch must be pruned")
	assert.Contains(t, refs, "refs/heads/feature", "new branch must be fetched")
	assert.Contains(t, refs, "refs/tags/v2.0.0", "new tag must be fetched")
}

// ─── BUG 1(b): a killed sync must not leave a directory that looks synced ────

// makeCorpse reproduces, byte for byte, the on-disk state that a SIGKILLed
// `git clone` leaves behind — which is exactly what exec.CommandContext does to
// the clone when the request context is cancelled by a client hang-up.
//
// Verified against the real thing (kill -9 of an in-flight clone of
// octocat/Hello-World): the directory survives, `git rev-parse
// --is-bare-repository` answers *true*, and refs/heads is empty. That last fact
// is why "is it a git repo?" is not a usable definition of "is it a mirror?".
func makeCorpse(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(path, 0o755))
	gitCmd(t, t.TempDir(), "init", "--quiet", "--bare", path)
	// An interrupted pack transfer leaves its temp pack behind.
	require.NoError(t, os.MkdirAll(filepath.Join(path, "objects", "pack"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(path, "objects", "pack", "tmp_pack_ABCDEF"),
		make([]byte, 4096), 0o644))
}

// TestEnsureSynced_RecoversFromKilledCloneCorpse is the RED test for the
// permanent-corruption defect: EnsureSynced's "exists" check is os.Stat, so a
// corpse left by a killed clone takes the exists branch FOREVER — the mirror is
// never re-cloned, and every subsequent client is served an empty repository.
func TestEnsureSynced_RecoversFromKilledCloneCorpse(t *testing.T) {
	up := newUpstreamRepo(t, 2)
	root := t.TempDir()
	m := newMirrorStore(root, time.Minute, time.Minute)
	ref := testRef()

	// A previous request was killed mid-clone and left this behind.
	makeCorpse(t, m.mirrorPath(ref))
	require.Empty(t, mirrorRefs(t, m.mirrorPath(ref)), "corpse must start with no refs")

	_, err := m.EnsureSynced(context.Background(), ref, up)
	require.NoError(t, err)

	refs := mirrorRefs(t, m.mirrorPath(ref))
	assert.Contains(t, refs, "refs/heads/master",
		"a corpse from a killed clone must be re-cloned, not served forever: "+
			"os.Stat succeeding is not the same claim as 'this is a usable mirror'")

	assert.NoFileExists(t, filepath.Join(m.mirrorPath(ref), "objects", "pack", "tmp_pack_ABCDEF"),
		"the corpse's orphaned temp pack must be gone, not counted in stats forever")
}

// TestEnsureSynced_SurvivesRequestCancellation is the RED test for the lifetime
// defect: the clone is bound to the *request* context, so a client hang-up
// SIGKILLs an in-flight clone and corrupts shared cache state.
//
// An already-cancelled request context is the limit case of a client that has
// hung up. The bare mirror is shared infrastructure, not per-request scratch:
// its sync must run to completion (bounded by the upstream timeout) regardless.
func TestEnsureSynced_SurvivesRequestCancellation(t *testing.T) {
	up := newUpstreamRepo(t, 2)
	root := t.TempDir()
	m := newMirrorStore(root, time.Minute, time.Minute)
	ref := testRef()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client gave up before we got to the clone

	_, err := m.EnsureSynced(ctx, ref, up)
	require.NoError(t, err,
		"a client hang-up must not abort a clone into shared cache state")

	assert.Contains(t, mirrorRefs(t, m.mirrorPath(ref)), "refs/heads/master",
		"the mirror must be complete and usable after a cancelled request")
}

// TestEnsureSynced_FailedCloneLeavesNoDirectory pins the atomicity invariant
// from the other side: when a sync fails outright, it must leave nothing at the
// mirror path that a later EnsureSynced could mistake for a synced mirror.
func TestEnsureSynced_FailedCloneLeavesNoDirectory(t *testing.T) {
	root := t.TempDir()
	m := newMirrorStore(root, time.Minute, 30*time.Second)
	ref := testRef()

	_, err := m.EnsureSynced(context.Background(), ref,
		filepath.Join(t.TempDir(), "no-such-upstream-repo.git"))
	require.Error(t, err, "cloning a nonexistent upstream must fail")

	assert.NoDirExists(t, m.mirrorPath(ref),
		"a failed sync must not leave a directory at the mirror path")

	// And the failure must not be sticky: a later good sync recovers.
	up := newUpstreamRepo(t, 1)
	_, err = m.EnsureSynced(context.Background(), ref, up)
	require.NoError(t, err)
	assert.Contains(t, mirrorRefs(t, m.mirrorPath(ref)), "refs/heads/master")
}

// TestEnsureSynced_ConcurrentColdClonesAreAtomic drives the race the brief
// calls out: several clients hitting the same cold repo at once. None may see a
// half-built mirror, and the mirror path must never be observable in a state
// that is neither absent nor complete.
func TestEnsureSynced_ConcurrentColdClonesAreAtomic(t *testing.T) {
	up := newUpstreamRepo(t, 3)
	root := t.TempDir()
	m := newMirrorStore(root, time.Minute, time.Minute)
	ref := testRef()

	const clients = 8
	var wg sync.WaitGroup
	errs := make([]error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.EnsureSynced(context.Background(), ref, up)
		}(i)
	}

	// While the racers run, an outside observer (the stats du walk, the cache
	// browser, git http-backend) must never see a partial mirror at the path.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			if _, err := os.Stat(m.mirrorPath(ref)); err == nil {
				assert.True(t, isUsableMirror(m.mirrorPath(ref)),
					"mirror path became visible before the clone completed: "+
						"a concurrent reader must see either nothing or a usable mirror")
			}
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()
	<-done

	for i, err := range errs {
		assert.NoErrorf(t, err, "client %d", i)
	}
	assert.Contains(t, mirrorRefs(t, m.mirrorPath(ref)), "refs/heads/master")

	// No temp scratch may survive a completed sync.
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(m.mirrorPath(ref)), tempMirrorPrefix+"*"))
	assert.Empty(t, leftovers, "temp clone scratch must not leak")
}
