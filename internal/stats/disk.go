// Package stats — disk.go: du-sb fallback and filesystem capacity helpers.
package stats

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/shirou/gopsutil/v3/disk"
)

// duBytes walks root and returns the total byte count of all regular files,
// mimicking `du -sb <root>`. It is used as a best-effort fallback for opaque
// caches (e.g., git bare mirrors) where blobs are not tracked in the
// MetadataStore and therefore absent from CacheSizeByProtocol.
//
// # A missing root is an error, not a zero
//
// The walk deliberately swallows per-entry errors so one unreadable
// subdirectory cannot void the whole measurement. filepath.WalkDir reports an
// absent ROOT through that very same callback, though — it invokes fn(root,
// nil, err) once and then returns whatever the callback returned — so without
// the explicit stat below a root that does not exist walked to (0, nil) and was
// indistinguishable from a root that exists and is empty.
//
// Those are different claims. "0 bytes" says we looked and found nothing
// cached; a root that is not there was never looked at, and the honest answer
// is "unknown". Both callers skip a protocol whose du fails, which leaves it out
// of the stats map so the dashboard renders "—" — the convention dto.go states
// ("render '—', never a fabricated zero") and that e181e5a applied to git's
// object count.
func duBytes(root string) (int64, error) {
	// Fail loudly for an unstattable root (missing, or a permission-denied
	// parent). Every deeper error stays best-effort inside the walk.
	if _, err := os.Stat(root); err != nil {
		return 0, fmt.Errorf("du: stat root %q: %w", root, err)
	}

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
