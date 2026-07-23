package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/ivanzzeth/specula/internal/artifact"
)

type countingClient struct {
	calls int
}

func (c *countingClient) Fetch(
	_ context.Context,
	_ artifact.ArtifactRef,
	_ []Upstream,
	_ ...RequestOption,
) (io.ReadCloser, artifact.UpstreamMeta, error) {
	c.calls++
	return nil, artifact.UpstreamMeta{}, errors.New("should not be called")
}

func (c *countingClient) Revalidate(
	_ context.Context,
	_ artifact.ArtifactRef,
	_ artifact.UpstreamMeta,
	_ []Upstream,
	_ ...RequestOption,
) (io.ReadCloser, artifact.UpstreamMeta, bool, error) {
	c.calls++
	return nil, artifact.UpstreamMeta{}, false, errors.New("should not be called")
}

func TestOfflineClient_NoNetwork(t *testing.T) {
	c := NewOfflineClient()
	_, _, err := c.Fetch(context.Background(), artifact.ArtifactRef{Protocol: "oci", Name: "x"}, nil)
	if !errors.Is(err, ErrOffline) {
		t.Fatalf("Fetch: want ErrOffline, got %v", err)
	}
	_, _, _, err = c.Revalidate(context.Background(), artifact.ArtifactRef{}, artifact.UpstreamMeta{}, nil)
	if !errors.Is(err, ErrOffline) {
		t.Fatalf("Revalidate: want ErrOffline, got %v", err)
	}
}

func TestMaybeOffline(t *testing.T) {
	inner := &countingClient{}
	if MaybeOffline(inner, false) != inner {
		t.Fatal("online should return inner")
	}
	off := MaybeOffline(inner, true)
	_, _, err := off.Fetch(context.Background(), artifact.ArtifactRef{}, nil)
	if !errors.Is(err, ErrOffline) {
		t.Fatalf("got %v", err)
	}
	if inner.calls != 0 {
		t.Fatalf("inner should not be called, calls=%d", inner.calls)
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(ErrOffline) {
		t.Fatal("ErrOffline")
	}
	if !IsNotFound(&StatusError{Upstream: "u", StatusCode: http.StatusNotFound}) {
		t.Fatal("StatusError 404")
	}
	if IsNotFound(&StatusError{Upstream: "u", StatusCode: http.StatusForbidden}) {
		t.Fatal("403 should not be IsNotFound")
	}
	if IsNotFound(errors.New("connection refused")) {
		t.Fatal("transport error")
	}
}
