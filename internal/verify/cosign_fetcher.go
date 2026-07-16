package verify

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// cosignSignatureAnnotation is the manifest-layer annotation key under which
// cosign's "simple signing" layout stores the base64 signature.
const cosignSignatureAnnotation = "dev.cosignproject.cosign/signature"

// maxCosignPayloadBytes bounds a single cosign simple-signing payload read. The
// payload is a small JSON document; anything larger is treated as hostile.
const maxCosignPayloadBytes = 1 << 20 // 1 MiB

// OCISignatureFetcher is the production SignatureFetcher: it discovers the
// cosign signatures attached to an OCI image by resolving the
// "sha256-<hex>.sig" companion tag via go-containerregistry — WITHOUT pulling
// in github.com/sigstore/cosign/v2 (see cosign.go for the binary-size rationale).
//
// The image lives behind one of the configured OCI mirror registries; the
// signature companion tag is co-located on the same registry+repository. Each
// configured registry host is tried in order until one yields signatures.
type OCISignatureFetcher struct {
	// registries are the registry hosts (e.g. "registry-1.docker.io",
	// "docker.m.daocloud.io") derived from the oci protocol upstreams.
	registries []string
	// options are the go-containerregistry remote options (auth, context is
	// added per-call).
	remoteOpts []remote.Option
}

// NewOCISignatureFetcher builds a fetcher over the given registry hosts. Hosts
// may be bare ("registry-1.docker.io") or full base URLs
// ("https://docker.m.daocloud.io") — the scheme is stripped. Authentication
// uses go-containerregistry's default keychain (anonymous when no credentials
// are configured), which covers the CN mirror + Docker Hub anonymous-pull case.
func NewOCISignatureFetcher(registries []string) *OCISignatureFetcher {
	hosts := make([]string, 0, len(registries))
	seen := make(map[string]struct{}, len(registries))
	for _, r := range registries {
		h := registryHost(r)
		if h == "" {
			continue
		}
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		hosts = append(hosts, h)
	}
	return &OCISignatureFetcher{
		registries: hosts,
		remoteOpts: []remote.Option{remote.WithAuthFromKeychain(authn.DefaultKeychain)},
	}
}

// Compile-time assertion that OCISignatureFetcher satisfies the interface.
var _ SignatureFetcher = (*OCISignatureFetcher)(nil)

// FetchSignatures resolves the cosign signature companion tag for ref's image
// digest across the configured registries and returns every attached signature.
// An empty slice with a nil error means no signature was found on any registry
// (an unsigned image). A non-nil error means every registry errored.
func (f *OCISignatureFetcher) FetchSignatures(ctx context.Context, ref artifact.ArtifactRef) ([]CosignSignature, error) {
	digest := ref.Digest
	if digest == "" {
		digest = ref.Version
	}
	sigTag, err := cosignSigTag(digest)
	if err != nil {
		return nil, err
	}
	if len(f.registries) == 0 {
		return nil, fmt.Errorf("cosign: no OCI registry configured for signature discovery")
	}

	var lastErr error
	for _, host := range f.registries {
		tagRef, err := name.NewTag(host + "/" + ref.Name + ":" + sigTag)
		if err != nil {
			lastErr = fmt.Errorf("cosign: build sig tag on %s: %w", host, err)
			continue
		}
		opts := append([]remote.Option{remote.WithContext(ctx)}, f.remoteOpts...)
		img, err := remote.Image(tagRef, opts...)
		if err != nil {
			// Most commonly a 404: no signature on this registry. Remember the
			// error but keep trying the other registries.
			lastErr = err
			continue
		}
		sigs, err := signaturesFromImage(img)
		if err != nil {
			lastErr = err
			continue
		}
		if len(sigs) > 0 {
			return sigs, nil
		}
	}
	// No registry produced signatures. Treat "not found everywhere" as unsigned
	// (empty, nil) so the verifier reports the honest "no signature" failure;
	// surface a transport error only when we never got a clean negative.
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}

// signaturesFromImage extracts the cosign signatures from a resolved signature
// image: each manifest layer carries the base64 signature in its annotation and
// the layer blob is the signed payload.
func signaturesFromImage(img v1.Image) ([]CosignSignature, error) {
	manifest, err := img.Manifest()
	if err != nil {
		return nil, fmt.Errorf("cosign: read sig manifest: %w", err)
	}
	var out []CosignSignature
	for _, layer := range manifest.Layers {
		b64 := strings.TrimSpace(layer.Annotations[cosignSignatureAnnotation])
		if b64 == "" {
			continue
		}
		l, err := img.LayerByDigest(layer.Digest)
		if err != nil {
			return nil, fmt.Errorf("cosign: read sig layer %s: %w", layer.Digest, err)
		}
		rc, err := l.Uncompressed()
		if err != nil {
			return nil, fmt.Errorf("cosign: open sig payload %s: %w", layer.Digest, err)
		}
		payload, err := io.ReadAll(io.LimitReader(rc, maxCosignPayloadBytes))
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("cosign: read sig payload %s: %w", layer.Digest, err)
		}
		out = append(out, CosignSignature{Payload: payload, Base64Sig: b64})
	}
	return out, nil
}

// cosignSigTag converts an image digest ("sha256:<hex>") into its cosign
// signature companion tag ("sha256-<hex>.sig").
func cosignSigTag(digest string) (string, error) {
	i := strings.IndexByte(digest, ':')
	if i <= 0 || i == len(digest)-1 {
		return "", fmt.Errorf("cosign: malformed image digest %q (want alg:hex)", digest)
	}
	return digest[:i] + "-" + digest[i+1:] + ".sig", nil
}

// registryHost normalises a configured upstream entry (bare host or base URL)
// into a bare registry host for go-containerregistry.
func registryHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			return u.Host
		}
	}
	// Bare host possibly with a path; keep only the host segment.
	if i := strings.IndexByte(raw, '/'); i > 0 {
		return raw[:i]
	}
	return raw
}
