package settings

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ivanzzeth/specula/internal/configstore"
)

// Source reports where a setting's currently effective value came from.
//
// These three names are the reference's and are preserved verbatim so its ported
// tests judge our implementation unmodified. Read them as:
//
//	runtime → the encrypted store (an operator's runtime override)
//	env     → the bootstrap snapshot (YAML config file or SPECULA_* env var)
//	unset   → neither; the consumer's built-in default applies
type Source string

const (
	SourceRuntime Source = "runtime" // a runtime override from the encrypted store
	SourceEnv     Source = "env"     // the bootstrap default (config file / env var)
	SourceUnset   Source = "unset"   // no override and no bootstrap value
)

// ErrUnknownKey means an unregistered setting key was requested.
var ErrUnknownKey = errors.New("settings: unknown key")

// ErrValidation wraps a kind-based validation failure (the admin handler maps it
// to 400).
var ErrValidation = errors.New("settings: validation failed")

// ErrConfigDisabled is returned by Set/Clear when the underlying encrypted store
// has no master key. It forwards configstore.ErrConfigDisabled so upper layers
// (the admin API) depend only on this package's errors.
var ErrConfigDisabled = configstore.ErrConfigDisabled

// ReloadHook is called with the newly effective value after a key is Set/Clear'd
// (hot reload). A non-nil error means the reload failed and the caller (the
// admin handler) reports it. A security-sensitive reload MUST keep its previous
// state inside the hook and must never degrade to permissive on failure.
type ReloadHook func(key, effective string) error

// Resolver layers the encrypted store's runtime overrides over the bootstrap
// snapshot and offers one read/write surface. Resolution order: an override in
// the store wins, otherwise the bootstrap value. Safe for concurrent use.
type Resolver struct {
	reg   *Registry
	store configstore.Store // encrypted runtime overrides (may be disabled: Set/Clear → ErrConfigDisabled)
	env   map[string]string // bootstrap snapshot (key→value; captured once at startup)

	mu    sync.RWMutex
	hooks map[string]ReloadHook
}

// NewResolver constructs a Resolver. env is a key→bootstrap-default snapshot
// (typically assembled by cmd/specula from the parsed config + environment);
// store is the encrypted config store (which may be in the disabled state).
func NewResolver(reg *Registry, store configstore.Store, env map[string]string) *Resolver {
	cp := make(map[string]string, len(env))
	for k, v := range env {
		cp[k] = v
	}
	return &Resolver{reg: reg, store: store, env: cp, hooks: make(map[string]ReloadHook)}
}

// Registry returns the underlying registry (the admin handler enumerates it).
func (r *Resolver) Registry() *Registry { return r.reg }

// ConfigEnabled reports whether runtime overrides are writable (the store holds
// a valid master key). When disabled, Set/Clear return ErrConfigDisabled but
// Effective/Source still work (seeing only bootstrap values).
func (r *Resolver) ConfigEnabled() bool {
	// MemStore/SQLStore both embed a *Crypter, but the Store interface does not
	// expose Enabled. Probe with a Keys call: the disabled state returns
	// ErrConfigDisabled.
	if r.store == nil {
		return false
	}
	_, err := r.store.Keys(context.Background())
	return !errors.Is(err, configstore.ErrConfigDisabled)
}

// OnReload registers (replacing any existing) a reload hook for key.
func (r *Resolver) OnReload(key string, hook ReloadHook) {
	r.mu.Lock()
	r.hooks[key] = hook
	r.mu.Unlock()
}

// Effective returns the currently effective value: the override wins, otherwise
// the bootstrap default. An unregistered key returns ErrUnknownKey.
func (r *Resolver) Effective(ctx context.Context, key string) (string, error) {
	if _, ok := r.reg.Lookup(key); !ok {
		return "", ErrUnknownKey
	}
	if v, ok, err := r.override(ctx, key); err == nil && ok {
		return v, nil
	}
	return r.env[key], nil
}

// Source returns where the effective value comes from. Unregistered → ErrUnknownKey.
func (r *Resolver) Source(ctx context.Context, key string) (Source, error) {
	if _, ok := r.reg.Lookup(key); !ok {
		return SourceUnset, ErrUnknownKey
	}
	if _, ok, err := r.override(ctx, key); err == nil && ok {
		return SourceRuntime, nil
	}
	if r.env[key] != "" {
		return SourceEnv, nil
	}
	return SourceUnset, nil
}

// override reads this key's runtime override from the store (disabled/missing →
// ok=false).
func (r *Resolver) override(ctx context.Context, key string) (string, bool, error) {
	if r.store == nil {
		return "", false, nil
	}
	v, ok, err := r.store.Get(ctx, key)
	if err != nil {
		if errors.Is(err, configstore.ErrConfigDisabled) {
			return "", false, nil
		}
		return "", false, err
	}
	return v, ok, nil
}

// Set writes a runtime override (encrypted, persisted) and fires the key's
// reload hook. Validation failure returns an error and writes nothing; a
// disabled store returns ErrConfigDisabled.
//
// If the hook fails, the override IS already persisted but did not take effect.
// The hook itself is responsible for not degrading (keeping its previous state);
// the caller maps the returned error to 5xx. This layer deliberately does not
// roll back, keeping the persistence semantics consistent.
func (r *Resolver) Set(ctx context.Context, key, value string) error {
	s, ok := r.reg.Lookup(key)
	if !ok {
		return ErrUnknownKey
	}
	if err := ValidateSetting(s, value); err != nil {
		return err
	}
	if r.store == nil {
		return configstore.ErrConfigDisabled
	}
	if err := r.store.Set(ctx, key, value); err != nil {
		return err
	}
	return r.fire(ctx, key)
}

// Clear removes the runtime override (falling back to the bootstrap default) and
// fires the reload hook. A disabled store returns ErrConfigDisabled.
func (r *Resolver) Clear(ctx context.Context, key string) error {
	if _, ok := r.reg.Lookup(key); !ok {
		return ErrUnknownKey
	}
	if r.store == nil {
		return configstore.ErrConfigDisabled
	}
	if err := r.store.Delete(ctx, key); err != nil {
		return err
	}
	return r.fire(ctx, key)
}

// fire invokes the key's reload hook with the current effective value (no hook →
// no-op).
func (r *Resolver) fire(ctx context.Context, key string) error {
	r.mu.RLock()
	hook := r.hooks[key]
	r.mu.RUnlock()
	if hook == nil {
		return nil
	}
	eff, err := r.Effective(ctx, key)
	if err != nil {
		return err
	}
	return hook(key, eff)
}

// Validate checks a value string against a kind (an empty value is always
// allowed — it means clear/disable). Enum membership cannot be checked from the
// kind alone; use ValidateSetting, which carries Setting.Enum.
func Validate(kind Kind, value string) error {
	return validate(kind, nil, value)
}

// ValidateSetting checks a value against a full descriptor (enum values are
// constrained by s.Enum).
func ValidateSetting(s Setting, value string) error {
	return validate(s.Kind, s.Enum, value)
}

func validate(kind Kind, enum []string, value string) error {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil
	}
	switch kind {
	case KindDuration:
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("%w: invalid duration %q: %v", ErrValidation, value, err)
		}
	case KindBool:
		switch strings.ToLower(v) {
		case "0", "1", "true", "false", "yes", "no":
		default:
			return fmt.Errorf("%w: invalid bool %q (want 0/1/true/false)", ErrValidation, value)
		}
	case KindInt:
		if _, err := strconv.Atoi(v); err != nil {
			return fmt.Errorf("%w: invalid int %q: %v", ErrValidation, value, err)
		}
	case KindFloat:
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			return fmt.Errorf("%w: invalid float %q: %v", ErrValidation, value, err)
		}
	case KindEnum:
		for _, allowed := range enum {
			if v == allowed {
				return nil
			}
		}
		return fmt.Errorf("%w: invalid value %q (want one of %s)", ErrValidation, value, strings.Join(enum, "/"))
	case KindString, KindSecret, KindList:
		// Free text / key material / list: unconstrained (list items are split
		// by the consumer via SplitCSV, which drops empty segments).
	}
	return nil
}

// Redacted returns the display form for a secret setting: NEVER the plaintext.
// Non-empty → "set (len=N, …last4)", exposing only the length and last 4 bytes.
// Empty → "".
func Redacted(value string) string {
	if value == "" {
		return ""
	}
	last := value
	if len(value) > 4 {
		last = value[len(value)-4:]
	}
	return fmt.Sprintf("set (len=%d, …%s)", len(value), last)
}
