package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// ErrSessionNotFound is returned when an upload UUID is unknown or expired.
var ErrSessionNotFound = errors.New("registry: upload session not found")

// UploadSession is the server-side state of an in-progress chunked blob upload.
// The OCI push protocol opens a session (POST), streams chunks (PATCH), then
// finalises with a declared digest (PUT). Bytes accumulate in a temp file in the
// quarantine area; on finalise the handler verifies the declared digest against
// the streamed content and atomically promotes it into CAS (blob dedup by
// digest means an already-present blob is a no-op).
type UploadSession struct {
	ID        string    // upload UUID (the <uuid> path segment)
	Repo      string    // target repository "<org>/<repo>"
	Path      string    // on-disk chunk-accumulation file (quarantine area)
	Offset    int64     // bytes written so far (next Content-Range start)
	StartedAt time.Time // creation time (for idle-session expiry / GC)
}

// UploadSessionStore persists in-progress upload sessions. The in-memory
// implementation is single-node; HA (R4) will back this with shared storage +
// the distributed lock so a session survives being resumed on another replica.
type UploadSessionStore interface {
	// Create opens a new session for repoName, allocating its chunk file.
	Create(ctx context.Context, repoName string) (*UploadSession, error)
	// Get returns the session by id, or ErrSessionNotFound.
	Get(ctx context.Context, id string) (*UploadSession, error)
	// Append writes r at the current offset and returns the new total offset.
	Append(ctx context.Context, id string, r io.Reader) (int64, error)
	// Delete discards a session and removes its chunk file (no-op if absent).
	Delete(ctx context.Context, id string) error
}

// MemorySessions is the single-node in-memory UploadSessionStore. Chunk bytes
// live in temp files under Dir; the map only holds metadata. Safe for concurrent
// use. This is the R2 default; endpoint bodies that consume it are still stubs.
type MemorySessions struct {
	// Dir is the directory for chunk-accumulation temp files (default os.TempDir).
	Dir string

	mu       sync.Mutex
	sessions map[string]*UploadSession
}

// NewMemorySessions constructs an in-memory session store using the OS temp dir.
func NewMemorySessions() *MemorySessions {
	return &MemorySessions{sessions: make(map[string]*UploadSession)}
}

// Compile-time assertion.
var _ UploadSessionStore = (*MemorySessions)(nil)

// Create opens a session and its backing chunk file.
func (m *MemorySessions) Create(_ context.Context, repoName string) (*UploadSession, error) {
	f, err := os.CreateTemp(m.Dir, "specula-upload-*")
	if err != nil {
		return nil, fmt.Errorf("registry: create upload temp: %w", err)
	}
	path := f.Name()
	_ = f.Close()

	s := &UploadSession{
		ID:        newUploadID(),
		Repo:      repoName,
		Path:      path,
		Offset:    0,
		StartedAt: time.Now().UTC(),
	}
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	return s.clone(), nil
}

// Get returns a copy of the session by id.
func (m *MemorySessions) Get(_ context.Context, id string) (*UploadSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return s.clone(), nil
}

// Append writes r to the session's chunk file at the current offset and returns
// the new total offset. Appends are serialised per store; the implementer may
// refine this to per-session locking + Content-Range validation.
func (m *MemorySessions) Append(_ context.Context, id string, r io.Reader) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return 0, ErrSessionNotFound
	}
	f, err := os.OpenFile(s.Path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("registry: open upload chunk file: %w", err)
	}
	defer f.Close()
	n, err := io.Copy(f, r)
	if err != nil {
		return s.Offset, fmt.Errorf("registry: append upload chunk: %w", err)
	}
	s.Offset += n
	return s.Offset, nil
}

// Delete removes a session and its chunk file.
func (m *MemorySessions) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil
	}
	delete(m.sessions, id)
	_ = os.Remove(s.Path)
	return nil
}

// clone returns a value copy so callers cannot mutate stored state.
func (s *UploadSession) clone() *UploadSession {
	cp := *s
	return &cp
}

// newUploadID returns a random 16-byte hex upload UUID.
func newUploadID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
