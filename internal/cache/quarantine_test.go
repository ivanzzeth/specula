package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"bytes"
	"testing"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/upstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type restartReader struct {
	prefix   []byte
	full     []byte
	phase    int // 0=prefix, 1=signal restart, 2=full
	prefixOff int
	fullOff   int
}

func (r *restartReader) Read(p []byte) (int, error) {
	switch r.phase {
	case 0:
		if r.prefixOff >= len(r.prefix) {
			r.phase = 1
			return 0, upstream.ErrRestartFromBeginning
		}
		n := copy(p, r.prefix[r.prefixOff:])
		r.prefixOff += n
		return n, nil
	case 1:
		r.phase = 2
		return 0, upstream.ErrRestartFromBeginning
	default:
		if r.fullOff >= len(r.full) {
			return 0, io.EOF
		}
		n := copy(p, r.full[r.fullOff:])
		r.fullOff += n
		if r.fullOff >= len(r.full) {
			return n, io.EOF
		}
		return n, nil
	}
}

func TestQuarantine_RestartFromBeginningResetsDigest(t *testing.T) {
	dir := t.TempDir()
	full := []byte("correct-final-bytes-for-digest")
	wantSum := sha256.Sum256(full)
	wantDigest := "sha256:" + hex.EncodeToString(wantSum[:])

	r := &restartReader{
		prefix: []byte("WRONGPREFIX"),
		full:   full,
	}
	art, cleanup, err := Quarantine(context.Background(), dir, r, artifact.UpstreamMeta{})
	require.NoError(t, err)
	defer cleanup()

	assert.Equal(t, wantDigest, art.Digest)
	assert.Equal(t, int64(len(full)), art.Size)
	got, err := os.ReadFile(art.Path)
	require.NoError(t, err)
	assert.Equal(t, full, got)
}

// blockingReader blocks in Read until ctx is cancelled, then returns ctx.Err().
type blockingReader struct{ ctx context.Context }

func (b blockingReader) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func TestQuarantine_RespectsContextCancel(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, cleanup, err := Quarantine(ctx, dir, blockingReader{ctx: ctx}, artifact.UpstreamMeta{})
		if cleanup != nil {
			cleanup()
		}
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.Canceled), "got %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Quarantine did not return after ctx cancel")
	}
}

// TestQuarantineAt_PreservesPartialOnCancel is the durable-resume contract:
// aborting a cold fill must leave bytes on disk (unlike Quarantine which deletes).
func TestQuarantineAt_PreservesPartialOnCancel(t *testing.T) {
	dir := t.TempDir()
	path := DurablePartialPath(dir, "sha256:deadbeef")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := QuarantineAt(ctx, path, blockingReader{ctx: ctx}, artifact.UpstreamMeta{})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.Canceled), "got %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("QuarantineAt did not return after ctx cancel")
	}
	// File may be empty (blocked before any byte) but must still exist OR
	// path may not exist if OpenFile raced — with blocking reader we opened it.
	_, err := os.Stat(path)
	require.NoError(t, err, "durable partial must survive cancel so the next fill can resume")
}

// TestQuarantineAt_ResumesFromExistingBytes hashes prefix already on disk then
// appends the remainder — the verify digest must match the full object.
func TestQuarantineAt_ResumesFromExistingBytes(t *testing.T) {
	dir := t.TempDir()
	full := []byte("prefix-then-suffix-for-durable-resume")
	wantSum := sha256.Sum256(full)
	wantDigest := "sha256:" + hex.EncodeToString(wantSum[:])

	path := DurablePartialPath(dir, wantDigest)
	require.NoError(t, os.WriteFile(path, full[:10], 0o600))

	art, cleanup, err := QuarantineAt(context.Background(), path, bytes.NewReader(full[10:]), artifact.UpstreamMeta{})
	require.NoError(t, err)
	defer cleanup()

	assert.Equal(t, wantDigest, art.Digest)
	assert.Equal(t, int64(len(full)), art.Size)
	got, err := os.ReadFile(art.Path)
	require.NoError(t, err)
	assert.Equal(t, full, got)
}
