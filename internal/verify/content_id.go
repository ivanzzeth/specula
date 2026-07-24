package verify

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
)

// Identity modes for ConsensusVerifier. CAS compares mirror votes to the
// streaming sha256 art.Digest (OCI / PyPI). ContentID quorums on
// mirror-advertised content identities (npm SSRI integrity, cargo cksum) and
// binds the quarantine body to that identity — never equating sha512 integrity
// to the CAS sha256 digest.
const (
	IdentityCAS       = "cas"
	IdentityContentID = "content_id"
)

// verifyBodyContentID checks that the quarantine file at path matches the
// advertised content identity. Supported forms:
//
//   - SSRI: "sha512-<base64>", "sha256-<base64>", "sha384-<base64>" (npm)
//   - CAS-style: "sha256:<hex>"
//   - Bare 64-char hex (cargo index cksum) — treated as sha256
func verifyBodyContentID(path, contentID string) error {
	contentID = strings.TrimSpace(contentID)
	if contentID == "" {
		return fmt.Errorf("empty content id")
	}
	if path == "" {
		return fmt.Errorf("quarantine path is empty")
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open quarantine: %w", err)
	}
	defer func() { _ = f.Close() }()

	switch {
	case strings.Contains(contentID, "-") && !strings.Contains(contentID, ":"):
		return verifySSRI(f, contentID)
	case strings.HasPrefix(contentID, "sha256:"):
		return verifyHexDigest(f, sha256.New(), strings.TrimPrefix(contentID, "sha256:"), "sha256")
	case looksLikeSHA256Hex(contentID):
		return verifyHexDigest(f, sha256.New(), contentID, "sha256")
	default:
		return fmt.Errorf("unsupported content id form %q", contentID)
	}
}

func looksLikeSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func verifyHexDigest(r io.Reader, h hash.Hash, wantHex, algo string) error {
	if _, err := io.Copy(h, r); err != nil {
		return fmt.Errorf("hash body: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, wantHex) {
		return fmt.Errorf("%s body mismatch: got %s, want %s", algo, got, strings.ToLower(wantHex))
	}
	return nil
}

// verifySSRI validates Subresource Integrity (npm dist.integrity): algo-base64.
func verifySSRI(r io.Reader, integrity string) error {
	dash := strings.IndexByte(integrity, '-')
	if dash <= 0 || dash == len(integrity)-1 {
		return fmt.Errorf("malformed SSRI %q", integrity)
	}
	algo := integrity[:dash]
	wantB64 := integrity[dash+1:]
	want, err := base64.StdEncoding.DecodeString(wantB64)
	if err != nil {
		// npm sometimes uses URL-safe base64 without padding.
		want, err = base64.RawURLEncoding.DecodeString(wantB64)
		if err != nil {
			return fmt.Errorf("SSRI base64: %w", err)
		}
	}

	var h hash.Hash
	switch algo {
	case "sha256":
		h = sha256.New()
	case "sha384":
		h = sha512.New384()
	case "sha512":
		h = sha512.New()
	default:
		return fmt.Errorf("unsupported SSRI algorithm %q", algo)
	}
	if _, err := io.Copy(h, r); err != nil {
		return fmt.Errorf("hash body: %w", err)
	}
	got := h.Sum(nil)
	if len(got) != len(want) {
		return fmt.Errorf("SSRI %s length mismatch", algo)
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Errorf("SSRI %s body mismatch", algo)
		}
	}
	return nil
}

// contentIDsEqual compares advertised content identities. Unlike digestsEqual
// (CAS sha256 only), this requires an exact string match so sha512-… is never
// quietly equated with sha256:….
func contentIDsEqual(a, b string) bool {
	return a != "" && b != "" && a == b
}
