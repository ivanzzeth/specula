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

// AsVerifyError unwraps err to a *VerifyError. Returns (nil, false) when err
// is not a VerifyError.
func AsVerifyError(err error) (*VerifyError, bool) {
	var ve *VerifyError
	if errors.As(err, &ve) {
		return ve, true
	}
	return nil, false
}
