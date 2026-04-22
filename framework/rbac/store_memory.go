package rbac

import (
	"fmt"
	"sync"
	"time"
)

// MemoryStore is a concurrency-safe in-memory RBACStore suitable for
// testing and single-instance deployments.
type MemoryStore struct {
	mu          sync.RWMutex
	roles       map[string]*Role       // roleID -> Role
	assignments map[string][]string    // userID -> []roleID
	permIndex   map[string]Permission  // permission ID -> Permission (global lookup)
}

// NewMemoryStore creates an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		roles:       make(map[string]*Role),
		assignments: make(map[string][]string),
		permIndex:   permissionMap(allPermissions()),
	}
}

// CreateRole persists a new role. Returns an error if the ID already exists.
func (s *MemoryStore) CreateRole(role *Role) error {
	if role == nil {
		return fmt.Errorf("rbac: role cannot be nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.roles[role.ID]; exists {
		return fmt.Errorf("rbac: role %q already exists", role.ID)
	}
	// Defensive copy
	cp := *role
	cp.Permissions = append([]Permission(nil), role.Permissions...)
	s.roles[role.ID] = &cp
	return nil
}

// GetRole returns a copy of the role by ID.
func (s *MemoryStore) GetRole(id string) (*Role, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.roles[id]
	if !ok {
		return nil, fmt.Errorf("rbac: role %q not found", id)
	}
	cp := *r
	cp.Permissions = append([]Permission(nil), r.Permissions...)
	return &cp, nil
}

// ListRoles returns a snapshot of all roles.
func (s *MemoryStore) ListRoles() ([]Role, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Role, 0, len(s.roles))
	for _, r := range s.roles {
		cp := *r
		cp.Permissions = append([]Permission(nil), r.Permissions...)
		out = append(out, cp)
	}
	return out, nil
}

// UpdateRole replaces an existing role. Returns an error if the role doesn't exist.
func (s *MemoryStore) UpdateRole(role *Role) error {
	if role == nil {
		return fmt.Errorf("rbac: role cannot be nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.roles[role.ID]; !exists {
		return fmt.Errorf("rbac: role %q not found", role.ID)
	}
	cp := *role
	cp.Permissions = append([]Permission(nil), role.Permissions...)
	cp.UpdatedAt = time.Now().UTC()
	s.roles[role.ID] = &cp
	return nil
}

// DeleteRole removes a role. System roles cannot be deleted.
func (s *MemoryStore) DeleteRole(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.roles[id]
	if !ok {
		return fmt.Errorf("rbac: role %q not found", id)
	}
	if r.IsSystem {
		return fmt.Errorf("rbac: cannot delete system role %q", id)
	}
	// Remove any user assignments referencing this role
	for uid, rids := range s.assignments {
		filtered := rids[:0]
		for _, rid := range rids {
			if rid != id {
				filtered = append(filtered, rid)
			}
		}
		if len(filtered) == 0 {
			delete(s.assignments, uid)
		} else {
			s.assignments[uid] = filtered
		}
	}
	delete(s.roles, id)
	return nil
}

// GetRolePermissions returns the permissions currently attached to a role.
func (s *MemoryStore) GetRolePermissions(roleID string) ([]Permission, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.roles[roleID]
	if !ok {
		return nil, fmt.Errorf("rbac: role %q not found", roleID)
	}
	out := append([]Permission(nil), r.Permissions...)
	return out, nil
}

// SetRolePermissions replaces the full permission set for a role.
// permissionIDs that don't exist in the global index are silently skipped.
func (s *MemoryStore) SetRolePermissions(roleID string, permissionIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.roles[roleID]
	if !ok {
		return fmt.Errorf("rbac: role %q not found", roleID)
	}
	if r.IsSystem {
		return fmt.Errorf("rbac: cannot modify permissions of system role %q", roleID)
	}
	var perms []Permission
	for _, pid := range permissionIDs {
		if p, exists := s.permIndex[pid]; exists {
			perms = append(perms, p)
		}
	}
	r.Permissions = perms
	r.UpdatedAt = time.Now().UTC()
	return nil
}

// AssignRole grants a role to a user. Duplicate assignments are idempotent.
func (s *MemoryStore) AssignRole(userID, roleID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.roles[roleID]; !ok {
		return fmt.Errorf("rbac: role %q not found", roleID)
	}
	rids := s.assignments[userID]
	for _, rid := range rids {
		if rid == roleID {
			return nil // already assigned
		}
	}
	s.assignments[userID] = append(rids, roleID)
	return nil
}

// GetUserRoles returns all roles assigned to a user.
func (s *MemoryStore) GetUserRoles(userID string) ([]Role, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rids := s.assignments[userID]
	out := make([]Role, 0, len(rids))
	for _, rid := range rids {
		if r, ok := s.roles[rid]; ok {
			cp := *r
			cp.Permissions = append([]Permission(nil), r.Permissions...)
			out = append(out, cp)
		}
	}
	return out, nil
}

// RemoveUserRole revokes a role from a user.
func (s *MemoryStore) RemoveUserRole(userID, roleID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rids := s.assignments[userID]
	filtered := rids[:0]
	found := false
	for _, rid := range rids {
		if rid == roleID {
			found = true
			continue
		}
		filtered = append(filtered, rid)
	}
	if !found {
		return fmt.Errorf("rbac: user %q does not have role %q", userID, roleID)
	}
	if len(filtered) == 0 {
		delete(s.assignments, userID)
	} else {
		s.assignments[userID] = filtered
	}
	return nil
}

// HasPermission checks whether a user has the specified permission through
// any of their assigned roles.
func (s *MemoryStore) HasPermission(userID string, resource Resource, operation Operation) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rids := s.assignments[userID]
	target := permID(resource, operation)
	for _, rid := range rids {
		r, ok := s.roles[rid]
		if !ok {
			continue
		}
		for _, p := range r.Permissions {
			if p.ID == target {
				return true, nil
			}
		}
	}
	return false, nil
}
