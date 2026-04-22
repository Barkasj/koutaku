-- RBAC Schema Migration for PostgreSQL
-- Run with: psql -f 001_rbac_schema.sql

CREATE TABLE IF NOT EXISTS rbac_roles (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    is_system   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS rbac_permissions (
    id          TEXT PRIMARY KEY,
    resource    TEXT NOT NULL,
    operation   TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    UNIQUE(resource, operation)
);

CREATE TABLE IF NOT EXISTS rbac_role_permissions (
    role_id       TEXT NOT NULL REFERENCES rbac_roles(id) ON DELETE CASCADE,
    permission_id TEXT NOT NULL REFERENCES rbac_permissions(id) ON DELETE CASCADE,
    PRIMARY KEY (role_id, permission_id)
);

CREATE TABLE IF NOT EXISTS rbac_user_roles (
    user_id     TEXT NOT NULL,
    role_id     TEXT NOT NULL REFERENCES rbac_roles(id) ON DELETE CASCADE,
    assigned_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, role_id)
);

CREATE INDEX IF NOT EXISTS idx_rbac_user_roles_user ON rbac_user_roles(user_id);

-- Seed all 56 permissions (14 resources × 4 operations)
INSERT INTO rbac_permissions (id, resource, operation, description) VALUES
    ('logs:view', 'logs', 'view', 'view logs'),
    ('logs:create', 'logs', 'create', 'create logs'),
    ('logs:update', 'logs', 'update', 'update logs'),
    ('logs:delete', 'logs', 'delete', 'delete logs'),
    ('model_provider:view', 'model_provider', 'view', 'view model_provider'),
    ('model_provider:create', 'model_provider', 'create', 'create model_provider'),
    ('model_provider:update', 'model_provider', 'update', 'update model_provider'),
    ('model_provider:delete', 'model_provider', 'delete', 'delete model_provider'),
    ('observability:view', 'observability', 'view', 'view observability'),
    ('observability:create', 'observability', 'create', 'create observability'),
    ('observability:update', 'observability', 'update', 'update observability'),
    ('observability:delete', 'observability', 'delete', 'delete observability'),
    ('plugins:view', 'plugins', 'view', 'view plugins'),
    ('plugins:create', 'plugins', 'create', 'create plugins'),
    ('plugins:update', 'plugins', 'update', 'update plugins'),
    ('plugins:delete', 'plugins', 'delete', 'delete plugins'),
    ('virtual_keys:view', 'virtual_keys', 'view', 'view virtual_keys'),
    ('virtual_keys:create', 'virtual_keys', 'create', 'create virtual_keys'),
    ('virtual_keys:update', 'virtual_keys', 'update', 'update virtual_keys'),
    ('virtual_keys:delete', 'virtual_keys', 'delete', 'delete virtual_keys'),
    ('user_provisioning:view', 'user_provisioning', 'view', 'view user_provisioning'),
    ('user_provisioning:create', 'user_provisioning', 'create', 'create user_provisioning'),
    ('user_provisioning:update', 'user_provisioning', 'update', 'update user_provisioning'),
    ('user_provisioning:delete', 'user_provisioning', 'delete', 'delete user_provisioning'),
    ('users:view', 'users', 'view', 'view users'),
    ('users:create', 'users', 'create', 'create users'),
    ('users:update', 'users', 'update', 'update users'),
    ('users:delete', 'users', 'delete', 'delete users'),
    ('audit_logs:view', 'audit_logs', 'view', 'view audit_logs'),
    ('audit_logs:create', 'audit_logs', 'create', 'create audit_logs'),
    ('audit_logs:update', 'audit_logs', 'update', 'update audit_logs'),
    ('audit_logs:delete', 'audit_logs', 'delete', 'delete audit_logs'),
    ('guardrails_config:view', 'guardrails_config', 'view', 'view guardrails_config'),
    ('guardrails_config:create', 'guardrails_config', 'create', 'create guardrails_config'),
    ('guardrails_config:update', 'guardrails_config', 'update', 'update guardrails_config'),
    ('guardrails_config:delete', 'guardrails_config', 'delete', 'delete guardrails_config'),
    ('guardrail_rules:view', 'guardrail_rules', 'view', 'view guardrail_rules'),
    ('guardrail_rules:create', 'guardrail_rules', 'create', 'create guardrail_rules'),
    ('guardrail_rules:update', 'guardrail_rules', 'update', 'update guardrail_rules'),
    ('guardrail_rules:delete', 'guardrail_rules', 'delete', 'delete guardrail_rules'),
    ('cluster:view', 'cluster', 'view', 'view cluster'),
    ('cluster:create', 'cluster', 'create', 'create cluster'),
    ('cluster:update', 'cluster', 'update', 'update cluster'),
    ('cluster:delete', 'cluster', 'delete', 'delete cluster'),
    ('settings:view', 'settings', 'view', 'view settings'),
    ('settings:create', 'settings', 'create', 'create settings'),
    ('settings:update', 'settings', 'update', 'update settings'),
    ('settings:delete', 'settings', 'delete', 'delete settings'),
    ('mcp_gateway:view', 'mcp_gateway', 'view', 'view mcp_gateway'),
    ('mcp_gateway:create', 'mcp_gateway', 'create', 'create mcp_gateway'),
    ('mcp_gateway:update', 'mcp_gateway', 'update', 'update mcp_gateway'),
    ('mcp_gateway:delete', 'mcp_gateway', 'delete', 'delete mcp_gateway'),
    ('adaptive_router:view', 'adaptive_router', 'view', 'view adaptive_router'),
    ('adaptive_router:create', 'adaptive_router', 'create', 'create adaptive_router'),
    ('adaptive_router:update', 'adaptive_router', 'update', 'update adaptive_router'),
    ('adaptive_router:delete', 'adaptive_router', 'delete', 'delete adaptive_router')
ON CONFLICT (id) DO NOTHING;

-- Seed system roles
INSERT INTO rbac_roles (id, name, description, is_system) VALUES
    ('admin', 'Admin', 'Full access to all resources and operations', TRUE),
    ('developer', 'Developer', 'Standard developer access: view all, create/update most resources', TRUE),
    ('viewer', 'Viewer', 'Read-only access to all resources', TRUE)
ON CONFLICT (id) DO NOTHING;

-- Admin: all 56 permissions
INSERT INTO rbac_role_permissions (role_id, permission_id)
SELECT 'admin', id FROM rbac_permissions
ON CONFLICT DO NOTHING;

-- Developer: 27 permissions
INSERT INTO rbac_role_permissions (role_id, permission_id) VALUES
    ('developer', 'logs:view'), ('developer', 'logs:create'),
    ('developer', 'model_provider:view'), ('developer', 'model_provider:create'), ('developer', 'model_provider:update'),
    ('developer', 'observability:view'), ('developer', 'observability:create'), ('developer', 'observability:update'),
    ('developer', 'plugins:view'), ('developer', 'plugins:create'), ('developer', 'plugins:update'),
    ('developer', 'virtual_keys:view'), ('developer', 'virtual_keys:create'), ('developer', 'virtual_keys:update'), ('developer', 'virtual_keys:delete'),
    ('developer', 'users:view'),
    ('developer', 'audit_logs:view'),
    ('developer', 'guardrails_config:view'), ('developer', 'guardrails_config:create'), ('developer', 'guardrails_config:update'),
    ('developer', 'guardrail_rules:view'), ('developer', 'guardrail_rules:create'), ('developer', 'guardrail_rules:update'), ('developer', 'guardrail_rules:delete'),
    ('developer', 'cluster:view'),
    ('developer', 'settings:view'), ('developer', 'settings:update')
ON CONFLICT DO NOTHING;

-- Viewer: 14 permissions
INSERT INTO rbac_role_permissions (role_id, permission_id) VALUES
    ('viewer', 'logs:view'), ('viewer', 'model_provider:view'), ('viewer', 'observability:view'),
    ('viewer', 'plugins:view'), ('viewer', 'virtual_keys:view'), ('viewer', 'users:view'),
    ('viewer', 'audit_logs:view'), ('viewer', 'guardrails_config:view'), ('viewer', 'guardrail_rules:view'),
    ('viewer', 'cluster:view'), ('viewer', 'settings:view'), ('viewer', 'mcp_gateway:view'),
    ('viewer', 'adaptive_router:view'), ('viewer', 'user_provisioning:view')
ON CONFLICT DO NOTHING;
