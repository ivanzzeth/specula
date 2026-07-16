package digestutil_test

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/ivanzzeth/specula/internal/digestutil"
)

// hexOf returns a lower-case hex string of n characters.
func hexOf(n int) string { return strings.Repeat("a", n) }

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		name          string
		in            string
		algo, hexPart string
		ok            bool
	}{
		{"well formed", "sha256:abc", "sha256", "abc", true},
		{"no separator", "sha256", "", "", false},
		{"empty algorithm", ":abc", "", "", false},
		{"empty hex", "sha256:", "", "", false},
		{"empty string", "", "", "", false},
		{"only separator", ":", "", "", false},
		{"extra separators belong to the hex half", "sha256:a:b", "sha256", "a:b", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			algo, hexPart, ok := digestutil.Split(tc.in)
			if ok != tc.ok || algo != tc.algo || hexPart != tc.hexPart {
				t.Errorf("Split(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.in, algo, hexPart, ok, tc.algo, tc.hexPart, tc.ok)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		ok   bool
	}{
		// The OCI image spec registers exactly these three algorithms.
		{"sha256", "sha256:" + hexOf(64), true},
		{"sha384", "sha384:" + hexOf(96), true},
		{"sha512", "sha512:" + hexOf(128), true},

		{"sha256 too short", "sha256:" + hexOf(63), false},
		{"sha256 too long", "sha256:" + hexOf(65), false},
		{"sha512 with sha256 length", "sha512:" + hexOf(64), false},
		{"unregistered algorithm", "md5:" + hexOf(32), false},
		{"uppercase hex is not canonical", "sha256:" + strings.ToUpper(hexOf(64)), false},
		{"non-hex characters", "sha256:" + strings.Repeat("z", 64), false},
		{"missing separator", "sha256" + hexOf(64), false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := digestutil.Validate(tc.in)
			if (err == nil) != tc.ok {
				t.Errorf("Validate(%q) error = %v, want ok=%v", tc.in, err, tc.ok)
			}
			if got := digestutil.IsValid(tc.in); got != tc.ok {
				t.Errorf("IsValid(%q) = %v, want %v", tc.in, got, tc.ok)
			}
		})
	}
}

// TestNewHasherMatchesStdlib verifies each algorithm's hasher produces the same
// digest the standard library does — the whole point of the package is that the
// bytes are addressed under the algorithm the client actually asked for.
func TestNewHasherMatchesStdlib(t *testing.T) {
	content := []byte("specula conformance content")

	sum256 := sha256.Sum256(content)
	sum384 := sha512.Sum384(content)
	sum512 := sha512.Sum512(content)

	for _, tc := range []struct {
		algo string
		want string
	}{
		{"sha256", hex.EncodeToString(sum256[:])},
		{"sha384", hex.EncodeToString(sum384[:])},
		{"sha512", hex.EncodeToString(sum512[:])},
	} {
		t.Run(tc.algo, func(t *testing.T) {
			h, err := digestutil.NewHasher(tc.algo)
			if err != nil {
				t.Fatalf("NewHasher(%q): %v", tc.algo, err)
			}
			if _, err := h.Write(content); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got := hex.EncodeToString(h.Sum(nil)); got != tc.want {
				t.Errorf("%s = %s, want %s", tc.algo, got, tc.want)
			}
		})
	}
}

func TestNewHasherUnsupported(t *testing.T) {
	for _, algo := range []string{"md5", "sha1", "", "SHA256"} {
		if _, err := digestutil.NewHasher(algo); err == nil {
			t.Errorf("NewHasher(%q) = nil error, want an unsupported-algorithm error", algo)
		}
	}
}

// TestHasherFor verifies the hasher is selected from the digest's own algorithm
// prefix, which is how the upload path honours a client's declared algorithm.
func TestHasherFor(t *testing.T) {
	content := []byte("addressed by the client's chosen algorithm")
	sum := sha512.Sum512(content)
	want := hex.EncodeToString(sum[:])

	h, err := digestutil.HasherFor("sha512:" + hexOf(128))
	if err != nil {
		t.Fatalf("HasherFor: %v", err)
	}
	if _, err := h.Write(content); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		t.Errorf("HasherFor sha512 produced %s, want %s", got, want)
	}
}

func TestHasherForRejectsMalformed(t *testing.T) {
	for _, d := range []string{"", "sha256", "md5:" + hexOf(32)} {
		if _, err := digestutil.HasherFor(d); err == nil {
			t.Errorf("HasherFor(%q) = nil error, want an error", d)
		}
	}
}
