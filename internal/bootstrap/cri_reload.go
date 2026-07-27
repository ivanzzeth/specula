package bootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

const CRIReloadStampFile = ".cri-reload-hash"

// CRIReloadHash fingerprints the desired CRI certs.d root for stamp comparison.
func CRIReloadHash(certsDir string) string {
	certsDir = strings.TrimRight(strings.TrimSpace(certsDir), "/")
	sum := sha256.Sum256([]byte("cri-config_path=" + certsDir + "\n"))
	return hex.EncodeToString(sum[:16])
}

// NeedsCRIReload reports whether stampDir's stamp differs from hash.
func NeedsCRIReload(stampDir, hash string) bool {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return false
	}
	prev, err := os.ReadFile(filepath.Join(stampDir, CRIReloadStampFile))
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(prev)) != hash
}

// WriteCRIReloadStamp persists hash after a successful containerd restart.
func WriteCRIReloadStamp(stampDir, hash string) error {
	if err := os.MkdirAll(stampDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stampDir, CRIReloadStampFile), []byte(strings.TrimSpace(hash)+"\n"), 0o644)
}
