// Package apikey manages user-created API keys and the caller→(org, subject)
// mapping used for non-session (CLI / docker login / registry) authentication.
// Adapted from ai-sandbox internal/controlplane/apikey.
//
// Key format: "spck_" + base64url(18 random bytes). Only the SHA-256 hash is
// stored at rest; the plaintext is returned exactly once, at creation time, and
// never persisted or logged. Each key is bound to an org (and optionally an
// issuing user). LookupSubject(token) verifies a presented key and returns its
// (orgID, synthetic subject "apikey:<id>"), which authorization uses as a
// stable, non-empty acl.Subject.UserID.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/org"
)

// DefaultOrgID is the fallback org for keys created without an explicit org.
// It is defined as org.DefaultOrgID so the two are equal by construction: the
// bootstrap default org is "org_default" and any fallback-created resource must
// land there, never in a non-existent org (which would look like a cross-org
// leak).
const DefaultOrgID = org.DefaultOrgID

// KeyPrefix is the plaintext key namespace ("spck_" = SPecula Key).
const KeyPrefix = "spck_"

// SubjectPrefix namespaces the synthetic acl subject id an API key authenticates
// as. API-key auth has no human user, so each key gets a stable synthetic
// subject id ("apikey:<keyID>") to keep created_by / owner_user_id non-empty and
// fail-closed against other org members.
const SubjectPrefix = "apikey:"

// SubjectID builds the stable synthetic subject id ("apikey:<keyID>") for a key.
// An empty keyID yields "" — with no stable identity, prefer empty and let the
// caller fail closed rather than return a guessable colliding value.
func SubjectID(keyID string) string {
	if strings.TrimSpace(keyID) == "" {
		return ""
	}
	return SubjectPrefix + keyID
}

// KeyInfo is the outward metadata for an API key (list/detail views). It never
// contains the plaintext key.
type KeyInfo struct {
	ID         string     `json:"id"`
	OrgID      string     `json:"org_id"`
	UserID     string     `json:"user_id,omitempty"` // issuing user (visibility ownership); empty = org-level, admin-only
	Label      string     `json:"label,omitempty"`
	Prefix     string     `json:"prefix"` // display-only truncated prefix; never reversible to plaintext
	Scopes     []string   `json:"scopes"` // registry scopes (pull/push); always normalised non-empty
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"` // nil = never expires
	Revoked    bool       `json:"revoked"`
}

// Store is the API-key persistence + lookup contract. Two implementations:
// SQLStore (DB-authoritative, multi-replica consistent) and MemStore (dev).
type Store interface {
	// Create issues a key for an org (no issuing user). Returns the stable id
	// and the plaintext key — the plaintext is visible only here, once.
	// scopes empty → DefaultScopes.
	Create(orgID, label string, scopes ...string) (id, rawKey string, err error)
	// CreateOwned issues a key for an org and records the issuing userID (the
	// acl subject string, e.g. org.UserSubjectID(user.ID)). orgID empty →
	// DefaultOrgID. scopes empty → DefaultScopes.
	CreateOwned(orgID, userID, label string, scopes ...string) (id, rawKey string, err error)
	// LookupSubject verifies a presented plaintext key and returns its org and
	// synthetic subject ("apikey:<id>"). ok=false for unknown / revoked /
	// expired keys (uniform failure, no distinction leaked).
	LookupSubject(token string) (orgID, subject string, ok bool)
	// LookupKey verifies a presented plaintext key and returns full KeyInfo
	// (including Scopes). Prefer this when registry scope enforcement is needed.
	LookupKey(token string) (KeyInfo, bool)
	// List returns an org's keys (newest→oldest, including revoked).
	List(orgID string) ([]KeyInfo, error)
	// Get returns a single key by (id, org); cross-org lookups miss.
	Get(orgID, id string) (KeyInfo, bool)
	// Revoke soft-deletes a key by (id, org); auth rejects it immediately.
	// Returns true if a matching, not-already-revoked key was found.
	Revoke(orgID, id string) (bool, error)
}

// ---- shared helpers (used by both implementations) ----------------------------

// hashKey returns the hex SHA-256 of a plaintext key, used as the at-rest hash
// and the api_keys primary key.
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// newRawKey mints a fresh plaintext key: "spck_" + base64url(18 bytes).
func newRawKey() string {
	var b [18]byte
	_, _ = rand.Read(b[:])
	return KeyPrefix + base64.RawURLEncoding.EncodeToString(b[:])
}

// newID returns a random 16-byte hex key id.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// prefixOf returns the display-only prefix of a plaintext key ("spck_" + 6
// chars + ellipsis); it is not reversible to the plaintext.
func prefixOf(rawKey string) string {
	const show = len(KeyPrefix) + 6
	if len(rawKey) <= show {
		return rawKey
	}
	return rawKey[:show] + "…"
}
