package acl

import "testing"

// TestCanAccess verifies the core CanAccess decision matrix across all
// visibility/access/subject combinations. Ported from ai-sandbox acl_test.go.
func TestCanAccess(t *testing.T) {
	const (
		owner    = "u-owner"
		other    = "u-other"
		orgA     = "org-a"
		orgB     = "org-b"
		otherOrg = "u-otherorg"
	)
	res := func(vis Visibility, acc Access) Resource {
		return Resource{OwnerUserID: owner, OrgID: orgA, Visibility: vis, Access: acc}
	}

	cases := []struct {
		name      string
		r         Resource
		s         Subject
		needWrite bool
		want      error
	}{
		// private
		{"private owner read", res(Private, Read), Subject{UserID: owner, OrgID: orgA}, false, nil},
		{"private owner write", res(Private, Read), Subject{UserID: owner, OrgID: orgA}, true, nil},
		{"private other-member read denied", res(Private, Write), Subject{UserID: other, OrgID: orgA}, false, ErrForbidden},
		{"private other-member write denied", res(Private, Write), Subject{UserID: other, OrgID: orgA}, true, ErrForbidden},
		{"private anon denied", res(Private, Read), Subject{}, false, ErrForbidden},

		// private with empty owner = fail-closed (empty owner never matches).
		{"private empty-owner non-admin denied", Resource{OwnerUserID: "", OrgID: orgA, Visibility: Private}, Subject{UserID: other, OrgID: orgA}, false, ErrForbidden},
		{"private empty-owner admin ok", Resource{OwnerUserID: "", OrgID: orgA, Visibility: Private}, Subject{UserID: "adm", OrgID: orgB, Admin: true}, false, nil},

		// org + read
		{"org-read member read", res(Org, Read), Subject{UserID: other, OrgID: orgA}, false, nil},
		{"org-read member write -> read-only", res(Org, Read), Subject{UserID: other, OrgID: orgA}, true, ErrReadOnly},
		{"org-read owner write ok", res(Org, Read), Subject{UserID: owner, OrgID: orgA}, true, nil},
		{"org-read other-org denied", res(Org, Read), Subject{UserID: otherOrg, OrgID: orgB}, false, ErrForbidden},

		// org + write
		{"org-write member read", res(Org, Write), Subject{UserID: other, OrgID: orgA}, false, nil},
		{"org-write member write", res(Org, Write), Subject{UserID: other, OrgID: orgA}, true, nil},
		{"org-write other-org denied", res(Org, Write), Subject{UserID: otherOrg, OrgID: orgB}, true, ErrForbidden},

		// public: anyone reads; writes require owner / admin (access field ignored for public).
		{"public anon read", res(Public, Read), Subject{}, false, nil},
		{"public anon write -> read-only", res(Public, Read), Subject{}, true, ErrReadOnly},
		{"public other-org read", res(Public, Read), Subject{UserID: otherOrg, OrgID: orgB}, false, nil},
		{"public other-org write -> read-only", res(Public, Read), Subject{UserID: otherOrg, OrgID: orgB}, true, ErrReadOnly},
		{"public owner write ok", res(Public, Read), Subject{UserID: owner, OrgID: orgA}, true, nil},
		{"public non-owner member write -> read-only", res(Public, Write), Subject{UserID: other, OrgID: orgA}, true, ErrReadOnly},

		// admin bypass: cross-org, even private resources.
		{"admin cross-org private read", res(Private, Read), Subject{UserID: "adm", OrgID: orgB, Admin: true}, false, nil},
		{"admin cross-org write", res(Private, Read), Subject{UserID: "adm", OrgID: orgB, Admin: true}, true, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanAccess(tc.r, tc.s, tc.needWrite); got != tc.want {
				t.Fatalf("CanAccess = %v, want %v", got, tc.want)
			}
			// CanAccessGranted(nil) must be bit-for-bit equivalent to CanAccess.
			if got := CanAccessGranted(tc.r, tc.s, tc.needWrite, nil); got != tc.want {
				t.Fatalf("CanAccessGranted(nil) = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCanAccessGranted covers the grant-aware paths where an external org's
// membership is extended via grantedOrgs. Private resources are not relaxed;
// public resources are unaffected; empty subject org cannot match a grant.
func TestCanAccessGranted(t *testing.T) {
	const (
		owner    = "u-owner"
		other    = "u-other"
		orgA     = "org-a"
		grantee  = "org-grantee"  // org that was explicitly granted access
		stranger = "org-stranger" // org that received no grant
	)
	res := func(vis Visibility, acc Access) Resource {
		return Resource{OwnerUserID: owner, OrgID: orgA, Visibility: vis, Access: acc}
	}
	grants := []string{grantee}

	cases := []struct {
		name      string
		r         Resource
		s         Subject
		needWrite bool
		grants    []string
		want      error
	}{
		// org+read: granted org member gets read-only.
		{"granted org read on org-read", res(Org, Read), Subject{UserID: other, OrgID: grantee}, false, grants, nil},
		{"granted org write on org-read -> read-only", res(Org, Read), Subject{UserID: other, OrgID: grantee}, true, grants, ErrReadOnly},

		// org+write: granted org member can read and write.
		{"granted org read on org-write", res(Org, Write), Subject{UserID: other, OrgID: grantee}, false, grants, nil},
		{"granted org write on org-write", res(Org, Write), Subject{UserID: other, OrgID: grantee}, true, grants, nil},

		// Non-granted (stranger) org is still denied.
		{"stranger org denied", res(Org, Write), Subject{UserID: other, OrgID: stranger}, false, grants, ErrForbidden},

		// private: grants do not relax private — non-owner is still forbidden.
		{"granted org denied on private read", res(Private, Write), Subject{UserID: other, OrgID: grantee}, false, grants, ErrForbidden},
		{"granted org denied on private write", res(Private, Write), Subject{UserID: other, OrgID: grantee}, true, grants, ErrForbidden},

		// public: grants are irrelevant.
		{"granted org public read", res(Public, Read), Subject{UserID: other, OrgID: grantee}, false, grants, nil},
		{"granted org public write -> read-only", res(Public, Read), Subject{UserID: other, OrgID: grantee}, true, grants, ErrReadOnly},

		// Owner always writes regardless of grants.
		{"owner write with grants", res(Private, Read), Subject{UserID: owner, OrgID: orgA}, true, grants, nil},

		// Multiple grants: hitting any one suffices.
		{"multi-grant hit", res(Org, Read), Subject{UserID: other, OrgID: grantee}, false, []string{stranger, grantee}, nil},

		// Empty subject OrgID must never collide with an empty grant entry.
		{"empty subject org with empty grant denied", res(Org, Read), Subject{UserID: other, OrgID: ""}, false, []string{""}, ErrForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanAccessGranted(tc.r, tc.s, tc.needWrite, tc.grants); got != tc.want {
				t.Fatalf("CanAccessGranted = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNormalizeVisibility verifies that unknown/empty values default to Private.
func TestNormalizeVisibility(t *testing.T) {
	if NormalizeVisibility("") != Private {
		t.Fatalf("empty should normalize to Private")
	}
	if NormalizeVisibility("unknown") != Private {
		t.Fatalf("unknown should normalize to Private")
	}
	if NormalizeVisibility(Org) != Org {
		t.Fatalf("org should stay org")
	}
	if NormalizeVisibility(Public) != Public {
		t.Fatalf("public should stay public")
	}
	if NormalizeVisibility(Private) != Private {
		t.Fatalf("private should stay private")
	}
}

// TestNormalizeAccess verifies that unknown/empty values default to Read.
func TestNormalizeAccess(t *testing.T) {
	if NormalizeAccess("") != Read {
		t.Fatalf("empty should normalize to Read")
	}
	if NormalizeAccess("unknown") != Read {
		t.Fatalf("unknown should normalize to Read")
	}
	if NormalizeAccess(Read) != Read {
		t.Fatalf("read should stay read")
	}
	if NormalizeAccess(Write) != Write {
		t.Fatalf("write should stay write")
	}
}
