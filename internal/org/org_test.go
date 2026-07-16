package org_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/org"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
)

// ── store factory helpers ──────────────────────────────────────────────────

// newMemStore returns a fresh in-memory org.Store.
func newMemStore() org.Store {
	return org.NewMemStore()
}

// newSQLiteStore opens a temp SQLite DB (migrations applied), returns an
// org.SQLStore backed by the same handle, and registers cleanup.
func newSQLiteStore(t *testing.T) org.Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "org_test.db")
	s, err := sqlite.NewSQLiteStore(dsn)
	require.NoError(t, err, "NewSQLiteStore must succeed")
	t.Cleanup(func() { _ = s.Close() })
	return org.NewSQLStore(s.DB())
}

// storeFactories returns the two implementations to run each test against.
type storeFactory func(t *testing.T) org.Store

func storeFactories(t *testing.T) []struct {
	name  string
	store org.Store
} {
	t.Helper()
	return []struct {
		name  string
		store org.Store
	}{
		{"mem", newMemStore()},
		{"sqlite", newSQLiteStore(t)},
	}
}

// ── role ladder ───────────────────────────────────────────────────────────

func TestRoleLadder(t *testing.T) {
	t.Run("NormalizeRole_unknown_maps_to_viewer", func(t *testing.T) {
		assert.Equal(t, org.RoleViewer, org.NormalizeRole(""))
		assert.Equal(t, org.RoleViewer, org.NormalizeRole("unknown"))
		assert.Equal(t, org.RoleViewer, org.NormalizeRole("superadmin"))
	})

	t.Run("NormalizeRole_preserves_known_roles", func(t *testing.T) {
		for _, role := range []string{org.RoleViewer, org.RoleEditor, org.RoleAdmin, org.RoleOwner} {
			assert.Equal(t, role, org.NormalizeRole(role))
		}
	})

	t.Run("AtLeast_ladder_ordering", func(t *testing.T) {
		// viewer < editor < admin < owner
		assert.True(t, org.AtLeast(org.RoleViewer, org.RoleViewer), "viewer >= viewer")
		assert.False(t, org.AtLeast(org.RoleViewer, org.RoleEditor), "viewer < editor")
		assert.True(t, org.AtLeast(org.RoleEditor, org.RoleViewer), "editor >= viewer")
		assert.True(t, org.AtLeast(org.RoleAdmin, org.RoleEditor), "admin >= editor")
		assert.True(t, org.AtLeast(org.RoleOwner, org.RoleAdmin), "owner >= admin")
		assert.False(t, org.AtLeast(org.RoleAdmin, org.RoleOwner), "admin < owner")
	})

	t.Run("UserSubjectID", func(t *testing.T) {
		assert.Equal(t, "user:1", org.UserSubjectID(1))
		assert.Equal(t, "user:42", org.UserSubjectID(42))
		assert.Equal(t, "user:0", org.UserSubjectID(0))
	})
}

// ── org CRUD ──────────────────────────────────────────────────────────────

func TestOrgCRUD(t *testing.T) {
	for _, tc := range storeFactories(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			// ── CreateOrg / GetOrg / GetOrgBySlug ──
			o := &org.Org{
				ID:   "org_test1",
				Name: "Test Org",
				Slug: "test-org",
			}
			require.NoError(t, tc.store.CreateOrg(ctx, o))
			assert.Equal(t, org.StatusActive, o.Status, "Status must be defaulted to active")

			got, err := tc.store.GetOrg(ctx, "org_test1")
			require.NoError(t, err)
			assert.Equal(t, "Test Org", got.Name)
			assert.Equal(t, "test-org", got.Slug)
			assert.Equal(t, org.StatusActive, got.Status)

			gotBySlug, err := tc.store.GetOrgBySlug(ctx, "test-org")
			require.NoError(t, err)
			assert.Equal(t, "org_test1", gotBySlug.ID)

			// ── ErrNotFound ──
			_, err = tc.store.GetOrg(ctx, "no-such-org")
			require.ErrorIs(t, err, org.ErrNotFound)

			_, err = tc.store.GetOrgBySlug(ctx, "no-such-slug")
			require.ErrorIs(t, err, org.ErrNotFound)

			// ── UpdateOrg (name only) ──
			got.Name = "Updated Org"
			require.NoError(t, tc.store.UpdateOrg(ctx, got))
			reloaded, err := tc.store.GetOrg(ctx, "org_test1")
			require.NoError(t, err)
			assert.Equal(t, "Updated Org", reloaded.Name)

			// ── SetOrgStatus ──
			require.NoError(t, tc.store.SetOrgStatus(ctx, "org_test1", org.StatusFrozen))
			frozen, err := tc.store.GetOrg(ctx, "org_test1")
			require.NoError(t, err)
			assert.True(t, frozen.Frozen(), "Org must report Frozen()==true after SetOrgStatus")

			// ── ListOrgs ──
			o2 := &org.Org{ID: "org_test2", Name: "Second Org", Slug: "second-org"}
			require.NoError(t, tc.store.CreateOrg(ctx, o2))
			orgs, err := tc.store.ListOrgs(ctx)
			require.NoError(t, err)
			assert.GreaterOrEqual(t, len(orgs), 2)

			// ── CountOrgs ──
			n, err := tc.store.CountOrgs(ctx)
			require.NoError(t, err)
			assert.Equal(t, len(orgs), n)

			// ── DeleteOrg ──
			require.NoError(t, tc.store.DeleteOrg(ctx, "org_test2"))
			_, err = tc.store.GetOrg(ctx, "org_test2")
			require.ErrorIs(t, err, org.ErrNotFound)
		})
	}
}

// ── first-user-owner bootstrap ────────────────────────────────────────────

// TestFirstUserOwnerBootstrap verifies the bootstrap scenario:
// when the first user registers, a default org is created and the user is
// added as its owner. This tests the org.Store methods that auth.Service will
// call during bootstrap (CountOrgs()==0 → CreateOrg + AddOrgMember).
func TestFirstUserOwnerBootstrap(t *testing.T) {
	for _, tc := range storeFactories(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			// No orgs yet.
			n, err := tc.store.CountOrgs(ctx)
			require.NoError(t, err)
			assert.Zero(t, n, "store must start empty")

			// Simulate what auth.Service.Register does after CountUsers()==0:
			// create the default org and add the new user as owner.
			firstUserEmail := "admin@example.com"
			firstUserID := int64(1)

			defaultOrg := &org.Org{
				ID:        org.DefaultOrgID,
				Name:      org.DefaultOrgName,
				Slug:      org.DefaultOrgSlug,
				CreatedBy: org.UserSubjectID(firstUserID),
			}
			require.NoError(t, tc.store.CreateOrg(ctx, defaultOrg))
			assert.Equal(t, org.StatusActive, defaultOrg.Status)

			ownerMember := &org.Member{
				OrgID: org.DefaultOrgID,
				Email: firstUserEmail,
				Role:  org.RoleOwner,
			}
			require.NoError(t, tc.store.AddOrgMember(ctx, ownerMember))

			// Verify org was created.
			gotOrg, err := tc.store.GetOrg(ctx, org.DefaultOrgID)
			require.NoError(t, err)
			assert.Equal(t, org.DefaultOrgName, gotOrg.Name)
			assert.Equal(t, org.DefaultOrgSlug, gotOrg.Slug)

			// Verify membership.
			mem, err := tc.store.GetOrgMember(ctx, org.DefaultOrgID, firstUserEmail)
			require.NoError(t, err)
			assert.Equal(t, org.RoleOwner, mem.Role)
			assert.Equal(t, org.DefaultOrgID, mem.OrgID)

			// CountOrgOwners must be 1.
			owners, err := tc.store.CountOrgOwners(ctx, org.DefaultOrgID)
			require.NoError(t, err)
			assert.Equal(t, 1, owners)

			// ListOrgsForEmail must include the default org.
			memberOrgs, err := tc.store.ListOrgsForEmail(ctx, firstUserEmail)
			require.NoError(t, err)
			require.Len(t, memberOrgs, 1)
			assert.Equal(t, org.DefaultOrgID, memberOrgs[0].ID)
			assert.Equal(t, org.RoleOwner, memberOrgs[0].Role)

			// CountOrgs must now be 1.
			n2, err := tc.store.CountOrgs(ctx)
			require.NoError(t, err)
			assert.Equal(t, 1, n2)
		})
	}
}

// ── membership CRUD ───────────────────────────────────────────────────────

func TestMembershipCRUD(t *testing.T) {
	for _, tc := range storeFactories(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			// Seed an org.
			o := &org.Org{ID: "org_m1", Name: "Member Test Org", Slug: "member-test"}
			require.NoError(t, tc.store.CreateOrg(ctx, o))

			// ── AddOrgMember (insert) ──
			m1 := &org.Member{
				OrgID: "org_m1",
				Email: "Alice@Example.COM", // mixed case to test normalization
				Role:  org.RoleEditor,
			}
			require.NoError(t, tc.store.AddOrgMember(ctx, m1))
			assert.Equal(t, "alice@example.com", m1.Email, "email must be normalised")
			assert.NotEmpty(t, m1.ID, "ID must be generated")

			// ── GetOrgMember ──
			got, err := tc.store.GetOrgMember(ctx, "org_m1", "alice@example.com")
			require.NoError(t, err)
			assert.Equal(t, org.RoleEditor, got.Role)
			assert.Equal(t, "alice@example.com", got.Email)

			// ── GetOrgMember: ErrNotFound ──
			_, err = tc.store.GetOrgMember(ctx, "org_m1", "nobody@example.com")
			require.ErrorIs(t, err, org.ErrNotFound)

			// ── AddOrgMember upsert (update role) ──
			upsert := &org.Member{
				OrgID: "org_m1",
				Email: "alice@example.com",
				Role:  org.RoleAdmin,
			}
			require.NoError(t, tc.store.AddOrgMember(ctx, upsert))

			reloaded, err := tc.store.GetOrgMember(ctx, "org_m1", "alice@example.com")
			require.NoError(t, err)
			assert.Equal(t, org.RoleAdmin, reloaded.Role, "upsert must update role")
			// Original ID must be preserved on upsert.
			assert.Equal(t, got.ID, reloaded.ID, "original member ID must be preserved on upsert")

			// ── AddOrgMember: default role (empty → editor) ──
			m2 := &org.Member{OrgID: "org_m1", Email: "bob@example.com"}
			require.NoError(t, tc.store.AddOrgMember(ctx, m2))
			assert.Equal(t, org.RoleEditor, m2.Role, "empty role must default to editor")

			// ── ListOrgMembers ──
			members, err := tc.store.ListOrgMembers(ctx, "org_m1")
			require.NoError(t, err)
			assert.Len(t, members, 2)

			// ── CountOrgAdmins ──
			admins, err := tc.store.CountOrgAdmins(ctx, "org_m1")
			require.NoError(t, err)
			assert.Equal(t, 1, admins, "alice is admin")

			// ── AddOrgMember with RoleOwner ──
			m3 := &org.Member{OrgID: "org_m1", Email: "carol@example.com", Role: org.RoleOwner}
			require.NoError(t, tc.store.AddOrgMember(ctx, m3))
			owners, err := tc.store.CountOrgOwners(ctx, "org_m1")
			require.NoError(t, err)
			assert.Equal(t, 1, owners)

			// ── RemoveOrgMember ──
			require.NoError(t, tc.store.RemoveOrgMember(ctx, "org_m1", "bob@example.com"))
			_, err = tc.store.GetOrgMember(ctx, "org_m1", "bob@example.com")
			require.ErrorIs(t, err, org.ErrNotFound)

			// ── RemoveOrgMember is idempotent ──
			require.NoError(t, tc.store.RemoveOrgMember(ctx, "org_m1", "bob@example.com"))

			// ── DeleteOrg cascades members ──
			require.NoError(t, tc.store.DeleteOrg(ctx, "org_m1"))
			_, err = tc.store.GetOrgMember(ctx, "org_m1", "alice@example.com")
			require.ErrorIs(t, err, org.ErrNotFound)
		})
	}
}

// ── ListOrgsForEmail ──────────────────────────────────────────────────────

func TestListOrgsForEmail(t *testing.T) {
	for _, tc := range storeFactories(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			// Create three orgs.
			for _, o := range []*org.Org{
				{ID: "org_a", Name: "Org A", Slug: "org-a"},
				{ID: "org_b", Name: "Org B", Slug: "org-b"},
				{ID: "org_c", Name: "Org C", Slug: "org-c"},
			} {
				// stagger created_at so ordering is deterministic in SQLite
				// (RFC3339Nano ordering = insertion ordering since we create them serially).
				require.NoError(t, tc.store.CreateOrg(ctx, o))
				time.Sleep(2 * time.Millisecond) // ensure distinct timestamps
			}

			// alice is a member of A (viewer) and B (editor).
			require.NoError(t, tc.store.AddOrgMember(ctx, &org.Member{
				OrgID: "org_a", Email: "alice@example.com", Role: org.RoleViewer,
			}))
			require.NoError(t, tc.store.AddOrgMember(ctx, &org.Member{
				OrgID: "org_b", Email: "alice@example.com", Role: org.RoleEditor,
			}))

			// bob is only in C (owner).
			require.NoError(t, tc.store.AddOrgMember(ctx, &org.Member{
				OrgID: "org_c", Email: "bob@example.com", Role: org.RoleOwner,
			}))

			// alice gets exactly org_b, org_a (newest-first order).
			aliceOrgs, err := tc.store.ListOrgsForEmail(ctx, "alice@example.com")
			require.NoError(t, err)
			require.Len(t, aliceOrgs, 2)
			// Org.Role is populated from the membership.
			roleByID := map[string]string{}
			for _, o := range aliceOrgs {
				roleByID[o.ID] = o.Role
			}
			assert.Equal(t, org.RoleViewer, roleByID["org_a"])
			assert.Equal(t, org.RoleEditor, roleByID["org_b"])

			// Mixed-case email must resolve correctly.
			aliceOrgs2, err := tc.store.ListOrgsForEmail(ctx, "ALICE@EXAMPLE.COM")
			require.NoError(t, err)
			assert.Len(t, aliceOrgs2, 2)

			// bob sees only org_c.
			bobOrgs, err := tc.store.ListOrgsForEmail(ctx, "bob@example.com")
			require.NoError(t, err)
			require.Len(t, bobOrgs, 1)
			assert.Equal(t, "org_c", bobOrgs[0].ID)
			assert.Equal(t, org.RoleOwner, bobOrgs[0].Role)

			// Unknown email returns empty slice, not error.
			nobody, err := tc.store.ListOrgsForEmail(ctx, "nobody@example.com")
			require.NoError(t, err)
			assert.Empty(t, nobody)
		})
	}
}

// ── invitation CRUD ───────────────────────────────────────────────────────

func TestInvitationCRUD(t *testing.T) {
	for _, tc := range storeFactories(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			o := &org.Org{ID: "org_inv", Name: "Invite Org", Slug: "invite-org"}
			require.NoError(t, tc.store.CreateOrg(ctx, o))

			// ── CreateInvitation defaults ──
			inv := &org.Invitation{
				OrgID:     "org_inv",
				Email:     "Dave@Example.COM",
				InvitedBy: "user:1",
				Token:     "tok-abc",
			}
			require.NoError(t, tc.store.CreateInvitation(ctx, inv))
			assert.Equal(t, "dave@example.com", inv.Email, "email must be normalised")
			assert.Equal(t, org.RoleViewer, inv.Role, "role must default to viewer")
			assert.Equal(t, org.InviteStatusPending, inv.Status, "status must default to pending")
			assert.NotEmpty(t, inv.ID, "ID must be generated")

			// ── GetInvitationByToken ──
			got, err := tc.store.GetInvitationByToken(ctx, "tok-abc")
			require.NoError(t, err)
			assert.Equal(t, "dave@example.com", got.Email)
			assert.Equal(t, org.InviteStatusPending, got.Status)
			assert.Equal(t, "user:1", got.InvitedBy)
			assert.False(t, got.Expired(), "invitation without ExpiresAt must not be expired")

			// ── GetInvitationByToken: ErrNotFound ──
			_, err = tc.store.GetInvitationByToken(ctx, "no-such-token")
			require.ErrorIs(t, err, org.ErrNotFound)

			// ── ExpiresAt / Expired() ──
			expiredInv := &org.Invitation{
				OrgID:     "org_inv",
				Email:     "expired@example.com",
				Token:     "tok-expired",
				ExpiresAt: time.Now().Add(-time.Hour),
			}
			require.NoError(t, tc.store.CreateInvitation(ctx, expiredInv))
			gotExpired, err := tc.store.GetInvitationByToken(ctx, "tok-expired")
			require.NoError(t, err)
			assert.True(t, gotExpired.Expired(), "invitation with past ExpiresAt must be Expired()")

			// ── SetInvitationStatus ──
			require.NoError(t, tc.store.SetInvitationStatus(ctx, inv.ID, org.InviteStatusAccepted))
			updated, err := tc.store.GetInvitationByToken(ctx, "tok-abc")
			require.NoError(t, err)
			assert.Equal(t, org.InviteStatusAccepted, updated.Status)

			// ── ListInvitations ──
			invs, err := tc.store.ListInvitations(ctx, "org_inv")
			require.NoError(t, err)
			assert.Len(t, invs, 2)

			// ── DeleteOrg cascades invitations ──
			require.NoError(t, tc.store.DeleteOrg(ctx, "org_inv"))
			_, err = tc.store.GetInvitationByToken(ctx, "tok-abc")
			require.ErrorIs(t, err, org.ErrNotFound)
		})
	}
}

// ── Org.Frozen / Invitation.Expired ──────────────────────────────────────

func TestOrgFrozenInvitationExpired(t *testing.T) {
	t.Run("nil_org_not_frozen", func(t *testing.T) {
		var o *org.Org
		assert.False(t, o.Frozen())
	})

	t.Run("active_org_not_frozen", func(t *testing.T) {
		o := &org.Org{Status: org.StatusActive}
		assert.False(t, o.Frozen())
	})

	t.Run("frozen_org_is_frozen", func(t *testing.T) {
		o := &org.Org{Status: org.StatusFrozen}
		assert.True(t, o.Frozen())
	})

	t.Run("nil_invitation_not_expired", func(t *testing.T) {
		var i *org.Invitation
		assert.False(t, i.Expired())
	})

	t.Run("zero_expiresAt_not_expired", func(t *testing.T) {
		i := &org.Invitation{}
		assert.False(t, i.Expired())
	})

	t.Run("future_expiresAt_not_expired", func(t *testing.T) {
		i := &org.Invitation{ExpiresAt: time.Now().Add(time.Hour)}
		assert.False(t, i.Expired())
	})

	t.Run("past_expiresAt_is_expired", func(t *testing.T) {
		i := &org.Invitation{ExpiresAt: time.Now().Add(-time.Second)}
		assert.True(t, i.Expired())
	})
}
