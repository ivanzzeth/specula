package verify

import (
	"bytes"
	"context"
	"crypto"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// protocolOCI is the ArtifactRef.Protocol value for OCI images.
const protocolOCI = "oci"

// ociManifestMediaTypes is the set of media types cosign actually signs: an
// image manifest or a manifest list / image index. cosign signs the manifest by
// digest — NEVER the config or layer blobs beneath it — so only artifacts with
// one of these upstream-reported content types are eligible for cosign
// verification. Everything else (layers, config, octet-stream blobs) is skipped.
var ociManifestMediaTypes = map[string]struct{}{
	"application/vnd.oci.image.manifest.v1+json":                {},
	"application/vnd.docker.distribution.manifest.v2+json":      {},
	"application/vnd.oci.image.index.v1+json":                   {},
	"application/vnd.docker.distribution.manifest.list.v2+json": {},
}

// isOCIManifestMediaType reports whether ct (a Content-Type value, possibly with
// parameters like "; charset=utf-8") is an OCI/docker image manifest or index.
func isOCIManifestMediaType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	_, ok := ociManifestMediaTypes[ct]
	return ok
}

// CosignConfig is the runtime configuration for OCI cosign verification with
// the transparency log DISABLED (DESIGN-REVIEW §1.1 — CN-offline / air-gap).
//
// Two authenticity anchors are supported (either or both):
//
//  1. Keys — long-lived publisher public keys (classic keyed cosign).
//  2. TrustedRoot — Sigstore trusted_root.json Fulcio CAs; signatures that
//     carry a leaf certificate chaining to those CAs verify offline without
//     contacting Rekor. This is the self-hosted / air-gap "keyless-style" path:
//     you trust whoever your Fulcio issues to. Live Rekor / CT / OIDC identity
//     policy is NOT consulted while Tlog is false.
//
// Tlog MUST remain false: online Rekor verification is unsupported in this build.
type CosignConfig struct {
	// Keys are filesystem paths to long-lived cosign PUBLIC keys (PEM). A
	// signature that verifies against ANY listed key passes (key rotation).
	// May be empty when TrustedRoot is set.
	Keys []string
	// Tlog MUST be false. A true value is rejected by NewCosignVerifier — the
	// field exists so the "tlog off" decision is explicit in config, not
	// implicit.
	Tlog bool
	// TrustedRoot is an optional filesystem path to a Sigstore
	// trusted_root.json. When set, attached signatures that include a Fulcio
	// leaf certificate (dev.sigstore.cosign/certificate) are verified against
	// the CAs in that file — offline, no Rekor. May be empty when Keys is set.
	TrustedRoot string
}

// CosignSignature is one discovered cosign signature attached to an image. In
// cosign's "simple signing" layout the signature is stored as a manifest layer:
// the layer blob is the payload, and the base64 signature lives in the layer's
// `dev.cosignproject.cosign/signature` annotation. Keyless-style signatures
// additionally carry a Fulcio leaf in `dev.sigstore.cosign/certificate`.
type CosignSignature struct {
	// Payload is the exact bytes the signature commits to (the simple-signing
	// document, which references the signed image digest).
	Payload []byte
	// Base64Sig is the value of the dev.cosignproject.cosign/signature annotation.
	Base64Sig string
	// CertPEM is the optional Fulcio leaf certificate PEM from
	// dev.sigstore.cosign/certificate (empty for keyed-only signatures).
	CertPEM []byte
	// ChainPEM is the optional certificate chain PEM from
	// dev.sigstore.cosign/chain (intermediates; may be empty when the
	// trusted_root already embeds intermediates).
	ChainPEM []byte
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
// verifying an attached cosign signature against configured long-lived public
// keys and/or a Fulcio CA trusted_root, with the transparency log disabled.
//
// # Dependency choice (binary-size sensitive)
//
// Verification uses github.com/sigstore/sigstore/pkg/signature (the standalone
// signature primitives) plus go-containerregistry (already a dependency) for
// signature discovery — deliberately AVOIDING github.com/sigstore/cosign/v2,
// whose Rekor/Fulcio/TUF/in-toto transitive tree would add tens of MB for
// online capabilities unused while tlog is false.
//
// # Self-gating
//
// Verify returns StatusSkip for any ref whose Protocol is not "oci", and for
// mutable/undigested refs (cosign signs the image manifest by digest).
//
// # Injection
//
// The SignatureFetcher is injected. A nil fetcher makes Verify fail closed so
// the chain never attests a signature it did not actually check.
type CosignVerifier struct {
	// verifiers are loaded once from the configured public keys (may be empty
	// when only TrustedRoot is configured).
	verifiers []signature.Verifier
	// keyPaths is retained for diagnostics/messages.
	keyPaths []string
	// trustedRoot is the optional Fulcio CA material for cert-backed signatures.
	trustedRoot *TrustedRoot
	// trustedRootPath is retained for diagnostics/messages.
	trustedRootPath string
	// fetcher discovers attached signatures for an image digest.
	fetcher SignatureFetcher
	// now is injectable for tests; nil means time.Now.
	now func() time.Time
}

// NewCosignVerifier loads configured public keys and/or a trusted_root and
// returns a verifier. tlog:true, or neither keys nor trusted_root, or an
// unreadable/invalid key/root, is a fatal wiring error (fail-fast). fetcher may
// be nil in a wiring skeleton, in which case Verify fails closed.
func NewCosignVerifier(cfg CosignConfig, fetcher SignatureFetcher) (*CosignVerifier, error) {
	if cfg.Tlog {
		return nil, fmt.Errorf("cosign: transparency-log verification is unsupported in this build " +
			"(CN-offline / air-gap mode only); set cosign.tlog: false")
	}
	trPath := strings.TrimSpace(cfg.TrustedRoot)
	if len(cfg.Keys) == 0 && trPath == "" {
		return nil, fmt.Errorf("cosign: at least one public key path or trusted_root is required")
	}

	verifiers := make([]signature.Verifier, 0, len(cfg.Keys))
	for _, path := range cfg.Keys {
		ver, err := signature.LoadVerifierFromPEMFile(path, crypto.SHA256)
		if err != nil {
			return nil, fmt.Errorf("cosign: load public key %q: %w", path, err)
		}
		verifiers = append(verifiers, ver)
	}

	var tr *TrustedRoot
	if trPath != "" {
		var err error
		tr, err = LoadTrustedRoot(trPath)
		if err != nil {
			return nil, fmt.Errorf("cosign: load trusted_root: %w", err)
		}
	}

	return &CosignVerifier{
		verifiers:       verifiers,
		keyPaths:        cfg.Keys,
		trustedRoot:     tr,
		trustedRootPath: trPath,
		fetcher:         fetcher,
	}, nil
}

// Compile-time assertion that CosignVerifier satisfies Verifier.
var _ Verifier = (*CosignVerifier)(nil)

func (v *CosignVerifier) Name() string        { return "cosign" }
func (v *CosignVerifier) Tier() artifact.Tier { return artifact.TierSigned }

func (v *CosignVerifier) clock() time.Time {
	if v.now != nil {
		return v.now()
	}
	return time.Now().UTC()
}

// Verify checks the OCI image's attached cosign signature(s) against configured
// keys and/or Fulcio certificates from trusted_root.
//
//   - Skipped (StatusSkip) for non-oci or mutable/undigested refs.
//   - Fail-closed (error) when no SignatureFetcher is wired.
//   - StatusFail when the image is unsigned or no signature verifies.
//   - StatusPass (TierSigned) when a signature verifies against a key or a
//     Fulcio-chained leaf (tlog disabled — no Rekor).
func (v *CosignVerifier) Verify(ctx context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	// Self-gate: only immutable, digest-resolved OCI artifacts.
	if ref.Protocol != protocolOCI || ref.Mutable || art.Digest == "" {
		return artifact.Result{
			Status:  artifact.StatusSkip,
			Tier:    artifact.TierChecksum,
			Message: "cosign: skipped (not a resolved oci image)",
		}, nil
	}

	// Manifest gate: cosign signs the image MANIFEST by digest, not its config
	// or layer blobs. In the pull-through path every blob arrives here as its own
	// immutable, digest-resolved oci artifact; without this gate cosign would
	// demand a `.sig` companion tag for each layer digest, find none, and
	// fail-close the whole pull on the first layer — making the `signed` tier
	// unusable for any real multi-blob image. Restrict to manifest/index media
	// types (registries serve blobs as application/octet-stream). An EMPTY
	// content type is treated as "run": real registries always label blob
	// responses, so an empty type never denotes a layer in production, and unit
	// tests that omit it still exercise the verification core.
	if ct := art.Meta.ContentType; ct != "" && !isOCIManifestMediaType(ct) {
		return artifact.Result{
			Status:  artifact.StatusSkip,
			Tier:    artifact.TierChecksum,
			Message: fmt.Sprintf("cosign: skipped (not an image manifest: content-type %q)", ct),
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
			Message: fmt.Sprintf("cosign: no cosign signature attached to %s (signed policy requires one)", refKey(ref)),
		}, nil
	}

	// Prefer keyed verification when keys are configured; then try Fulcio leaf
	// certs against trusted_root. A single valid path is sufficient.
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
		if v.trustedRoot != nil && len(sig.CertPEM) > 0 {
			if certErr := v.verifyCertSignature(raw, sig); certErr == nil {
				return artifact.Result{
					Status:  artifact.StatusPass,
					Tier:    artifact.TierSigned,
					Message: fmt.Sprintf("cosign: signature verified for %s via Fulcio certificate against trusted_root (tlog disabled; no Rekor)", refKey(ref)),
				}, nil
			}
		}
	}

	parts := []string{}
	if n := len(v.verifiers); n > 0 {
		parts = append(parts, fmt.Sprintf("%d configured key(s)", n))
	}
	if v.trustedRoot != nil {
		parts = append(parts, "trusted_root Fulcio CAs")
	}
	anchor := "configured anchors"
	if len(parts) > 0 {
		anchor = strings.Join(parts, " / ")
	}
	return artifact.Result{
		Status:  artifact.StatusFail,
		Tier:    artifact.TierSigned,
		Message: fmt.Sprintf("cosign: no attached signature verified against %s for %s", anchor, refKey(ref)),
	}, nil
}

// verifyCertSignature validates the Fulcio leaf (and optional chain) against
// trusted_root, then verifies the signature with the leaf public key.
func (v *CosignVerifier) verifyCertSignature(rawSig []byte, sig CosignSignature) error {
	leaf, err := parseLeafCertPEM(sig.CertPEM)
	if err != nil {
		return fmt.Errorf("parse leaf certificate: %w", err)
	}
	if err := v.trustedRoot.VerifyCertificateChain(leaf, sig.ChainPEM, v.clock()); err != nil {
		return err
	}
	ver, err := signature.LoadVerifier(leaf.PublicKey, crypto.SHA256)
	if err != nil {
		return fmt.Errorf("load verifier from leaf public key: %w", err)
	}
	if err := ver.VerifySignature(bytes.NewReader(rawSig), bytes.NewReader(sig.Payload)); err != nil {
		return fmt.Errorf("leaf signature verify: %w", err)
	}
	return nil
}
