// Package registrytoken implements the Docker Registry v2 Bearer-token auth
// flow for Specula's hosted registry: an RS256 token Service that mints and
// verifies access-scoped JWTs, a /token HTTP handler that turns Basic
// credentials (email:apikey or email:password) into such a JWT, and a /v2/
// Bearer challenge middleware that gates the registry data plane.
//
// It is deliberately separate from internal/auth's HS256 *session* JWTs: the
// registry token is RS256 (the Distribution ecosystem standard, verifiable with
// a published public key), carries repository access claims instead of user
// identity, and is short-lived. The two keying materials never mix.
//
// Reused primitives (per REGISTRY-DESIGN §3): apikey.LookupSubject for
// email:apikey authentication and acl.CanAccess (behind the ScopeAuthorizer
// seam) for per-repository authorization. This package owns only the token
// format and the HTTP glue.
package registrytoken

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// rsaKeyBits is the modulus size for a freshly generated registry signing key.
// 2048 is the Distribution-ecosystem norm and keeps token signing cheap.
const rsaKeyBits = 2048

// EnsureKeyPair loads the RS256 signing private key from path, generating and
// persisting a new one (PKCS#8 PEM, 0600) when the file is absent. This mirrors
// the "ensureSecret" pattern used for the session HS256 secret: a stable
// keypair survives restarts and is shared across HA replicas when path points
// at shared storage, so tokens minted by one replica verify on another.
//
// An empty path is rejected: unlike an ephemeral HS256 session secret, an
// ephemeral registry key would make every previously issued token unverifiable
// and break in-flight docker push/pull across restarts, so callers must supply
// a durable location.
func EnsureKeyPair(path string) (*rsa.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("registrytoken: key path is empty (a durable RS256 key file is required)")
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		key, perr := parsePrivateKeyPEM(data)
		if perr != nil {
			return nil, fmt.Errorf("registrytoken: parse key %q: %w", path, perr)
		}
		return key, nil
	case errors.Is(err, os.ErrNotExist):
		return generateAndPersist(path)
	default:
		return nil, fmt.Errorf("registrytoken: read key %q: %w", path, err)
	}
}

// generateAndPersist mints a fresh RSA key and writes it as a PKCS#8 PEM file
// with 0600 permissions, creating parent directories as needed.
func generateAndPersist(path string) (*rsa.PrivateKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return nil, fmt.Errorf("registrytoken: generate key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("registrytoken: marshal key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("registrytoken: mkdir %q: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("registrytoken: write key %q: %w", path, err)
	}
	return key, nil
}

// parsePrivateKeyPEM decodes a PEM block holding a PKCS#8 or PKCS#1 RSA key.
func parsePrivateKeyPEM(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("unexpected key type %T (want RSA)", parsed)
	}
	return key, nil
}
