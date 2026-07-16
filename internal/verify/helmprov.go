package verify

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// HelmProvVerifier verifies a Helm chart provenance file (`<chart>.tgz.prov`):
// a clear-signed document containing the chart's SHA256 plus metadata, signed
// with a GPG key whose public half lives in a local, out-of-band keyring
// (DESIGN-REVIEW §1.1 — a "signed" gold standard on par with apt).
//
//	local keyring  →  detached/clear GPG signature over the .prov body
//	.prov body     →  "files:" block SHA256 of the chart <chart>.tgz
//
// A mirror cannot forge the signature without the publisher's private key, so a
// passing check attests real publisher authenticity, not transport integrity.
//
// # Provenance format
//
// The .prov file is a PGP clear-signed document whose plaintext body is a YAML
// block that mirrors the chart's Chart.yaml, followed by a "files:" mapping
// whose values are "sha256:<hex>":
//
//	files:
//	  <chart>.tgz: sha256:<64-char-hex>
//
// This is the exact format produced and consumed by helm.sh/helm/v3/pkg/provenance
// (see pkg/provenance/provenance.go GenerateKey / Verify). We implement the same
// verification without importing helm.sh/helm/v3 (which drags in k8s.io and bumps
// the Go floor — see DEPS phase note). The cryptographic primitive used is
// ProtonMail/go-crypto's clearsign package, which is the official successor to
// the deprecated golang.org/x/crypto/openpgp used by older helm versions.
//
// # Tier
//
// Tier() is TierSigned. When no .prov attachment is present the caller
// receives StatusWarn at TierChecksum (degrade, not fail) — a missing signature
// is a tier downgrade, not a rejection.
//
// # Self-gating
//
// Verify is a no-op StatusPass for any ref whose Protocol is not "helm" or
// whose Version does not end with ".tgz" (i.e. .prov files and index.yaml
// are skipped — they are not the artifact being integrity-checked here).
type HelmProvVerifier struct {
	keyring     openpgp.EntityList
	keyringPath string
}

// NewHelmProvVerifier loads the GPG keyring at keyringPath (armored or binary)
// and returns a verifier anchored on those keys. An empty path or an
// unreadable/unparseable keyring is a fatal wiring error.
func NewHelmProvVerifier(keyringPath string) (*HelmProvVerifier, error) {
	if keyringPath == "" {
		return nil, fmt.Errorf("helmprov: keyring path is required for .prov signed verification")
	}
	el, err := loadKeyring(keyringPath)
	if err != nil {
		return nil, fmt.Errorf("helmprov: load keyring %q: %w", keyringPath, err)
	}
	if len(el) == 0 {
		return nil, fmt.Errorf("helmprov: keyring %q contains no keys", keyringPath)
	}
	return &HelmProvVerifier{keyring: el, keyringPath: keyringPath}, nil
}

// Compile-time assertion that HelmProvVerifier satisfies Verifier.
var _ Verifier = (*HelmProvVerifier)(nil)

func (v *HelmProvVerifier) Name() string        { return "helmprov" }
func (v *HelmProvVerifier) Tier() artifact.Tier { return artifact.TierSigned }

// Verify checks the .prov clear-signed GPG provenance for a quarantined chart.
//
// Self-gating rules (returns StatusPass/TierChecksum):
//   - ref.Protocol != "helm"
//   - ref.Mutable == true  (index.yaml, not a chart)
//   - ref.Version does not end with ".tgz"  (e.g. the .prov file itself)
//
// When no .prov attachment is in art.Meta.Attachments[0] the verifier returns
// StatusWarn/TierChecksum (PRD §信任模型: "无 .prov 降级"). The chain
// continues; higher-tier verifiers (e.g. TofuVerifier) may still run.
//
// When a .prov is present the full chain is run:
//  1. Parse the clear-signed document.
//  2. Verify the GPG signature against the loaded keyring.
//  3. Extract the chart SHA256 from the "files:" block.
//  4. Compare it with art.Digest (computed during streaming quarantine).
//
// A signature check failure or a digest mismatch returns StatusFail/TierSigned.
func (v *HelmProvVerifier) Verify(_ context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	// Self-gate 1: non-helm protocol.
	if ref.Protocol != "helm" {
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: "helmprov: skipped (not a helm artifact)",
		}, nil
	}
	// Self-gate 2: mutable refs (index.yaml).
	if ref.Mutable {
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: "helmprov: skipped (mutable ref — index.yaml, not a chart)",
		}, nil
	}
	// Self-gate 3: only process .tgz files, not .prov or other extensions.
	if !strings.HasSuffix(ref.Version, ".tgz") {
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: "helmprov: skipped (not a .tgz chart artifact)",
		}, nil
	}

	// Check for an attached .prov file (placed in Attachments[0] by the handler
	// when it successfully fetched the .prov from upstream).
	if len(art.Meta.Attachments) == 0 || len(art.Meta.Attachments[0]) == 0 {
		// No .prov available — degrade to TierChecksum with a WARN.
		return artifact.Result{
			Status:  artifact.StatusWarn,
			Tier:    artifact.TierChecksum,
			Message: fmt.Sprintf("helmprov: no .prov attachment for %s — degraded to checksum tier", ref.Version),
		}, nil
	}

	provBytes := art.Meta.Attachments[0]
	return v.verifyProv(provBytes, ref, art)
}

// verifyProv is the inner implementation once we have confirmed there is a
// .prov attachment available.
func (v *HelmProvVerifier) verifyProv(provBytes []byte, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	// 1. Parse the clear-signed PGP document.
	block, _ := clearsign.Decode(provBytes)
	if block == nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("helmprov: %s .prov is not a valid clear-signed PGP document", ref.Version),
		}, nil
	}

	// 2. Verify the GPG signature against the keyring.
	// block.VerifySignature uses openpgp.VerifyDetachedSignature over block.Bytes.
	_, err := block.VerifySignature(v.keyring, nil)
	if err != nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("helmprov: GPG signature verification failed for %s: %v", ref.Version, err),
		}, nil
	}

	// 3. Extract the expected chart SHA256 from the "files:" block.
	expectedDigest, err := extractChartDigest(block.Plaintext, ref.Version)
	if err != nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("helmprov: failed to extract chart digest from .prov for %s: %v", ref.Version, err),
		}, nil
	}

	// 4. Compare the extracted digest against the streaming-computed art.Digest.
	// art.Digest is in the form "sha256:<hex>" computed by cache.Quarantine.
	if !digestsMatch(expectedDigest, art.Digest) {
		return artifact.Result{
			Status: artifact.StatusFail,
			Tier:   artifact.TierSigned,
			Message: fmt.Sprintf(
				"helmprov: chart digest mismatch for %s: .prov says %s, actual is %s",
				ref.Version, expectedDigest, art.Digest,
			),
		}, nil
	}

	return artifact.Result{
		Status:  artifact.StatusPass,
		Tier:    artifact.TierSigned,
		Message: fmt.Sprintf("helmprov: GPG signature verified, chart digest confirmed %s → %s", ref.Version, art.Digest),
	}, nil
}

// extractChartDigest parses the plaintext body of a Helm provenance document
// and returns the sha256 digest for chartFilename from the "files:" block.
//
// The expected format inside the "files:" block is:
//
//	files:
//	  <chart>.tgz: sha256:<hexdigest>
//
// Returns an error when no matching entry with a sha256 prefix is found.
func extractChartDigest(plaintext []byte, chartFilename string) (string, error) {
	lines := strings.Split(string(plaintext), "\n")
	inFiles := false

	for _, raw := range lines {
		// Normalise line endings.
		line := strings.TrimRight(raw, "\r")

		// Detect the start of the "files:" block.
		if strings.TrimSpace(line) == "files:" {
			inFiles = true
			continue
		}
		if !inFiles {
			continue
		}
		// A non-indented, non-empty line after "files:" ends the block.
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
			break
		}
		// Parse indented entry: "  <name>: <digest>"
		entry := strings.TrimSpace(line)
		if entry == "" {
			continue
		}
		// Split on the first ": " to separate filename from digest.
		colonSpace := strings.Index(entry, ": ")
		if colonSpace < 0 {
			continue
		}
		name := entry[:colonSpace]
		digest := strings.TrimSpace(entry[colonSpace+2:])

		if name != chartFilename {
			continue
		}
		if !strings.HasPrefix(digest, "sha256:") {
			return "", fmt.Errorf("helmprov: digest for %q is not sha256 format: %q", chartFilename, digest)
		}
		return digest, nil
	}

	return "", fmt.Errorf("helmprov: no entry for %q found in files: block", chartFilename)
}

// digestsMatch reports whether d1 and d2 refer to the same sha256 digest,
// tolerating the optional "sha256:" prefix on either side.
func digestsMatch(d1, d2 string) bool {
	norm := func(d string) string {
		return strings.TrimPrefix(d, "sha256:")
	}
	h1 := norm(d1)
	h2 := norm(d2)
	return h1 != "" && h1 == h2 && bytes.Equal([]byte(h1), []byte(h2))
}
