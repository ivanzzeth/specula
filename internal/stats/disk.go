// Package stats — disk.go: du-sb fallback and filesystem capacity helpers.
package stats

import (
	"io/fs"
	"path/filepath"

	"github.com/shirou/gopsutil/v3/disk"
)

// duBytes walks root and returns the total byte count of all regular files,
// mimicking `du -sb <root>`. It is used as a best-effort fallback for opaque
// caches (e.g., git bare mirrors) where blobs are not tracked in the
// MetadataStore and therefore absent from CacheSizeByProtocol.
func duBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			// Skip unreadable entries rather than aborting the whole walk.
			return nil //nolint:nilerr
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return nil //nolint:nilerr
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// diskFreeBytes returns the available free bytes on the filesystem that
// contains path. Used by the control-plane dashboard capacity reporter.
func diskFreeBytes(path string) (uint64, error) {
	usage, err := disk.Usage(path)
	if err != nil {
		return 0, err
	}
	return usage.Free, nil
}
