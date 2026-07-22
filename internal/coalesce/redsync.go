package coalesce

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-redsync/redsync/v4"
	"github.com/go-redsync/redsync/v4/redis/goredis/v9"
	"github.com/redis/go-redis/v9"
)

// DefaultRedisKeyPrefix is prepended to lock keys when RedisConfig.KeyPrefix is empty.
const DefaultRedisKeyPrefix = "specula:lock:"

// RedisOptions configures NewRedsyncLocker (thin adapter over go-redis + redsync).
type RedisOptions struct {
	Addr      string
	Password  string
	DB        int
	KeyPrefix string
}

// RedsyncLocker is a coalesce.Locker backed by go-redsync/redsync/v4.
// It does not invent a lock protocol — Acquire/Release map 1:1 to Mutex
// LockContext / UnlockContext.
type RedsyncLocker struct {
	rs     *redsync.Redsync
	client *redis.Client
	prefix string
}

// NewRedsyncLocker builds a Locker from RedisOptions. The returned closer
// shuts down the underlying go-redis client (call on process shutdown).
func NewRedsyncLocker(opts RedisOptions) (*RedsyncLocker, func() error, error) {
	addr := strings.TrimSpace(opts.Addr)
	if addr == "" {
		return nil, nil, fmt.Errorf("coalesce: redis addr is required")
	}
	prefix := opts.KeyPrefix
	if prefix == "" {
		prefix = DefaultRedisKeyPrefix
	}
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: opts.Password,
		DB:       opts.DB,
	})
	pool := goredis.NewPool(client)
	return &RedsyncLocker{
		rs:     redsync.New(pool),
		client: client,
		prefix: prefix,
	}, client.Close, nil
}

var _ Locker = (*RedsyncLocker)(nil)

// Acquire obtains a redsync mutex for key with the given TTL (mutex expiry).
func (l *RedsyncLocker) Acquire(ctx context.Context, key string, ttl time.Duration) (Lock, error) {
	if ttl <= 0 {
		ttl = DefaultLockTTL
	}
	name := l.prefix + key
	mu := l.rs.NewMutex(name,
		redsync.WithExpiry(ttl),
		redsync.WithTries(32),
		redsync.WithRetryDelay(50*time.Millisecond),
	)
	if err := mu.LockContext(ctx); err != nil {
		return nil, fmt.Errorf("coalesce: redsync lock %q: %w", name, err)
	}
	return &redsyncLock{mu: mu}, nil
}

type redsyncLock struct {
	mu *redsync.Mutex
}

var _ Lock = (*redsyncLock)(nil)

func (lk *redsyncLock) Token() string { return lk.mu.Value() }

func (lk *redsyncLock) Release(ctx context.Context) error {
	ok, err := lk.mu.UnlockContext(ctx)
	if err != nil {
		return fmt.Errorf("coalesce: redsync unlock: %w", err)
	}
	if !ok {
		// Ownership lost (expiry) — same semantics as localLocker TTL eviction.
		return nil
	}
	return nil
}
