// Package cache — error types.
package cache

import (
	"errors"
	"fmt"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// ErrCacheMiss is returned by Serve when the artifact is absent or its
// mutable TTL has expired.
var ErrCacheMiss = errors.New("cache: miss")

// VerifyError is returned by Store when the verification chain rejects the
// quarantined artifact. It carries the full Result so handlers can log the
// tier and return a meaningful 502 response.
type VerifyError struct {
	Ref    artifact.ArtifactRef
	Result artifact.Result
}

func (e *VerifyError) Error() string {
	return fmt.Sprintf("cache: verify %s/%s@%s failed: tier=%s status=%s: %s",
		e.Ref.Protocol, e.Ref.Name, e.Ref.Version,
		e.Result.Tier, e.Result.Status, e.Result.Message)
}

// PinMismatchError is returned by Lookup (and therefore Serve) when a ref
// carries a caller-supplied digest pin that contradicts the digest of the entry
// the cache holds for that ref.
//
// This is distinct from a verification failure. The stored bytes are still
// trusted — ARCHITECTURE §3's "CAS is never re-verified" is untouched, and no
// blob is re-hashed. What failed is the REQUEST's own assertion: the caller
// said "serve me <Want> or fail", and the cache holds <Got>. Answering 200 with
// <Got> would silently ignore an explicit integrity assertion.
//
// A pin mismatch is never grounds for evicting or invalidating the entry: a
// caller who can supply an arbitrary pin must not be able to use it as a
// cache-denial lever.
type PinMismatchError struct {
	Ref  artifact.ArtifactRef
	Want string // digest the caller pinned
	Got  string // digest of the entry actually cached under this ref
}

func (e *PinMismatchError) Error() string {
	return fmt.Sprintf("cache: digest pin mismatch for %s/%s@%s: caller pinned %s, cache holds %s",
		e.Ref.Protocol, e.Ref.Name, e.Ref.Version, e.Want, e.Got)
}

// AsPinMismatchError unwraps err to a *PinMismatchError. Returns (nil, false)
// when err is not a PinMismatchError.
func AsPinMismatchError(err error) (*PinMismatchError, bool) {
	var pe *PinMismatchError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// AsVerifyError unwraps err to a *VerifyError. Returns (nil, false) when err
// is not a VerifyError.
func AsVerifyError(err error) (*VerifyError, bool) {
	var ve *VerifyError
	if errors.As(err, &ve) {
		return ve, true
	}
	return nil, false
}
