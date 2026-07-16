package configstore

import "context"

// Store is the encrypted runtime-configuration KV abstraction: values cross the
// interface as plaintext, and the implementation is responsible for encrypting
// them before they reach durable storage (ciphertext BLOB).
//
// Two implementations: SQLStore (database-authoritative, consistent across HA
// replicas) and MemStore (tests / no-database dev). In the disabled state (the
// Crypter holds no master key) every method returns ErrConfigDisabled.
type Store interface {
	// Get returns the plaintext value for key. Missing → ("", false, nil);
	// disabled → ErrConfigDisabled.
	Get(ctx context.Context, key string) (plaintext string, ok bool, err error)
	// Set upserts idempotently; the plaintext is encrypted before storage.
	Set(ctx context.Context, key, plaintext string) error
	// Delete removes key (a missing key is success).
	Delete(ctx context.Context, key string) error
	// Keys returns every stored key (values excluded), ascending.
	Keys(ctx context.Context) ([]string, error)
}
