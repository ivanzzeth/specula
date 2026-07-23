package config

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed example.yaml
var ExampleYAML []byte

// WriteExampleIfMissing writes the embedded reference config to path when the
// file does not exist. transform, when non-nil, rewrites the YAML before write
// (e.g. system install remaps ~/.specula → /var/lib/specula).
// created is true only when a new file was written.
func WriteExampleIfMissing(path string, transform func(string) string) (created bool, err error) {
	if path == "" {
		return false, fmt.Errorf("config: empty path")
	}
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("config: stat %q: %w", path, err)
	}

	body := string(ExampleYAML)
	if transform != nil {
		body = transform(body)
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, fmt.Errorf("config: mkdir %q: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return false, fmt.Errorf("config: write %q: %w", path, err)
	}
	return true, nil
}

// LoadOrCreate loads path, creating it from the embedded example when missing.
// created is true when the file was freshly written.
func LoadOrCreate(path string) (cfg *Config, created bool, err error) {
	created, err = WriteExampleIfMissing(path, nil)
	if err != nil {
		return nil, false, err
	}
	cfg, err = Load(path)
	if err != nil {
		return nil, created, err
	}
	return cfg, created, nil
}
