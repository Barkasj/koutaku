package rbac

// RBACStore defines the persistence contract for the RBAC subsystem.
// Implementations may be in-memory (testing) or backed by a database (production).
type RBACStore interface {
	// --- Role CRUD ---
	CreateRole(role *Role) error
	GetRole(id string) (*Role, error)
	ListRoles() ([]Role, error)
	UpdateRole(role *Role) error
	DeleteRole(id string) error

	// --- Permission management ---
	GetRolePermissions(roleID string) ([]Permission, error)
	SetRolePermissions(roleID string, permissionIDs []string) error

	// --- User-role assignments ---
	AssignRole(userID string, roleID string) error
	GetUserRoles(userID string) ([]Role, error)
	RemoveUserRole(userID string, roleID string) error

	// --- Authorization check ---
	HasPermission(userID string, resource Resource, operation Operation) (bool, error)
}
