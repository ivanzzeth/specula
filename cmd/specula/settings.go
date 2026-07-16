package main

// Runtime settings assembly: the encrypted config store, the settings Resolver,
// and the ensureSecret pattern ported from ai-sandbox's cmd/controlplane.
//
// The problem this solves, concretely. Before this, an unset auth.jwt_secret
// made resolveJWTSecret generate a random secret at EVERY boot. Two consequences,
// both real:
//
//   - restart the process and every logged-in user is silently signed out; and
//   - run two replicas and they sign with DIFFERENT keys, so a session minted by
//     replica A is rejected by replica B — logins flap at random behind a load
//     balancer.
//
// ensureSecret fixes both with one idea: generate the secret ONCE, persist it
// into the encrypted, database-backed store, and read it back on the next boot.
// Durability and HA-sharing fall out of the same write, because the database is
// what every replica already shares.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"

	"github.com/ivanzzeth/specula/internal/config"
	"github.com/ivanzzeth/specula/internal/configstore"
	"github.com/ivanzzeth/specula/internal/registrytoken"
	"github.com/ivanzzeth/specula/internal/settings"
)

// buildConfigStore constructs the encrypted runtime-config store over the same
// database handle the multi-tenant stores use.
//
// An empty auth.config_secret yields a DISABLED store (no error): runtime
// settings become read-only and secrets fall back to their legacy behaviour with
// a warning. A non-empty but malformed key is a FATAL error — see
// configstore.NewCrypter for why silence there is dangerous.
func buildConfigStore(cfg *config.Config, db *sql.DB, log *slog.Logger) (configstore.Store, error) {
	crypter, err := configstore.NewCrypter(cfg.Auth.ConfigSecret)
	if err != nil {
		// Loud, fatal, and actionable: the operator asked for encryption at rest
		// and the key is wrong. Starting anyway would mean either no encryption
		// or a store that cannot read its own data.
		return nil, fmt.Errorf("auth.config_secret is set but invalid: %w "+
			"(want base64 of exactly 32 bytes, e.g. `openssl rand -base64 32`)", err)
	}
	if !crypter.Enabled() {
		log.Warn("specula: auth.config_secret is not set — the encrypted settings store is DISABLED. " +
			"Runtime settings are read-only, and auto-generated secrets (session signing key, registry " +
			"token key) cannot be persisted. Set auth.config_secret (SPECULA_AUTH__CONFIG_SECRET) to " +
			"`openssl rand -base64 32` for production.")
	}

	switch cfg.Storage.Meta.Driver {
	case "postgres":
		return configstore.NewSQLStorePostgres(db, crypter), nil
	default:
		return configstore.NewSQLStore(db, crypter), nil
	}
}

// buildSettingsResolver assembles the Resolver over (bootstrap snapshot +
// encrypted store).
//
// The bootstrap snapshot is settings.EnvBootstrap (which reads the registry, so
// a declared EnvVar genuinely works) OVERLAID with values from the parsed config.
// The overlay matters: Specula's config comes from koanf — YAML file first, then
// SPECULA_* env overrides — so a jwt_secret written in specula.yaml would be
// invisible to a pure os.LookupEnv bootstrap. Merging both means the setting is
// bootstrapped from wherever the operator actually put it, and the value already
// carries koanf's file→env precedence.
func buildSettingsResolver(cfg *config.Config, store configstore.Store, regKeyPEM string) *settings.Resolver {
	reg := settings.DefaultRegistry()
	boot := settings.EnvBootstrap(reg)

	// Overlay the parsed config (covers YAML, and agrees with the env var when
	// that is where the value came from).
	if cfg.Auth.JWTSecret != "" {
		boot[settings.KeyAuthJWTSecret] = cfg.Auth.JWTSecret
	}
	// The registry key's bootstrap default is the existing on-disk PEM, so an
	// established single-node deployment keeps its exact keypair on upgrade.
	if regKeyPEM != "" {
		boot[settings.KeyRegistryTokenKey] = regKeyPEM
	}

	return settings.NewResolver(reg, store, boot)
}

// mustEffective reads a setting's effective value, treating an error as empty.
// Every key passed here is registered, so the only error path is a store read
// failure — which must degrade to "unset" (and therefore "generate one") rather
// than abort startup.
func mustEffective(ctx context.Context, r *settings.Resolver, key string) string {
	v, err := r.Effective(ctx, key)
	if err != nil {
		return ""
	}
	return v
}

// ensureSecret is the port of ai-sandbox's cmd/controlplane ensureSecret.
//
// If current is non-empty it is already durable (config file, env, or a previous
// run's persisted value) — return it. Otherwise generate a fresh 32-byte secret
// and PERSIST it through the resolver into the encrypted store, so the next boot
// and every other replica read back the same value.
//
// Returns (secret, persisted). persisted=false means the store was unavailable
// and the secret is ephemeral — the caller MUST warn loudly, because that is the
// old broken behaviour and the operator needs to know they still have it.
func ensureSecret(ctx context.Context, r *settings.Resolver, key, current string) (string, bool) {
	if current != "" {
		return current, true
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is catastrophic and essentially never happens;
		// surface it rather than silently returning a low-entropy secret.
		return "", false
	}
	gen := hex.EncodeToString(b[:])
	if err := r.Set(ctx, key, gen); err == nil {
		return gen, true
	}
	return gen, false
}

// ensureJWTSecret resolves the HS256 session signing secret, generating and
// persisting one on first start. Replaces the old resolveJWTSecret, whose
// ephemeral secret invalidated sessions on every restart and was never shared
// across replicas.
func ensureJWTSecret(ctx context.Context, r *settings.Resolver, log *slog.Logger) ([]byte, error) {
	secret, persisted := ensureSecret(ctx, r, settings.KeyAuthJWTSecret,
		mustEffective(ctx, r, settings.KeyAuthJWTSecret))
	if secret == "" {
		return nil, fmt.Errorf("crypto/rand failed generating the session signing secret")
	}
	if !persisted {
		// The exact warning the old code emitted, kept because the failure mode
		// it describes is still real when there is no master key to encrypt with.
		log.Warn("specula: auth.jwt_secret is unset and the encrypted settings store is unavailable — " +
			"generated an EPHEMERAL secret. Sessions will be invalidated on restart and are NOT valid " +
			"across replicas. Set auth.config_secret (SPECULA_AUTH__CONFIG_SECRET) so the generated " +
			"secret can be persisted, or set auth.jwt_secret explicitly.")
	} else if src, _ := r.Source(ctx, settings.KeyAuthJWTSecret); src == settings.SourceRuntime {
		log.Info("specula: session signing secret loaded from the encrypted settings store",
			"source", string(src))
	}
	return []byte(secret), nil
}

// ensureRegistryKey resolves the RS256 registry token signing key.
//
// Order of preference:
//  1. the encrypted settings store (shared by every replica — the goal);
//  2. the on-disk PEM at auth.registry_token_key_path, which is MIGRATED into the
//     store on first start so an existing deployment keeps its exact key and
//     simply gains HA-shareability;
//  3. a freshly generated key, persisted into the store.
//
// When the store is unavailable it falls back to the legacy file behaviour
// (EnsureKeyPair) with a warning, so a dev instance without a master key keeps
// working exactly as before.
func ensureRegistryKey(ctx context.Context, r *settings.Resolver, keyPath string, log *slog.Logger) (*rsa.PrivateKey, error) {
	eff := mustEffective(ctx, r, settings.KeyRegistryTokenKey)

	if eff == "" {
		// Nothing in the store and no PEM on disk: mint one.
		key, pemBytes, err := registrytoken.GenerateKeyPEM()
		if err != nil {
			return nil, err
		}
		if err := r.Set(ctx, settings.KeyRegistryTokenKey, string(pemBytes)); err != nil {
			// No store: fall back to the file, preserving the old behaviour.
			log.Warn("specula: registry token key could not be persisted to the encrypted settings store — "+
				"falling back to an on-disk PEM. The key is node-local, so tokens minted here will NOT "+
				"verify on another replica. Set auth.config_secret to share it.",
				"key_path", keyPath, "err", err)
			return registrytoken.EnsureKeyPair(keyPath)
		}
		log.Info("specula: generated a registry token key and persisted it to the encrypted settings store " +
			"(shared across replicas)")
		return key, nil
	}

	key, err := registrytoken.ParsePrivateKeyPEM([]byte(eff))
	if err != nil {
		return nil, fmt.Errorf("registry token key is not a valid PEM private key: %w", err)
	}

	// Migrate an on-disk key into the store, once. Source==env means the value
	// came from the bootstrap snapshot (the PEM file), not the store.
	if src, _ := r.Source(ctx, settings.KeyRegistryTokenKey); src != settings.SourceRuntime {
		if err := r.Set(ctx, settings.KeyRegistryTokenKey, eff); err != nil {
			log.Warn("specula: registry token key remains on local disk only — it could not be migrated "+
				"into the encrypted settings store, so tokens minted here will NOT verify on another "+
				"replica. Set auth.config_secret to share it.",
				"key_path", keyPath, "err", err)
		} else {
			log.Info("specula: migrated the on-disk registry token key into the encrypted settings store; "+
				"every replica now verifies the same tokens", "key_path", keyPath)
		}
	}
	return key, nil
}

// readRegistryKeyPEM reads the existing on-disk registry PEM, if any, so it can
// seed the settings bootstrap snapshot. A missing file is not an error — that is
// simply a first start.
func readRegistryKeyPEM(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
