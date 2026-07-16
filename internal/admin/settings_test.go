package admin

// Ported (translated + re-pointed at Specula's harness) from ai-sandbox
// internal/controlplane/api/admin_settings_test.go and
// admin_settings_delete_test.go. Their assertions judge our implementation.
//
// One deliberate deviation: GET /admin/settings answers a SettingsResponse
// envelope rather than the bare JSON array the reference returned, matching this
// codebase's house convention (ConfigResponse/EventsResponse) and carrying
// config_enabled for the UI. The substance of every ported assertion —
// redaction, no plaintext anywhere in the body, source, restart_required, and
// the status-code mapping — is preserved exactly.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/settings"
)

// fakeResolver is the SettingsResolver test double: in-memory overrides plus a
// configurable disabled state and hook error. Ported from their fakeResolver.
type fakeResolver struct {
	reg       *settings.Registry
	enabled   bool
	overrides map[string]string
	env       map[string]string
	hookErr   error // non-nil → Set/Clear return it (simulating a reload failure)
}

func newFakeResolver(enabled bool) *fakeResolver {
	return &fakeResolver{
		reg:       settings.DefaultRegistry(),
		enabled:   enabled,
		overrides: map[string]string{},
		env:       map[string]string{settings.KeyOrgMaxPerUser: "3"},
	}
}

func (f *fakeResolver) Registry() *settings.Registry { return f.reg }
func (f *fakeResolver) ConfigEnabled() bool          { return f.enabled }

func (f *fakeResolver) Effective(_ context.Context, key string) (string, error) {
	if _, ok := f.reg.Lookup(key); !ok {
		return "", settings.ErrUnknownKey
	}
	if v, ok := f.overrides[key]; ok {
		return v, nil
	}
	return f.env[key], nil
}

func (f *fakeResolver) Source(_ context.Context, key string) (settings.Source, error) {
	if _, ok := f.reg.Lookup(key); !ok {
		return settings.SourceUnset, settings.ErrUnknownKey
	}
	if _, ok := f.overrides[key]; ok {
		return settings.SourceRuntime, nil
	}
	if f.env[key] != "" {
		return settings.SourceEnv, nil
	}
	return settings.SourceUnset, nil
}

func (f *fakeResolver) Set(_ context.Context, key, value string) error {
	s, ok := f.reg.Lookup(key)
	if !ok {
		return settings.ErrUnknownKey
	}
	if !f.enabled {
		return settings.ErrConfigDisabled
	}
	if err := settings.ValidateSetting(s, value); err != nil {
		return err
	}
	f.overrides[key] = value
	return f.hookErr
}

func (f *fakeResolver) Clear(_ context.Context, key string) error {
	if _, ok := f.reg.Lookup(key); !ok {
		return settings.ErrUnknownKey
	}
	if !f.enabled {
		return settings.ErrConfigDisabled
	}
	delete(f.overrides, key)
	return f.hookErr
}

// settingsHarness builds an admin server with a settings resolver injected, and
// returns it plus an admin session token.
func settingsHarness(t *testing.T, fr SettingsResolver) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	h.srv.settings = fr
	_, tok := h.mustCreateAdmin(t)
	return h, tok
}

func decodeSettings(t *testing.T, body []byte) SettingsResponse {
	t.Helper()
	var resp SettingsResponse
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp
}

func findSetting(resp SettingsResponse, key string) (SettingView, bool) {
	for _, v := range resp.Settings {
		if v.Key == key {
			return v, true
		}
	}
	return SettingView{}, false
}

// ---- ported: list ------------------------------------------------------------

func TestSettingsListRequiresAdmin(t *testing.T) {
	h, adminTok := settingsHarness(t, newFakeResolver(true))

	// A logged-in NON-admin must not read the settings (they include secrets).
	_, userTok := h.mustCreateUser(t, "user@example.com")
	rr := h.do(http.MethodGet, "/api/v1/admin/settings", userTok, nil)
	assert.Equal(t, http.StatusForbidden, rr.Code, "non-admin must not list settings")

	// No session at all → 401.
	rr = h.do(http.MethodGet, "/api/v1/admin/settings", "", nil)
	assert.Equal(t, http.StatusUnauthorized, rr.Code, "anonymous must not list settings")

	// Admin → 200.
	rr = h.do(http.MethodGet, "/api/v1/admin/settings", adminTok, nil)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestSettingsListRedactsSecrets(t *testing.T) {
	fr := newFakeResolver(true)
	fr.overrides[settings.KeyAuthJWTSecret] = "supersecretsigningkeyvalue"
	h, tok := settingsHarness(t, fr)

	rr := h.do(http.MethodGet, "/api/v1/admin/settings", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	// The single most important assertion in this file.
	assert.NotContains(t, rr.Body.String(), "supersecretsigningkeyvalue",
		"secret plaintext leaked in the list response")

	resp := decodeSettings(t, rr.Body.Bytes())
	v, ok := findSetting(resp, settings.KeyAuthJWTSecret)
	require.True(t, ok, "auth.jwt_secret must be listed")
	assert.Empty(t, v.Value, "a secret must not populate the value field")
	assert.True(t, v.Set, "a secret with a value must report set=true")
	assert.NotEmpty(t, v.Display, "a secret must carry a masked display")
	assert.True(t, v.Secret)
}

// ---- ported: put -------------------------------------------------------------

func TestSettingsPutDisabledStore503(t *testing.T) {
	h, tok := settingsHarness(t, newFakeResolver(false)) // disabled
	rr := h.do(http.MethodPut, "/api/v1/admin/settings/"+settings.KeyOrgMaxPerUser, tok,
		jsonBody(PutSettingRequest{Value: "5"}))
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code, "PUT on a disabled store must be 503")

	// GET still works (showing bootstrap/default only).
	rr = h.do(http.MethodGet, "/api/v1/admin/settings", tok, nil)
	assert.Equal(t, http.StatusOK, rr.Code, "GET must still work on a disabled store")
	assert.False(t, decodeSettings(t, rr.Body.Bytes()).ConfigEnabled)
}

func TestSettingsPutRestartRequiredFlag(t *testing.T) {
	h, tok := settingsHarness(t, newFakeResolver(true))
	// auth.jwt_secret is NOT hot-reload → after a write, restart_required=true.
	rr := h.do(http.MethodPut, "/api/v1/admin/settings/"+settings.KeyAuthJWTSecret, tok,
		jsonBody(PutSettingRequest{Value: "new-signing-secret"}))
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())

	var v SettingView
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &v))
	assert.True(t, v.RestartRequired, "a non-hot-reload setting must report restart_required after a change")
	assert.Equal(t, string(settings.SourceRuntime), v.Source)
	// Even the write's own response must not echo the secret back.
	assert.NotContains(t, rr.Body.String(), "new-signing-secret",
		"the PUT response echoed the secret that was just written")
}

func TestSettingsPutHotReloadNoRestartRequired(t *testing.T) {
	h, tok := settingsHarness(t, newFakeResolver(true))
	rr := h.do(http.MethodPut, "/api/v1/admin/settings/"+settings.KeyOrgMaxPerUser, tok,
		jsonBody(PutSettingRequest{Value: "9"}))
	require.Equal(t, http.StatusOK, rr.Code)

	var v SettingView
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &v))
	assert.False(t, v.RestartRequired, "a hot-reload setting must never demand a restart")
	assert.Equal(t, "9", v.Value, "a non-secret setting echoes its value")
	assert.True(t, v.HotReload)
}

func TestSettingsPutValidationError400(t *testing.T) {
	h, tok := settingsHarness(t, newFakeResolver(true))
	rr := h.do(http.MethodPut, "/api/v1/admin/settings/"+settings.KeyOrgMaxPerUser, tok,
		jsonBody(PutSettingRequest{Value: "notanumber"}))
	assert.Equal(t, http.StatusBadRequest, rr.Code, "PUT with an invalid int must be 400")
}

func TestSettingsPutUnknownKey404(t *testing.T) {
	h, tok := settingsHarness(t, newFakeResolver(true))
	rr := h.do(http.MethodPut, "/api/v1/admin/settings/no.such.key", tok,
		jsonBody(PutSettingRequest{Value: "x"}))
	assert.Equal(t, http.StatusNotFound, rr.Code, "PUT with an unknown key must be 404")
}

func TestSettingsHotReloadHookErr500(t *testing.T) {
	fr := newFakeResolver(true)
	fr.hookErr = errors.New("reload boom")
	h, tok := settingsHarness(t, fr)
	rr := h.do(http.MethodPut, "/api/v1/admin/settings/"+settings.KeyOrgMaxPerUser, tok,
		jsonBody(PutSettingRequest{Value: "4"}))
	assert.Equal(t, http.StatusInternalServerError, rr.Code, "a failing reload hook must be 500")
}

// ---- ported: delete ----------------------------------------------------------

func TestDeleteSettingClearsOverride(t *testing.T) {
	fr := newFakeResolver(true)
	fr.overrides[settings.KeyOrgMaxPerUser] = "9"
	h, tok := settingsHarness(t, fr)

	rr := h.do(http.MethodDelete, "/api/v1/admin/settings/"+settings.KeyOrgMaxPerUser, tok, nil)
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	assert.NotContains(t, fr.overrides, settings.KeyOrgMaxPerUser, "override not cleared")

	// The response shows the bootstrap fallback.
	var v SettingView
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &v))
	assert.Equal(t, "3", v.Value, "after clear the bootstrap default is effective")
	assert.Equal(t, string(settings.SourceEnv), v.Source)
}

func TestDeleteSettingUnknownKey(t *testing.T) {
	h, tok := settingsHarness(t, newFakeResolver(true))
	rr := h.do(http.MethodDelete, "/api/v1/admin/settings/no.such.key", tok, nil)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestDeleteSettingNotEnabled(t *testing.T) {
	// No resolver injected at all.
	h := newHarness(t)
	_, tok := h.mustCreateAdmin(t)
	rr := h.do(http.MethodDelete, "/api/v1/admin/settings/"+settings.KeyOrgMaxPerUser, tok, nil)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code, "nil settings resolver → 503")

	rr = h.do(http.MethodGet, "/api/v1/admin/settings", tok, nil)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

func TestDeleteSettingConfigDisabled(t *testing.T) {
	h, tok := settingsHarness(t, newFakeResolver(false))
	rr := h.do(http.MethodDelete, "/api/v1/admin/settings/"+settings.KeyOrgMaxPerUser, tok, nil)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"clearing an override with the store disabled must map to an error, not silent success")
}

// ---- Specula-local: the whole-surface redaction guard -------------------------

// TestNoSecretSettingEverEchoed sweeps EVERY registered secret: set a
// recognisable plaintext, then assert it appears in no response body from any of
// the three endpoints. A per-key test is easy to forget when a new secret is
// added; this one cannot be.
func TestNoSecretSettingEverEchoed(t *testing.T) {
	fr := newFakeResolver(true)
	const canary = "CANARY-PLAINTEXT-MUST-NEVER-APPEAR"
	var secretKeys []string
	for _, s := range settings.DefaultRegistry().All() {
		if s.Redact {
			secretKeys = append(secretKeys, s.Key)
			fr.overrides[s.Key] = canary + "-" + s.Key
		}
	}
	require.NotEmpty(t, secretKeys, "registry declares no secrets — this guard would be vacuous")
	h, tok := settingsHarness(t, fr)

	rr := h.do(http.MethodGet, "/api/v1/admin/settings", tok, nil)
	assert.NotContains(t, rr.Body.String(), canary, "GET list leaked a secret")

	for _, key := range secretKeys {
		rr = h.do(http.MethodPut, "/api/v1/admin/settings/"+key, tok,
			jsonBody(PutSettingRequest{Value: canary + "-put"}))
		assert.NotContains(t, rr.Body.String(), canary, "PUT %s echoed the secret back", key)

		rr = h.do(http.MethodDelete, "/api/v1/admin/settings/"+key, tok, nil)
		assert.NotContains(t, rr.Body.String(), canary, "DELETE %s leaked the secret", key)
	}
}
