// Package clicreds stores Specula CLI credentials on disk (npm-style), under
// ~/.config/specula/credentials.json. The token is a Specula API key (spck_…).
package clicreds

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// EnvToken overrides the persisted API key (like NPM_TOKEN).
	EnvToken = "SPECULA_TOKEN"
	// EnvControlPlane overrides the control-plane base URL.
	EnvControlPlane = "SPECULA_CONTROL_PLANE"
	// EnvAddr is an alias for EnvControlPlane (shorter for CLI users).
	EnvAddr = "SPECULA_ADDR"

	fileName = "credentials.json"
	dirName  = "specula"
)

// Credentials is the on-disk / resolved CLI auth state.
type Credentials struct {
	ControlPlane string `json:"control_plane"`
	Token        string `json:"token"`
}

// Dir returns ~/.config/specula (or $XDG_CONFIG_HOME/specula).
func Dir() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, dirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", dirName), nil
}

// Path is the credentials file path.
func Path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, fileName), nil
}

// Load reads credentials from disk. Missing file returns empty Credentials and nil error.
func Load() (Credentials, error) {
	p, err := Path()
	if err != nil {
		return Credentials{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Credentials{}, nil
		}
		return Credentials{}, err
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return Credentials{}, fmt.Errorf("parse %s: %w", p, err)
	}
	c.ControlPlane = strings.TrimRight(strings.TrimSpace(c.ControlPlane), "/")
	c.Token = strings.TrimSpace(c.Token)
	return c, nil
}

// Save writes credentials with mode 0600 (owner-only), creating the config dir.
func Save(c Credentials) error {
	c.ControlPlane = strings.TrimRight(strings.TrimSpace(c.ControlPlane), "/")
	c.Token = strings.TrimSpace(c.Token)
	if c.Token == "" {
		return errors.New("token is required")
	}
	d, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	p := filepath.Join(d, fileName)
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(p, b, 0o600)
}

// Clear removes the credentials file. No-op if missing.
func Clear() error {
	p, err := Path()
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Resolve merges flag/env/file sources. Flag values win when non-empty;
// then env; then the credentials file; then defaults.
func Resolve(flagAddr, flagToken, defaultAddr string) (Credentials, error) {
	file, err := Load()
	if err != nil {
		return Credentials{}, err
	}

	addr := strings.TrimRight(strings.TrimSpace(flagAddr), "/")
	if addr == "" {
		addr = strings.TrimRight(strings.TrimSpace(os.Getenv(EnvControlPlane)), "/")
	}
	if addr == "" {
		addr = strings.TrimRight(strings.TrimSpace(os.Getenv(EnvAddr)), "/")
	}
	if addr == "" {
		addr = file.ControlPlane
	}
	if addr == "" {
		addr = strings.TrimRight(strings.TrimSpace(defaultAddr), "/")
	}

	token := strings.TrimSpace(flagToken)
	if token == "" {
		token = strings.TrimSpace(os.Getenv(EnvToken))
	}
	if token == "" {
		token = file.Token
	}

	return Credentials{ControlPlane: addr, Token: token}, nil
}
