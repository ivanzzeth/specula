package meta

import (
	"context"
	"fmt"
	"sync"
)

// Factory constructs a MetadataStore from a DSN.
type Factory func(ctx context.Context, dsn string) (MetadataStore, error)

var (
	driversMu sync.RWMutex
	drivers   = map[string]Factory{}
)

// Register adds a named metadata driver factory. Safe for concurrent use.
// Intended for opt-in packages (postgres) via init().
func Register(name string, f Factory) {
	driversMu.Lock()
	defer driversMu.Unlock()
	drivers[name] = f
}

// Open constructs a MetadataStore by registered driver name.
func Open(ctx context.Context, name, dsn string) (MetadataStore, error) {
	driversMu.RLock()
	f, ok := drivers[name]
	driversMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("meta: unknown driver %q (blank-import the driver package?)", name)
	}
	return f(ctx, dsn)
}
