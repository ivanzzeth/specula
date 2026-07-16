// Package configstore is Specula's encrypted runtime-configuration KV: the
// storage substrate under internal/settings. Sensitive runtime configuration
// (session signing secrets, the registry token key) is sealed with AES-256-GCM
// before it touches the database; the master key comes only from the process
// configuration (auth.config_secret / SPECULA_AUTH__CONFIG_SECRET, base64 of 32
// bytes) and is never itself stored in the database it protects.
//
// NAMING: this package is deliberately NOT called "config". Specula already has
// internal/config — the koanf YAML/env file configuration, which is a different
// thing entirely (a static, operator-authored startup snapshot). This package is
// the *dynamic, runtime-writable, encrypted* store that the settings Resolver
// writes overrides through. Ported from ai-sandbox
// internal/controlplane/config (which had no such name collision).
//
// Design points (preserved from the reference):
//   - Master key missing → the Crypter enters a DISABLED state (Enabled()==false)
//     and every Store method returns ErrConfigDisabled rather than panicking, so
//     callers degrade gracefully (dev/e2e run fine without a key, with runtime
//     overrides simply unavailable).
//   - Master key present but MALFORMED → NewCrypter returns an error and never
//     silently degrades to disabled. A non-empty key means the operator intends
//     encryption-at-rest; treating a typo'd key as "unset" would put secrets in
//     the database in plaintext while the operator believes encryption is on.
//   - Seal(plaintext) → nonce||ciphertext (random nonce from crypto/rand);
//     Open reverses it. Values are ciphertext BLOBs in the DB; plaintext never
//     lands in storage and never enters a log line.
package configstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// ErrConfigDisabled is returned by every Store method when the Crypter holds no
// valid master key (the disabled state).
var ErrConfigDisabled = errors.New("configstore: disabled (auth.config_secret unset or invalid)")

// ErrCiphertextTooShort means the stored blob is too short to contain a nonce
// (corrupt data, or not a product of this scheme).
var ErrCiphertextTooShort = errors.New("configstore: ciphertext too short")

// MasterKeyBytes is the required decoded length of the base64 master key.
const MasterKeyBytes = 32

// Crypter seals/opens configuration values with AES-256-GCM. The zero value is
// the disabled state; construct with NewCrypter.
type Crypter struct {
	aead cipher.AEAD // nil = disabled
}

// NewCrypter builds a Crypter from a base64-encoded 32-byte master key.
//
// Key UNSET (secretB64 == "") → a disabled Crypter is returned with err == nil;
// callers decide via Enabled(). This is the graceful-degradation path for dev
// and tests.
//
// Key SET BUT MALFORMED (non-empty, but not valid base64 / not 32 bytes) → an
// ERROR is returned and the Crypter is nil. This never silently degrades: a
// non-empty key is a statement of intent to encrypt at rest, and accepting a
// mistyped key as "unset" would write secrets to the database in plaintext while
// the operator believed encryption was enabled. Callers must treat this as fatal
// at startup.
func NewCrypter(secretB64 string) (*Crypter, error) {
	if secretB64 == "" {
		return &Crypter{}, nil // unset: disabled state
	}
	key, err := base64.StdEncoding.DecodeString(secretB64)
	if err != nil {
		return nil, fmt.Errorf("configstore: auth.config_secret is not valid base64: %w", err)
	}
	if len(key) != MasterKeyBytes {
		return nil, fmt.Errorf("configstore: auth.config_secret must decode to %d bytes, got %d",
			MasterKeyBytes, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("configstore: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("configstore: new gcm: %w", err)
	}
	return &Crypter{aead: aead}, nil
}

// NewMasterKey generates a fresh base64-encoded 32-byte master key. Offered so
// operators can produce a valid auth.config_secret without reaching for openssl.
func NewMasterKey() (string, error) {
	var k [MasterKeyBytes]byte
	if _, err := io.ReadFull(rand.Reader, k[:]); err != nil {
		return "", fmt.Errorf("configstore: generate master key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(k[:]), nil
}

// Enabled reports whether the Crypter holds a valid master key.
func (c *Crypter) Enabled() bool { return c != nil && c.aead != nil }

// Seal encrypts plaintext, returning nonce||ciphertext. Disabled → ErrConfigDisabled.
func (c *Crypter) Seal(plaintext []byte) ([]byte, error) {
	if !c.Enabled() {
		return nil, ErrConfigDisabled
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("configstore: nonce: %w", err)
	}
	// Seal(dst=nonce, ...) appends the ciphertext after the nonce, yielding
	// nonce||ciphertext in one allocation.
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts nonce||ciphertext. Disabled → ErrConfigDisabled; a short blob →
// ErrCiphertextTooShort; a failed authentication (wrong key / tampering) → error.
func (c *Crypter) Open(sealed []byte) ([]byte, error) {
	if !c.Enabled() {
		return nil, ErrConfigDisabled
	}
	ns := c.aead.NonceSize()
	if len(sealed) < ns {
		return nil, ErrCiphertextTooShort
	}
	nonce, ciphertext := sealed[:ns], sealed[ns:]
	pt, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("configstore: open: %w", err)
	}
	return pt, nil
}
