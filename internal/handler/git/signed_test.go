package git

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/metrics"
	"github.com/ivanzzeth/specula/internal/verify"
)

// signed_test.go — behavioural coverage for the git `signed` trust tier
// (PRD §G2: "签名 tag/commit（配 allowed-signers）；否则 tofu").
//
// These tests drive the whole chain with the REAL git client and a REAL
// SSH-signed tag: sign with a generated ed25519 key, place its public half in a
// git allowed-signers file (the local, out-of-band trust anchor — obtainable in
// CN exactly like the apt keyring), clone through the handler, and assert the
// tier the mirror EARNED.

// signKeyFixture generates an ed25519 signing key and an allowed-signers file
// authorising principal for it. Returns (pubKeyPath, allowedSignersPath).
func signKeyFixture(t *testing.T, principal string) (pub, allowedSigners string) {
	t.Helper()
	dir := t.TempDir()
	priv := filepath.Join(dir, "id_ed25519")
	pub = priv + ".pub"
	kg := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", principal, "-f", priv)
	out, err := kg.CombinedOutput()
	require.NoErrorf(t, err, "ssh-keygen: %s", out)

	pubBytes, err := os.ReadFile(pub)
	require.NoError(t, err)
	allowedSigners = filepath.Join(dir, "allowed_signers")
	require.NoError(t, os.WriteFile(allowedSigners,
		[]byte(principal+" "+strings.TrimSpace(string(pubBytes))+"\n"), 0o600))
	return pub, allowedSigners
}

// signGitCmd runs git in dir with a hermetic environment so the tester's global
// gitconfig cannot leak a conflicting gpg.format / signingkey into the fixture.
func signGitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// signedUpstream publishes a bare repo (built from work) over Smart HTTP and
// returns the serving host.
func signedUpstream(t *testing.T, project, work string) (host, mirrorDir string, ms *memMeta) {
	t.Helper()
	upRoot := t.TempDir()
	bare := filepath.Join(upRoot, project+gitSuffix)
	require.NoError(t, os.MkdirAll(filepath.Dir(bare), 0o755))
	signGitCmd(t, work, "clone", "--bare", "--quiet", "--", work, bare)
	upSrv := newUpstreamGitServer(t, upRoot)
	host = strings.TrimPrefix(upSrv.URL, "http://")
	mirrorDir = t.TempDir()
	ms = newMemMeta()
	return host, mirrorDir, ms
}

// TestSignedRef_RecordsSignedTier is the acceptance test for outcome (a): a
// mirror whose ref tip is a validly-signed tag from an allowed signer must EARN
// the `signed` tier, visible both in RepoTier (cache-browser surface) and in
// specula_verification_total (the /metrics surface).
func TestSignedRef_RecordsSignedTier(t *testing.T) {
	pub, allowed := signKeyFixture(t, "tester@specula")

	work := t.TempDir()
	signGitCmd(t, work, "init", "--quiet", "--initial-branch=master", work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "README"), []byte("v1\n"), 0o644))
	signGitCmd(t, work, "add", "README")
	signGitCmd(t, work, "commit", "--quiet", "-m", "initial")
	signGitCmd(t, work, "config", "gpg.format", "ssh")
	signGitCmd(t, work, "config", "user.signingkey", pub)
	signGitCmd(t, work, "tag", "-s", "v1.0.0", "-m", "signed release")
	signGitCmd(t, work, "tag", "v0.9.0") // unsigned lightweight tag alongside

	const project = "octocat/Hello-World"
	host, mirrorDir, ms := signedUpstream(t, project, work)

	v, err := verify.NewGitSignedVerifier(allowed, "warn")
	require.NoError(t, err)

	h := NewHandler(
		WithMirrorDir(mirrorDir),
		WithAllowedUpstreams([]string{host}),
		WithPublicOnly(false),
		WithUpstreamScheme("http"),
		WithMeta(ms),
		WithSignedRefsVerifier(v),
		WithSyncStaleAfter(time.Minute),
	)
	proxy := httptest.NewServer(h)
	t.Cleanup(proxy.Close)

	before := testutil.ToFloat64(
		metrics.VerificationTotal.WithLabelValues("git", "gitsigned", "signed", "pass"))

	dst := filepath.Join(t.TempDir(), "clone")
	gitCmd(t, t.TempDir(), "clone", "--quiet", "--",
		proxy.URL+"/"+host+"/"+project+gitSuffix, dst)

	repo := host + "/" + project
	tier := RepoTier(context.Background(), ms, mirrorDir, repo)
	assert.Equal(t, artifact.TierSigned.String(), tier,
		"a mirror with a validly-signed tag must earn the signed tier")

	after := testutil.ToFloat64(
		metrics.VerificationTotal.WithLabelValues("git", "gitsigned", "signed", "pass"))
	assert.Greater(t, after, before,
		"a verified signed ref must record a signed/pass verification series on /metrics")
}

// TestUnsignedRepo_StaysTofu proves the honest floor: a repo with NO signed ref
// records tofu, never signed, even with the verifier wired.
func TestUnsignedRepo_StaysTofu(t *testing.T) {
	_, allowed := signKeyFixture(t, "tester@specula")

	work := t.TempDir()
	signGitCmd(t, work, "init", "--quiet", "--initial-branch=master", work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "README"), []byte("v1\n"), 0o644))
	signGitCmd(t, work, "add", "README")
	signGitCmd(t, work, "commit", "--quiet", "-m", "initial")
	signGitCmd(t, work, "tag", "v1.0.0") // unsigned

	const project = "octocat/Unsigned"
	host, mirrorDir, ms := signedUpstream(t, project, work)

	v, err := verify.NewGitSignedVerifier(allowed, "warn")
	require.NoError(t, err)

	h := NewHandler(
		WithMirrorDir(mirrorDir),
		WithAllowedUpstreams([]string{host}),
		WithPublicOnly(false),
		WithUpstreamScheme("http"),
		WithMeta(ms),
		WithSignedRefsVerifier(v),
		WithSyncStaleAfter(time.Minute),
	)
	proxy := httptest.NewServer(h)
	t.Cleanup(proxy.Close)

	dst := filepath.Join(t.TempDir(), "clone")
	gitCmd(t, t.TempDir(), "clone", "--quiet", "--",
		proxy.URL+"/"+host+"/"+project+gitSuffix, dst)

	tier := RepoTier(context.Background(), ms, mirrorDir, host+"/"+project)
	assert.Equal(t, artifact.TierTofu.String(), tier,
		"an unsigned repo must stay at tofu, never claim signed")
}

// untrustedUpstream builds an upstream whose tag v1.0.0 is signed by a principal
// that is NOT in `allowed` — a forged/rotated signature. Returns wiring for a
// handler carrying a verifier with the given policy.
func untrustedUpstream(t *testing.T, project, policy string) (proxyURL, host, mirrorDir string, ms *memMeta) {
	t.Helper()
	// A signing key whose principal is deliberately absent from `allowed`.
	attackerPub, _ := signKeyFixture(t, "attacker@evil")
	// A separate allowed-signers file that trusts a DIFFERENT principal only.
	_, allowed := signKeyFixture(t, "legit@specula")

	work := t.TempDir()
	signGitCmd(t, work, "init", "--quiet", "--initial-branch=master", work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "README"), []byte("v1\n"), 0o644))
	signGitCmd(t, work, "add", "README")
	signGitCmd(t, work, "commit", "--quiet", "-m", "initial")
	signGitCmd(t, work, "config", "gpg.format", "ssh")
	signGitCmd(t, work, "config", "user.signingkey", attackerPub)
	signGitCmd(t, work, "tag", "-s", "v1.0.0", "-m", "forged")

	host, mirrorDir, ms = signedUpstream(t, project, work)
	v, err := verify.NewGitSignedVerifier(allowed, policy)
	require.NoError(t, err)
	h := NewHandler(
		WithMirrorDir(mirrorDir),
		WithAllowedUpstreams([]string{host}),
		WithPublicOnly(false),
		WithUpstreamScheme("http"),
		WithMeta(ms),
		WithSignedRefsVerifier(v),
		WithSyncStaleAfter(time.Minute),
	)
	proxy := httptest.NewServer(h)
	t.Cleanup(proxy.Close)
	return proxy.URL, host, mirrorDir, ms
}

// TestUntrusted_EnforceFailsClosed: under policy=enforce, a ref whose signature
// does not authenticate against the anchor is a compromise signal — the mirror
// is NOT served (bcc92b4 fail-closed precedent). The real client's clone fails.
func TestUntrusted_EnforceFailsClosed(t *testing.T) {
	proxyURL, host, _, _ := untrustedUpstream(t, "octocat/Forged", "enforce")

	dst := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command("git", "clone", "--quiet", "--",
		proxyURL+"/"+host+"/octocat/Forged"+gitSuffix, dst)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	require.Error(t, err,
		"clone MUST fail closed when a signed ref does not verify under enforce policy; got success:\n%s", out)
}

// TestUntrusted_WarnServesButStaysTofu: under policy=warn (the default, since
// signed refs are opt-in), an untrusted signature degrades to tofu and still
// serves — it must NOT record `signed`, and it must emit a gitsigned/fail series.
func TestUntrusted_WarnServesButStaysTofu(t *testing.T) {
	before := testutil.ToFloat64(
		metrics.VerificationTotal.WithLabelValues("git", "gitsigned", "signed", "fail"))

	proxyURL, host, mirrorDir, ms := untrustedUpstream(t, "octocat/WarnForged", "warn")

	dst := filepath.Join(t.TempDir(), "clone")
	gitCmd(t, t.TempDir(), "clone", "--quiet", "--",
		proxyURL+"/"+host+"/octocat/WarnForged"+gitSuffix, dst)

	tier := RepoTier(context.Background(), ms, mirrorDir, host+"/octocat/WarnForged")
	assert.Equal(t, artifact.TierTofu.String(), tier,
		"an untrusted signature under warn must degrade to tofu, never claim signed")

	after := testutil.ToFloat64(
		metrics.VerificationTotal.WithLabelValues("git", "gitsigned", "signed", "fail"))
	assert.Greater(t, after, before,
		"an untrusted signature must record a signed/fail verification series")
}
