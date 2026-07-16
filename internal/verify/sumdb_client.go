package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	xsumdb "golang.org/x/mod/sumdb"
	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

// defaultSumDBKey is the well-known verifier key for sum.golang.org.
// sum.golang.google.cn is a CN-accessible CDN mirror that signs notes with
// the same key (same underlying transparency log).
// Source: cmd/go/internal/modfetch/sumdb.go
const defaultSumDBKey = "sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8"

// specOps implements sumdb.ClientOps for SumDBVerifier.
//
// It routes remote reads to the configured HTTP sumdb endpoint, maintains an
// in-memory tile/lookup cache for the lifetime of the verifier instance, and
// enforces anti-rollback via TreeSizeStore: when a WriteConfig call presents a
// new signed tree head, the tree size is checked against the persisted
// high-water mark; a regression causes SecurityError and returns a hard error.
type specOps struct {
	baseURL  string        // sumdb HTTP base URL (no trailing slash)
	vkeyText string        // "<name>+<id>+<pubkey>" verifier key (with trailing newline)
	name     string        // sumdb name extracted from the verifier key
	store    TreeSizeStore // may be nil; anti-rollback disabled when nil
	httpc    *http.Client

	mu     sync.Mutex
	latest []byte            // in-memory bytes of the latest signed tree head
	tiles  map[string][]byte // in-memory tile / lookup cache

	secMu  sync.Mutex
	secErr error // non-nil after SecurityError is called
}

// newSpecOps constructs a specOps. vkeyText is the verifier key
// ("<name>+<id>+<pubkey>"); empty defaults to the sum.golang.org key.
// baseURL is the sumdb HTTP endpoint (e.g. "https://sum.golang.google.cn").
// httpc may be nil (http.DefaultClient with 30 s timeout is used).
func newSpecOps(vkeyText, baseURL string, store TreeSizeStore, httpc *http.Client) (*specOps, error) {
	if vkeyText == "" {
		vkeyText = defaultSumDBKey
	}
	// Validate the verifier key format and extract the sumdb name.
	v, err := note.NewVerifier(strings.TrimSpace(vkeyText))
	if err != nil {
		return nil, fmt.Errorf("sumdb: bad verifier key: %w", err)
	}
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	return &specOps{
		baseURL:  strings.TrimRight(baseURL, "/"),
		vkeyText: strings.TrimSpace(vkeyText) + "\n", // sumdb.Client expects trailing newline
		name:     v.Name(),
		store:    store,
		httpc:    httpc,
		tiles:    make(map[string][]byte),
	}, nil
}

// securityError returns the first security error captured, if any.
func (o *specOps) securityError() error {
	o.secMu.Lock()
	defer o.secMu.Unlock()
	return o.secErr
}

// -- sumdb.ClientOps implementation --

// ReadRemote fetches path from the configured sumdb endpoint.
// path begins with "/lookup" or "/tile/".
func (o *specOps) ReadRemote(path string) ([]byte, error) {
	u := o.baseURL + path
	resp, err := o.httpc.Get(u) //nolint:noctx // ClientOps interface has no context
	if err != nil {
		return nil, fmt.Errorf("sumdb: GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("sumdb: GET %s: HTTP %d: %s", u, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return io.ReadAll(resp.Body)
}

// ReadConfig returns config file data.
//   - "key" → the verifier key text (with trailing newline)
//   - "{name}/latest" → in-memory latest signed tree head (nil = start fresh)
func (o *specOps) ReadConfig(file string) ([]byte, error) {
	if file == "key" {
		return []byte(o.vkeyText), nil
	}
	// "{name}/latest"
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.latest == nil {
		return nil, nil // start from an empty tree; anti-rollback enforced in WriteConfig
	}
	return append([]byte(nil), o.latest...), nil
}

// WriteConfig atomically updates "{name}/latest".
//
// Anti-rollback: if the new signed head's tree size N is less than the
// persisted high-water mark, SecurityError is called and a hard error is
// returned (NOT ErrWriteConflict, so the Client does not retry).
func (o *specOps) WriteConfig(file string, old, new []byte) error {
	if file == "key" {
		return errors.New("sumdb: key config is read-only")
	}

	// CAS: if current != old, signal write-conflict so the Client retries.
	o.mu.Lock()
	current := o.latest
	o.mu.Unlock()
	if !bytes.Equal(current, old) {
		return xsumdb.ErrWriteConflict
	}

	// Anti-rollback: parse new tree size and compare with persisted high-water.
	if len(new) > 0 && o.store != nil {
		newN, err := parseTreeSizeFromNote(new)
		if err != nil {
			return fmt.Errorf("sumdb: parse tree size from signed note: %w", err)
		}
		stored, err := o.store.GetTreeSize(context.Background(), o.name)
		if err != nil {
			return fmt.Errorf("sumdb: read tree size from store for %q: %w", o.name, err)
		}
		if newN < stored {
			msg := fmt.Sprintf(
				"sumdb anti-rollback: signed tree size %d < persisted high-water %d for %q — possible rollback or fork attack",
				newN, stored, o.name,
			)
			o.SecurityError(msg)
			o.secMu.Lock()
			err := o.secErr
			o.secMu.Unlock()
			return err // hard error, not ErrWriteConflict
		}
		if newN > stored {
			if err := o.store.SetTreeSize(context.Background(), o.name, newN); err != nil {
				return fmt.Errorf("sumdb: persist tree size %d for %q: %w", newN, o.name, err)
			}
		}
	}

	o.mu.Lock()
	o.latest = append([]byte(nil), new...)
	o.mu.Unlock()
	return nil
}

// ReadCache returns a cached tile or lookup record. Any error is treated as a
// cache miss by the Client.
func (o *specOps) ReadCache(file string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	data, ok := o.tiles[file]
	if !ok {
		return nil, errors.New("cache miss")
	}
	return append([]byte(nil), data...), nil
}

// WriteCache stores tile or lookup data in the in-memory cache.
func (o *specOps) WriteCache(file string, data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tiles[file] = append([]byte(nil), data...)
}

// Log is a no-op (the sumdb.Client calls this for informational messages).
func (o *specOps) Log(_ string) {}

// SecurityError records the first security error message. The sumdb.Client
// calls this when it detects inconsistent signed tree heads (fork / rollback).
func (o *specOps) SecurityError(msg string) {
	o.secMu.Lock()
	defer o.secMu.Unlock()
	if o.secErr == nil {
		o.secErr = fmt.Errorf("sumdb security: %s", msg)
	}
}

// parseTreeSizeFromNote extracts the tree size N from a signed sumdb note
// without re-verifying the signature (the sumdb.Client already verified it
// before calling WriteConfig; we only need the size for anti-rollback).
//
// Signed note wire format:
//
//	go.sum database tree\n
//	N\n
//	HASH\n
//	\n
//	— <signer> <sig>\n
//
// The note text (everything before the blank-line separator) ends with "\n"
// (the HASH line's newline). bytes.Cut on "\n\n" excludes that trailing "\n"
// from the "before" slice, so we restore it before handing to tlog.ParseTree.
func parseTreeSizeFromNote(signed []byte) (int64, error) {
	var text []byte
	before, _, found := bytes.Cut(signed, []byte("\n\n"))
	if found {
		// Restore the "\n" that was part of the double-newline separator
		// but belongs to the note text (HASH line's trailing newline).
		text = append(before, '\n')
	} else {
		// No blank-line separator: treat the whole buffer as note text.
		text = bytes.TrimRight(signed, "\n")
		text = append(text, '\n')
	}
	// tlog.ParseTree expects exactly: "go.sum database tree\nN\nHASH\n"
	tree, err := tlog.ParseTree(text)
	if err != nil {
		return 0, fmt.Errorf("parseTreeSizeFromNote: %w", err)
	}
	return tree.N, nil
}
