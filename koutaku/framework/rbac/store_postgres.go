package rbac

import (
	"fmt"
	"time"

	"gorm.io/gorm"
)

// PostgresMigrations contains the SQL statements required to bootstrap
// the RBAC tables in PostgreSQL.
var PostgresMigrations = []string{
	`CREATE TABLE IF NOT EXISTS rbac_roles (
		id          TEXT PRIMARY KEY,
		name        TEXT NOT NULL UNIQUE,
		description TEXT NOT NULL DEFAULT '',
		is_system   BOOLEAN NOT NULL DEFAULT FALSE,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE TABLE IF NOT EXISTS rbac_permissions (
		id          TEXT PRIMARY KEY,
		resource    TEXT NOT NULL,
		operation   TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		UNIQUE(resource, operation)
	)`,
	`CREATE TABLE IF NOT EXISTS rbac_role_permissions (
		role_id       TEXT NOT NULL REFERENCES rbac_roles(id) ON DELETE CASCADE,
		permission_id TEXT NOT NULL REFERENCES rbac_permissions(id) ON DELETE CASCADE,
		PRIMARY KEY (role_id, permission_id)
	)`,
	`CREATE TABLE IF NOT EXISTS rbac_user_roles (
		user_id     TEXT NOT NULL,
		role_id     TEXT NOT NULL REFERENCES rbac_roles(id) ON DELETE CASCADE,
		assigned_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (user_id, role_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_rbac_user_roles_user ON rbac_user_roles(user_id)`,
}

// --- GORM models for PostgreSQL persistence ---

type pgRole struct {
	ID          string    `gorm:"primaryKey;column:id"`
	Name        string    `gorm:"column:name"`
	Description string    `gorm:"column:description"`
	IsSystem    bool      `gorm:"column:is_system"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

func (pgRole) TableName() string { return "rbac_roles" }

type pgPermission struct {
	ID          string `gorm:"primaryKey;column:id"`
	Resource    string `gorm:"column:resource"`
	Operation   string `gorm:"column:operation"`
	Description string `gorm:"column:description"`
}

func (pgPermission) TableName() string { return "rbac_permissions" }

type pgRolePermission struct {
	RoleID       string `gorm:"primaryKey;column:role_id"`
	PermissionID string `gorm:"primaryKey;column:permission_id"`
}

func (pgRolePermission) TableName() string { return "rbac_role_permissions" }

type pgUserRole struct {
	UserID     string    `gorm:"primaryKey;column:user_id"`
	RoleID     string    `gorm:"primaryKey;column:role_id"`
	AssignedAt time.Time `gorm:"column:assigned_at"`
}

func (pgUserRole) TableName() string { return "rbac_user_roles" }

// PostgresStore is a GORM-backed RBACStore implementation for PostgreSQL.
type PostgresStore struct {
	db *gorm.DB
}

// NewPostgresStore creates a PostgresStore. Call Migrate() to ensure tables exist.
func NewPostgresStore(db *gorm.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// Migrate runs all DDL statements to create the RBAC schema.
func (s *PostgresStore) Migrate() error {
	for _, stmt := range PostgresMigrations {
		if err := s.db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("rbac migration: %w", err)
		}
	}
	return nil
}

// CreateRole inserts a new role and its permissions.
func (s *PostgresStore) CreateRole(role *Role) error {
	if role == nil {
		return fmt.Errorf("rbac: role cannot be nil")
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		pg := toPgRole(role)
		if err := tx.Create(&pg).Error; err != nil {
			return err
		}
		return s.syncRolePerms(tx, role.ID, role.Permissions)
	})
}

// GetRole loads a role with its permissions.
func (s *PostgresStore) GetRole(id string) (*Role, error) {
	var pg pgRole
	if err := s.db.First(&pg, "id = ?", id).Error; err != nil {
		return nil, fmt.Errorf("rbac: role %q not found: %w", id, err)
	}
	perms, err := s.loadPerms(id)
	if err != nil {
		return nil, err
	}
	r := fromPgRole(&pg, perms)
	return &r, nil
}

// ListRoles returns all roles with their permissions.
func (s *PostgresStore) ListRoles() ([]Role, error) {
	var pgs []pgRole
	if err := s.db.Find(&pgs).Error; err != nil {
		return nil, err
	}
	out := make([]Role, 0, len(pgs))
	for _, pg := range pgs {
		perms, err := s.loadPerms(pg.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, fromPgRole(&pg, perms))
	}
	return out, nil
}

// UpdateRole updates role metadata and replaces permissions.
func (s *PostgresStore) UpdateRole(role *Role) error {
	if role == nil {
		return fmt.Errorf("rbac: role cannot be nil")
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		pg := toPgRole(role)
		pg.UpdatedAt = time.Now().UTC()
		if err := tx.Save(&pg).Error; err != nil {
			return err
		}
		// Replace permissions
		if err := tx.Where("role_id = ?", role.ID).Delete(&pgRolePermission{}).Error; err != nil {
			return err
		}
		return s.syncRolePerms(tx, role.ID, role.Permissions)
	})
}

// DeleteRole removes a role (system roles are protected at the application layer).
func (s *PostgresStore) DeleteRole(id string) error {
	var pg pgRole
	if err := s.db.First(&pg, "id = ?", id).Error; err != nil {
		return fmt.Errorf("rbac: role %q not found: %w", id, err)
	}
	if pg.IsSystem {
		return fmt.Errorf("rbac: cannot delete system role %q", id)
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("role_id = ?", id).Delete(&pgRolePermission{}).Error; err != nil {
			return err
		}
		if err := tx.Where("role_id = ?", id).Delete(&pgUserRole{}).Error; err != nil {
			return err
		}
		return tx.Delete(&pgRole{}, "id = ?", id).Error
	})
}

// GetRolePermissions returns permissions for a role.
func (s *PostgresStore) GetRolePermissions(roleID string) ([]Permission, error) {
	return s.loadPerms(roleID)
}

// SetRolePermissions replaces all permissions for a role.
func (s *PostgresStore) SetRolePermissions(roleID string, permissionIDs []string) error {
	var pg pgRole
	if err := s.db.First(&pg, "id = ?", roleID).Error; err != nil {
		return fmt.Errorf("rbac: role %q not found: %w", roleID, err)
	}
	if pg.IsSystem {
		return fmt.Errorf("rbac: cannot modify permissions of system role %q", roleID)
	}
	var perms []Permission
	for _, pid := range permissionIDs {
		if p, ok := permissionMap(allPermissions())[pid]; ok {
			perms = append(perms, p)
		}
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("role_id = ?", roleID).Delete(&pgRolePermission{}).Error; err != nil {
			return err
		}
		return s.syncRolePerms(tx, roleID, perms)
	})
}

// AssignRole grants a role to a user (idempotent).
func (s *PostgresStore) AssignRole(userID, roleID string) error {
	var pg pgRole
	if err := s.db.First(&pg, "id = ?", roleID).Error; err != nil {
		return fmt.Errorf("rbac: role %q not found: %w", roleID, err)
	}
	return s.db.Clauses().Create(&pgUserRole{
		UserID:     userID,
		RoleID:     roleID,
		AssignedAt: time.Now().UTC(),
	}).Error
}

// GetUserRoles returns all roles assigned to a user.
func (s *PostgresStore) GetUserRoles(userID string) ([]Role, error) {
	var links []pgUserRole
	if err := s.db.Where("user_id = ?", userID).Find(&links).Error; err != nil {
		return nil, err
	}
	out := make([]Role, 0, len(links))
	for _, link := range links {
		r, err := s.GetRole(link.RoleID)
		if err != nil {
			continue // skip stale references
		}
		out = append(out, *r)
	}
	return out, nil
}

// RemoveUserRole revokes a role from a user.
func (s *PostgresStore) RemoveUserRole(userID, roleID string) error {
	res := s.db.Where("user_id = ? AND role_id = ?", userID, roleID).Delete(&pgUserRole{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("rbac: user %q does not have role %q", userID, roleID)
	}
	return nil
}

// HasPermission checks if a user has a specific permission through any assigned role.
func (s *PostgresStore) HasPermission(userID string, resource Resource, operation Operation) (bool, error) {
	target := permID(resource, operation)
	var count int64
	err := s.db.Model(&pgUserRole{}).
		Joins("JOIN rbac_role_permissions rp ON rp.role_id = rbac_user_roles.role_id").
		Where("rbac_user_roles.user_id = ? AND rp.permission_id = ?", userID, target).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// --- helpers ---

func (s *PostgresStore) loadPerms(roleID string) ([]Permission, error) {
	var links []pgRolePermission
	if err := s.db.Where("role_id = ?", roleID).Find(&links).Error; err != nil {
		return nil, err
	}
	idx := permissionMap(allPermissions())
	out := make([]Permission, 0, len(links))
	for _, l := range links {
		if p, ok := idx[l.PermissionID]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *PostgresStore) syncRolePerms(tx *gorm.DB, roleID string, perms []Permission) error {
	for _, p := range perms {
		// Upsert permission record
		pgP := pgPermission{
			ID:          p.ID,
			Resource:    string(p.Resource),
			Operation:   string(p.Operation),
			Description: p.Description,
		}
		if err := tx.Clauses().Create(&pgP).Error; err != nil {
			// Ignore duplicate key (permission already seeded)
		}
		link := pgRolePermission{RoleID: roleID, PermissionID: p.ID}
		if err := tx.Create(&link).Error; err != nil {
			return err
		}
	}
	return nil
}

func toPgRole(r *Role) pgRole {
	return pgRole{
		ID:          r.ID,
		Name:        r.Name,
		Description: r.Description,
		IsSystem:    r.IsSystem,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

func fromPgRole(pg *pgRole, perms []Permission) Role {
	return Role{
		ID:          pg.ID,
		Name:        pg.Name,
		Description: pg.Description,
		IsSystem:    pg.IsSystem,
		Permissions: perms,
		CreatedAt:   pg.CreatedAt,
		UpdatedAt:   pg.UpdatedAt,
	}
}
