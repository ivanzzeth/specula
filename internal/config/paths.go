package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultDataDirName is the directory under the user home used for local
// single-node data (blobs, sqlite meta, git mirrors, registry token key).
const DefaultDataDirName = ".specula"

// DefaultDataDir returns $HOME/.specula. Used for built-in storage defaults and
// as the expansion target for "~/.specula/…" paths in YAML.
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: resolve home directory: %w", err)
	}
	return filepath.Join(home, DefaultDataDirName), nil
}

// ExpandPath expands a leading "~/" (or bare "~") to the user home directory.
// Other paths are returned unchanged. Empty input stays empty.
func ExpandPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("config: expand %q: %w", p, err)
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

func expandPathField(dst *string) error {
	expanded, err := ExpandPath(*dst)
	if err != nil {
		return err
	}
	*dst = expanded
	return nil
}

// expandConfigPaths rewrites ~-prefixed filesystem paths after YAML/env merge.
// PostgreSQL DSNs are left alone (they are URLs, not home-relative paths).
func expandConfigPaths(cfg *Config) error {
	if err := expandPathField(&cfg.Storage.Blob.Local.Root); err != nil {
		return err
	}
	if cfg.Storage.Meta.Driver == "sqlite" || cfg.Storage.Meta.Driver == "" {
		if err := expandPathField(&cfg.Storage.Meta.DSN); err != nil {
			return err
		}
	}
	if err := expandPathField(&cfg.Auth.RegistryTokenKeyPath); err != nil {
		return err
	}
	for name, pc := range cfg.Protocols {
		v := &pc.Verification
		if err := expandPathField(&v.CosignKey); err != nil {
			return err
		}
		if err := expandPathField(&v.Keyring); err != nil {
			return err
		}
		if err := expandPathField(&v.AllowedSigners); err != nil {
			return err
		}
		if v.Cosign != nil {
			for i := range v.Cosign.Keys {
				if err := expandPathField(&v.Cosign.Keys[i]); err != nil {
					return err
				}
			}
		}
		if v.GPG != nil {
			if err := expandPathField(&v.GPG.Keyring); err != nil {
				return err
			}
		}
		if v.Provenance != nil {
			if err := expandPathField(&v.Provenance.Keyring); err != nil {
				return err
			}
		}
		if v.SignedRefs != nil {
			if err := expandPathField(&v.SignedRefs.AllowedSigners); err != nil {
				return err
			}
		}
		if pc.Git != nil {
			if err := expandPathField(&pc.Git.MirrorDir); err != nil {
				return err
			}
		}
		cfg.Protocols[name] = pc
	}
	return nil
}

// applyStorageDefaults fills empty local-storage paths with $HOME/.specula/….
func applyStorageDefaults(cfg *Config) error {
	dataDir, err := DefaultDataDir()
	if err != nil {
		return err
	}
	if cfg.Storage.Blob.Driver == "" {
		cfg.Storage.Blob.Driver = "local"
	}
	if cfg.Storage.Blob.Driver == "local" && cfg.Storage.Blob.Local.Root == "" {
		cfg.Storage.Blob.Local.Root = filepath.Join(dataDir, "blobs")
	}
	if cfg.Storage.Meta.Driver == "" {
		cfg.Storage.Meta.Driver = "sqlite"
	}
	if cfg.Storage.Meta.Driver == "sqlite" && cfg.Storage.Meta.DSN == "" {
		cfg.Storage.Meta.DSN = filepath.Join(dataDir, "meta.db")
	}
	for name, pc := range cfg.Protocols {
		if pc.Git != nil && pc.Git.MirrorDir == "" {
			pc.Git.MirrorDir = filepath.Join(dataDir, "git")
			cfg.Protocols[name] = pc
		}
	}
	return nil
}
