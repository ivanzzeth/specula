// Package local provides a content-addressed BlobStore backed by the local
// filesystem. Blobs are stored under sharded directories (first 2 hex chars of
// the digest), written via atomic temp→rename so readers never see a partial
// file, and verified by streaming sha256 during Put — the blob is never
// buffered wholly in memory.
package local

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	// Blank imports register sha384 and sha512 with the crypto package so that
	// go-digest's Algorithm.Available() returns true for all three OCI-registered
	// digest algorithms (sha256 is linked by default; sha384/sha512 are not).
	_ "crypto/sha512"

	"github.com/ivanzzeth/specula/internal/store/blob"
	godigest "github.com/opencontainers/go-digest"
)

// Sentinel errors returned by LocalDiskDriver.
var (
	// ErrNotFound is returned by Get when the digest is not in the store.
	ErrNotFound = errors.New("local: blob not found")

	// ErrDigestMismatch is returned by Put when the bytes streamed from the
	// reader do not hash to the declared digest. The temp file is removed
	// before this error surfaces so no corrupt data lands in the store.
	ErrDigestMismatch = errors.New("local: digest mismatch")
)

// LocalDiskDriver stores blobs by content hash under Root using sharded
// directories (first 2 hex chars) and atomic temp→rename. CAS dedup is
// inherent: two calls with the same digest store the same physical file.
// Concurrent Puts of the same digest are safe: os.Rename on Linux is atomic
// and, since the CAS invariant guarantees identical bytes for the same digest,
// a concurrent rename that replaces an already-committed blob is harmless.
type LocalDiskDriver struct {
	Root string
}

// NewLocalDiskDriver constructs a LocalDiskDriver rooted at root.
func NewLocalDiskDriver(root string) *LocalDiskDriver {
	return &LocalDiskDriver{Root: root}
}

// Compile-time assertion that LocalDiskDriver satisfies blob.BlobStore.
var _ blob.BlobStore = (*LocalDiskDriver)(nil)

// hexDigest strips an optional "<alg>:" prefix and returns the raw hex string.
// It validates the hex string has at least 2 characters (required for shard dir).
func hexDigest(digest string) (string, error) {
	if idx := strings.IndexByte(digest, ':'); idx >= 0 {
		digest = digest[idx+1:]
	}
	if len(digest) < 2 {
		return "", fmt.Errorf("local: digest too short: %q", digest)
	}
	return digest, nil
}

// blobPath returns the canonical on-disk path for a digest inside Root.
func (d *LocalDiskDriver) blobPath(digest string) (string, error) {
	h, err := hexDigest(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(d.Root, h[:2], h), nil
}

// Get returns an io.ReadCloser for the range [offset, offset+length) of the
// blob, plus the full (unranged) object size. Pass length < 0 to read to
// end-of-blob. The returned reader must be closed by the caller.
func (d *LocalDiskDriver) Get(ctx context.Context, digest string, offset, length int64) (io.ReadCloser, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	path, err := d.blobPath(digest)
	if err != nil {
		return nil, 0, err
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, fmt.Errorf("%w: %s", ErrNotFound, digest)
		}
		return nil, 0, fmt.Errorf("local: open blob %s: %w", digest, err)
	}

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("local: stat blob %s: %w", digest, err)
	}
	fullSize := fi.Size()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return nil, 0, fmt.Errorf("local: seek blob %s: %w", digest, err)
		}
	}

	if length < 0 {
		return f, fullSize, nil
	}
	return &limitedReadCloser{f: f, rem: length}, fullSize, nil
}

// Put stores the bytes from r under digest. It is idempotent: if a blob with
// the same digest already exists the call returns immediately without reading r.
// The streaming sha256 of the written bytes must match digest; on mismatch the
// temp file is removed and ErrDigestMismatch is returned.
func (d *LocalDiskDriver) Put(ctx context.Context, digest string, r io.Reader, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	path, err := d.blobPath(digest)
	if err != nil {
		return err
	}

	// Idempotent fast-path: blob already stored.
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	shardDir := filepath.Dir(path)
	if err := os.MkdirAll(shardDir, 0o755); err != nil {
		return fmt.Errorf("local: mkdir shard %s: %w", shardDir, err)
	}

	// Temp file lives in the same shard dir so rename is always same-device.
	tmp, err := os.CreateTemp(shardDir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("local: create temp: %w", err)
	}
	tmpName := tmp.Name()

	// Track whether we closed the file explicitly; the defer avoids a double-close.
	tmpClosed := false
	committed := false
	defer func() {
		if !tmpClosed {
			tmp.Close()
		}
		if !committed {
			os.Remove(tmpName)
		}
	}()

	// Stream: hash and write simultaneously — never holds entire blob in memory.
	// The hash algorithm is selected from the digest's own algorithm prefix so
	// the CAS is digest-algorithm agnostic (sha256 / sha384 / sha512).
	dgst := godigest.Digest(digest)
	if err := dgst.Validate(); err != nil {
		return fmt.Errorf("local: invalid digest %q: %w", digest, err)
	}
	h := dgst.Algorithm().Hash()
	mw := io.MultiWriter(tmp, h)
	written, err := io.Copy(mw, r)
	if err != nil {
		return fmt.Errorf("local: copy blob: %w", err)
	}
	if size >= 0 && written != size {
		return fmt.Errorf("local: size mismatch: expected %d got %d", size, written)
	}

	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("local: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("local: close temp: %w", err)
	}
	tmpClosed = true

	// Verify the digest of what we actually received.
	actualHex := hex.EncodeToString(h.Sum(nil))
	expectedHex, _ := hexDigest(digest) // already validated; ignore secondary error
	if actualHex != expectedHex {
		return fmt.Errorf("%w: expected %s got %s", ErrDigestMismatch, expectedHex, actualHex)
	}

	// Atomic promotion: on Linux os.Rename is atomic and replaces the
	// destination. If a concurrent writer raced us here, the CAS invariant
	// (same digest → same bytes) means the replacement is safe.
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("local: rename blob: %w", err)
	}
	committed = true
	return nil
}

// Exists reports whether a blob with the given digest is present.
func (d *LocalDiskDriver) Exists(ctx context.Context, digest string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	path, err := d.blobPath(digest)
	if err != nil {
		return false, err
	}

	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("local: stat %s: %w", digest, err)
}

// Delete removes the blob. It is a no-op if the blob does not exist.
func (d *LocalDiskDriver) Delete(ctx context.Context, digest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	path, err := d.blobPath(digest)
	if err != nil {
		return err
	}

	err = os.Remove(path)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return fmt.Errorf("local: remove %s: %w", digest, err)
}

// UsageBytes returns the total bytes of all blobs stored under Root.
// It walks the shard tree and counts each unique inode once so that any
// external hardlinks are not double-counted.
func (d *LocalDiskDriver) UsageBytes(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	if _, err := os.Stat(d.Root); os.IsNotExist(err) {
		return 0, nil
	}

	var total int64
	seen := make(map[uint64]struct{})

	err := filepath.WalkDir(d.Root, func(path string, de fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil // entry removed while walking
			}
			return walkErr
		}
		if de.IsDir() {
			return ctx.Err() // propagate cancellation between dirs
		}

		info, err := de.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil // removed just after listing
			}
			return err
		}

		// Dedup by inode so external hardlinks are counted once (Unix only).
		if ino, ok := fileIno(info); ok {
			if _, already := seen[ino]; already {
				return nil
			}
			seen[ino] = struct{}{}
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("local: usage walk: %w", err)
	}
	return total, nil
}

// limitedReadCloser wraps *os.File and enforces a byte cap, closing the
// underlying file on Close. It is the analog of io.LimitedReader but
// implements io.ReadCloser.
type limitedReadCloser struct {
	f   *os.File
	rem int64
}

func (l *limitedReadCloser) Read(p []byte) (int, error) {
	if l.rem <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > l.rem {
		p = p[:l.rem]
	}
	n, err := l.f.Read(p)
	l.rem -= int64(n)
	return n, err
}

func (l *limitedReadCloser) Close() error {
	return l.f.Close()
}
