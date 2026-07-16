package org

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemStore is the in-memory Store for dev/tests. Its semantics are
// byte-for-byte equivalent to SQLStore: email normalised to lower-case,
// defaults filled, AddOrgMember is an upsert, ErrNotFound on miss, lists
// ordered newest→oldest, returned values are copies.
//
// Returning copies is a hard constraint: Org.Role / Org.SystemAccess are
// per-request transient fields written by the auth layer; returning the
// internal pointer would let one handler corrupt another request's view.
type MemStore struct {
	mu          sync.RWMutex
	orgs        map[string]*Org        // id → *Org
	members     map[string]*Member     // memberKey(orgID,email) → *Member
	invitations map[string]*Invitation // id → *Invitation
}

// NewMemStore constructs an empty in-memory Store.
func NewMemStore() *MemStore {
	return &MemStore{
		orgs:        map[string]*Org{},
		members:     map[string]*Member{},
		invitations: map[string]*Invitation{},
	}
}

// normEmail normalises an email address to lower-case + trimmed whitespace,
// matching the SQL store's strings.ToLower(strings.TrimSpace(...)) calls.
func normEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// memberKey produces a composite map key for a (orgID, email) pair.
// The NUL separator cannot appear in a valid email or org ID, so there is
// no ambiguity.
func memberKey(orgID, email string) string { return orgID + "\x00" + normEmail(email) }

func cloneOrg(o *Org) *Org                      { c := *o; return &c }
func cloneMember(m *Member) *Member             { c := *m; return &c }
func cloneInvitation(i *Invitation) *Invitation { c := *i; return &c }

// ── orgs ──────────────────────────────────────────────────────────────────

// CreateOrg inserts an organization. Status defaults to "active"; CreatedAt
// defaults to now (stored copy only, not written back to o).
func (s *MemStore) CreateOrg(ctx context.Context, o *Org) error {
	if o.Status == "" {
		o.Status = StatusActive
	}
	if o.ID == "" {
		o.ID = newID("org_")
	}
	stored := cloneOrg(o)
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orgs[stored.ID] = stored
	return nil
}

// GetOrg returns a copy of the org by ID, or ErrNotFound.
func (s *MemStore) GetOrg(ctx context.Context, id string) (*Org, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if o, ok := s.orgs[id]; ok {
		return cloneOrg(o), nil
	}
	return nil, ErrNotFound
}

// GetOrgBySlug returns a copy of the org by slug, or ErrNotFound.
func (s *MemStore) GetOrgBySlug(ctx context.Context, slug string) (*Org, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, o := range s.orgs {
		if o.Slug == slug {
			return cloneOrg(o), nil
		}
	}
	return nil, ErrNotFound
}

// ListOrgs returns all orgs newest-first.
func (s *MemStore) ListOrgs(ctx context.Context) ([]*Org, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Org, 0, len(s.orgs))
	for _, o := range s.orgs {
		out = append(out, cloneOrg(o))
	}
	sortOrgsNewestFirst(out)
	return out, nil
}

// UpdateOrg updates the org's display name (slug / status / created fields
// each have dedicated paths), matching SQLStore.UpdateOrg semantics.
func (s *MemStore) UpdateOrg(ctx context.Context, o *Org) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.orgs[o.ID]; ok {
		cur.Name = o.Name
	}
	// SQL UPDATE with zero rows is not an error; mirror that here.
	return nil
}

// DeleteOrg removes the org and its identity-domain rows (invitations +
// members), mirroring the transactional cascade in SQLStore.DeleteOrg.
func (s *MemStore) DeleteOrg(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, inv := range s.invitations {
		if inv.OrgID == id {
			delete(s.invitations, k)
		}
	}
	for k, m := range s.members {
		if m.OrgID == id {
			delete(s.members, k)
		}
	}
	delete(s.orgs, id)
	return nil
}

// ListOrgsForEmail returns each org the email is a member of (newest→oldest),
// with Org.Role set to the member's role.
func (s *MemStore) ListOrgsForEmail(ctx context.Context, email string) ([]*Org, error) {
	email = normEmail(email)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Org
	for _, m := range s.members {
		if m.Email != email {
			continue
		}
		if o, ok := s.orgs[m.OrgID]; ok {
			c := cloneOrg(o)
			c.Role = m.Role
			out = append(out, c)
		}
	}
	sortOrgsNewestFirst(out)
	return out, nil
}

// SetOrgStatus sets the lifecycle status (active|frozen).
func (s *MemStore) SetOrgStatus(ctx context.Context, id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.orgs[id]; ok {
		o.Status = status
	}
	return nil
}

// CountOrgAdmins returns the number of admin-role members (including the
// legacy "org_admin" alias), matching SQLStore semantics.
func (s *MemStore) CountOrgAdmins(ctx context.Context, orgID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, m := range s.members {
		if m.OrgID == orgID && (m.Role == RoleAdmin || m.Role == "org_admin") {
			n++
		}
	}
	return n, nil
}

// CountOrgOwners returns the number of owner-role members.
func (s *MemStore) CountOrgOwners(ctx context.Context, orgID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, m := range s.members {
		if m.OrgID == orgID && m.Role == RoleOwner {
			n++
		}
	}
	return n, nil
}

// CountOrgs returns the total org count.
func (s *MemStore) CountOrgs(ctx context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.orgs), nil
}

// CountOrgsByCreator returns how many orgs the given user self-created (by
// CreatedBy), matching SQLStore.CountOrgsByCreator.
func (s *MemStore) CountOrgsByCreator(ctx context.Context, userID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, o := range s.orgs {
		if o.CreatedBy == userID {
			n++
		}
	}
	return n, nil
}

// ── org_members ───────────────────────────────────────────────────────────

// AddOrgMember upserts a (org_id, email) membership. Email is normalised;
// role defaults to editor. On conflict the stored role is updated and the
// original row ID is preserved (matching SQLStore.AddOrgMember upsert).
func (s *MemStore) AddOrgMember(ctx context.Context, m *Member) error {
	m.Email = normEmail(m.Email)
	if m.Role == "" {
		m.Role = RoleEditor
	}
	key := memberKey(m.OrgID, m.Email)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.members[key]; ok {
		existing.Role = m.Role // upsert: change role only, keep original id/created_at
		return nil
	}
	if m.ID == "" {
		m.ID = newID("mem_")
	}
	stored := cloneMember(m)
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now()
	}
	s.members[key] = stored
	return nil
}

// GetOrgMember returns the member for (orgID, email), or ErrNotFound.
func (s *MemStore) GetOrgMember(ctx context.Context, orgID, email string) (*Member, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.members[memberKey(orgID, email)]; ok {
		return cloneMember(m), nil
	}
	return nil, ErrNotFound
}

// ListOrgMembers returns all members of an org newest-first.
func (s *MemStore) ListOrgMembers(ctx context.Context, orgID string) ([]*Member, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Member
	for _, m := range s.members {
		if m.OrgID == orgID {
			out = append(out, cloneMember(m))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// RemoveOrgMember deletes the (orgID, email) membership. No-op if absent.
func (s *MemStore) RemoveOrgMember(ctx context.Context, orgID, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.members, memberKey(orgID, email))
	return nil
}

// ── org_invitations ───────────────────────────────────────────────────────

// CreateInvitation inserts a pending invitation. Email is normalised; role
// defaults to viewer; status defaults to pending; ID is generated if empty.
func (s *MemStore) CreateInvitation(ctx context.Context, inv *Invitation) error {
	inv.Email = normEmail(inv.Email)
	if inv.Role == "" {
		inv.Role = RoleViewer
	}
	if inv.Status == "" {
		inv.Status = InviteStatusPending
	}
	if inv.ID == "" {
		inv.ID = newID("inv_")
	}
	stored := cloneInvitation(inv)
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invitations[stored.ID] = stored
	return nil
}

// GetInvitationByToken returns the invitation with the given token, or ErrNotFound.
func (s *MemStore) GetInvitationByToken(ctx context.Context, token string) (*Invitation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, inv := range s.invitations {
		if inv.Token == token {
			return cloneInvitation(inv), nil
		}
	}
	return nil, ErrNotFound
}

// ListInvitations returns all invitations for an org newest-first.
func (s *MemStore) ListInvitations(ctx context.Context, orgID string) ([]*Invitation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Invitation
	for _, inv := range s.invitations {
		if inv.OrgID == orgID {
			out = append(out, cloneInvitation(inv))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// SetInvitationStatus transitions an invitation to the new status.
func (s *MemStore) SetInvitationStatus(ctx context.Context, id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inv, ok := s.invitations[id]; ok {
		inv.Status = status
	}
	return nil
}

// ── sorting helpers ───────────────────────────────────────────────────────

// sortOrgsNewestFirst sorts orgs by CreatedAt descending (newest → oldest),
// matching `ORDER BY created_at DESC` in all SQLStore list queries.
func sortOrgsNewestFirst(orgs []*Org) {
	sort.SliceStable(orgs, func(i, j int) bool {
		return orgs[i].CreatedAt.After(orgs[j].CreatedAt)
	})
}
