// Package specula is the one-shot facade for the Specula core SDK.
//
//	s, err := specula.New(ctx, specula.Options{DataDir: "./data"})
//	entry, err := s.Get(ctx, artifact.ArtifactRef{...})
//
// For HTTP embedding, use pkg/embed (keeps protocol handlers out of the default
// SDK dependency set). See docs/LIBRARY.md.
package specula

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/ivanzzeth/specula/pkg/artifact"
	"github.com/ivanzzeth/specula/pkg/cache"
	"github.com/ivanzzeth/specula/pkg/store/blob"
	"github.com/ivanzzeth/specula/pkg/store/local"
	"github.com/ivanzzeth/specula/pkg/store/meta"
	"github.com/ivanzzeth/specula/pkg/store/sqlite"
	"github.com/ivanzzeth/specula/pkg/upstream"
	"github.com/ivanzzeth/specula/pkg/verify"
)

// Options configures a Specula instance. Zero value is not valid — set DataDir
// or explicit Blob/Meta stores.
type Options struct {
	// DataDir, when set, creates local blob + sqlite stores under
	// DataDir/blobs and DataDir/meta.db (unless Blob/Meta override).
	DataDir string

	// Blob overrides the CAS blob store. If nil and DataDir is set, local disk
	// under DataDir/blobs is used.
	Blob blob.BlobStore

	// Meta overrides the metadata store. If nil and DataDir is set, SQLite at
	// DataDir/meta.db is used.
	Meta meta.MetadataStore

	// QuarantineDir is where verify-on-write temp files land. Defaults to
	// DataDir/quarantine or os.TempDir().
	QuarantineDir string

	// Upstreams maps protocol → ordered fallback mirrors for Get().
	// If empty, Get() only serves cache hits (no fetch).
	Upstreams map[string][]upstream.Upstream

	// MaxBytes is the immutable-cache capacity ceiling (0 = unlimited).
	// When exceeded after a Store/Get promote, oldest unpinned entries are
	// evicted. See cache.WithMaxBytes.
	MaxBytes int64

	// Verifiers, when non-nil, replaces the default checksum+tofu chain.
	Verifiers []verify.Verifier

	// Logger for structured logs. Defaults to slog.Default().
	Logger *slog.Logger

	// CloseMeta, when non-nil, is called from Server.Close.
	CloseMeta func() error
}

// Server is a configured Specula core: cache + verify + programmatic Get/Open.
type Server struct {
	opts          Options
	blobs         blob.BlobStore
	meta          meta.MetadataStore
	chain         *verify.Chain
	cm            cache.CacheManager
	upstream      upstream.Client
	quarantineDir string
	log           *slog.Logger
	closeMeta     func() error
}

// New constructs a Server from Options. The returned Server must be Closed.
func New(ctx context.Context, opts Options) (*Server, error) {
	_ = ctx
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	blobs := opts.Blob
	metaStore := opts.Meta
	closeMeta := opts.CloseMeta

	if opts.DataDir != "" {
		if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
			return nil, fmt.Errorf("specula: mkdir data dir: %w", err)
		}
		if blobs == nil {
			blobRoot := filepath.Join(opts.DataDir, "blobs")
			if err := os.MkdirAll(blobRoot, 0o755); err != nil {
				return nil, fmt.Errorf("specula: mkdir blobs: %w", err)
			}
			blobs = local.New(blobRoot)
		}
		if metaStore == nil {
			dsn := filepath.Join(opts.DataDir, "meta.db")
			ms, err := sqlite.NewSQLiteStore(dsn)
			if err != nil {
				return nil, fmt.Errorf("specula: open sqlite: %w", err)
			}
			metaStore = ms
			if closeMeta == nil {
				closeMeta = ms.Close
			}
		}
	}

	if blobs == nil {
		return nil, fmt.Errorf("specula: Blob store required (set Options.Blob or Options.DataDir)")
	}
	if metaStore == nil {
		return nil, fmt.Errorf("specula: Meta store required (set Options.Meta or Options.DataDir)")
	}

	qdir := opts.QuarantineDir
	if qdir == "" {
		if opts.DataDir != "" {
			qdir = filepath.Join(opts.DataDir, "quarantine")
		} else {
			qdir = os.TempDir()
		}
	}
	if err := os.MkdirAll(qdir, 0o755); err != nil {
		return nil, fmt.Errorf("specula: mkdir quarantine: %w", err)
	}

	verifiers := opts.Verifiers
	if verifiers == nil {
		verifiers = []verify.Verifier{
			verify.NewChecksumVerifier(),
			verify.NewTofuVerifier(verify.NewMetaTofuStore(metaStore)),
		}
	}
	chain := verify.NewChain(verifiers...)
	cmOpts := []cache.Option{cache.WithLogger(log)}
	if opts.MaxBytes > 0 {
		cmOpts = append(cmOpts, cache.WithMaxBytes(opts.MaxBytes))
	}
	cm := cache.New(blobs, metaStore, chain, cmOpts...)

	return &Server{
		opts:          opts,
		blobs:         blobs,
		meta:          metaStore,
		chain:         chain,
		cm:            cm,
		upstream:      upstream.NewClient(),
		quarantineDir: qdir,
		log:           log,
		closeMeta:     closeMeta,
	}, nil
}

// Close releases metadata resources when a closer was registered.
func (s *Server) Close() error {
	if s.closeMeta != nil {
		return s.closeMeta()
	}
	return nil
}

// CacheManager returns the underlying two-tier cache (advanced wiring).
func (s *Server) CacheManager() cache.CacheManager { return s.cm }

// Chain returns the verification chain (advanced wiring).
func (s *Server) Chain() *verify.Chain { return s.chain }

// Meta returns the metadata store.
func (s *Server) Meta() meta.MetadataStore { return s.meta }

// Blob returns the CAS blob store.
func (s *Server) Blob() blob.BlobStore { return s.blobs }

// QuarantineDir returns the on-disk quarantine directory.
func (s *Server) QuarantineDir() string { return s.quarantineDir }

// Upstreams returns the configured per-protocol upstream lists.
func (s *Server) Upstreams() map[string][]upstream.Upstream { return s.opts.Upstreams }

// UpstreamClient returns the shared upstream client.
func (s *Server) UpstreamClient() upstream.Client { return s.upstream }

// Logger returns the server logger.
func (s *Server) Logger() *slog.Logger { return s.log }

// Get looks up ref in the verified cache, or on miss fetches via configured
// upstreams, runs verify-on-write, and returns the CacheEntry.
//
// Never returns unverified bytes. The entry's Tier is the honest tier achieved.
func (s *Server) Get(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	entry, err := s.cm.Lookup(ctx, ref)
	if err != nil {
		return nil, err
	}
	if entry != nil {
		return entry, nil
	}

	ups := s.opts.Upstreams[ref.Protocol]
	if len(ups) == 0 {
		return nil, fmt.Errorf("specula: cache miss for %s/%s@%s and no upstreams for protocol %q",
			ref.Protocol, ref.Name, ref.Version, ref.Protocol)
	}

	rc, umeta, err := s.upstream.Fetch(ctx, ref, ups)
	if err != nil {
		return nil, fmt.Errorf("specula: upstream fetch: %w", err)
	}
	defer rc.Close()

	art, cleanup, err := cache.Quarantine(ctx, s.quarantineDir, rc, umeta)
	if err != nil {
		return nil, fmt.Errorf("specula: quarantine: %w", err)
	}

	entry, err = s.cm.Store(ctx, ref, art)
	if err != nil {
		cleanup()
		return nil, err
	}
	return entry, nil
}

// Open returns a reader for an already-verified CacheEntry (offset 0, full length).
// Only serves verified CAS content.
func (s *Server) Open(ctx context.Context, entry *artifact.CacheEntry) (io.ReadCloser, error) {
	if entry == nil {
		return nil, fmt.Errorf("specula: nil entry")
	}
	rc, _, err := s.cm.Serve(ctx, entry.Ref, 0, -1)
	if err != nil {
		if es, ok := s.cm.(cache.EntryServer); ok {
			return es.ServeEntry(ctx, entry, 0, -1)
		}
		return nil, err
	}
	return rc, nil
}

// VerifyFile runs the verification chain over an already-quarantined on-disk
// artifact without promoting it to the cache.
func (s *Server) VerifyFile(ctx context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	return s.chain.Verify(ctx, ref, art)
}
