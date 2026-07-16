package verify

import (
	"bytes"
	"context"
	"crypto"
	"encoding/base64"
	"fmt"

	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// protocolOCI is the ArtifactRef.Protocol value for OCI images.
const protocolOCI = "oci"

// CosignConfig is the runtime configuration for keyed cosign verification with
// the transparency log DISABLED (DESIGN-REVIEW §1.1 — the CN-offline cosign
// anchor: `cosign verify --key <pub> --insecure-ignore-tlog`).
//
// Keyless verification (Fulcio/Rekor/OIDC) is intentionally NOT supported: it
// requires reaching the Rekor transparency log and Fulcio CA, which are
// typically unreachable in the target deployment. Tlog therefore MUST be false.
type CosignConfig struct {
	// Keys are filesystem paths to long-lived cosign PUBLIC keys (PEM). A
	// signature that verifies against ANY listed key passes (key rotation).
	Keys []string
	// Tlog MUST be false. A true value is rejected by NewCosignVerifier — the
	// field exists so the "keyed, tlog off" decision is explicit in config, not
	// implicit.
	Tlog bool
}

// CosignSignature is one discovered cosign signature attached to an image. In
// cosign's "simple signing" layout the signature is stored as a manifest layer:
// the layer blob is the payload, and the base64 signature lives in the layer's
// `dev.cosignproject.cosign/signature` annotation.
type CosignSignature struct {
	// Payload is the exact bytes the signature commits to (the simple-signing
	// document, which references the signed image digest).
	Payload []byte
	// Base64Sig is the value of the dev.cosignproject.cosign/signature annotation.
	Base64Sig string
}

// SignatureFetcher discovers the cosign signatures attached to the OCI image
// referenced by ref, WITHOUT pulling in the full sigstore/cosign/v2 tree:
// the production implementation wraps go-containerregistry to resolve the
// `sha256-<hex>.sig` companion tag for the image digest and read its layer
// payloads + signature annotations. Injectable so the verification core below
// is unit-testable with a fake.
type SignatureFetcher interface {
	// FetchSignatures returns every cosign signature attached to ref's image
	// digest. An empty slice (no error) means the image is unsigned.
	FetchSignatures(ctx context.Context, ref artifact.ArtifactRef) ([]CosignSignature, error)
}

// CosignVerifier attests the signed tier (TierSigned) for OCI images by
// verifying an attached cosign signature against one or more configured
// long-lived public keys, with the transparency log disabled.
//
// # Dependency choice (binary-size sensitive)
//
// Verification uses github.com/sigstore/sigstore/pkg/signature (the standalone
// signature primitives: LoadVerifierFromPEMFile + Verifier.VerifySignature)
// plus go-containerregistry (already a dependency) for signature discovery —
// deliberately AVOIDING github.com/sigstore/cosign/v2, whose Rekor/Fulcio/TUF/
// in-toto transitive tree would add tens of MB to the binary for capabilities
// (keyless, tlog) that are unusable in the offline/keyed deployment anyway.
//
// # Self-gating
//
// Verify is a no-op StatusPass for any ref whose Protocol is not "oci", and for
// mutable/undigested refs (cosign signs the image manifest by digest).
//
// # Injection
//
// The SignatureFetcher is injected. Until a production fetcher (go-container-
// registry) is wired, a nil fetcher makes Verify fail closed so the chain never
// attests a signature it did not actually check.
type CosignVerifier struct {
	// verifiers are loaded once from the configured public keys.
	verifiers []signature.Verifier
	// keyPaths is retained for diagnostics/messages.
	keyPaths []string
	// fetcher discovers attached signatures for an image digest.
	fetcher SignatureFetcher
}

// NewCosignVerifier loads every configured public key and returns a verifier
// anchored on them. tlog:true, an empty key list, or an unreadable/invalid key
// is a fatal wiring error (fail-fast; never silently trust). fetcher may be nil
// in a wiring skeleton, in which case Verify fails closed.
func NewCosignVerifier(cfg CosignConfig, fetcher SignatureFetcher) (*CosignVerifier, error) {
	if cfg.Tlog {
		return nil, fmt.Errorf("cosign: transparency-log verification is unsupported in this build " +
			"(CN-offline keyed mode only); set cosign.tlog: false")
	}
	if len(cfg.Keys) == 0 {
		return nil, fmt.Errorf("cosign: at least one public key path is required for keyed verification")
	}
	verifiers := make([]signature.Verifier, 0, len(cfg.Keys))
	for _, path := range cfg.Keys {
		ver, err := signature.LoadVerifierFromPEMFile(path, crypto.SHA256)
		if err != nil {
			return nil, fmt.Errorf("cosign: load public key %q: %w", path, err)
		}
		verifiers = append(verifiers, ver)
	}
	return &CosignVerifier{
		verifiers: verifiers,
		keyPaths:  cfg.Keys,
		fetcher:   fetcher,
	}, nil
}

// Compile-time assertion that CosignVerifier satisfies Verifier.
var _ Verifier = (*CosignVerifier)(nil)

func (v *CosignVerifier) Name() string        { return "cosign" }
func (v *CosignVerifier) Tier() artifact.Tier { return artifact.TierSigned }

// Verify checks the OCI image's attached cosign signature(s) against the
// configured keys.
//
//   - Skipped (StatusPass, TierChecksum) for non-oci or mutable/undigested refs.
//   - Fail-closed (error) when no SignatureFetcher is wired.
//   - StatusFail when the image is unsigned or no signature verifies against any
//     configured key.
//   - StatusPass (TierSigned) when a signature verifies against a configured key.
func (v *CosignVerifier) Verify(ctx context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	// Self-gate: only immutable, digest-resolved OCI artifacts.
	if ref.Protocol != protocolOCI || ref.Mutable || art.Digest == "" {
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: "cosign: skipped (not a resolved oci image)",
		}, nil
	}

	// Fail closed if the discovery seam is not wired — never attest unchecked.
	if v.fetcher == nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: "cosign: no SignatureFetcher wired (fail-closed)",
		}, fmt.Errorf("cosign: %w (SignatureFetcher is nil)", errNotImplemented)
	}

	sigs, err := v.fetcher.FetchSignatures(ctx, ref)
	if err != nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("cosign: fetch signatures for %s: %v", refKey(ref), err),
		}, nil
	}
	if len(sigs) == 0 {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("cosign: no cosign signature attached to %s (keyed policy requires one)", refKey(ref)),
		}, nil
	}

	// A single valid signature against any configured key is sufficient.
	for _, sig := range sigs {
		raw, decErr := base64.StdEncoding.DecodeString(sig.Base64Sig)
		if decErr != nil {
			continue
		}
		for _, ver := range v.verifiers {
			if verErr := ver.VerifySignature(bytes.NewReader(raw), bytes.NewReader(sig.Payload)); verErr == nil {
				return artifact.Result{
					Status:  artifact.StatusPass,
					Tier:    artifact.TierSigned,
					Message: fmt.Sprintf("cosign: signature verified for %s against a configured key (tlog disabled)", refKey(ref)),
				}, nil
			}
		}
	}

	return artifact.Result{
		Status:  artifact.StatusFail,
		Tier:    artifact.TierSigned,
		Message: fmt.Sprintf("cosign: no attached signature verified against any of %d configured key(s) for %s", len(v.verifiers), refKey(ref)),
	}, nil
}
