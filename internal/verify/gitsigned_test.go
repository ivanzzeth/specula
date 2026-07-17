package verify

// Tests for GitSignedVerifier (gitsigned.go).
//
// Traceable to:
//   - PRD §G2 "git: signed (可选) — 签名 tag/commit (配 allowed-signers)"
//   - ARCHITECTURE: signed refs are opt-in; unsigned refs degrade to tofu, not fail
//
// VerifyRef is the real per-ref classifier. These tests drive it against real
// git objects: an SSH-signed annotated tag (RefSigned), an unsigned lightweight
// tag (RefUnsigned), and a signed tag whose principal is NOT in the anchor
// (RefUntrusted). The signing key is generated per-test; verification is fully
// offline against a local allowed-signers file (the CN-obtainable anchor).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// ─────────────────────────────────────────────────────────────────────────────
// Constructor
// ─────────────────────────────────────────────────────────────────────────────

func TestNewGitSignedVerifier_EmptyPath(t *testing.T) {
	_, err := NewGitSignedVerifier("", "warn")
	require.Error(t, err, "empty path must be rejected")
	assert.Contains(t, err.Error(), "allowed-signers")
}

func TestNewGitSignedVerifier_ValidPath(t *testing.T) {
	f := filepath.Join(t.TempDir(), "allowed-signers")
	require.NoError(t, os.WriteFile(f, []byte(""), 0o600))

	v, err := NewGitSignedVerifier(f, "")
	if err != nil {
		t.Skipf("git not found on PATH: %v", err)
	}
	assert.Equal(t, "gitsigned", v.Name())
	assert.Equal(t, artifact.TierSigned, v.Tier())
	assert.Equal(t, "warn", v.Policy(), "empty policy defaults to warn")
	assert.False(t, v.Enforce())
}

func TestNewGitSignedVerifier_EnforcePolicy(t *testing.T) {
	f := filepath.Join(t.TempDir(), "allowed-signers")
	require.NoError(t, os.WriteFile(f, []byte(""), 0o600))
	v, err := NewGitSignedVerifier(f, "enforce")
	if err != nil {
		t.Skipf("git not found on PATH: %v", err)
	}
	assert.True(t, v.Enforce(), "enforce policy must fail closed on untrusted signatures")
}

// TestGitSignedVerifier_Interface verifies the inert Chain entry self-gates to a
// skip for every ref — the real work is VerifyRef, not the shared Chain.
func TestGitSignedVerifier_Interface(t *testing.T) {
	f := filepath.Join(t.TempDir(), "allowed-signers")
	require.NoError(t, os.WriteFile(f, []byte(""), 0o600))
	v, err := NewGitSignedVerifier(f, "warn")
	if err != nil {
		t.Skipf("skipping: %v", err)
	}
	assert.Equal(t, "gitsigned", v.Name())
	assert.Equal(t, artifact.TierSigned, v.Tier())

	res, verr := v.Verify(context.Background(),
		artifact.ArtifactRef{Protocol: "git", Name: "github.com/o/r", Version: "v1"},
		&artifact.Artifact{})
	require.NoError(t, verr)
	assert.Equal(t, artifact.StatusSkip, res.Status,
		"chain entry must be an inert skip; VerifyRef does the real verification")
	assert.Equal(t, artifact.TierChecksum, res.Tier, "an inert skip must not claim TierSigned")

	var _ Verifier = v
}

// ─────────────────────────────────────────────────────────────────────────────
// VerifyRef — the real classifier
// ─────────────────────────────────────────────────────────────────────────────

type sigTestRepo struct {
	dir            string
	allowedSigners string
}

func gitVerifyTestCmd(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
}

// newSigTestRepo creates a repo where:
//   - tag "signed"    is an SSH-signed annotated tag by an ALLOWED principal,
//   - tag "unsigned"  is a plain lightweight tag,
//   - tag "untrusted" is an SSH-signed annotated tag by a principal NOT in the
//     allowed-signers file.
func newSigTestRepo(t *testing.T) *sigTestRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skipf("ssh-keygen not on PATH: %v", err)
	}

	base := t.TempDir()
	keydir := filepath.Join(base, "keys")
	require.NoError(t, os.MkdirAll(keydir, 0o700))

	genKey := func(name, principal string) (pubPath, pubLine string) {
		priv := filepath.Join(keydir, name)
		out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", principal, "-f", priv).CombinedOutput()
		require.NoErrorf(t, err, "ssh-keygen: %s", out)
		pubBytes, err := os.ReadFile(priv + ".pub")
		require.NoError(t, err)
		return priv + ".pub", strings.TrimSpace(string(pubBytes))
	}

	allowedPub, allowedPubLine := genKey("allowed", "allowed@specula")
	otherPub, _ := genKey("other", "attacker@specula")

	allowedSigners := filepath.Join(base, "allowed_signers")
	require.NoError(t, os.WriteFile(allowedSigners,
		[]byte("allowed@specula "+allowedPubLine+"\n"), 0o600))

	repo := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	env := []string{
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	}
	gitVerifyTestCmd(t, repo, env, "init", "--quiet", "--initial-branch=master", repo)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README"), []byte("hi\n"), 0o644))
	gitVerifyTestCmd(t, repo, env, "add", "README")
	gitVerifyTestCmd(t, repo, env, "commit", "--quiet", "-m", "init")
	gitVerifyTestCmd(t, repo, env, "config", "gpg.format", "ssh")

	gitVerifyTestCmd(t, repo, env, "-c", "user.signingkey="+allowedPub,
		"tag", "-s", "signed", "-m", "signed by allowed")
	gitVerifyTestCmd(t, repo, env, "-c", "user.signingkey="+otherPub,
		"tag", "-s", "untrusted", "-m", "signed by attacker")
	gitVerifyTestCmd(t, repo, env, "tag", "unsigned")

	return &sigTestRepo{dir: repo, allowedSigners: allowedSigners}
}

func TestVerifyRef_Signed(t *testing.T) {
	r := newSigTestRepo(t)
	v, err := NewGitSignedVerifier(r.allowedSigners, "warn")
	require.NoError(t, err)

	trust, msg, err := v.VerifyRef(context.Background(), r.dir, "signed")
	require.NoError(t, err)
	assert.Equal(t, RefSigned, trust,
		"a tag signed by an allowed principal must be RefSigned; git said: %s", msg)
}

func TestVerifyRef_Unsigned(t *testing.T) {
	r := newSigTestRepo(t)
	v, err := NewGitSignedVerifier(r.allowedSigners, "warn")
	require.NoError(t, err)

	trust, _, err := v.VerifyRef(context.Background(), r.dir, "unsigned")
	require.NoError(t, err)
	assert.Equal(t, RefUnsigned, trust,
		"a plain lightweight tag must be RefUnsigned (opt-in, degrade to tofu)")
}

func TestVerifyRef_Untrusted(t *testing.T) {
	r := newSigTestRepo(t)
	v, err := NewGitSignedVerifier(r.allowedSigners, "warn")
	require.NoError(t, err)

	trust, msg, err := v.VerifyRef(context.Background(), r.dir, "untrusted")
	require.NoError(t, err)
	assert.Equal(t, RefUntrusted, trust,
		"a tag signed by a principal NOT in allowed-signers must be RefUntrusted, not RefUnsigned; git said: %s", msg)
}

// TestVerifyRef_RealPublicRepo_PGPTagDegrades drives VerifyRef against a REAL
// public repo's REAL signed tag: git/git's v2.43.0 is a PGP-signed annotated
// tag (Junio C Hamano). Under an SSH allowed-signers anchor we hold no PGP key,
// so the honest classification is RefUntrusted (signature present, not
// authenticated by our anchor) → degrade to tofu, NEVER a false `signed`. This
// proves the verifier runs against real-world signatures without over-claiming.
// Network-gated: skipped when github.com is unreachable.
func TestVerifyRef_RealPublicRepo_PGPTagDegrades(t *testing.T) {
	if testing.Short() {
		t.Skip("network test skipped in -short mode")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}

	dir := t.TempDir()
	initEnv := []string{"GIT_TERMINAL_PROMPT=0"}
	gitVerifyTestCmd(t, dir, initEnv, "init", "--quiet", dir)

	// Fetch ONLY the v2.43.0 tag object, shallow + blobless, to stay fast.
	fetch := exec.Command("git", "-c", "protocol.version=2",
		"fetch", "--depth=1", "--filter=blob:none",
		"https://github.com/git/git", "tag", "v2.43.0")
	fetch.Dir = dir
	fetch.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := fetch.CombinedOutput(); err != nil {
		t.Skipf("github.com unreachable, skipping real-repo test: %v\n%s", err, out)
	}

	anchor := filepath.Join(dir, "ssh_allowed_signers") // trusts nobody's PGP key
	require.NoError(t, os.WriteFile(anchor, []byte(""), 0o600))
	v, err := NewGitSignedVerifier(anchor, "warn")
	require.NoError(t, err)

	trust, msg, err := v.VerifyRef(context.Background(), dir, "v2.43.0")
	require.NoError(t, err)
	assert.Equal(t, RefUntrusted, trust,
		"a real PGP-signed public tag under an SSH anchor must degrade to tofu (RefUntrusted), never falsely signed; git said: %s", msg)
}
