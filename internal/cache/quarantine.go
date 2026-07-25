// Package cache — quarantine streaming helper (fix C3).
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// Quarantine streams r into a temporary file under dir, computing the sha256
// digest with a streaming hash.Hash (never buffering the full content in
// memory — fix C3 for multi-GB blobs). It returns the on-disk Artifact for
// handoff to Store plus a cleanup function that removes the temp file.
//
// If r returns upstream.ErrRestartFromBeginning (Range resume got a full 200
// body because the mirror ignored Range), the temp file is truncated and the
// hasher reset so the digest still matches the final bytes.
//
// ctx cancellation aborts the copy promptly.
//
// Usage pattern:
//
//	art, cleanup, err := Quarantine(ctx, dir, body, meta)
//	if err != nil { ... }
//	entry, err := cm.Store(ctx, ref, art) // removes art.Path on success
//	if err != nil {
//	    cleanup() // remove on failure; no-op if Store already removed it
//	}
func Quarantine(ctx context.Context, dir string, r io.Reader, umeta artifact.UpstreamMeta) (*artifact.Artifact, func(), error) {
	f, err := os.CreateTemp(dir, "specula-quarantine-*")
	if err != nil {
		return nil, nil, fmt.Errorf("cache: quarantine create temp: %w", err)
	}
	path := f.Name()
	cleanup := func() {
		_ = os.Remove(path)
	}

	h := sha256.New()
	var n int64
	buf := make([]byte, 32*1024)

	for {
		if err := ctx.Err(); err != nil {
			cleanup()
			_ = f.Close()
			return nil, nil, fmt.Errorf("cache: quarantine write: %w", err)
		}
		nr, readErr := r.Read(buf)
		if nr > 0 {
			nw, werr := f.Write(buf[:nr])
			if werr != nil {
				cleanup()
				_ = f.Close()
				return nil, nil, fmt.Errorf("cache: quarantine write: %w", werr)
			}
			if nw != nr {
				cleanup()
				_ = f.Close()
				return nil, nil, fmt.Errorf("cache: quarantine write: short write")
			}
			_, _ = h.Write(buf[:nr])
			n += int64(nr)
		}
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, upstream.ErrRestartFromBeginning) {
			if err := resetQuarantine(f, h); err != nil {
				cleanup()
				_ = f.Close()
				return nil, nil, err
			}
			n = 0
			continue
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		cleanup()
		_ = f.Close()
		return nil, nil, fmt.Errorf("cache: quarantine write: %w", readErr)
	}

	if closeErr := f.Close(); closeErr != nil {
		cleanup()
		return nil, nil, fmt.Errorf("cache: quarantine close: %w", closeErr)
	}

	art := &artifact.Artifact{
		Path:   path,
		Digest: "sha256:" + hex.EncodeToString(h.Sum(nil)),
		Size:   n,
		Meta:   umeta,
	}
	return art, cleanup, nil
}

func resetQuarantine(f *os.File, h hash.Hash) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("cache: quarantine restart seek: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("cache: quarantine restart truncate: %w", err)
	}
	h.Reset()
	return nil
}
