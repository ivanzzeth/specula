package org_test

// Ported (translated) from ai-sandbox internal/controlplane/org/p3_sql_test.go
// TestCountOrgsByCreator + mem_test.go's equivalent assertion, run here against
// BOTH implementations via the existing storeFactories harness. This is the core
// query behind the settings.KeyOrgMaxPerUser quota: if it miscounts, the quota
// either fails open (unlimited orgs) or locks users out of their escape hatch.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/org"
)

func TestCountOrgsByCreator(t *testing.T) {
	for _, tc := range storeFactories(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := tc.store

			// User A creates 2 orgs; user B creates 1.
			require.NoError(t, s.CreateOrg(ctx, &org.Org{ID: "org_a1", Name: "A1", Slug: "a1", CreatedBy: "user:1"}))
			require.NoError(t, s.CreateOrg(ctx, &org.Org{ID: "org_a2", Name: "A2", Slug: "a2", CreatedBy: "user:1"}))
			require.NoError(t, s.CreateOrg(ctx, &org.Org{ID: "org_b1", Name: "B1", Slug: "b1", CreatedBy: "user:2"}))

			n, err := s.CountOrgsByCreator(ctx, "user:1")
			require.NoError(t, err)
			assert.Equal(t, 2, n, "user:1 self-created 2 orgs")

			n, err = s.CountOrgsByCreator(ctx, "user:2")
			require.NoError(t, err)
			assert.Equal(t, 1, n, "user:2 self-created 1 org")

			// A user who created nothing → 0, not an error.
			n, err = s.CountOrgsByCreator(ctx, "user:nonexistent")
			require.NoError(t, err)
			assert.Equal(t, 0, n, "unknown creator counts zero without erroring")
		})
	}
}

// TestCountOrgsByCreatorIgnoresMembership guards the quota's intent, which the
// reference documents but never asserts: the limit counts orgs you CREATED, not
// orgs you belong to. Being invited into somebody else's org must not consume
// your own allowance.
func TestCountOrgsByCreatorIgnoresMembership(t *testing.T) {
	for _, tc := range storeFactories(t) {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := tc.store

			require.NoError(t, s.CreateOrg(ctx, &org.Org{ID: "org_owned", Name: "Owned", Slug: "owned", CreatedBy: "user:1"}))
			require.NoError(t, s.CreateOrg(ctx, &org.Org{ID: "org_theirs", Name: "Theirs", Slug: "theirs", CreatedBy: "user:2"}))
			// user:1 is invited into user:2's org.
			require.NoError(t, s.AddOrgMember(ctx, &org.Member{
				OrgID: "org_theirs", Email: "one@example.com", Role: org.RoleEditor,
			}))

			n, err := s.CountOrgsByCreator(ctx, "user:1")
			require.NoError(t, err)
			assert.Equal(t, 1, n, "membership in another user's org must not count against the create quota")
		})
	}
}
