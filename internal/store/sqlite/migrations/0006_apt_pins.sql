-- +goose Up
-- apt GPG trust-chain pins (internal/verify.AptPinStore).
--
-- These are NOT a cache. They are required chain state: a pool .deb whose hash
-- no verified InRelease pins can only fail closed. Holding them in one process's
-- heap contradicts PRD §G3 ("Specula 实例无状态" — shared state lives only in the
-- blob store + metadata DB, no gossip): behind a load balancer, the replica that
-- serves `apt-get update` is routinely not the replica that serves the `.deb`,
-- and any restart would break a client whose apt list is still valid.
--
-- `scope` is the trust anchor's identity — a digest over the keyring's primary
-- key fingerprints. Pins mean "the holder of these keys signed an InRelease
-- committing path P to hash H", so the anchor is part of the key: pins written
-- under one keyring must never be read by a verifier anchored on another. It is
-- deliberately NOT the upstream host — mirrors are interchangeable views of one
-- repo, and keying by the serving mirror would break the chain on mirror
-- failover.

-- Index pins: the SHA256s an InRelease commits to, per suite.
-- Mutable tier (ARCHITECTURE §3): wholly REPLACED each time a newer InRelease is
-- verified for the suite, so a superseded signed index cannot be served forever.
CREATE TABLE IF NOT EXISTS apt_index_pins (
    scope    TEXT NOT NULL,
    repo     TEXT NOT NULL,
    suite    TEXT NOT NULL,
    rel_path TEXT NOT NULL,
    sha256   TEXT NOT NULL,
    PRIMARY KEY (scope, repo, suite, rel_path)
);

-- Pool pins: pool path → SHA256, learned from a Packages index already verified
-- against a signed InRelease.
--
-- Immutable tier (ARCHITECTURE §3/§6): a pool path embeds version + architecture
-- and denotes exactly one byte sequence forever, so pins are NOT expired when a
-- newer InRelease supersedes the index that produced them — that is what lets a
-- client with a still-valid apt list download after a restart.
--
-- `suite` is deliberately absent: the Debian pool is shared across suites by
-- design, and a .deb request carries no suite, so a suite-keyed lookup would be
-- impossible to perform. `repo` IS present so that two repositories under one
-- anchor pinning the same path to different hashes is detectable (and refused)
-- rather than silently resolved to whichever wrote last.
CREATE TABLE IF NOT EXISTS apt_pool_pins (
    scope     TEXT NOT NULL,
    repo      TEXT NOT NULL,
    pool_path TEXT NOT NULL,
    sha256    TEXT NOT NULL,
    PRIMARY KEY (scope, repo, pool_path)
);

-- The read path is always (scope, pool_path): a pool ref carries no repo.
CREATE INDEX IF NOT EXISTS idx_apt_pool_pins_lookup
    ON apt_pool_pins (scope, pool_path);

-- +goose Down
DROP INDEX IF EXISTS idx_apt_pool_pins_lookup;
DROP TABLE IF EXISTS apt_pool_pins;
DROP TABLE IF EXISTS apt_index_pins;
