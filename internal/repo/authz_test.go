package repo_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ivanzzeth/specula/internal/acl"
	"github.com/ivanzzeth/specula/internal/org"
	"github.com/ivanzzeth/specula/internal/repo"
)

// TestAuthorize is the decision matrix for the shared hosted-repo authorization
// used by BOTH the registry /token path and the Admin API. It exists because a
// production outage came from those two surfaces each answering the question
// their own way: a repo row is created on first push with
// owner_user_id = the pushing credential's subject, so whichever key pushed
// first became the repo's permanent sole owner. The org's own admin key then got
// `access: null` from /token, and rotating the push key orphaned the repo.
//
// The invariant: org membership + role is the tenancy boundary;
// repos.owner_user_id is attribution. The role axis may only ADD access for an
// authenticated caller whose role was resolved in the RESOURCE's own org.
func TestAuthorize(t *testing.T) {
	const (
		orgA    = "org-a"
		orgB    = "org-b"
		pusher  = "apikey:pusher" // pushed first → became the owner
		another = "apikey:other"  // a different credential in the same org
	)

	private := acl.Resource{OwnerUserID: pusher, OrgID: orgA, Visibility: acl.Private}
	public := acl.Resource{OwnerUserID: pusher, OrgID: orgA, Visibility: acl.Public}

	// role builds the grant a caller in orgA presents for a given held role.
	role := func(have, need string) repo.OrgRoleGrant {
		return repo.OrgRoleGrant{OrgID: orgA, Have: have, Need: need}
	}

	cases := []struct {
		name      string
		res       acl.Resource
		s         acl.Subject
		needWrite bool
		granted   []string
		role      repo.OrgRoleGrant
		want      error
	}{
		// ── the live case: tenancy beats first-pusher identity ──
		{
			"org-admin key reads private repo owned by another key",
			private, acl.Subject{UserID: another, OrgID: orgA}, false, nil,
			role(org.RoleAdmin, org.RoleViewer), nil,
		},
		{
			"org-admin key writes private repo owned by another key",
			private, acl.Subject{UserID: another, OrgID: orgA}, true, nil,
			role(org.RoleAdmin, org.RoleEditor), nil,
		},
		{
			"org editor writes private repo owned by another key",
			private, acl.Subject{UserID: "user:9", OrgID: orgA}, true, nil,
			role(org.RoleEditor, org.RoleEditor), nil,
		},
		{
			"org viewer reads private repo owned by another key",
			private, acl.Subject{UserID: "user:9", OrgID: orgA}, false, nil,
			role(org.RoleViewer, org.RoleViewer), nil,
		},
		{
			"org-admin key writes same-org PUBLIC repo owned by another key",
			public, acl.Subject{UserID: another, OrgID: orgA}, true, nil,
			role(org.RoleAdmin, org.RoleEditor), nil,
		},

		// ── the ladder still bites ──
		{
			"org viewer cannot write (needs editor)",
			private, acl.Subject{UserID: "user:9", OrgID: orgA}, true, nil,
			role(org.RoleViewer, org.RoleEditor), acl.ErrForbidden,
		},
		{
			"org editor cannot administer (needs admin)",
			private, acl.Subject{UserID: "user:9", OrgID: orgA}, true, nil,
			role(org.RoleEditor, org.RoleAdmin), acl.ErrForbidden,
		},

		// ── the role axis widens NOTHING for the wrong caller ──
		{
			"anonymous denied on private even if a role is somehow supplied",
			private, acl.Subject{}, false, nil,
			role(org.RoleOwner, org.RoleViewer), acl.ErrForbidden,
		},
		{
			"cross-org caller denied: role resolved in another tenant",
			private, acl.Subject{UserID: "apikey:foreign", OrgID: orgB}, false, nil,
			repo.OrgRoleGrant{OrgID: orgB, Have: org.RoleOwner, Need: org.RoleViewer},
			acl.ErrForbidden,
		},
		{
			"no role in the resource's org → denied",
			private, acl.Subject{UserID: "user:99"}, false, nil,
			role("", org.RoleViewer), acl.ErrForbidden,
		},
		{
			"unrecognised role string is NOT silently downgraded to viewer",
			private, acl.Subject{UserID: "user:99", OrgID: orgA}, false, nil,
			role("vieweer", org.RoleViewer), acl.ErrForbidden,
		},
		{
			"empty Need never grants",
			private, acl.Subject{UserID: "user:9", OrgID: orgA}, false, nil,
			role(org.RoleOwner, ""), acl.ErrForbidden,
		},
		{
			"empty grant OrgID never matches (guards a caller that forgot to set it)",
			private, acl.Subject{UserID: "user:9", OrgID: orgA}, false, nil,
			repo.OrgRoleGrant{Have: org.RoleOwner, Need: org.RoleViewer},
			acl.ErrForbidden,
		},

		// ── acl-only paths stay bit-for-bit as before ──
		{
			"owner still reads its own private repo with no role at all",
			private, acl.Subject{UserID: pusher, OrgID: orgA}, false, nil,
			repo.OrgRoleGrant{}, nil,
		},
		{
			"owner still writes its own private repo with no role at all",
			private, acl.Subject{UserID: pusher, OrgID: orgA}, true, nil,
			repo.OrgRoleGrant{}, nil,
		},
		{
			"anonymous still reads a public repo",
			public, acl.Subject{}, false, nil, repo.OrgRoleGrant{}, nil,
		},
		{
			"anonymous write on public stays read-only, not forbidden",
			public, acl.Subject{}, true, nil, repo.OrgRoleGrant{}, acl.ErrReadOnly,
		},
		{
			"cross-org read grant still allows a roleless outsider",
			acl.Resource{OwnerUserID: pusher, OrgID: orgA, Visibility: acl.Org, Access: acl.Read},
			acl.Subject{UserID: "user:77", OrgID: orgB}, false, []string{orgB},
			repo.OrgRoleGrant{}, nil,
		},
		{
			"system admin still bypasses cross-org",
			private, acl.Subject{UserID: "user:1", OrgID: orgB, Admin: true}, true, nil,
			repo.OrgRoleGrant{}, nil,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := repo.Authorize(tc.res, tc.s, tc.needWrite, tc.granted, tc.role)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestAuthorize_NoRole_EquivalentToACL pins that supplying an empty
// OrgRoleGrant leaves the decision byte-for-byte identical to calling
// acl.CanAccessGranted directly. The role axis must be purely additive: any
// divergence here would mean the shared wrapper changed a decision on a path
// that never asked for the org ladder at all.
func TestAuthorize_NoRole_EquivalentToACL(t *testing.T) {
	resources := []acl.Resource{
		{OwnerUserID: "apikey:p", OrgID: "org-a", Visibility: acl.Private},
		{OwnerUserID: "apikey:p", OrgID: "org-a", Visibility: acl.Public},
		{OwnerUserID: "apikey:p", OrgID: "org-a", Visibility: acl.Org, Access: acl.Read},
		{OwnerUserID: "apikey:p", OrgID: "org-a", Visibility: acl.Org, Access: acl.Write},
		{OwnerUserID: "", OrgID: "org-a", Visibility: acl.Private}, // empty owner: fail-closed
	}
	subjects := []acl.Subject{
		{},
		{UserID: "apikey:p", OrgID: "org-a"},
		{UserID: "user:9", OrgID: "org-a"},
		{UserID: "user:77", OrgID: "org-b"},
		{UserID: "user:1", OrgID: "org-b", Admin: true},
	}
	for _, res := range resources {
		for _, s := range subjects {
			for _, needWrite := range []bool{false, true} {
				want := acl.CanAccessGranted(res, s, needWrite, nil)
				got := repo.Authorize(res, s, needWrite, nil, repo.OrgRoleGrant{})
				assert.Equal(t, want, got,
					"res=%+v subject=%+v needWrite=%v", res, s, needWrite)
			}
		}
	}
}
