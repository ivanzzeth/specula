package local_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ivanzzeth/specula/internal/store/local"
)

// blobDigest computes the sha256 of data and returns the "sha256:<hex>" form.
func blobDigest(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// newDriver builds a LocalDiskDriver under a t.TempDir() that is cleaned up
// automatically.
func newDriver(t *testing.T) *local.LocalDiskDriver {
	t.Helper()
	return local.NewLocalDiskDriver(t.TempDir())
}

// ---- basic round-trip -------------------------------------------------------

func TestPutGet_RoundTrip(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	data := []byte("hello, specula CAS store")
	dgst := blobDigest(data)

	if err := d.Put(ctx, dgst, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, size, err := d.Get(ctx, dgst, 0, -1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()

	if size != int64(len(data)) {
		t.Errorf("full size: got %d want %d", size, len(data))
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content mismatch: got %q want %q", got, data)
	}
}

func TestPut_Idempotent(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	data := []byte("idempotent blob content")
	dgst := blobDigest(data)

	for i := 0; i < 3; i++ {
		if err := d.Put(ctx, dgst, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatalf("Put attempt %d: %v", i, err)
		}
	}

	exists, err := d.Exists(ctx, dgst)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("blob must exist after repeated puts")
	}
}

// ---- error paths ------------------------------------------------------------

func TestPut_DigestMismatch(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	data := []byte("real bytes")
	wrongDigest := "sha256:" + hex.EncodeToString(make([]byte, 32)) // all-zero hash

	err := d.Put(ctx, wrongDigest, bytes.NewReader(data), int64(len(data)))
	if err == nil {
		t.Fatal("expected ErrDigestMismatch, got nil")
	}
	if !errors.Is(err, local.ErrDigestMismatch) {
		t.Errorf("expected ErrDigestMismatch, got %v", err)
	}

	// Temp file must be cleaned up; the wrongly-digested path must not exist.
	exists, err := d.Exists(ctx, wrongDigest)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("corrupted blob must not be stored after digest mismatch")
	}
}

func TestGet_NotFound(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	none := "sha256:" + hex.EncodeToString(make([]byte, 32))
	_, _, err := d.Get(ctx, none, 0, -1)
	if err == nil {
		t.Fatal("expected ErrNotFound, got nil")
	}
	if !errors.Is(err, local.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestPut_SizeMismatch(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	data := []byte("some bytes here")
	dgst := blobDigest(data)

	// Declare the wrong (larger) size; io.Copy will read only len(data) bytes.
	err := d.Put(ctx, dgst, bytes.NewReader(data), int64(len(data))+100)
	if err == nil {
		t.Fatal("expected size-mismatch error, got nil")
	}
}

// ---- Range reads ------------------------------------------------------------

func TestGet_RangeRead(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	data := []byte("0123456789abcdef") // 16 bytes
	dgst := blobDigest(data)
	if err := d.Put(ctx, dgst, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	tests := []struct {
		name   string
		offset int64
		length int64
		want   []byte
	}{
		{"full (length -1)", 0, -1, data},
		{"offset only", 4, -1, data[4:]},
		{"offset + length", 4, 6, data[4:10]},
		{"first 5", 0, 5, data[:5]},
		{"single byte mid", 7, 1, data[7:8]},
		{"last byte", 15, 1, data[15:16]},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc, fullSize, err := d.Get(ctx, dgst, tc.offset, tc.length)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer rc.Close()

			if fullSize != int64(len(data)) {
				t.Errorf("fullSize: got %d want %d", fullSize, len(data))
			}
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("content: got %q want %q", got, tc.want)
			}
		})
	}
}

// ---- Exists / Delete --------------------------------------------------------

func TestExists(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	data := []byte("exists check")
	dgst := blobDigest(data)

	exists, err := d.Exists(ctx, dgst)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("should not exist before put")
	}

	if err := d.Put(ctx, dgst, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	exists, err = d.Exists(ctx, dgst)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("should exist after put")
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	data := []byte("blob to delete")
	dgst := blobDigest(data)

	// Delete of non-existent blob is a no-op.
	if err := d.Delete(ctx, dgst); err != nil {
		t.Fatalf("Delete non-existent: %v", err)
	}

	if err := d.Put(ctx, dgst, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := d.Delete(ctx, dgst); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	exists, err := d.Exists(ctx, dgst)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("blob must not exist after delete")
	}

	// Subsequent Delete is also a no-op.
	if err := d.Delete(ctx, dgst); err != nil {
		t.Fatalf("second Delete: %v", err)
	}

	// Get after delete must return ErrNotFound.
	_, _, err = d.Get(ctx, dgst, 0, -1)
	if !errors.Is(err, local.ErrNotFound) {
		t.Errorf("Get after delete: expected ErrNotFound, got %v", err)
	}
}

// ---- UsageBytes -------------------------------------------------------------

func TestUsageBytes_Empty(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	usage, err := d.UsageBytes(ctx)
	if err != nil {
		t.Fatalf("UsageBytes (empty): %v", err)
	}
	if usage != 0 {
		t.Errorf("expected 0, got %d", usage)
	}
}

func TestUsageBytes_NonExistentRoot(t *testing.T) {
	// Root directory that was never created must return 0, not an error.
	d := local.NewLocalDiskDriver(filepath.Join(t.TempDir(), "does-not-exist"))
	ctx := context.Background()

	usage, err := d.UsageBytes(ctx)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if usage != 0 {
		t.Errorf("expected 0, got %d", usage)
	}
}

func TestUsageBytes_TracksBlobs(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	blobs := [][]byte{
		[]byte("first blob"),
		[]byte("second blob, a bit longer"),
		[]byte("third blob, the longest of the three"),
	}
	var want int64
	for _, b := range blobs {
		want += int64(len(b))
		dgst := blobDigest(b)
		if err := d.Put(ctx, dgst, bytes.NewReader(b), int64(len(b))); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	usage, err := d.UsageBytes(ctx)
	if err != nil {
		t.Fatalf("UsageBytes: %v", err)
	}
	if usage != want {
		t.Errorf("usage: got %d want %d", usage, want)
	}

	// Delete one blob; usage must drop.
	del := blobs[1]
	if err := d.Delete(ctx, blobDigest(del)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	usage, err = d.UsageBytes(ctx)
	if err != nil {
		t.Fatalf("UsageBytes after delete: %v", err)
	}
	want -= int64(len(del))
	if usage != want {
		t.Errorf("usage after delete: got %d want %d", usage, want)
	}
}

// ---- Sharding ---------------------------------------------------------------

func TestSharding_PathLayout(t *testing.T) {
	// Blobs must be stored at <root>/<2-char-shard>/<fullhex>.
	ctx := context.Background()
	root := t.TempDir()
	d := local.NewLocalDiskDriver(root)

	data := []byte("shard path layout test")
	dgst := blobDigest(data)
	hexPart := dgst[len("sha256:"):]
	shard := hexPart[:2]

	if err := d.Put(ctx, dgst, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	expected := filepath.Join(root, shard, hexPart)
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("blob not found at shard path %s: %v", expected, err)
	}
}

func TestSharding_MultipleBlobs(t *testing.T) {
	// 30 distinct blobs should create at least one shard directory.
	ctx := context.Background()
	root := t.TempDir()
	d := local.NewLocalDiskDriver(root)

	for i := 0; i < 30; i++ {
		data := []byte(fmt.Sprintf("multi-shard-blob-%d", i))
		dgst := blobDigest(data)
		if err := d.Put(ctx, dgst, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var dirCount int
	for _, e := range entries {
		if e.IsDir() {
			dirCount++
		}
	}
	if dirCount == 0 {
		t.Error("no shard directories created")
	}
	t.Logf("shard directories in use: %d", dirCount)
}

// ---- Digest format ----------------------------------------------------------

func TestDigestFormats_PrefixedAndBare(t *testing.T) {
	// Both "sha256:<hex>" and bare "<hex>" must address the same blob.
	ctx := context.Background()
	d := newDriver(t)

	data := []byte("digest format compatibility")
	h := sha256.Sum256(data)
	hexStr := hex.EncodeToString(h[:])
	prefixed := "sha256:" + hexStr

	// Put with prefixed form.
	if err := d.Put(ctx, prefixed, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put prefixed: %v", err)
	}

	// Exists with bare hex must find the same blob.
	exists, err := d.Exists(ctx, hexStr)
	if err != nil {
		t.Fatalf("Exists bare: %v", err)
	}
	if !exists {
		t.Error("bare-hex Exists should find blob put with prefixed digest")
	}

	// Get with bare hex.
	rc, _, err := d.Get(ctx, hexStr, 0, -1)
	if err != nil {
		t.Fatalf("Get bare: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Error("content mismatch when getting with bare hex")
	}

	// Idempotent Put with bare hex.
	if err := d.Put(ctx, hexStr, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Put bare hex: %v", err)
	}
}

// ---- Concurrency ------------------------------------------------------------

func TestConcurrentPuts_SameDigest(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	data := []byte("concurrent same-digest blob")
	dgst := blobDigest(data)

	const goroutines = 20
	var wg sync.WaitGroup
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = d.Put(ctx, dgst, bytes.NewReader(data), int64(len(data)))
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	rc, _, err := d.Get(ctx, dgst, 0, -1)
	if err != nil {
		t.Fatalf("Get after concurrent puts: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data) {
		t.Error("content mismatch after concurrent puts")
	}
}

// ---- Large blob streaming ---------------------------------------------------

func TestLargeBlob_Streaming(t *testing.T) {
	// 32 MB — proves that Put + Get work at scale without buffering the whole
	// blob in memory (the implementation streams through crypto/sha256 into a
	// temp file; this test validates correctness, not instrumented memory use).
	const blobSize = 32 << 20 // 32 MiB
	ctx := context.Background()
	d := newDriver(t)

	data := make([]byte, blobSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	h := sha256.Sum256(data)
	dgst := "sha256:" + hex.EncodeToString(h[:])

	if err := d.Put(ctx, dgst, bytes.NewReader(data), int64(blobSize)); err != nil {
		t.Fatalf("Put large blob: %v", err)
	}

	rc, size, err := d.Get(ctx, dgst, 0, -1)
	if err != nil {
		t.Fatalf("Get large blob: %v", err)
	}
	defer rc.Close()

	if size != int64(blobSize) {
		t.Errorf("size: got %d want %d", size, blobSize)
	}

	// Verify the retrieved content by streaming its digest.
	hRead := sha256.New()
	n, err := io.Copy(hRead, rc)
	if err != nil {
		t.Fatalf("read large blob: %v", err)
	}
	if n != int64(blobSize) {
		t.Errorf("read %d bytes, want %d", n, blobSize)
	}
	gotDigest := "sha256:" + hex.EncodeToString(hRead.Sum(nil))
	if gotDigest != dgst {
		t.Error("large blob digest mismatch on retrieval")
	}
}

func TestLargeBlob_RangeRead(t *testing.T) {
	// 8 MB blob; read a 2 MB window from the middle.
	const blobSize = 8 << 20
	ctx := context.Background()
	d := newDriver(t)

	data := make([]byte, blobSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	h := sha256.Sum256(data)
	dgst := "sha256:" + hex.EncodeToString(h[:])

	if err := d.Put(ctx, dgst, bytes.NewReader(data), int64(blobSize)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	const offset = 3 << 20 // 3 MiB in
	const length = 2 << 20 // 2 MiB window

	rc, fullSize, err := d.Get(ctx, dgst, offset, length)
	if err != nil {
		t.Fatalf("Get range: %v", err)
	}
	defer rc.Close()

	if fullSize != int64(blobSize) {
		t.Errorf("fullSize: got %d want %d", fullSize, blobSize)
	}

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll range: %v", err)
	}
	if int64(len(got)) != length {
		t.Errorf("range read length: got %d want %d", len(got), length)
	}
	if !bytes.Equal(got, data[offset:offset+length]) {
		t.Error("range read content mismatch")
	}
}

// ---- Empty blob -------------------------------------------------------------

func TestEmptyBlob(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)

	data := []byte{}
	dgst := blobDigest(data)

	if err := d.Put(ctx, dgst, bytes.NewReader(data), 0); err != nil {
		t.Fatalf("Put empty: %v", err)
	}

	exists, err := d.Exists(ctx, dgst)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("empty blob must exist after put")
	}

	rc, size, err := d.Get(ctx, dgst, 0, -1)
	if err != nil {
		t.Fatalf("Get empty: %v", err)
	}
	defer rc.Close()

	if size != 0 {
		t.Errorf("size: got %d want 0", size)
	}
	got, _ := io.ReadAll(rc)
	if len(got) != 0 {
		t.Errorf("expected empty read, got %d bytes", len(got))
	}
}
