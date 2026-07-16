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

// mirrorStore manages on-disk bare git mirrors.
type mirrorStore struct {
	root       string
	staleAfter time.Duration

	// mu guards lastSync and syncMu maps (metadata-level lock, not path-level).
	mu       sync.Mutex
	lastSync map[string]time.Time
	syncMu   map[string]*sync.Mutex
}

func newMirrorStore(root string, staleAfter time.Duration) *mirrorStore {
	return &mirrorStore{
		root:       root,
		staleAfter: staleAfter,
		lastSync:   map[string]time.Time{},
		syncMu:     map[string]*sync.Mutex{},
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

// EnsureSynced creates or refreshes the bare mirror for ref, using
// upstreamURL as the remote source. Concurrent callers for the same
// mirror path block on a per-path mutex; only one clone/fetch runs.
//
// If the mirror exists and was last synced within staleAfter, the call
// returns immediately (cache hit).
func (m *mirrorStore) EnsureSynced(ctx context.Context, ref repoRef, upstreamURL string) error {
	key := ref.mirrorRelPath()
	l := m.lockForKey(key)
	l.Lock()
	defer l.Unlock()

	path := m.mirrorPath(ref)
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		m.mu.Lock()
		last := m.lastSync[key]
		m.mu.Unlock()
		if time.Since(last) < m.staleAfter {
			return nil // fresh mirror — serve from disk
		}
		return m.fetch(ctx, path, key)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("git mirror: stat %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("git mirror: mkdirall %s: %w", filepath.Dir(path), err)
	}
	return m.clone(ctx, upstreamURL, path, key)
}

// clone runs `git clone --mirror` to create a new bare mirror.
func (m *mirrorStore) clone(ctx context.Context, src, path, key string) error {
	cmd := exec.CommandContext(ctx, "git", "clone", "--mirror", "--", src, path)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone --mirror: %w: %s", err, trimGitOutput(out))
	}
	m.mu.Lock()
	m.lastSync[key] = time.Now()
	m.mu.Unlock()
	return nil
}

// fetch runs `git remote update --prune` to refresh an existing mirror.
func (m *mirrorStore) fetch(ctx context.Context, path, key string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "remote", "update", "--prune")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git remote update: %w: %s", err, trimGitOutput(out))
	}
	m.mu.Lock()
	m.lastSync[key] = time.Now()
	m.mu.Unlock()
	return nil
}

// trimGitOutput caps the output snippet included in error messages.
func trimGitOutput(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 512 {
		return s[:512] + "…"
	}
	return s
}
