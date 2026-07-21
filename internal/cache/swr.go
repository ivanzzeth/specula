package cache

import (
	"context"
	"time"

	"golang.org/x/sync/singleflight"
)

// swrTimeout bounds detached background revalidation (RFC 5861 SWR).
const swrTimeout = 30 * time.Second

var swrSF singleflight.Group

// StartBackgroundRefresh runs fn once per key, coalesced across callers.
// The work is detached from the request context so client disconnect does not
// cancel the refresh; a fixed timeout still bounds upstream wait.
func StartBackgroundRefresh(key string, fn func(ctx context.Context) error) {
	if key == "" || fn == nil {
		return
	}
	go func() {
		_, _, _ = swrSF.Do(key, func() (any, error) {
			ctx, cancel := context.WithTimeout(context.Background(), swrTimeout)
			defer cancel()
			return nil, fn(ctx)
		})
	}()
}
