// Alg-confusion regression tests for the RS256 registry-token verifier.
//
// These guard the golang-jwt/v5 swap: Verify must reject a token whose header
// advertises an algorithm other than RS256 — most importantly alg=none, and
// the HS256 confusion attack where an attacker signs with the RSA *public* key
// treated as an HMAC secret.
package registrytoken_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ivanzzeth/specula/internal/registrytoken"
)

var b64raw = base64.RawURLEncoding

// forgeToken builds a JWT with an attacker-chosen alg header. sig is appended
// verbatim as the signature segment.
func forgeToken(t *testing.T, alg string, sig []byte) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT"})
	pl, _ := json.Marshal(map[string]any{
		"iss":    "specula-test",
		"aud":    "specula",
		"sub":    "attacker",
		"exp":    time.Now().Add(time.Hour).Unix(),
		"iat":    time.Now().Unix(),
		"access": []any{},
	})
	unsigned := b64raw.EncodeToString(hdr) + "." + b64raw.EncodeToString(pl)
	return unsigned + "." + b64raw.EncodeToString(sig)
}

// TestVerifyRejectsAlgNone covers the classic alg=none bypass.
//
// Two distinct rejection paths, both of which must deny authentication:
//   - empty signature  → ErrMalformed (the structural 3-segment check rejects
//     an empty signature segment before the alg check is reached).
//   - non-empty signature → ErrAlg (the manual JOSE header pre-check).
func TestVerifyRejectsAlgNone(t *testing.T) {
	svc := newTestSvc(t)

	for _, tc := range []struct {
		name    string
		sig     []byte
		wantErr error
	}{
		{"empty signature", nil, registrytoken.ErrMalformed},
		{"non-empty signature", []byte("attacker-supplied"), registrytoken.ErrAlg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := svc.Verify(forgeToken(t, "none", tc.sig))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("alg=none: got %v, want %v", err, tc.wantErr)
			}
			if claims != nil {
				t.Fatalf("alg=none must never return claims, got %+v", claims)
			}
		})
	}
}

// TestVerifyRejectsAlgNoneCaseVariants ensures the alg check is not bypassable
// by casing tricks. A non-empty signature is used so the alg check (not the
// structural check) is the component under test.
func TestVerifyRejectsAlgNoneCaseVariants(t *testing.T) {
	svc := newTestSvc(t)
	for _, alg := range []string{"None", "NONE", "nOnE"} {
		claims, err := svc.Verify(forgeToken(t, alg, []byte("attacker-supplied")))
		if !errors.Is(err, registrytoken.ErrAlg) {
			t.Fatalf("alg=%q must be rejected with ErrAlg, got %v", alg, err)
		}
		if claims != nil {
			t.Fatalf("alg=%q must never return claims", alg)
		}
	}
}

// TestVerifyRejectsHS256Confusion is the RSA->HMAC confusion attack: the
// attacker signs with the *public* key bytes as an HMAC secret and claims
// alg=HS256. A verifier that picks the algorithm from the header would accept.
func TestVerifyRejectsHS256Confusion(t *testing.T) {
	key, err := registrytoken.EnsureKeyPair(t.TempDir() + "/reg.pem")
	if err != nil {
		t.Fatalf("EnsureKeyPair: %v", err)
	}
	svc := registrytoken.NewService(key, "specula-test", "specula", 0)

	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}

	hdr, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	pl, _ := json.Marshal(map[string]any{
		"iss": "specula-test",
		"aud": "specula",
		"sub": "attacker",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	unsigned := b64raw.EncodeToString(hdr) + "." + b64raw.EncodeToString(pl)

	mac := hmac.New(sha256.New, pubDER)
	mac.Write([]byte(unsigned))
	forged := unsigned + "." + b64raw.EncodeToString(mac.Sum(nil))

	if _, err := svc.Verify(forged); !errors.Is(err, registrytoken.ErrAlg) {
		t.Fatalf("HS256 confusion must be rejected with ErrAlg, got %v", err)
	}
}
