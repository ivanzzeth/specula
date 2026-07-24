-- +goose Up
-- Monotonic high-water for apt signed InRelease Date (PRD §G2 anti-rollback / H2).
-- Rejects an older-but-still-validly-signed InRelease that would roll pins back.
CREATE TABLE IF NOT EXISTS apt_index_highwater (
    scope     TEXT    NOT NULL,
    repo      TEXT    NOT NULL,
    suite     TEXT    NOT NULL,
    date_unix INTEGER NOT NULL,
    PRIMARY KEY (scope, repo, suite)
);

-- +goose Down
DROP TABLE IF EXISTS apt_index_highwater;
