// Package cache — quarantine streaming helper (fix C3).
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// Quarantine streams r into a temporary file under dir, computing the sha256
// digest with a streaming hash.Hash (never buffering the full content in
// memory — fix C3 for multi-GB blobs). It returns the on-disk Artifact for
// handoff to Store plus a cleanup function that removes the temp file.
//
// Usage pattern:
//
//	art, cleanup, err := Quarantine(ctx, dir, body, meta)
//	if err != nil { ... }
//	entry, err := cm.Store(ctx, ref, art) // removes art.Path on success
//	if err != nil {
//	    cleanup() // remove on failure; no-op if Store already removed it
//	}
func Quarantine(_ context.Context, dir string, r io.Reader, umeta artifact.UpstreamMeta) (*artifact.Artifact, func(), error) {
	f, err := os.CreateTemp(dir, "specula-quarantine-*")
	if err != nil {
		return nil, nil, fmt.Errorf("cache: quarantine create temp: %w", err)
	}
	path := f.Name()
	cleanup := func() {
		// Best-effort removal; idempotent (second call is a no-op by virtue of
		// Remove returning an error that we discard).
		_ = os.Remove(path)
	}

	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), r)
	closeErr := f.Close()

	if copyErr != nil {
		cleanup()
		return nil, nil, fmt.Errorf("cache: quarantine write: %w", copyErr)
	}
	if closeErr != nil {
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
