package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/blob"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// SharedSessions is the multi-replica UploadSessionStore.
//
// Pushing a blob is a stateful multi-request protocol: POST opens a session,
// PATCH streams chunks, PUT finalises. MemorySessions keeps that state in one
// process's map and its bytes in that process's temp dir, so behind a Service or
// Ingress with replicas > 1 the PATCH/PUT lands on a replica that never saw the
// POST and the push dies with BLOB_UPLOAD_UNKNOWN. Scaling to one replica "fixed"
// it; cookie affinity does not, because crane and docker do not carry an
// Ingress's affinity cookie.
//
// So neither half of the session may be process-local:
//
//   - Metadata (repo, offset, chunk list) goes in the mutable metadata tier —
//     already shared, already implemented by both the sqlite and postgres
//     drivers, and in HA that is postgres.
//   - Chunk BYTES go in the blob store under a staging namespace, so the replica
//     that finalises can read chunks a different replica received. In HA the blob
//     store is S3, which every replica can read.
//
// Chunks are staged CONTENT-ADDRESSED, under the digest of the chunk's own bytes,
// because the blob store is a strict CAS: Put hashes what it receives and refuses
// a key that does not match. An opaque staging namespace is therefore not
// available, which has one consequence worth stating plainly — a monolithic
// upload's single chunk has the same digest as the finished blob, so the staged
// object and the promoted object are the SAME object. That is why finishing a
// session goes through Complete(id, promotedDigest) and not Delete: cleaning up
// via Delete would evict the blob that was just pushed.
//
// Not supported, by protocol rather than by omission: concurrent PATCHes to the
// SAME upload session. OCI pushes chunks sequentially (each reply carries the
// Range to continue from), and the handler rejects a non-contiguous chunk with
// 416. Two racing chunk writes to one session would read-modify-write the same
// metadata row; the loser's chunk would be dropped rather than silently
// misordered, and the client's next contiguity check would catch it.
type SharedSessions struct {
	meta  meta.MetadataStore
	blobs blob.BlobStore

	// dir is local scratch used only WITHIN one request: a chunk is spooled here
	// to learn its length (a chunked-transfer PATCH has no Content-Length, and
	// BlobStore.Put needs a size) and then moved into the staging namespace. It is
	// never the place a later request reads from.
	dir string

	// idle is how long a session may go untouched before it is treated as absent.
	idle time.Duration

	log *slog.Logger
}

// Compile-time assertion.
var _ UploadSessionStore = (*SharedSessions)(nil)

// DefaultUploadIdleTimeout is how long an untouched upload session survives.
// Long enough for a slow client pushing a large layer over a bad link, short
// enough that abandoned sessions do not pin staged chunks forever.
const DefaultUploadIdleTimeout = 6 * time.Hour

// NewSharedSessions builds a session store backed by the shared metadata tier
// and blob store. dir is local scratch for the duration of a single request.
func NewSharedSessions(m meta.MetadataStore, b blob.BlobStore, dir string, log *slog.Logger) *SharedSessions {
	if log == nil {
		log = slog.Default()
	}
	return &SharedSessions{meta: m, blobs: b, dir: dir, idle: DefaultUploadIdleTimeout, log: log}
}

// uploadStateKey namespaces upload sessions inside the mutable tier.
func uploadStateKey(id string) string { return "oci-upload:" + id }

// sharedState is the JSON payload stored in the mutable tier.
type sharedState struct {
	Repo      string        `json:"repo"`
	Offset    int64         `json:"offset"`
	StartedAt time.Time     `json:"started_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Chunks    []sharedChunk `json:"chunks"`
}

type sharedChunk struct {
	// Key is the CAS digest the chunk's bytes are stored under.
	Key  string `json:"key"`
	Size int64  `json:"size"`
	// PreExisted records that this digest was already in the blob store before
	// the upload staged it — identical bytes were already cached or pushed. Such
	// an object is not ours to remove: cleaning up the session must not evict a
	// blob another request depends on.
	PreExisted bool `json:"pre_existed,omitempty"`
}

// Create opens a session. Only metadata is written: a session with no chunks yet
// is exactly a zero-offset upload, which is what POST /blobs/uploads/ means.
func (s *SharedSessions) Create(ctx context.Context, repoName string) (*UploadSession, error) {
	id := newUploadID()
	now := time.Now().UTC()
	st := &sharedState{Repo: repoName, StartedAt: now, UpdatedAt: now}
	if err := s.save(ctx, id, st); err != nil {
		return nil, err
	}
	return &UploadSession{ID: id, Repo: repoName, Offset: 0, StartedAt: now}, nil
}

// Get returns the session, or ErrSessionNotFound when it is unknown or idle past
// the timeout. Path is deliberately empty: with a shared store the bytes are not
// at a filesystem path any single replica can open, so a caller must go through
// Open rather than reach for sess.Path.
func (s *SharedSessions) Get(ctx context.Context, id string) (*UploadSession, error) {
	st, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	return &UploadSession{
		ID:        id,
		Repo:      st.Repo,
		Offset:    st.Offset,
		StartedAt: st.StartedAt,
	}, nil
}

// Append stages r as the session's next chunk and returns the new total offset.
//
// A zero-byte body is a no-op that does not create a staging object: the closing
// PUT of a chunked upload has an empty body, and an empty staged chunk would be
// one more object to read back and delete for no content.
func (s *SharedSessions) Append(ctx context.Context, id string, r io.Reader) (int64, error) {
	st, err := s.load(ctx, id)
	if err != nil {
		return 0, err
	}

	// Spool locally to learn the length: PATCH may arrive chunked with no
	// Content-Length, and BlobStore.Put needs a size.
	tmp, err := os.CreateTemp(s.dir, "specula-upload-chunk-*")
	if err != nil {
		return st.Offset, fmt.Errorf("registry: create chunk spool: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	// Hash while spooling: the CAS key IS the content digest, and one pass over
	// the body gives both the length Put needs and that digest.
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	if err != nil {
		return st.Offset, fmt.Errorf("registry: spool chunk: %w", err)
	}
	if n == 0 {
		return st.Offset, nil
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return st.Offset, fmt.Errorf("registry: rewind chunk spool: %w", err)
	}
	key := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	// Whether the object is already there decides whether cleanup may remove it.
	// Checked before Put, since Put on an existing digest is an idempotent no-op
	// and would leave the two cases indistinguishable.
	preExisted, existsErr := s.blobs.Exists(ctx, key)
	if existsErr != nil {
		// Unknown provenance: treat it as not ours, so a cleanup can only ever
		// under-delete. Losing a staged chunk is worse than keeping one.
		s.log.Warn("registry: staged chunk exists check", "key", key, "err", existsErr)
		preExisted = true
	}

	if err := s.blobs.Put(ctx, key, tmp, n); err != nil {
		return st.Offset, fmt.Errorf("registry: stage chunk %s: %w", key, err)
	}

	// Metadata last: a staged chunk no session references is unreferenced CAS
	// content, whereas a referenced chunk that was never stored would make the
	// upload unreadable and surface at PUT, far from the cause.
	st.Chunks = append(st.Chunks, sharedChunk{Key: key, Size: n, PreExisted: preExisted})
	st.Offset += n
	st.UpdatedAt = time.Now().UTC()
	if err := s.save(ctx, id, st); err != nil {
		return st.Offset - n, err
	}
	return st.Offset, nil
}

// Open returns the session's accumulated bytes in write order, concatenated
// across however many replicas received them.
func (s *SharedSessions) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	st, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(st.Chunks))
	for _, c := range st.Chunks {
		keys = append(keys, c.Key)
	}
	return &chunkReader{ctx: ctx, blobs: s.blobs, keys: keys}, nil
}

// Delete drops an abandoned session and the chunks it staged.
func (s *SharedSessions) Delete(ctx context.Context, id string) error {
	return s.discard(ctx, id, "")
}

// Complete drops a finished session, keeping the object that became the blob.
func (s *SharedSessions) Complete(ctx context.Context, id, promotedDigest string) error {
	return s.discard(ctx, id, promotedDigest)
}

// discard removes the session row and the staged chunks it is safe to remove:
// not the promoted blob, and not an object that was already in the store before
// this upload staged it.
func (s *SharedSessions) discard(ctx context.Context, id, keepDigest string) error {
	st, err := s.load(ctx, id)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return nil
		}
		return err
	}
	for _, c := range st.Chunks {
		if c.Key == keepDigest || c.PreExisted {
			continue
		}
		if derr := s.blobs.Delete(ctx, c.Key); derr != nil {
			// Best effort: the session must still go away, or a client retrying
			// the same UUID would resume an upload we already refused.
			s.log.Warn("registry: delete staged upload chunk", "key", c.Key, "err", derr)
		}
	}
	if err := s.meta.DeleteMutable(ctx, uploadStateKey(id)); err != nil {
		return fmt.Errorf("registry: delete upload session %s: %w", id, err)
	}
	return nil
}

func (s *SharedSessions) save(ctx context.Context, id string, st *sharedState) error {
	payload, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("registry: encode upload session: %w", err)
	}
	err = s.meta.PutMutable(ctx, artifact.MutableEntry{
		Key:        uploadStateKey(id),
		Protocol:   "oci",
		Payload:    payload,
		TTLSeconds: int64(s.idle.Seconds()),
		FetchedAt:  time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("registry: save upload session %s: %w", id, err)
	}
	return nil
}

func (s *SharedSessions) load(ctx context.Context, id string) (*sharedState, error) {
	e, err := s.meta.GetMutable(ctx, uploadStateKey(id))
	if err != nil {
		return nil, fmt.Errorf("registry: read upload session %s: %w", id, err)
	}
	if e == nil {
		return nil, ErrSessionNotFound
	}
	var st sharedState
	if err := json.Unmarshal(e.Payload, &st); err != nil {
		// A row we cannot decode is not a session anyone can continue.
		s.log.Warn("registry: corrupt upload session row", "id", id, "err", err)
		return nil, ErrSessionNotFound
	}
	// The mutable tier stores TTL but does not enforce it on read, so idle expiry
	// is enforced here rather than assumed.
	if s.idle > 0 && !st.UpdatedAt.IsZero() && time.Since(st.UpdatedAt) > s.idle {
		return nil, ErrSessionNotFound
	}
	return &st, nil
}

// chunkReader concatenates staged chunks, opening each only when the previous one
// is exhausted so finalising a large blob does not hold every chunk open at once.
type chunkReader struct {
	ctx   context.Context
	blobs blob.BlobStore
	keys  []string

	cur io.ReadCloser
	i   int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	for {
		if c.cur == nil {
			if c.i >= len(c.keys) {
				return 0, io.EOF
			}
			rc, _, err := c.blobs.Get(c.ctx, c.keys[c.i], 0, -1)
			if err != nil {
				return 0, fmt.Errorf("registry: read staged chunk %s: %w", c.keys[c.i], err)
			}
			c.cur = rc
			c.i++
		}
		n, err := c.cur.Read(p)
		if n > 0 {
			// Report bytes now; a chunk boundary is not the caller's business.
			if errors.Is(err, io.EOF) {
				_ = c.cur.Close()
				c.cur = nil
				return n, nil
			}
			return n, err
		}
		if errors.Is(err, io.EOF) {
			_ = c.cur.Close()
			c.cur = nil
			continue
		}
		if err != nil {
			return 0, err
		}
	}
}

func (c *chunkReader) Close() error {
	if c.cur != nil {
		err := c.cur.Close()
		c.cur = nil
		return err
	}
	return nil
}
