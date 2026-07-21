package admin

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/grant"
	"github.com/ivanzzeth/specula/internal/org"
	"github.com/ivanzzeth/specula/internal/repo"
)

func TestRepoGrantsCRUD(t *testing.T) {
	h := newRepoHarness(t)
	h.srv.grants = grant.NewMemStore()

	ownerOrgID := "org_owner"
	granteeOrgID := "org_grantee"
	h.seedOrg(t, ownerOrgID, "owner")
	h.seedOrg(t, granteeOrgID, "grantee")

	ownerTok := h.seedMember(t, ownerOrgID, "owner@example.com", org.RoleOwner)
	rp := h.seedRepo(t, ownerOrgID, "owner/app", repo.VisibilityPrivate, "user:1")

	t.Run("upsert and list org grant", func(t *testing.T) {
		rr := h.do("PUT", "/api/v1/orgs/owner/repos/app/grants", ownerTok, jsonBody(UpsertGrantRequest{
			SubjectType: grant.SubjectOrg,
			SubjectID:   "grantee",
			Access:      grant.AccessRead,
		}))
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var dto GrantDTO
		decodeJSON(t, rr, &dto)
		assert.Equal(t, grant.SubjectOrg, dto.SubjectType)
		assert.Equal(t, granteeOrgID, dto.SubjectID)
		assert.Equal(t, grant.AccessRead, dto.Access)

		listRR := h.do("GET", "/api/v1/orgs/owner/repos/app/grants", ownerTok, nil)
		require.Equal(t, http.StatusOK, listRR.Code)
		var resp GrantsResponse
		decodeJSON(t, listRR, &resp)
		require.Len(t, resp.Grants, 1)
		assert.Equal(t, granteeOrgID, resp.Grants[0].SubjectID)
	})

	t.Run("grantee can read private repo via grant", func(t *testing.T) {
		granteeTok := h.seedMember(t, granteeOrgID, "grantee@example.com", org.RoleViewer)
		rr := h.do("GET", "/api/v1/orgs/owner/repos/app", granteeTok, nil)
		assert.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	})

	t.Run("delete grant", func(t *testing.T) {
		rr := h.do("DELETE", "/api/v1/orgs/owner/repos/app/grants/org/"+granteeOrgID, ownerTok, nil)
		assert.Equal(t, http.StatusNoContent, rr.Code)
		assert.Empty(t, h.srv.grants.GrantedOrgs(grantResourceTypeRepo, rp.ID))

		granteeTok := h.seedMember(t, granteeOrgID, "grantee2@example.com", org.RoleViewer)
		rr = h.do("GET", "/api/v1/orgs/owner/repos/app", granteeTok, nil)
		assert.Equal(t, http.StatusNotFound, rr.Code, "grant revoked → hide existence")
	})
}
