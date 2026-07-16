package verify

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// GPGVerifier verifies the apt end-to-end GPG signature chain against a local,
// out-of-band keyring (DESIGN-REVIEW §1.1 — the apt "signed" gold standard):
//
//	local keyring  →  InRelease detached/clear-signed by a distro key
//	InRelease      →  SHA256 of each Packages/Release index file
//	Packages       →  SHA256 of each pool/*.deb
//
// Because the keyring is shipped out-of-band with the OS (never from the mirror)
// a malicious mirror cannot forge the chain: it lacks the distro private key.
// This makes apt authenticity fully offline-verifiable and transport-agnostic.
//
// # Tier
//
// Tier() is TierSigned: a passing GPG chain attests real publisher authenticity,
// not mere transport integrity.
//
// # Self-gating
//
// Verify is a no-op StatusPass for any ref whose Protocol is not "apt": a single
// global verification Chain may include this verifier without it acting on other
// protocols.
//
// # Chain state
//
// The verifier keeps an in-memory chain state that is populated as artifacts flow
// through the verify-on-write pipeline in request order:
//
//  1. InRelease is verified (GPG signature) → SHA256s of Packages files are cached.
//  2. Packages is verified (SHA256 from InRelease) → SHA256s of .deb files are cached.
//  3. .deb is verified (SHA256 from Packages) → TierSigned PASS returned.
//
// Failing to find prerequisite data (InRelease not yet verified, Packages not yet
// verified) causes a fail-closed StatusFail so no artifact is promoted without a
// complete chain.
type GPGVerifier struct {
	// keyring holds the trusted distro signing keys parsed from the keyring file.
	keyring openpgp.EntityList
	// keyringPath is retained for diagnostics/messages.
	keyringPath string

	// mu guards the in-memory chain state below.
	mu sync.RWMutex

	// suiteSHA256s maps cache key ("repo:suite") → (relative-to-suite-path → sha256hex).
	// Populated when an InRelease file is successfully GPG-verified.
	// Example key: ":noble" for a root-mounted Ubuntu noble repo.
	// Example inner key: "main/binary-amd64/Packages" → "abc123…"
	suiteSHA256s map[string]map[string]string

	// poolSHA256s maps pool path ("pool/component/dir/file.deb") → sha256hex.
	// Populated when a Packages file is successfully SHA256-verified against InRelease.
	// All repo Packages share the same flat namespace since pool paths are globally unique.
	poolSHA256s map[string]string
}

// NewGPGVerifier loads the GPG keyring at keyringPath (armored or binary) and
// returns a verifier anchored on those keys. An empty path or an unreadable /
// unparseable keyring is a fatal wiring error (fail-fast, never silently trust).
func NewGPGVerifier(keyringPath string) (*GPGVerifier, error) {
	if keyringPath == "" {
		return nil, fmt.Errorf("gpg: keyring path is required for apt signed verification")
	}
	el, err := loadKeyring(keyringPath)
	if err != nil {
		return nil, fmt.Errorf("gpg: load keyring %q: %w", keyringPath, err)
	}
	if len(el) == 0 {
		return nil, fmt.Errorf("gpg: keyring %q contains no keys", keyringPath)
	}
	return &GPGVerifier{
		keyring:      el,
		keyringPath:  keyringPath,
		suiteSHA256s: make(map[string]map[string]string),
		poolSHA256s:  make(map[string]string),
	}, nil
}

// Compile-time assertion that GPGVerifier satisfies Verifier.
var _ Verifier = (*GPGVerifier)(nil)

func (v *GPGVerifier) Name() string        { return "gpg" }
func (v *GPGVerifier) Tier() artifact.Tier { return artifact.TierSigned }

// Verify walks the apt GPG signature chain for the quarantined artifact.
//
// Skipped (StatusPass at TierChecksum) for any non-apt ref. For apt refs:
//   - Mutable dists/ path ending in "InRelease": verifies the PGP clear-signed
//     message against the local keyring, then caches the SHA256 sums for
//     subsequent Packages verifications.
//   - Mutable dists/ Packages file: verifies the artifact's SHA256 against the
//     InRelease chain state, then caches .deb SHA256s for pool verification.
//   - Immutable pool/ .deb: verifies the artifact's SHA256 against the Packages
//     chain state, returning TierSigned on success.
//   - Other dists/ files (Release, Release.gpg, Sources, ...): pass-through at
//     TierChecksum (SHA256 handled by ChecksumVerifier if ref.Digest is set).
func (v *GPGVerifier) Verify(ctx context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	if ref.Protocol != "apt" {
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: "gpg: skipped (not an apt artifact)",
		}, nil
	}

	if ref.Mutable {
		return v.verifyDists(ref, art)
	}
	return v.verifyPool(ref, art)
}

// --------------------------------------------------------------------------
// dists/ verification
// --------------------------------------------------------------------------

// verifyDists routes mutable dists/ artifacts by file type.
func (v *GPGVerifier) verifyDists(ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	version := ref.Version

	switch {
	case strings.HasSuffix(version, "InRelease"):
		return v.verifyInRelease(ref, art)
	case isPackagesFile(version):
		return v.verifyPackages(ref, art)
	default:
		// Release, Release.gpg, Sources, Translation-*, Contents-*, etc.:
		// SHA256 integrity is optionally covered by ChecksumVerifier via ref.Digest.
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: fmt.Sprintf("gpg: dists file %q not GPG-chain verifiable (InRelease covers SHA256)", version),
		}, nil
	}
}

// isPackagesFile returns true when the last path component looks like a Packages
// index (compressed or not), which is the file type whose SHA256 is pinned in
// InRelease and which itself pins .deb SHA256s.
func isPackagesFile(version string) bool {
	base := version
	if i := strings.LastIndexByte(version, '/'); i >= 0 {
		base = version[i+1:]
	}
	// Packages, Packages.gz, Packages.xz, Packages.bz2
	return base == "Packages" ||
		strings.HasPrefix(base, "Packages.") ||
		base == "Sources" ||
		strings.HasPrefix(base, "Sources.")
}

// verifyInRelease reads the InRelease clear-signed PGP message from the
// quarantine file, verifies the signature against the local keyring, and
// populates the suiteSHA256s cache so subsequent Packages verifications can
// confirm their content hash against the signed release.
func (v *GPGVerifier) verifyInRelease(ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	data, err := os.ReadFile(art.Path)
	if err != nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: read InRelease quarantine file: %v", err),
		}, fmt.Errorf("gpg: read InRelease: %w", err)
	}

	// Parse the clear-signed PGP message. Decode returns nil on malformed input.
	block, _ := clearsign.Decode(data)
	if block == nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: "gpg: InRelease is not a valid clear-signed PGP message",
		}, nil
	}

	// Verify the embedded PGP signature against the local keyring.
	if _, err := block.VerifySignature(v.keyring, nil); err != nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: InRelease GPG signature verification failed: %v", err),
		}, nil
	}

	// Parse SHA256 sums from the signed plaintext.
	sha256s := parseInReleaseSHA256s(block.Plaintext)

	// Derive suite from the dists path: "noble/InRelease" → "noble".
	suite := strings.TrimSuffix(ref.Version, "/InRelease")
	if suite == ref.Version {
		// Fallback for "InRelease" at the root.
		suite = ""
	}
	cacheKey := ref.Name + ":" + suite

	v.mu.Lock()
	v.suiteSHA256s[cacheKey] = sha256s
	v.mu.Unlock()

	return artifact.Result{
		Status:  artifact.StatusPass,
		Tier:    artifact.TierSigned,
		Message: fmt.Sprintf("gpg: InRelease GPG verified for suite %q (%d file hashes pinned, key=%q)", suite, len(sha256s), cacheKey),
	}, nil
}

// verifyPackages verifies the quarantine SHA256 of a Packages file against the
// SHA256 pinned in the InRelease chain state. On success, it parses the Packages
// content to populate the poolSHA256s cache for subsequent .deb verifications.
func (v *GPGVerifier) verifyPackages(ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	// Derive suite and relative path from the dists version string.
	// Example: ref.Version = "noble/main/binary-amd64/Packages"
	//          → suite = "noble", relPath = "main/binary-amd64/Packages"
	idx := strings.IndexByte(ref.Version, '/')
	if idx < 0 {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: Packages path %q has no suite component", ref.Version),
		}, nil
	}
	suite := ref.Version[:idx]
	relPath := ref.Version[idx+1:]
	cacheKey := ref.Name + ":" + suite

	// Look up expected SHA256 from InRelease chain state.
	v.mu.RLock()
	sums, ok := v.suiteSHA256s[cacheKey]
	v.mu.RUnlock()

	if !ok {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: InRelease not yet verified for suite %q (key=%q) — cannot verify Packages chain", suite, cacheKey),
		}, nil
	}

	expectedHex, listed := sums[relPath]
	if !listed {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: Packages file %q not listed in InRelease SHA256 sums (suite=%q, key=%q)", relPath, suite, cacheKey),
		}, nil
	}

	// art.Digest is "sha256:<hex>"; compare against the InRelease hex.
	actualHex := strings.TrimPrefix(art.Digest, "sha256:")
	if actualHex != expectedHex {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: Packages %q SHA256 mismatch: got %s, expected %s (chain: InRelease→Packages)", relPath, actualHex, expectedHex),
		}, nil
	}

	// Parse Packages content to extract pool .deb SHA256s.
	data, err := os.ReadFile(art.Path)
	if err != nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: read Packages quarantine file: %v", err),
		}, fmt.Errorf("gpg: read Packages: %w", err)
	}

	poolHashes := parsePackagesSHA256s(data)

	// Merge into the shared pool SHA256s map.
	v.mu.Lock()
	for poolPath, h := range poolHashes {
		v.poolSHA256s[poolPath] = h
	}
	v.mu.Unlock()

	return artifact.Result{
		Status:  artifact.StatusPass,
		Tier:    artifact.TierSigned,
		Message: fmt.Sprintf("gpg: Packages %q SHA256 chain-verified (InRelease→Packages) — %d .deb hashes pinned", relPath, len(poolHashes)),
	}, nil
}

// --------------------------------------------------------------------------
// pool/ verification
// --------------------------------------------------------------------------

// verifyPool verifies a pool .deb artifact's SHA256 against the sha256 stored in
// the Packages index (which was itself verified against InRelease). All three
// links must have been traversed in order for a PASS to be returned.
func (v *GPGVerifier) verifyPool(ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	// Pool path is "pool/<name>/<version>" matching the "Filename:" field in Packages.
	poolPath := "pool/" + ref.Name + "/" + ref.Version

	v.mu.RLock()
	expectedHex, ok := v.poolSHA256s[poolPath]
	v.mu.RUnlock()

	if !ok {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: pool file %q not found in verified Packages index (request Packages before .deb)", poolPath),
		}, nil
	}

	actualHex := strings.TrimPrefix(art.Digest, "sha256:")
	if actualHex != expectedHex {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: pool file %q SHA256 mismatch: got %s, expected %s (chain: InRelease→Packages→.deb)", poolPath, actualHex, expectedHex),
		}, nil
	}

	return artifact.Result{
		Status:  artifact.StatusPass,
		Tier:    artifact.TierSigned,
		Message: fmt.Sprintf("gpg: pool file %q SHA256 chain-verified (InRelease→Packages→.deb, TierSigned)", poolPath),
	}, nil
}

// --------------------------------------------------------------------------
// Parsing helpers
// --------------------------------------------------------------------------

// parseInReleaseSHA256s extracts the SHA256 field from the signed plaintext of
// an InRelease file. Returns a map from relative-to-suite file path to SHA256 hex.
//
// The SHA256 section format is:
//
//	SHA256:
//	 <sha256hex> <size> <relpath>
//	 <sha256hex> <size> <relpath>
func parseInReleaseSHA256s(plaintext []byte) map[string]string {
	result := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(plaintext))
	inSHA256 := false

	for scanner.Scan() {
		line := scanner.Text()
		if line == "SHA256:" {
			inSHA256 = true
			continue
		}
		if inSHA256 {
			if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				// " <sha256hex> <size> <relpath>"
				fields := strings.Fields(strings.TrimLeft(line, " \t"))
				if len(fields) == 3 {
					sha256hex := fields[0]
					relPath := fields[2]
					result[relPath] = sha256hex
				}
			} else {
				// Non-indented line ends the SHA256 section.
				inSHA256 = false
			}
		}
	}
	return result
}

// parsePackagesSHA256s extracts Filename and SHA256 from a debian Packages index.
// Returns a map from pool path (e.g. "pool/main/l/libc6/libc6_2.35_amd64.deb")
// to SHA256 hex.
//
// Packages files consist of RFC822-style stanzas separated by blank lines.
func parsePackagesSHA256s(data []byte) map[string]string {
	result := make(map[string]string)
	var filename, sha256sum string

	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Increase the scanner buffer for large Packages files.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	commit := func() {
		if filename != "" && sha256sum != "" {
			result[filename] = sha256sum
		}
		filename, sha256sum = "", ""
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			commit()
			continue
		}
		switch {
		case strings.HasPrefix(line, "Filename: "):
			filename = strings.TrimPrefix(line, "Filename: ")
		case strings.HasPrefix(line, "SHA256: "):
			sha256sum = strings.TrimPrefix(line, "SHA256: ")
		}
	}
	// Commit the last stanza (Packages files may or may not end with a blank line).
	commit()
	return result
}

// loadKeyring reads an OpenPGP keyring from path, trying armored (ASCII) first
// and falling back to the binary format. Shared by the apt and Helm .prov
// verifiers.
func loadKeyring(path string) (openpgp.EntityList, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	el, armErr := openpgp.ReadArmoredKeyRing(f)
	if armErr == nil {
		return el, nil
	}
	// Rewind and retry as a binary keyring.
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	el, binErr := openpgp.ReadKeyRing(f)
	if binErr != nil {
		return nil, fmt.Errorf("not a valid armored (%v) or binary (%v) keyring", armErr, binErr)
	}
	return el, nil
}
