// Package rbac provides Role-Based Access Control for the Koutaku AI Gateway.
package rbac

import "time"

// Resource represents a system resource that can be protected by RBAC.
type Resource string

const (
	ResourceLogs             Resource = "logs"
	ResourceModelProvider    Resource = "model_provider"
	ResourceObservability    Resource = "observability"
	ResourcePlugins          Resource = "plugins"
	ResourceVirtualKeys      Resource = "virtual_keys"
	ResourceUserProvisioning Resource = "user_provisioning"
	ResourceUsers            Resource = "users"
	ResourceAuditLogs        Resource = "audit_logs"
	ResourceGuardrailsConfig Resource = "guardrails_config"
	ResourceGuardrailRules   Resource = "guardrail_rules"
	ResourceCluster          Resource = "cluster"
	ResourceSettings         Resource = "settings"
	ResourceMCPGateway       Resource = "mcp_gateway"
	ResourceAdaptiveRouter   Resource = "adaptive_router"
)

// AllResources returns every defined Resource.
func AllResources() []Resource {
	return []Resource{
		ResourceLogs, ResourceModelProvider, ResourceObservability, ResourcePlugins,
		ResourceVirtualKeys, ResourceUserProvisioning, ResourceUsers, ResourceAuditLogs,
		ResourceGuardrailsConfig, ResourceGuardrailRules, ResourceCluster, ResourceSettings,
		ResourceMCPGateway, ResourceAdaptiveRouter,
	}
}

// Operation represents a CRUD operation.
type Operation string

const (
	OperationView   Operation = "view"
	OperationCreate Operation = "create"
	OperationUpdate Operation = "update"
	OperationDelete Operation = "delete"
)

// AllOperations returns every defined Operation.
func AllOperations() []Operation {
	return []Operation{OperationView, OperationCreate, OperationUpdate, OperationDelete}
}

// Permission links a resource and operation together with metadata.
type Permission struct {
	ID          string    `json:"id"`
	Resource    Resource  `json:"resource"`
	Operation   Operation `json:"operation"`
	Description string    `json:"description"`
}

// Role groups a set of permissions and metadata.
type Role struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	IsSystem    bool         `json:"is_system"`
	Permissions []Permission `json:"permissions"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// UserRole links a user to a role.
type UserRole struct {
	UserID     string    `json:"user_id"`
	RoleID     string    `json:"role_id"`
	AssignedAt time.Time `json:"assigned_at"`
}

// --- System role IDs ---

const (
	RoleAdmin     = "admin"
	RoleDeveloper = "developer"
	RoleViewer    = "viewer"
)

// --- Permission IDs ---
// Convention: "resource:operation"

func permID(r Resource, o Operation) string {
	return string(r) + ":" + string(o)
}

// buildPermission creates a Permission with auto-generated ID and description.
func buildPermission(r Resource, o Operation) Permission {
	return Permission{
		ID:          permID(r, o),
		Resource:    r,
		Operation:   o,
		Description: string(o) + " " + string(r),
	}
}

// allPermissions returns every possible resource×operation combination.
func allPermissions() []Permission {
	var perms []Permission
	for _, r := range AllResources() {
		for _, o := range AllOperations() {
			perms = append(perms, buildPermission(r, o))
		}
	}
	return perms
}

// permissionMap indexes permissions by ID for O(1) lookup.
func permissionMap(perms []Permission) map[string]Permission {
	m := make(map[string]Permission, len(perms))
	for _, p := range perms {
		m[p.ID] = p
	}
	return m
}

// filterPerms returns only the permissions whose IDs are in the allow set.
func filterPerms(all []Permission, allow map[string]bool) []Permission {
	var out []Permission
	for _, p := range all {
		if allow[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

// --- Default system role definitions ---

// defaultAdminRole returns the Admin role (all permissions).
func defaultAdminRole(now time.Time) Role {
	all := allPermissions()
	return Role{
		ID:          RoleAdmin,
		Name:        "Admin",
		Description: "Full access to all resources and operations",
		IsSystem:    true,
		Permissions: all,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// defaultDeveloperRole returns the Developer role (27 permissions).
// Developers can view everything, create/update most resources,
// but cannot delete critical resources or manage user provisioning.
func defaultDeveloperRole(now time.Time) Role {
	allow := map[string]bool{
		// Logs
		permID(ResourceLogs, OperationView):   true,
		permID(ResourceLogs, OperationCreate): true,
		// ModelProvider
		permID(ResourceModelProvider, OperationView):   true,
		permID(ResourceModelProvider, OperationCreate): true,
		permID(ResourceModelProvider, OperationUpdate): true,
		// Observability
		permID(ResourceObservability, OperationView):   true,
		permID(ResourceObservability, OperationCreate): true,
		permID(ResourceObservability, OperationUpdate): true,
		// Plugins
		permID(ResourcePlugins, OperationView):   true,
		permID(ResourcePlugins, OperationCreate): true,
		permID(ResourcePlugins, OperationUpdate): true,
		// VirtualKeys
		permID(ResourceVirtualKeys, OperationView):   true,
		permID(ResourceVirtualKeys, OperationCreate): true,
		permID(ResourceVirtualKeys, OperationUpdate): true,
		permID(ResourceVirtualKeys, OperationDelete): true,
		// Users (view only)
		permID(ResourceUsers, OperationView): true,
		// AuditLogs (view only)
		permID(ResourceAuditLogs, OperationView): true,
		// GuardrailsConfig
		permID(ResourceGuardrailsConfig, OperationView):   true,
		permID(ResourceGuardrailsConfig, OperationCreate): true,
		permID(ResourceGuardrailsConfig, OperationUpdate): true,
		// GuardrailRules
		permID(ResourceGuardrailRules, OperationView):   true,
		permID(ResourceGuardrailRules, OperationCreate): true,
		permID(ResourceGuardrailRules, OperationUpdate): true,
		permID(ResourceGuardrailRules, OperationDelete): true,
		// Cluster (view only)
		permID(ResourceCluster, OperationView): true,
		// Settings (view + update)
		permID(ResourceSettings, OperationView):   true,
		permID(ResourceSettings, OperationUpdate): true,
	}
	all := allPermissions()
	return Role{
		ID:          RoleDeveloper,
		Name:        "Developer",
		Description: "Standard developer access: view all, create/update most resources, no user provisioning or critical deletes",
		IsSystem:    true,
		Permissions: filterPerms(all, allow),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// defaultViewerRole returns the Viewer role (14 permissions).
// Viewers have read-only access to most resources.
func defaultViewerRole(now time.Time) Role {
	allow := map[string]bool{
		permID(ResourceLogs, OperationView):               true,
		permID(ResourceModelProvider, OperationView):      true,
		permID(ResourceObservability, OperationView):      true,
		permID(ResourcePlugins, OperationView):            true,
		permID(ResourceVirtualKeys, OperationView):        true,
		permID(ResourceUsers, OperationView):              true,
		permID(ResourceAuditLogs, OperationView):          true,
		permID(ResourceGuardrailsConfig, OperationView):   true,
		permID(ResourceGuardrailRules, OperationView):     true,
		permID(ResourceCluster, OperationView):            true,
		permID(ResourceSettings, OperationView):           true,
		permID(ResourceMCPGateway, OperationView):         true,
		permID(ResourceAdaptiveRouter, OperationView):     true,
		permID(ResourceUserProvisioning, OperationView):   true,
	}
	all := allPermissions()
	return Role{
		ID:          RoleViewer,
		Name:        "Viewer",
		Description: "Read-only access to all resources",
		IsSystem:    true,
		Permissions: filterPerms(all, allow),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// SystemRoles returns the three built-in roles seeded at startup.
func SystemRoles(now time.Time) []Role {
	return []Role{
		defaultAdminRole(now),
		defaultDeveloperRole(now),
		defaultViewerRole(now),
	}
}
