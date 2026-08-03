-- +goose Up
-- Unify org.DefaultOrgID with DefaultOrgSlug (both "default"). A prior
-- revision used the distinct id "org_default" while the slug was already
-- "default" (see internal/org/org.go), which let the SAME org be pushed to
-- under two different namespaces — once by id ("org_default/<repo>"), once
-- by slug ("default/<repo>") — via admin/repos.go:repoNameCandidates trying
-- both. Live instances therefore already have BOTH "default/<repo>" and
-- "org_default/<repo>" repo rows under org_id = 'org_default'. This
-- migration rewrites every org_id-bearing table to the unified id, then
-- collapses the duplicate repo rows.
--
-- Idempotent: once no row has org_id/id = 'org_default' (or a repos.name
-- prefixed "org_default/"), every statement below matches zero rows.

-- 1. Rewrite the FK-ish org_id column everywhere BEFORE touching repos.name,
--    so no UNIQUE(org_id, name) collision is possible from this step alone
--    (name is untouched here).
UPDATE org_members     SET org_id = 'default' WHERE org_id = 'org_default';
UPDATE org_invitations SET org_id = 'default' WHERE org_id = 'org_default';
UPDATE api_keys        SET org_id = 'default' WHERE org_id = 'org_default';
UPDATE repos           SET org_id = 'default' WHERE org_id = 'org_default';

-- 2. Rewrite the org row itself. orgs.slug is already 'default'; only the id
--    changes.
UPDATE orgs SET id = 'default' WHERE id = 'org_default';

-- 3. Collapse duplicate "org_default/<repo>" rows now that org_id has been
--    unified. Drop repo_tags / resource_grants belonging to the row that is
--    about to be deleted first, so no orphaned rows are left pointing at a
--    repos.id that no longer exists.
DELETE FROM repo_tags
WHERE repo_id IN (
    SELECT id FROM repos
    WHERE name LIKE 'org_default/%'
      AND EXISTS (
          SELECT 1 FROM repos AS r2
          WHERE r2.org_id = repos.org_id
            AND r2.name = 'default/' || substr(repos.name, length('org_default/') + 1)
      )
);

DELETE FROM resource_grants
WHERE resource_type = 'repo'
  AND resource_id IN (
      SELECT id FROM repos
      WHERE name LIKE 'org_default/%'
        AND EXISTS (
            SELECT 1 FROM repos AS r2
            WHERE r2.org_id = repos.org_id
              AND r2.name = 'default/' || substr(repos.name, length('org_default/') + 1)
        )
  );

-- Where the canonical "default/<repo>" already exists (same org), the
-- legacy "org_default/<repo>" duplicate is deleted outright — its tags, if
-- any differed from the canonical row's, are not merged; migration favours
-- the pre-existing canonical row for simplicity, matching the (org_id,
-- name) UNIQUE index this repo_tags/resource_grants cleanup above already
-- respects.
DELETE FROM repos
WHERE name LIKE 'org_default/%'
  AND EXISTS (
      SELECT 1 FROM repos AS r2
      WHERE r2.org_id = repos.org_id
        AND r2.name = 'default/' || substr(repos.name, length('org_default/') + 1)
  );

-- No canonical row existed for this one — just rename it in place.
UPDATE repos
SET name = 'default/' || substr(name, length('org_default/') + 1)
WHERE name LIKE 'org_default/%';

-- +goose Down
-- Not reversible: the legacy "org_default" identity is intentionally
-- retired. Rewriting 'default' -> 'org_default' on down-migrate could
-- collide with an org that legitimately took the id "default" afterwards,
-- and the collapsed repo duplicates cannot be un-merged.
SELECT 1;
