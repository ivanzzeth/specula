package admin

// Runtime settings admin endpoints (all admin-only; routes in routes.go):
//
//	GET    /api/v1/admin/settings        → list every known setting (secrets redacted; carries source/restart_required)
//	PUT    /api/v1/admin/settings/{key}  body{value} → validate + persist to the encrypted store + fire the reload hook
//	DELETE /api/v1/admin/settings/{key}  → clear the runtime override (fall back to bootstrap) + fire the reload hook
//
// Contract: the config file/env is the bootstrap default, the encrypted store is
// the runtime override, resolution order override>bootstrap. Secret settings are
// redacted in responses (set/unset + length/last-4 only, NEVER the plaintext).
// When the encrypted store is disabled (no auth.config_secret) PUT/DELETE answer
// 503 while GET still works (showing bootstrap/default only).
//
// Ported from ai-sandbox internal/controlplane/api/admin_settings.go.

import (
	"context"
	"errors"
	"net/http"

	"github.com/ivanzzeth/specula/internal/settings"
)

// SettingsResolver is the runtime-settings contract the admin handlers depend
// on, implemented by *settings.Resolver. It is an interface so the admin package
// does not bind to the concrete type and so the handlers are trivially testable
// with a fake (accept interfaces, return structs — the house style here).
type SettingsResolver interface {
	Registry() *settings.Registry
	ConfigEnabled() bool
	Effective(ctx context.Context, key string) (string, error)
	Source(ctx context.Context, key string) (settings.Source, error)
	Set(ctx context.Context, key, value string) error
	Clear(ctx context.Context, key string) error
}

// ---- DTO ----

// SettingView is one setting's outward projection. A secret's plaintext is
// never a field of this struct — that is a structural guarantee, not a
// convention: there is no field for it to leak through.
type SettingView struct {
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	Source string `json:"source"` // runtime | env | unset
	// Value carries the effective value for NON-secret settings only. A secret
	// leaves it empty and populates Set/Display instead.
	Value           string `json:"value,omitempty"`
	Secret          bool   `json:"secret"`              // redacted kind
	Set             bool   `json:"set"`                 // has a non-empty effective value
	Display         string `json:"display,omitempty"`   // secret's redacted display (len/last-4)
	HotReload       bool   `json:"hot_reload"`          // true = effective immediately
	RestartRequired bool   `json:"restart_required"`    // !hot_reload AND currently overridden (i.e. changed)
	Dangerous       bool   `json:"dangerous,omitempty"` // UI must demand a second confirmation
	Desc            string `json:"desc,omitempty"`
}

// PutSettingRequest is the PUT body.
type PutSettingRequest struct {
	Value string `json:"value"`
}

// ---- handlers ----

// handleListSettings → GET /api/v1/admin/settings.
func (s *Server) handleListSettings(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings not enabled")
		return
	}
	ctx := r.Context()
	all := s.settings.Registry().All()
	out := make([]SettingView, 0, len(all))
	for _, st := range all {
		out = append(out, s.settingView(ctx, st))
	}
	writeJSON(w, http.StatusOK, SettingsResponse{
		Settings:      out,
		ConfigEnabled: s.settings.ConfigEnabled(),
	})
}

// handlePutSetting → PUT /api/v1/admin/settings/{key}.
func (s *Server) handlePutSetting(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings not enabled")
		return
	}
	key := r.PathValue("key")
	if _, ok := s.settings.Registry().Lookup(key); !ok {
		writeError(w, http.StatusNotFound, "unknown setting key")
		return
	}
	var req PutSettingRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		return
	}
	if err := s.settings.Set(r.Context(), key, req.Value); err != nil {
		s.writeSettingErr(w, key, err)
		return
	}
	// Deliberately log the key and NOT the value: this endpoint's whole purpose
	// includes writing secrets.
	s.log.Info("admin: setting updated", "key", key)
	writeJSON(w, http.StatusOK, s.settingView(r.Context(), s.mustLookupSetting(key)))
}

// handleDeleteSetting → DELETE /api/v1/admin/settings/{key}.
func (s *Server) handleDeleteSetting(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings not enabled")
		return
	}
	key := r.PathValue("key")
	if _, ok := s.settings.Registry().Lookup(key); !ok {
		writeError(w, http.StatusNotFound, "unknown setting key")
		return
	}
	if err := s.settings.Clear(r.Context(), key); err != nil {
		s.writeSettingErr(w, key, err)
		return
	}
	s.log.Info("admin: setting override cleared", "key", key)
	writeJSON(w, http.StatusOK, s.settingView(r.Context(), s.mustLookupSetting(key)))
}

// ---- helpers ----

func (s *Server) mustLookupSetting(key string) settings.Setting {
	st, _ := s.settings.Registry().Lookup(key)
	return st
}

// settingView assembles one setting's outward projection, redacting secrets.
func (s *Server) settingView(ctx context.Context, st settings.Setting) SettingView {
	eff, _ := s.settings.Effective(ctx, st.Key)
	src, _ := s.settings.Source(ctx, st.Key)
	v := SettingView{
		Key:             st.Key,
		Kind:            string(st.Kind),
		Source:          string(src),
		Secret:          st.Redact,
		HotReload:       st.HotReload,
		RestartRequired: !st.HotReload && src == settings.SourceRuntime,
		Dangerous:       st.Dangerous,
		Desc:            st.Desc,
	}
	if st.Redact {
		// Secret: never echo the plaintext — only set/unset plus a masked display.
		v.Set = eff != ""
		v.Display = settings.Redacted(eff)
	} else {
		v.Value = eff
		v.Set = eff != ""
	}
	return v
}

// writeSettingErr maps a settings-layer error onto an HTTP status:
//
//	store disabled  → 503 (no auth.config_secret: nothing can be persisted encrypted)
//	unknown key     → 404
//	validation      → 400
//	otherwise (reload hook failure, DB error) → 500
func (s *Server) writeSettingErr(w http.ResponseWriter, key string, err error) {
	switch {
	case errors.Is(err, settings.ErrConfigDisabled):
		writeError(w, http.StatusServiceUnavailable,
			"encrypted config store disabled: set auth.config_secret (SPECULA_AUTH__CONFIG_SECRET) to manage runtime settings")
	case errors.Is(err, settings.ErrUnknownKey):
		writeError(w, http.StatusNotFound, "unknown setting key")
	case errors.Is(err, settings.ErrValidation):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		// The error may carry the rejected value; log it server-side, and give
		// the client a generic message rather than risk echoing a secret back.
		s.log.Error("admin: setting write failed", "key", key, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to apply setting")
	}
}
