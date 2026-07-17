package local_test

// local_errors_test.go covers the error branches that cannot be reached through
// the happy-path tests in local_test.go:
//
//   - context cancellation before any I/O (ctx.Err() early return in every method)
//   - too-short digest hex part (hexDigest len < 2 guard)
//   - invalid digest format that passes hexDigest but fails godigest.Validate
//   - os.Open / os.Stat / os.Remove / os.MkdirAll failure paths (EACCES)
//   - filepath.WalkDir walk error (shard dir inaccessible)
//   - inode deduplication in UsageBytes (hardlink counted once)
//
// Permission-dependent tests are skipped when running as root.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ivanzzeth/specula/internal/store/local"
)

// skipIfRoot marks a test as skipped when the process runs as root; permission
// errors (EACCES) do not apply to root and the test would give false results.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("skipping permission test: process runs as root")
	}
}

// ── Context cancellation ──────────────────────────────────────────────────────

func TestGet_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := newDriver(t)
	_, _, err := d.Get(ctx, blobDigest([]byte("irrelevant")), 0, -1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Get cancelled ctx: want context.Canceled, got %v", err)
	}
}

func TestPut_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := []byte("will not be written")
	d := newDriver(t)
	err := d.Put(ctx, blobDigest(data), bytes.NewReader(data), int64(len(data)))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Put cancelled ctx: want context.Canceled, got %v", err)
	}
}

func TestExists_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := newDriver(t)
	_, err := d.Exists(ctx, blobDigest([]byte("x")))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Exists cancelled ctx: want context.Canceled, got %v", err)
	}
}

func TestDelete_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := newDriver(t)
	err := d.Delete(ctx, blobDigest([]byte("x")))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete cancelled ctx: want context.Canceled, got %v", err)
	}
}

func TestUsageBytes_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := newDriver(t)
	_, err := d.UsageBytes(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("UsageBytes cancelled ctx: want context.Canceled, got %v", err)
	}
}

// ── Short digest (hexDigest len < 2 guard) ────────────────────────────────────

// TestGet_DigestTooShort verifies that a digest whose hex part is only 1
// character long is rejected before any filesystem I/O.
// "sha256:a" → stripped hex = "a" → len(1) < 2 → error.
func TestGet_DigestTooShort(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	_, _, err := d.Get(ctx, "sha256:a", 0, -1)
	if err == nil {
		t.Fatal("Get with 1-char hex: want error, got nil")
	}
	if errors.Is(err, local.ErrNotFound) {
		t.Errorf("short-digest error must not look like ErrNotFound: %v", err)
	}
}

func TestExists_DigestTooShort(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	_, err := d.Exists(ctx, "sha256:a")
	if err == nil {
		t.Fatal("Exists with 1-char hex: want error, got nil")
	}
}

func TestDelete_DigestTooShort(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	err := d.Delete(ctx, "sha256:a")
	if err == nil {
		t.Fatal("Delete with 1-char hex: want error, got nil")
	}
}

// ── Invalid digest format (passes hexDigest but fails godigest.Validate) ──────

// TestPut_InvalidDigestFormat verifies the "local: invalid digest" error path.
// "sha256:ab" passes hexDigest (hex part has 2 chars ≥ 2) but fails
// godigest.Validate because sha256 requires exactly 64 hex characters.
// ARCHITECTURE §6 / DESIGN-REVIEW C3: the declared digest must be a valid
// algorithm:hex pair before any blob data is accepted.
func TestPut_InvalidDigestFormat(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	data := []byte("some test bytes")
	err := d.Put(ctx, "sha256:ab", bytes.NewReader(data), int64(len(data)))
	if err == nil {
		t.Fatal("Put with invalid digest format: want error, got nil")
	}
	// Must not be ErrDigestMismatch — the digest was structurally invalid, not a
	// hash collision.
	if errors.Is(err, local.ErrDigestMismatch) {
		t.Errorf("invalid-format digest must not return ErrDigestMismatch: %v", err)
	}

	// The temp file cleanup must have run: no blob at the (malformed) path.
	exists, existsErr := d.Exists(ctx, "sha256:ab")
	if existsErr != nil || exists {
		t.Errorf("blob must not persist after invalid-format Put: exists=%v err=%v", exists, existsErr)
	}
}

// ── Permission-based error paths (skipped as root) ───────────────────────────

// TestGet_OpenError covers the "local: open blob" path: a non-IsNotExist
// failure from os.Open when the shard directory is inaccessible (mode 0o000).
func TestGet_OpenError(t *testing.T) {
	skipIfRoot(t)
	ctx := context.Background()
	root := t.TempDir()
	d := local.NewLocalDiskDriver(root)

	data := []byte("get open error blob")
	dgst := blobDigest(data)
	if err := d.Put(ctx, dgst, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	hexPart := dgst[len("sha256:"):]
	shardDir := filepath.Join(root, hexPart[:2])
	if err := os.Chmod(shardDir, 0o000); err != nil {
		t.Fatalf("chmod shard dir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(shardDir, 0o755) }) //nolint:errcheck

	_, _, err := d.Get(ctx, dgst, 0, -1)
	if err == nil {
		t.Fatal("Get into 0o000 shard dir: want error, got nil")
	}
	if errors.Is(err, local.ErrNotFound) {
		t.Errorf("EACCES on open must not surface as ErrNotFound: %v", err)
	}
}

// TestPut_MkdirError covers the "local: mkdir shard" path: os.MkdirAll fails
// when the store root is not writable (mode 0o555).
func TestPut_MkdirError(t *testing.T) {
	skipIfRoot(t)
	ctx := context.Background()
	root := t.TempDir()
	d := local.NewLocalDiskDriver(root)

	// No blobs stored yet; make root non-writable so MkdirAll cannot create a
	// shard directory.
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	t.Cleanup(func() { os.Chmod(root, 0o755) }) //nolint:errcheck

	data := []byte("mkdir error test data")
	dgst := blobDigest(data)
	err := d.Put(ctx, dgst, bytes.NewReader(data), int64(len(data)))
	if err == nil {
		t.Fatal("Put into non-writable root: want error, got nil")
	}
	if errors.Is(err, local.ErrDigestMismatch) || errors.Is(err, local.ErrNotFound) {
		t.Errorf("mkdir error must not look like a CAS sentinel: %v", err)
	}
}

// TestExists_StatError covers the "local: stat <digest>" path: os.Stat fails
// with EACCES (not ENOENT) when the shard directory is inaccessible (mode 0o000).
func TestExists_StatError(t *testing.T) {
	skipIfRoot(t)
	ctx := context.Background()
	root := t.TempDir()
	d := local.NewLocalDiskDriver(root)

	data := []byte("exists stat error blob")
	dgst := blobDigest(data)
	if err := d.Put(ctx, dgst, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	hexPart := dgst[len("sha256:"):]
	shardDir := filepath.Join(root, hexPart[:2])
	if err := os.Chmod(shardDir, 0o000); err != nil {
		t.Fatalf("chmod shard dir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(shardDir, 0o755) }) //nolint:errcheck

	exists, err := d.Exists(ctx, dgst)
	if err == nil {
		t.Fatalf("Exists on 0o000 shard dir: want error, got exists=%v err=nil", exists)
	}
	if exists {
		t.Errorf("Exists must return false on error, got true")
	}
}

// TestDelete_RemoveError covers the "local: remove <digest>" path: os.Remove
// fails with EACCES when the shard directory is read-only (mode 0o555, no write).
func TestDelete_RemoveError(t *testing.T) {
	skipIfRoot(t)
	ctx := context.Background()
	root := t.TempDir()
	d := local.NewLocalDiskDriver(root)

	data := []byte("delete remove error blob")
	dgst := blobDigest(data)
	if err := d.Put(ctx, dgst, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	hexPart := dgst[len("sha256:"):]
	shardDir := filepath.Join(root, hexPart[:2])
	// Remove write permission: unlink requires write on the parent directory.
	if err := os.Chmod(shardDir, 0o555); err != nil {
		t.Fatalf("chmod shard dir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(shardDir, 0o755) }) //nolint:errcheck

	err := d.Delete(ctx, dgst)
	if err == nil {
		t.Fatal("Delete in read-only shard dir: want error, got nil")
	}
}

// TestUsageBytes_WalkError verifies that a non-IsNotExist error from WalkDir
// surfaces rather than being silently swallowed.
// ARCHITECTURE §6: disk/permission errors must surface, not be swallowed.
func TestUsageBytes_WalkError(t *testing.T) {
	skipIfRoot(t)
	ctx := context.Background()
	root := t.TempDir()
	d := local.NewLocalDiskDriver(root)

	data := []byte("walk error test blob")
	dgst := blobDigest(data)
	if err := d.Put(ctx, dgst, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	hexPart := dgst[len("sha256:"):]
	shardDir := filepath.Join(root, hexPart[:2])
	// Make the shard directory completely inaccessible: WalkDir receives EACCES
	// when it tries to read its contents (it can still stat the dir from the
	// parent, but cannot open it to list entries).
	if err := os.Chmod(shardDir, 0o000); err != nil {
		t.Fatalf("chmod shard dir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(shardDir, 0o755) }) //nolint:errcheck

	_, err := d.UsageBytes(ctx)
	if err == nil {
		t.Fatal("UsageBytes with inaccessible shard dir: want error, got nil")
	}
}

// ── Inode deduplication ───────────────────────────────────────────────────────

// TestUsageBytes_HardlinkDedup verifies that two directory entries sharing the
// same inode (created via os.Link) are counted only once in UsageBytes.
// This is the CAS dedup contract: physical deduplication must not inflate usage.
func TestUsageBytes_HardlinkDedup(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	d := local.NewLocalDiskDriver(root)

	data := []byte("dedup test blob content")
	dgst := blobDigest(data)
	if err := d.Put(ctx, dgst, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	hexPart := dgst[len("sha256:"):]
	blobPath := filepath.Join(root, hexPart[:2], hexPart)
	linkPath := filepath.Join(root, "hardlink-dup")

	if err := os.Link(blobPath, linkPath); err != nil {
		t.Fatalf("os.Link (create hardlink): %v", err)
	}

	usage, err := d.UsageBytes(ctx)
	if err != nil {
		t.Fatalf("UsageBytes: %v", err)
	}
	// Two directory entries, one inode: usage must equal the blob size exactly
	// once, not twice.
	if usage != int64(len(data)) {
		t.Errorf("UsageBytes = %d; want %d (each unique inode counted once)", usage, len(data))
	}
}
