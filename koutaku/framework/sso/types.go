// Package sso provides enterprise-grade Single Sign-On (SSO) integration
// for Koutaku AI Gateway with support for multiple identity providers.
package sso

import (
	"context"
	"time"
)

// ProviderType represents the type of SSO provider.
type ProviderType string

const (
	// ProviderTypeEntraID represents Microsoft Entra ID (Azure AD).
	ProviderTypeEntraID ProviderType = "entra_id"
	// ProviderTypeGoogle represents Google Workspace.
	ProviderTypeGoogle ProviderType = "google"
	// ProviderTypeKeycloak represents Keycloak.
	ProviderTypeKeycloak ProviderType = "keycloak"
	// ProviderTypeOkta represents Okta.
	ProviderTypeOkta ProviderType = "okta"
	// ProviderTypeZitadel represents Zitadel.
	ProviderTypeZitadel ProviderType = "zitadel"
)

// SSOConfig represents the configuration for SSO integration.
type SSOConfig struct {
	// Enabled enables SSO integration.
	Enabled bool `json:"enabled"`
	// Providers is a list of SSO provider configurations.
	Providers []ProviderConfig `json:"providers"`
	// DefaultRole is the default role assigned to new users.
	DefaultRole string `json:"default_role,omitempty"`
	// AutoCreateUsers enables automatic user creation on first login.
	AutoCreateUsers bool `json:"auto_create_users,omitempty"`
	// SessionDuration is the duration of SSO sessions.
	SessionDuration time.Duration `json:"session_duration,omitempty"`
}

// ProviderConfig represents the configuration for a specific SSO provider.
type ProviderConfig struct {
	// Type is the provider type.
	Type ProviderType `json:"type"`
	// Name is a human-readable name for the provider.
	Name string `json:"name"`
	// ClientID is the OAuth2 client ID.
	ClientID string `json:"client_id"`
	// ClientSecret is the OAuth2 client secret.
	ClientSecret string `json:"client_secret"`
	// Issuer is the OIDC issuer URL.
	Issuer string `json:"issuer"`
	// RedirectURI is the OAuth2 redirect URI.
	RedirectURI string `json:"redirect_uri"`
	// Scopes are the OAuth2 scopes to request.
	Scopes []string `json:"scopes,omitempty"`
	// Enabled enables this provider.
	Enabled bool `json:"enabled"`
	// Provider-specific configuration.
	EntraID *EntraIDConfig `json:"entra_id,omitempty"`
	Google  *GoogleConfig  `json:"google,omitempty"`
	Keycloak *KeycloakConfig `json:"keycloak,omitempty"`
	Okta    *OktaConfig    `json:"okta,omitempty"`
	Zitadel *ZitadelConfig `json:"zitadel,omitempty"`
}

// EntraIDConfig contains Microsoft Entra ID (Azure AD) specific configuration.
type EntraIDConfig struct {
	// TenantID is the Azure AD tenant ID.
	TenantID string `json:"tenant_id"`
	// Authority is the authority URL.
	// Defaults to "https://login.microsoftonline.com/{tenant_id}".
	Authority string `json:"authority,omitempty"`
}

// GoogleConfig contains Google Workspace specific configuration.
type GoogleConfig struct {
	// HostedDomain is the Google Workspace domain.
	// Example: "example.com"
	HostedDomain string `json:"hosted_domain,omitempty"`
}

// KeycloakConfig contains Keycloak specific configuration.
type KeycloakConfig struct {
	// Realm is the Keycloak realm name.
	Realm string `json:"realm"`
	// BaseURL is the Keycloak base URL.
	// Example: "https://keycloak.example.com"
	BaseURL string `json:"base_url"`
}

// OktaConfig contains Okta specific configuration.
type OktaConfig struct {
	// Domain is the Okta domain.
	// Example: "dev-123456.okta.com"
	Domain string `json:"domain"`
	// AuthorizationServer is the Okta authorization server.
	// Defaults to "default".
	AuthorizationServer string `json:"authorization_server,omitempty"`
}

// ZitadelConfig contains Zitadel specific configuration.
type ZitadelConfig struct {
	// InstanceURL is the Zitadel instance URL.
	// Example: "https://instance.zitadel.cloud"
	InstanceURL string `json:"instance_url"`
}

// User represents a user authenticated via SSO.
type User struct {
	// ID is the unique user identifier.
	ID string `json:"id"`
	// Email is the user's email address.
	Email string `json:"email"`
	// Name is the user's display name.
	Name string `json:"name"`
	// FirstName is the user's first name.
	FirstName string `json:"first_name,omitempty"`
	// LastName is the user's last name.
	LastName string `json:"last_name,omitempty"`
	// Picture is the URL to the user's profile picture.
	Picture string `json:"picture,omitempty"`
	// Provider is the SSO provider type.
	Provider ProviderType `json:"provider"`
	// ProviderUserID is the user's ID at the SSO provider.
	ProviderUserID string `json:"provider_user_id"`
	// Groups is the list of groups the user belongs to.
	Groups []string `json:"groups,omitempty"`
	// Roles is the list of roles assigned to the user.
	Roles []string `json:"roles,omitempty"`
	// Metadata contains additional user metadata.
	Metadata map[string]string `json:"metadata,omitempty"`
	// CreatedAt is when the user was created.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the user was last updated.
	UpdatedAt time.Time `json:"updated_at"`
	// LastLoginAt is when the user last logged in.
	LastLoginAt time.Time `json:"last_login_at,omitempty"`
	// Active indicates if the user account is active.
	Active bool `json:"active"`
}

// Session represents an SSO session.
type Session struct {
	// ID is the session identifier.
	ID string `json:"id"`
	// UserID is the user ID this session belongs to.
	UserID string `json:"user_id"`
	// Provider is the SSO provider type.
	Provider ProviderType `json:"provider"`
	// AccessToken is the OAuth2 access token.
	AccessToken string `json:"access_token"`
	// RefreshToken is the OAuth2 refresh token.
	RefreshToken string `json:"refresh_token,omitempty"`
	// IDToken is the OIDC ID token.
	IDToken string `json:"id_token,omitempty"`
	// ExpiresAt is when the session expires.
	ExpiresAt time.Time `json:"expires_at"`
	// CreatedAt is when the session was created.
	CreatedAt time.Time `json:"created_at"`
	// LastAccessedAt is when the session was last accessed.
	LastAccessedAt time.Time `json:"last_accessed_at"`
}

// AuthRequest represents an authentication request.
type AuthRequest struct {
	// Provider is the SSO provider to use.
	Provider ProviderType `json:"provider"`
	// RedirectURI is where to redirect after authentication.
	RedirectURI string `json:"redirect_uri,omitempty"`
	// State is a random string for CSRF protection.
	State string `json:"state"`
	// CodeChallenge is the PKCE code challenge.
	CodeChallenge string `json:"code_challenge,omitempty"`
	// CodeChallengeMethod is the PKCE code challenge method.
	CodeChallengeMethod string `json:"code_challenge_method,omitempty"`
}

// AuthResponse represents an authentication response.
type AuthResponse struct {
	// Code is the authorization code.
	Code string `json:"code"`
	// State is the state parameter from the request.
	State string `json:"state"`
}

// TokenResponse represents a token response.
type TokenResponse struct {
	// AccessToken is the OAuth2 access token.
	AccessToken string `json:"access_token"`
	// TokenType is the token type (usually "Bearer").
	TokenType string `json:"token_type"`
	// ExpiresIn is the token lifetime in seconds.
	ExpiresIn int `json:"expires_in"`
	// RefreshToken is the OAuth2 refresh token.
	RefreshToken string `json:"refresh_token,omitempty"`
	// IDToken is the OIDC ID token.
	IDToken string `json:"id_token,omitempty"`
	// Scope is the granted scope.
	Scope string `json:"scope,omitempty"`
}

// SSOProvider is the interface for SSO provider implementations.
type SSOProvider interface {
	// GetType returns the provider type.
	GetType() ProviderType
	// GetName returns the provider name.
	GetName() string
	// GetAuthURL returns the URL to redirect users for authentication.
	GetAuthURL(state string, opts ...AuthOption) (string, error)
	// Exchange exchanges an authorization code for tokens.
	Exchange(ctx context.Context, code string) (*TokenResponse, error)
	// GetUserInfo retrieves user information using an access token.
	GetUserInfo(ctx context.Context, accessToken string) (*User, error)
	// RefreshToken refreshes an access token using a refresh token.
	RefreshToken(ctx context.Context, refreshToken string) (*TokenResponse, error)
	// RevokeToken revokes an access or refresh token.
	RevokeToken(ctx context.Context, token string) error
	// ValidateToken validates an access token.
	ValidateToken(ctx context.Context, token string) (*User, error)
	// GetJWKS returns the JSON Web Key Set for token validation.
	GetJWKS(ctx context.Context) (interface{}, error)
}

// AuthOption represents an option for authentication requests.
type AuthOption func(*AuthOptions)

// AuthOptions contains options for authentication requests.
type AuthOptions struct {
	// Scopes are additional scopes to request.
	Scopes []string
	// Prompt is the prompt parameter.
	Prompt string
	// LoginHint is the login hint.
	LoginHint string
	// HD is the hosted domain (Google-specific).
	HD string
}

// WithScopes sets additional scopes.
func WithScopes(scopes ...string) AuthOption {
	return func(o *AuthOptions) {
		o.Scopes = append(o.Scopes, scopes...)
	}
}

// WithPrompt sets the prompt parameter.
func WithPrompt(prompt string) AuthOption {
	return func(o *AuthOptions) {
		o.Prompt = prompt
	}
}

// WithLoginHint sets the login hint.
func WithLoginHint(hint string) AuthOption {
	return func(o *AuthOptions) {
		o.LoginHint = hint
	}
}

// WithHostedDomain sets the hosted domain (Google-specific).
func WithHostedDomain(hd string) AuthOption {
	return func(o *AuthOptions) {
		o.HD = hd
	}
}

// UserStore is the interface for user storage.
type UserStore interface {
	// GetUserByID retrieves a user by ID.
	GetUserByID(ctx context.Context, id string) (*User, error)
	// GetUserByEmail retrieves a user by email.
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	// GetUserByProviderID retrieves a user by provider and provider user ID.
	GetUserByProviderID(ctx context.Context, provider ProviderType, providerUserID string) (*User, error)
	// CreateUser creates a new user.
	CreateUser(ctx context.Context, user *User) error
	// UpdateUser updates an existing user.
	UpdateUser(ctx context.Context, user *User) error
	// DeleteUser deletes a user.
	DeleteUser(ctx context.Context, id string) error
	// ListUsers lists all users.
	ListUsers(ctx context.Context, opts ...ListOption) ([]*User, error)
	// GetUsersByGroup retrieves users in a group.
	GetUsersByGroup(ctx context.Context, group string) ([]*User, error)
	// GetUsersByRole retrieves users with a specific role.
	GetUsersByRole(ctx context.Context, role string) ([]*User, error)
}

// SessionStore is the interface for session storage.
type SessionStore interface {
	// GetSession retrieves a session by ID.
	GetSession(ctx context.Context, id string) (*Session, error)
	// CreateSession creates a new session.
	CreateSession(ctx context.Context, session *Session) error
	// UpdateSession updates an existing session.
	UpdateSession(ctx context.Context, session *Session) error
	// DeleteSession deletes a session.
	DeleteSession(ctx context.Context, id string) error
	// GetUserSessions retrieves all sessions for a user.
	GetUserSessions(ctx context.Context, userID string) ([]*Session, error)
	// DeleteUserSessions deletes all sessions for a user.
	DeleteUserSessions(ctx context.Context, userID string) error
	// CleanupExpiredSessions removes expired sessions.
	CleanupExpiredSessions(ctx context.Context) error
}

// ListOption represents an option for listing users.
type ListOption func(*ListOptions)

// ListOptions contains options for listing users.
type ListOptions struct {
	// Limit is the maximum number of users to return.
	Limit int
	// Offset is the number of users to skip.
	Offset int
	// Provider filters by provider type.
	Provider ProviderType
	// Group filters by group.
	Group string
	// Role filters by role.
	Role string
	// Active filters by active status.
	Active *bool
}

// WithLimit sets the limit.
func WithLimit(limit int) ListOption {
	return func(o *ListOptions) {
		o.Limit = limit
	}
}

// WithOffset sets the offset.
func WithOffset(offset int) ListOption {
	return func(o *ListOptions) {
		o.Offset = offset
	}
}

// WithProvider filters by provider.
func WithProvider(provider ProviderType) ListOption {
	return func(o *ListOptions) {
		o.Provider = provider
	}
}

// WithGroup filters by group.
func WithGroup(group string) ListOption {
	return func(o *ListOptions) {
		o.Group = group
	}
}

// WithRole filters by role.
func WithRole(role string) ListOption {
	return func(o *ListOptions) {
		o.Role = role
	}
}

// WithActive filters by active status.
func WithActive(active bool) ListOption {
	return func(o *ListOptions) {
		o.Active = &active
	}
}

// SSOManager is the interface for managing SSO providers and users.
type SSOManager interface {
	// GetProvider returns the SSO provider for the given type.
	GetProvider(providerType ProviderType) (SSOProvider, error)
	// GetAuthURL returns the authentication URL for the given provider.
	GetAuthURL(providerType ProviderType, state string, opts ...AuthOption) (string, error)
	// HandleCallback handles the SSO callback and returns a user.
	HandleCallback(ctx context.Context, providerType ProviderType, code string) (*User, *Session, error)
	// ValidateSession validates a session and returns the user.
	ValidateSession(ctx context.Context, sessionID string) (*User, error)
	// Logout logs out a user by invalidating their session.
	Logout(ctx context.Context, sessionID string) error
	// GetUser returns a user by ID.
	GetUser(ctx context.Context, userID string) (*User, error)
	// ListUsers lists all users.
	ListUsers(ctx context.Context, opts ...ListOption) ([]*User, error)
	// UpdateUser updates a user.
	UpdateUser(ctx context.Context, user *User) error
	// DeleteUser deletes a user.
	DeleteUser(ctx context.Context, userID string) error
	// SyncGroups synchronizes group memberships from the SSO provider.
	SyncGroups(ctx context.Context, providerType ProviderType) error
	// GetProviders returns all configured SSO providers.
	GetProviders() []SSOProvider
}
