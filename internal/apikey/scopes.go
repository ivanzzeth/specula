package apikey

import (
	"encoding/json"
	"strings"
)

// Registry scopes a key may hold. Empty/omitted at create time → DefaultScopes.
const (
	ScopePull = "pull"
	ScopePush = "push"
)

// DefaultScopes is the pre-scope-era behaviour: full registry access within the org.
var DefaultScopes = []string{ScopePull, ScopePush}

// NormalizeScopes lowercases and dedupes known scopes. Unknown tokens are dropped.
// Empty input returns a copy of DefaultScopes. Order is always pull, then push
// when both are present.
func NormalizeScopes(in []string) []string {
	if len(in) == 0 {
		out := make([]string, len(DefaultScopes))
		copy(out, DefaultScopes)
		return out
	}
	var wantPull, wantPush bool
	for _, s := range in {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case ScopePull:
			wantPull = true
		case ScopePush:
			wantPush = true
		}
	}
	if !wantPull && !wantPush {
		out := make([]string, len(DefaultScopes))
		copy(out, DefaultScopes)
		return out
	}
	var out []string
	if wantPull {
		out = append(out, ScopePull)
	}
	if wantPush {
		out = append(out, ScopePush)
	}
	return out
}

// EncodeScopes serialises scopes for the api_keys.scopes column (JSON array).
func EncodeScopes(scopes []string) string {
	scopes = NormalizeScopes(scopes)
	b, err := json.Marshal(scopes)
	if err != nil {
		return `["pull","push"]`
	}
	return string(b)
}

// DecodeScopes parses the api_keys.scopes column. Empty/invalid → DefaultScopes.
func DecodeScopes(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return NormalizeScopes(nil)
	}
	var parsed []string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		parts := strings.Split(raw, ",")
		return NormalizeScopes(parts)
	}
	return NormalizeScopes(parsed)
}

// AllowsAction reports whether scopes permit a Docker Distribution registry action.
// "delete" requires push (write). Unknown actions are denied.
func AllowsAction(scopes []string, action string) bool {
	scopes = NormalizeScopes(scopes)
	has := map[string]bool{}
	for _, s := range scopes {
		has[s] = true
	}
	switch action {
	case "pull":
		return has[ScopePull]
	case "push", "delete":
		return has[ScopePush]
	default:
		return false
	}
}
