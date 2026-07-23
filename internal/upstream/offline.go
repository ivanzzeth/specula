package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// ErrOffline is returned by OfflineClient when server.mode=offline blocks an
// outbound fetch. Handlers should map it to HTTP 404 (cache miss / air-gap).
var ErrOffline = errors.New("upstream: offline mode — no outbound fetch")

// OfflineClient implements Client and never contacts the network.
type OfflineClient struct{}

// NewOfflineClient returns a Client that rejects every Fetch/Revalidate.
func NewOfflineClient() Client { return OfflineClient{} }

// MaybeOffline returns NewOfflineClient when offline is true, otherwise inner.
func MaybeOffline(inner Client, offline bool) Client {
	if offline {
		return NewOfflineClient()
	}
	return inner
}

func (OfflineClient) Fetch(
	_ context.Context,
	_ artifact.ArtifactRef,
	_ []Upstream,
	_ ...RequestOption,
) (io.ReadCloser, artifact.UpstreamMeta, error) {
	return nil, artifact.UpstreamMeta{}, ErrOffline
}

func (OfflineClient) Revalidate(
	_ context.Context,
	_ artifact.ArtifactRef,
	_ artifact.UpstreamMeta,
	_ []Upstream,
	_ ...RequestOption,
) (io.ReadCloser, artifact.UpstreamMeta, bool, error) {
	return nil, artifact.UpstreamMeta{}, false, ErrOffline
}

// IsNotFound reports whether err means the artifact is unavailable to serve
// from upstream: offline mode, or a definitive upstream HTTP 404.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrOffline) {
		return true
	}
	var se *StatusError
	if errors.As(err, &se) && se.StatusCode == http.StatusNotFound {
		return true
	}
	return false
}
