package verify

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ulikunitz/xz"
	debcontrol "pault.ag/go/debian/control"

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
	// Acquire-By-Hash: rewrite a content-addressed index request back to the
	// canonical path InRelease pins, then route normally (see resolveByHashRef).
	ref = v.resolveByHashRef(ref)
	version := ref.Version

	switch {
	case strings.HasSuffix(version, "InRelease"):
		return v.verifyInRelease(ref, art)
	case isPackagesFile(version):
		return v.verifyPackages(ref, art)
	default:
		// Release.gpg, Translation-*, Contents-*, DEP11 cnf, etc.: these files
		// are listed in the InRelease SHA256 section. If InRelease has been
		// GPG-verified for this suite, verify the file's SHA256 against the
		// pinned value and return TierSigned. If InRelease has not yet been
		// seen (or the file is not listed), fall through at TierChecksum so the
		// pipeline is not blocked (ChecksumVerifier + TofuVerifier still apply).
		return v.verifyInReleasePin(ref, art)
	}
}

// maxIndexPlaintextBytes caps the decompressed size of a Packages/Sources index.
// noble/main/binary-amd64/Packages is ~50 MB plaintext; 512 MB leaves generous
// headroom for the largest suites while bounding a decompression bomb.
const maxIndexPlaintextBytes = int64(512 << 20)

// byHashSHA256Marker is the path element apt inserts for content-addressed index
// fetches under `Acquire-By-Hash: yes`: <dir>/by-hash/SHA256/<hex>.
const byHashSHA256Marker = "/by-hash/SHA256/"

// resolveByHashRef normalises an Acquire-By-Hash index request back to the
// canonical suite-relative path that InRelease actually pins, so all downstream
// routing and chain logic works unchanged.
//
// Why this is not a downgrade of the trust chain (PRD §G2, DESIGN-REVIEW §3):
// by-hash exists precisely so apt fetches EXACTLY the index the signature covers.
// The requested hash IS the pin. We do not trust the requester's hash: we accept
// it only if that hex appears among the SHA256 sums of a GPG-VERIFIED InRelease
// for this suite, in the same directory. Resolution therefore proves the fetched
// address is signed; the existing `actualHex != expectedHex` check then proves
// the mirror returned the bytes that address commits to.
//
// Real apt-get update requests dists/noble/main/binary-amd64/by-hash/SHA256/37cb…
// while InRelease lists main/binary-amd64/Packages.xz and zero by-hash paths. The
// former literal `sums[relPath]` lookup missed every time, so apt — the PRD's
// end-to-end gold standard — degraded to tofu for everything it really fetches.
//
// An unresolvable by-hash path (hash not pinned, or a non-SHA256 by-hash variant
// such as by-hash/MD5Sum) is returned unchanged: it then falls through to the
// TierChecksum pass-through, which is the safe direction.
func (v *GPGVerifier) resolveByHashRef(ref artifact.ArtifactRef) artifact.ArtifactRef {
	idx := strings.IndexByte(ref.Version, '/')
	if idx < 0 {
		return ref
	}
	suite, relPath := ref.Version[:idx], ref.Version[idx+1:]

	i := strings.Index(relPath, byHashSHA256Marker)
	if i < 0 {
		return ref
	}
	dir := relPath[:i]
	hex := relPath[i+len(byHashSHA256Marker):]
	if dir == "" || hex == "" || strings.Contains(hex, "/") {
		return ref
	}

	v.mu.RLock()
	sums := v.suiteSHA256s[ref.Name+":"+suite]
	v.mu.RUnlock()
	if sums == nil {
		// InRelease not GPG-verified for this suite yet — nothing to resolve
		// against. Never guess.
		return ref
	}

	for canonical, pinnedHex := range sums {
		// Directory must match: a hash pinned for one component/architecture
		// must not authorise an index served from another path.
		if pinnedHex == hex && pathDir(canonical) == dir {
			ref.Version = suite + "/" + canonical
			return ref
		}
	}
	return ref
}

// pathDir returns the directory part of a slash-separated relative path, or ""
// when there is none. (path.Dir returns "." for a bare name; "" is the useful
// answer here since a bare name has no by-hash directory.)
func pathDir(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return ""
	}
	return p[:i]
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
//
// GPG verification and RFC2822 paragraph parsing are both delegated to
// pault.ag/go/debian/control.NewParagraphReader, which uses
// ProtonMail/go-crypto under the hood — the same stack we use for helmprov.
// This replaces the former hand-rolled bufio.Scanner parse of the SHA256
// section with a spec-correct control-file parser.
func (v *GPGVerifier) verifyInRelease(ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	data, err := os.ReadFile(art.Path)
	if err != nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: read InRelease quarantine file: %v", err),
		}, fmt.Errorf("gpg: read InRelease: %w", err)
	}

	// Guard: the pault.ag/go/debian/control library silently parses plain
	// RFC2822 content as unsigned when there are no PGP headers — it does
	// NOT return an error for missing signatures. We must explicitly reject
	// unsigned InRelease files; accepting them would let an attacker inject
	// arbitrary SHA256 sums without possessing the distro private key.
	// (DESIGN-REVIEW §1.1: the signing chain MUST start at a GPG-signed root.)
	if !bytes.HasPrefix(data, []byte("-----BEGIN PGP SIGNED MESSAGE-----")) {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: "gpg: InRelease is not a PGP clear-signed document — unsigned InRelease rejected (DESIGN-REVIEW §1.1)",
		}, nil
	}

	// NewParagraphReader detects the "-----BEGIN PGP " header, verifies the
	// clear-signed GPG signature against v.keyring, and positions the reader
	// on the signed plaintext. An error means the signature is invalid or
	// the document is malformed — either case is a fatal chain break.
	pr, err := debcontrol.NewParagraphReader(bytes.NewReader(data), &v.keyring)
	if err != nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: InRelease GPG verification failed: %v", err),
		}, nil
	}

	// Read the single RFC2822 paragraph that constitutes the InRelease body.
	para, err := pr.Next()
	if err != nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: InRelease paragraph parse failed: %v", err),
		}, nil
	}

	// Parse SHA256 sums from the "SHA256:" multi-line field in the paragraph.
	// The library stores continuation lines (leading space stripped) joined by
	// "\n", so each line is "<sha256hex> <size> <relpath>".
	sha256s := parseInReleaseSHA256Field(para.Values["SHA256"])

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
	// Use pault.ag/go/debian/control.ParseBinaryIndex for spec-correct RFC2822
	// stanza parsing instead of a hand-rolled bufio.Scanner.
	data, err := os.ReadFile(art.Path)
	if err != nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: read Packages quarantine file: %v", err),
		}, fmt.Errorf("gpg: read Packages: %w", err)
	}

	// Decompress first: real mirrors serve ONLY compressed indices (aliyun 404s
	// dists/noble/main/binary-amd64/Packages and 200s Packages.xz / Packages.gz),
	// and apt prefers .xz. isPackagesFile has always matched the compressed
	// variants, but the raw bytes used to go straight to the RFC2822 parser, which
	// fail-closed on them. That was dormant while by-hash never reached here.
	plain, err := decompressIndex(relPath, data)
	if err != nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: decompress Packages index %q: %v", relPath, err),
		}, nil
	}

	entries, err := debcontrol.ParseBinaryIndex(bufio.NewReader(bytes.NewReader(plain)))
	if err != nil {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: parse Packages index %q: %v", relPath, err),
		}, nil
	}

	poolHashes := make(map[string]string, len(entries))
	for i := range entries {
		if entries[i].Filename != "" && entries[i].SHA256 != "" {
			poolHashes[entries[i].Filename] = entries[i].SHA256
		}
	}

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

// verifyInReleasePin attempts to verify a dists file that is neither InRelease
// nor a Packages/Sources index but that may be listed in the InRelease SHA256
// section (Translation-*, Contents-*, DEP11 Components, cnf, etc.).
//
//   - suite and relPath are derived from ref.Version the same way verifyPackages
//     does: "noble/main/i18n/Translation-en" → suite="noble", relPath="main/i18n/Translation-en".
//   - If InRelease has not yet been GPG-verified for this suite (suiteSHA256s
//     has no entry), returns TierChecksum PASS — the pipeline is not blocked.
//   - If InRelease was verified but the file is not listed, returns TierChecksum
//     PASS (some dists files such as Release.gpg are never in SHA256).
//   - If listed and SHA256 matches, returns TierSigned PASS.
//   - If listed but SHA256 mismatches, returns TierSigned FAIL (tamper detected).
func (v *GPGVerifier) verifyInReleasePin(ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	version := ref.Version
	idx := strings.IndexByte(version, '/')
	if idx < 0 {
		// No suite component in the path — cannot derive a cache key.
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: fmt.Sprintf("gpg: dists file %q has no suite component — pass-through at TierChecksum", version),
		}, nil
	}
	suite := version[:idx]
	relPath := version[idx+1:]
	cacheKey := ref.Name + ":" + suite

	v.mu.RLock()
	sums, hasInRelease := v.suiteSHA256s[cacheKey]
	v.mu.RUnlock()

	if !hasInRelease {
		// InRelease not yet GPG-verified for this suite — cannot chain-verify.
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: fmt.Sprintf("gpg: dists file %q not GPG-chain verifiable (InRelease not yet verified, suite=%q)", version, suite),
		}, nil
	}

	expectedHex, listed := sums[relPath]
	if !listed {
		// File is not in the InRelease SHA256 section (e.g. Release.gpg).
		return artifact.Result{
			Status:  artifact.StatusPass,
			Tier:    artifact.TierChecksum,
			Message: fmt.Sprintf("gpg: dists file %q not listed in InRelease SHA256 (suite=%q) — pass-through at TierChecksum", version, suite),
		}, nil
	}

	// File is pinned by InRelease: verify the SHA256.
	actualHex := strings.TrimPrefix(art.Digest, "sha256:")
	if actualHex != expectedHex {
		return artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierSigned,
			Message: fmt.Sprintf("gpg: dists file %q SHA256 mismatch: got %s, expected %s (InRelease chain)", relPath, actualHex, expectedHex),
		}, nil
	}

	return artifact.Result{
		Status:  artifact.StatusPass,
		Tier:    artifact.TierSigned,
		Message: fmt.Sprintf("gpg: dists file %q SHA256 chain-verified via InRelease (TierSigned)", relPath),
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

// parseInReleaseSHA256Field extracts the per-file SHA256 entries from the
// value of the "SHA256:" field as stored by pault.ag/go/debian/control's
// ParagraphReader. That library strips the leading space from each
// continuation line and joins them with "\n", so each line is:
//
//	<sha256hex> <size> <relpath>
//
// The returned map is from relative-to-suite file path to SHA256 hex.
func parseInReleaseSHA256Field(value string) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(value, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 {
			// <sha256hex> <size> <relpath>
			result[fields[2]] = fields[0]
		}
	}
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

// --------------------------------------------------------------------------
// Index decompression
// --------------------------------------------------------------------------

// decompressIndex returns the plaintext of a Packages/Sources index, decoding the
// compression implied by relPath's extension.
//
// Every real mirror serves these indices compressed and ONLY compressed —
// mirrors.aliyun.com/ubuntu returns 404 for dists/noble/main/binary-amd64/Packages
// but 200 for Packages.xz and Packages.gz — and apt prefers .xz. The SHA256 that
// InRelease pins (and that by-hash addresses) covers the COMPRESSED bytes, so the
// chain check runs on the raw bytes and only the .deb-hash extraction needs the
// plaintext. Decoding therefore happens here, after the digest has been verified:
// we never decompress unverified bytes.
//
// Unrecognised extensions are returned unchanged, which keeps an uncompressed
// index working and lets the RFC2822 parser produce the error for genuine junk.
func decompressIndex(relPath string, data []byte) ([]byte, error) {
	return decompressIndexLimit(relPath, data, maxIndexPlaintextBytes)
}

// decompressIndexLimit is decompressIndex with an injectable plaintext cap.
// The cap is a parameter rather than a bare reference to maxIndexPlaintextBytes
// so the bound can be PROVEN by a test instead of asserted by a comment:
// exercising the real 512 MB constant would mean allocating 512 MB per test.
// TestDecompressIndex_LimitIs512MB pins the production value separately, so the
// constant and the enforcement are both covered.
func decompressIndexLimit(relPath string, data []byte, limit int64) ([]byte, error) {
	var r io.Reader
	switch {
	case strings.HasSuffix(relPath, ".gz"):
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer zr.Close()
		r = zr
	case strings.HasSuffix(relPath, ".xz"):
		zr, err := xz.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("xz: %w", err)
		}
		r = zr
	case strings.HasSuffix(relPath, ".bz2"):
		r = bzip2.NewReader(bytes.NewReader(data))
	default:
		return data, nil // uncompressed
	}

	// Bounded: a decompressed Packages index is tens of MB (noble/main is ~50 MB);
	// the cap stops a decompression bomb from an untrusted mirror turning into an
	// OOM. The bytes are digest-verified but the mirror still chose their content.
	//
	// Read limit+1: reading exactly `limit` cannot distinguish a stream that ends
	// there from one that was truncated by the LimitReader, so the extra byte is
	// what makes "exceeds" mean exceeds. The former code read `limit` and rejected
	// at `len >= limit`, i.e. it refused an index of exactly the cap while its
	// error said the index had exceeded it.
	plain, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(plain)) > limit {
		return nil, fmt.Errorf("decompressed index exceeds %d bytes — refusing (possible decompression bomb)",
			limit)
	}
	return plain, nil
}
