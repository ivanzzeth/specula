package blob

import (
	"fmt"
	"sync"
)

// Factory constructs a BlobStore from a string-keyed config map.
type Factory func(cfg map[string]string) (BlobStore, error)

var (
	driversMu sync.RWMutex
	drivers   = map[string]Factory{}
)

func init() {
	// local is always registered (light deps).
	Register("local", func(cfg map[string]string) (BlobStore, error) {
		root := cfg["root"]
		if root == "" {
			return nil, fmt.Errorf("blob: local driver requires cfg[\"root\"]")
		}
		// Imported lazily via openLocal to avoid an import cycle with pkg/store/local.
		return openLocal(root), nil
	})
}

// openLocal is set by the local driver package's init, or by register_local.go.
var openLocal = func(root string) BlobStore {
	panic("blob: local driver not linked — import github.com/ivanzzeth/specula/pkg/store/local")
}

// SetLocalOpener wires the local-disk factory. Called from pkg/store/local init.
func SetLocalOpener(fn func(root string) BlobStore) {
	openLocal = fn
}

// Register adds a named blob driver factory. Safe for concurrent use.
// Intended for opt-in packages (s3) via init().
func Register(name string, f Factory) {
	driversMu.Lock()
	defer driversMu.Unlock()
	drivers[name] = f
}

// Open constructs a BlobStore by registered driver name.
func Open(name string, cfg map[string]string) (BlobStore, error) {
	driversMu.RLock()
	f, ok := drivers[name]
	driversMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("blob: unknown driver %q (blank-import the driver package?)", name)
	}
	return f(cfg)
}
