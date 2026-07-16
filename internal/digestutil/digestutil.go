// Package digestutil provides algorithm-agnostic handling of OCI content
// digests of the form "<algorithm>:<hex>". The OCI Distribution/Image specs
// register sha256 (canonical), sha384 and sha512; a conformant registry must
// accept and address content by the client's declared algorithm rather than
// assuming sha256.
//
// Only the Go standard library is used (crypto/sha256, crypto/sha512), so no
// new module dependency is required.
package digestutil

import (
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"hash"
	"strings"
)

// algoHexLen maps each registered digest algorithm to the exact length of its
// lower-case hex encoding. An algorithm absent from this map is unsupported.
var algoHexLen = map[string]int{
	"sha256": 64,
	"sha384": 96,
	"sha512": 128,
}

// Split parses "<algo>:<hex>" into its algorithm and hex halves. ok is false
// when the string lacks a single non-empty algorithm and hex component.
func Split(d string) (algo, hexStr string, ok bool) {
	i := strings.IndexByte(d, ':')
	if i <= 0 || i >= len(d)-1 {
		return "", "", false
	}
	return d[:i], d[i+1:], true
}

// isLowerHex reports whether s is a non-empty lower-case hex string.
func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return len(s) > 0
}

// Validate reports whether d is a well-formed digest ("<algo>:<hex>") for a
// registered, supported algorithm with a correctly-sized lower-case hex payload.
func Validate(d string) error {
	algo, hexStr, ok := Split(d)
	if !ok {
		return fmt.Errorf("digestutil: %q is not of the form algorithm:hex", d)
	}
	want, known := algoHexLen[algo]
	if !known {
		return fmt.Errorf("digestutil: unsupported digest algorithm %q", algo)
	}
	if len(hexStr) != want || !isLowerHex(hexStr) {
		return fmt.Errorf("digestutil: invalid %s hex %q", algo, hexStr)
	}
	return nil
}

// IsValid is the boolean form of Validate.
func IsValid(d string) bool { return Validate(d) == nil }

// NewHasher returns a fresh hash.Hash for the given algorithm ("sha256",
// "sha384" or "sha512"), or an error for an unsupported algorithm.
func NewHasher(algo string) (hash.Hash, error) {
	switch algo {
	case "sha256":
		return sha256.New(), nil
	case "sha384":
		return sha512.New384(), nil
	case "sha512":
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("digestutil: unsupported digest algorithm %q", algo)
	}
}

// HasherFor returns a fresh hash.Hash for the algorithm of digest d.
func HasherFor(d string) (hash.Hash, error) {
	algo, _, ok := Split(d)
	if !ok {
		return nil, fmt.Errorf("digestutil: %q is not of the form algorithm:hex", d)
	}
	return NewHasher(algo)
}
