package rbac

import (
	"context"
	"net/http"
	"strings"
)

// contextKey is an unexported type for context keys in this package.
type contextKey string

const (
	// ContextKeyUserID is the context key under which the authenticated user ID
	// is stored. The authentication layer (JWT, API key, etc.) is responsible
	// for populating this before the RBAC middleware runs.
	ContextKeyUserID contextKey = "rbac_user_id"
)

// ResourceOperation maps an HTTP method to the RBAC operation it implies.
func methodToOperation(method string) Operation {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return OperationView
	case http.MethodPost:
		return OperationCreate
	case http.MethodPut, http.MethodPatch:
		return OperationUpdate
	case http.MethodDelete:
		return OperationDelete
	default:
		return OperationView
	}
}

// RouteResource maps a URL path prefix to the RBAC resource it protects.
// Adjust the mappings below to match your API surface.
var routeResourceMap = map[string]Resource{
	"/api/logs":              ResourceLogs,
	"/api/providers":         ResourceModelProvider,
	"/api/observability":     ResourceObservability,
	"/api/plugins":           ResourcePlugins,
	"/api/virtual-keys":      ResourceVirtualKeys,
	"/api/user-provisioning": ResourceUserProvisioning,
	"/api/users":             ResourceUsers,
	"/api/audit-logs":        ResourceAuditLogs,
	"/api/guardrails":        ResourceGuardrailsConfig,
	"/api/guardrails/rules":  ResourceGuardrailRules,
	"/api/cluster":           ResourceCluster,
	"/api/settings":          ResourceSettings,
	"/api/mcp-gateway":       ResourceMCPGateway,
	"/api/adaptive-router":   ResourceAdaptiveRouter,
}

// ResolveResource determines which RBAC resource a request path targets.
// Longer prefixes are matched first so sub-routes take priority.
func ResolveResource(path string) (Resource, bool) {
	bestLen := 0
	var bestRes Resource
	for prefix, res := range routeResourceMap {
		if strings.HasPrefix(path, prefix) && len(prefix) > bestLen {
			bestLen = len(prefix)
			bestRes = res
		}
	}
	if bestLen == 0 {
		return "", false
	}
	return bestRes, true
}

// Middleware returns an http.Handler middleware that enforces RBAC.
// Requests without a valid user ID in context are rejected with 401.
// Requests where the user lacks the required permission are rejected with 403.
func Middleware(store RBACStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract user ID from context (set by upstream auth middleware)
			userID, _ := r.Context().Value(ContextKeyUserID).(string)
			if userID == "" {
				http.Error(w, `{"error":"unauthorized","message":"authentication required"}`, http.StatusUnauthorized)
				return
			}

			// Resolve resource from path
			resource, ok := ResolveResource(r.URL.Path)
			if !ok {
				// Unknown resource — allow through (non-API routes, health checks, etc.)
				next.ServeHTTP(w, r)
				return
			}

			operation := methodToOperation(r.Method)

			allowed, err := store.HasPermission(userID, resource, operation)
			if err != nil {
				http.Error(w, `{"error":"internal_error","message":"permission check failed"}`, http.StatusInternalServerError)
				return
			}
			if !allowed {
				http.Error(w, `{"error":"forbidden","message":"insufficient permissions"}`, http.StatusForbidden)
				return
			}

			// Permission granted — continue
			next.ServeHTTP(w, r)
		})
	}
}

// WithUserID returns a new context with the user ID set for RBAC checks.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ContextKeyUserID, userID)
}

// GetUserID extracts the user ID from context.
func GetUserID(ctx context.Context) string {
	uid, _ := ctx.Value(ContextKeyUserID).(string)
	return uid
}
