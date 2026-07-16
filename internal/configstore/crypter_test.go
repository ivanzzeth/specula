package configstore

// Ported verbatim (translated) from ai-sandbox
// internal/controlplane/config/crypter_test.go. Their assertions judge our
// implementation: a failure here is our bug, not theirs.

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
)

// testSecret generates a valid base64(32 bytes) master key.
func testSecret(t *testing.T) string {
	t.Helper()
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.StdEncoding.EncodeToString(k[:])
}

func TestCrypterSealOpenRoundTrip(t *testing.T) {
	c, err := NewCrypter(testSecret(t))
	if err != nil {
		t.Fatalf("NewCrypter: %v", err)
	}
	if !c.Enabled() {
		t.Fatal("expected enabled")
	}
	pt := []byte("registry-token-signing-key-子串")
	sealed, err := c.Seal(pt)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, pt) {
		t.Fatal("ciphertext leaks plaintext")
	}
	got, err := c.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round trip mismatch: %q", got)
	}
}

func TestCrypterSealUsesRandomNonce(t *testing.T) {
	c, _ := NewCrypter(testSecret(t))
	a, _ := c.Seal([]byte("same"))
	b, _ := c.Seal([]byte("same"))
	if bytes.Equal(a, b) {
		t.Fatal("two seals of same plaintext should differ (random nonce)")
	}
}

func TestCrypterOpenWrongKeyFails(t *testing.T) {
	c1, _ := NewCrypter(testSecret(t))
	c2, _ := NewCrypter(testSecret(t))
	sealed, _ := c1.Seal([]byte("secret"))
	if _, err := c2.Open(sealed); err == nil {
		t.Fatal("expected Open with wrong key to fail")
	}
}

func TestCrypterOpenTooShort(t *testing.T) {
	c, _ := NewCrypter(testSecret(t))
	if _, err := c.Open([]byte("x")); !errors.Is(err, ErrCiphertextTooShort) {
		t.Fatalf("expected ErrCiphertextTooShort, got %v", err)
	}
}

// TestCrypterDisabledWhenUnset: key UNSET (empty) → disabled state, no error
// (graceful degradation for dev/e2e).
func TestCrypterDisabledWhenUnset(t *testing.T) {
	c, err := NewCrypter("")
	if err != nil {
		t.Fatalf("NewCrypter(\"\") should not error: %v", err)
	}
	if c.Enabled() {
		t.Fatal("expected disabled")
	}
	if _, err := c.Seal([]byte("x")); !errors.Is(err, ErrConfigDisabled) {
		t.Fatalf("Seal: expected ErrConfigDisabled, got %v", err)
	}
	if _, err := c.Open([]byte("0123456789ab")); !errors.Is(err, ErrConfigDisabled) {
		t.Fatalf("Open: expected ErrConfigDisabled, got %v", err)
	}
}

// TestCrypterErrorsWhenMalformed: key SET BUT MALFORMED (non-empty) → error,
// never a silent degrade to disabled. Otherwise a one-character typo in the key
// is treated as "unset" and secrets land in the database in plaintext while the
// operator believes encryption is on.
func TestCrypterErrorsWhenMalformed(t *testing.T) {
	cases := map[string]string{
		"bad base64":   "!!!not base64!!!",
		"wrong length": base64.StdEncoding.EncodeToString([]byte("too short")),
	}
	for name, secret := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := NewCrypter(secret)
			if err == nil {
				t.Fatal("NewCrypter must ERROR on a non-empty malformed key (fail-loud), got nil")
			}
			if c != nil {
				t.Fatalf("expected nil Crypter on error, got %+v", c)
			}
		})
	}
}

// TestNewMasterKeyIsUsable is Specula-local: the helper we offer operators must
// produce a key NewCrypter actually accepts (a generator that emits a key the
// loader rejects is a trap).
func TestNewMasterKeyIsUsable(t *testing.T) {
	k, err := NewMasterKey()
	if err != nil {
		t.Fatalf("NewMasterKey: %v", err)
	}
	c, err := NewCrypter(k)
	if err != nil || !c.Enabled() {
		t.Fatalf("generated key rejected by NewCrypter: err=%v enabled=%v", err, c.Enabled())
	}
}
