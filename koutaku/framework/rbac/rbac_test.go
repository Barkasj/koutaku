package rbac

import (
	"testing"
)

func TestSystemRolesExist(t *testing.T) {
	store, err := Init()
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	roles, err := store.ListRoles()
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}

	if len(roles) != 3 {
		t.Fatalf("expected 3 system roles, got %d", len(roles))
	}

	// Check admin has all permissions
	adminPerms, err := store.GetRolePermissions(RoleAdmin)
	if err != nil {
		t.Fatalf("GetRolePermissions(admin): %v", err)
	}

	expectedAdminPerms := len(AllResources()) * len(AllOperations())
	if len(adminPerms) != expectedAdminPerms {
		t.Errorf("admin: expected %d permissions, got %d", expectedAdminPerms, len(adminPerms))
	}
}

func TestHasPermission(t *testing.T) {
	store, err := Init()
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Assign admin role
	if err := store.AssignRole("user-1", RoleAdmin); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	// Admin should have all permissions
	ok, err := store.HasPermission("user-1", ResourceLogs, OperationView)
	if err != nil {
		t.Fatalf("HasPermission: %v", err)
	}
	if !ok {
		t.Error("admin should have logs:view permission")
	}

	// Unassigned user should have no permissions
	ok, err = store.HasPermission("user-unknown", ResourceLogs, OperationView)
	if err != nil {
		t.Fatalf("HasPermission: %v", err)
	}
	if ok {
		t.Error("unassigned user should not have logs:view permission")
	}
}

func TestCustomRole(t *testing.T) {
	store, err := Init()
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Create custom role
	role := &Role{
		ID:          "auditor",
		Name:        "Auditor",
		Description: "Read-only audit access",
		IsSystem:    false,
		Permissions: []Permission{
			{ID: permID(ResourceAuditLogs, OperationView), Resource: ResourceAuditLogs, Operation: OperationView},
			{ID: permID(ResourceLogs, OperationView), Resource: ResourceLogs, Operation: OperationView},
		},
	}
	if err := store.CreateRole(role); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}

	// Assign and check
	if err := store.AssignRole("user-auditor", "auditor"); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	ok, _ := store.HasPermission("user-auditor", ResourceAuditLogs, OperationView)
	if !ok {
		t.Error("auditor should have audit_logs:view")
	}

	ok, _ = store.HasPermission("user-auditor", ResourceAuditLogs, OperationDelete)
	if ok {
		t.Error("auditor should NOT have audit_logs:delete")
	}
}
