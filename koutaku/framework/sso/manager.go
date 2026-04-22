package sso

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultSSOManager implements the SSOManager interface.
type DefaultSSOManager struct {
	mu          sync.RWMutex
	config      SSOConfig
	providers   map[ProviderType]SSOProvider
	userStore   UserStore
	sessionStore SessionStore
	logger      *log.Logger
}

// NewSSOManager creates a new SSO manager with the given configuration.
func NewSSOManager(config SSOConfig, userStore UserStore, sessionStore SessionStore, logger *log.Logger) (*DefaultSSOManager, error) {
	if logger == nil {
		logger = log.Default()
	}

	if userStore == nil {
		return nil, fmt.Errorf("user store is required")
	}

	if sessionStore == nil {
		return nil, fmt.Errorf("session store is required")
	}

	manager := &DefaultSSOManager{
		config:       config,
		providers:    make(map[ProviderType]SSOProvider),
		userStore:    userStore,
		sessionStore: sessionStore,
		logger:       logger,
	}

	// Initialize providers.
	for _, providerConfig := range config.Providers {
		if !providerConfig.Enabled {
			continue
		}

		provider, err := createProvider(providerConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create provider %s: %w", providerConfig.Name, err)
		}

		manager.providers[provider.GetType()] = provider
		manager.logger.Printf("[SSO] Initialized provider: %s (%s)", provider.GetName(), provider.GetType())
	}

	return manager, nil
}

// createProvider creates an SSO provider based on the configuration.
func createProvider(config ProviderConfig) (SSOProvider, error) {
	switch config.Type {
	case ProviderTypeEntraID:
		return NewEntraIDProvider(config), nil
	case ProviderTypeGoogle:
		return NewGoogleProvider(config), nil
	case ProviderTypeKeycloak:
		return NewKeycloakProvider(config), nil
	case ProviderTypeOkta:
		return NewOktaProvider(config), nil
	case ProviderTypeZitadel:
		return NewZitadelProvider(config), nil
	default:
		return nil, fmt.Errorf("unsupported provider type: %s", config.Type)
	}
}

// GetProvider returns the SSO provider for the given type.
func (m *DefaultSSOManager) GetProvider(providerType ProviderType) (SSOProvider, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	provider, ok := m.providers[providerType]
	if !ok {
		return nil, fmt.Errorf("provider %s not configured", providerType)
	}

	return provider, nil
}

// GetAuthURL returns the authentication URL for the given provider.
func (m *DefaultSSOManager) GetAuthURL(providerType ProviderType, state string, opts ...AuthOption) (string, error) {
	provider, err := m.GetProvider(providerType)
	if err != nil {
		return "", err
	}

	return provider.GetAuthURL(state, opts...)
}

// HandleCallback handles the SSO callback and returns a user.
func (m *DefaultSSOManager) HandleCallback(ctx context.Context, providerType ProviderType, code string) (*User, *Session, error) {
	provider, err := m.GetProvider(providerType)
	if err != nil {
		return nil, nil, err
	}

	// Exchange code for tokens.
	tokenResp, err := provider.Exchange(ctx, code)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to exchange code: %w", err)
	}

	// Get user info.
	user, err := provider.GetUserInfo(ctx, tokenResp.AccessToken)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get user info: %w", err)
	}

	// Check if user exists.
	existingUser, err := m.userStore.GetUserByProviderID(ctx, providerType, user.ProviderUserID)
	if err != nil {
		// User doesn't exist, create if auto-create is enabled.
		if m.config.AutoCreateUsers {
			if err := m.userStore.CreateUser(ctx, user); err != nil {
				return nil, nil, fmt.Errorf("failed to create user: %w", err)
			}
			m.logger.Printf("[SSO] Created new user: %s (%s)", user.Email, providerType)
		} else {
			return nil, nil, fmt.Errorf("user not found and auto-create is disabled")
		}
	} else {
		// Update existing user.
		existingUser.Name = user.Name
		existingUser.FirstName = user.FirstName
		existingUser.LastName = user.LastName
		existingUser.Picture = user.Picture
		existingUser.Groups = user.Groups
		existingUser.Roles = user.Roles
		existingUser.Metadata = user.Metadata
		existingUser.UpdatedAt = time.Now()
		existingUser.LastLoginAt = time.Now()

		if err := m.userStore.UpdateUser(ctx, existingUser); err != nil {
			return nil, nil, fmt.Errorf("failed to update user: %w", err)
		}

		user = existingUser
		m.logger.Printf("[SSO] Updated user: %s (%s)", user.Email, providerType)
	}

	// Create session.
	sessionDuration := m.config.SessionDuration
	if sessionDuration == 0 {
		sessionDuration = 24 * time.Hour
	}

	session := &Session{
		ID:             uuid.New().String(),
		UserID:         user.ID,
		Provider:       providerType,
		AccessToken:    tokenResp.AccessToken,
		RefreshToken:   tokenResp.RefreshToken,
		IDToken:        tokenResp.IDToken,
		ExpiresAt:      time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
		CreatedAt:      time.Now(),
		LastAccessedAt: time.Now(),
	}

	if err := m.sessionStore.CreateSession(ctx, session); err != nil {
		return nil, nil, fmt.Errorf("failed to create session: %w", err)
	}

	m.logger.Printf("[SSO] Created session for user: %s", user.Email)

	return user, session, nil
}

// ValidateSession validates a session and returns the user.
func (m *DefaultSSOManager) ValidateSession(ctx context.Context, sessionID string) (*User, error) {
	session, err := m.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session not found: %w", err)
	}

	// Check if session is expired.
	if time.Now().After(session.ExpiresAt) {
		// Try to refresh the token.
		if session.RefreshToken != "" {
			provider, err := m.GetProvider(session.Provider)
			if err == nil {
				tokenResp, err := provider.RefreshToken(ctx, session.RefreshToken)
				if err == nil {
					session.AccessToken = tokenResp.AccessToken
					session.RefreshToken = tokenResp.RefreshToken
					session.ExpiresAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
					session.LastAccessedAt = time.Now()

					if err := m.sessionStore.UpdateSession(ctx, session); err != nil {
						m.logger.Printf("[SSO] Failed to update session: %v", err)
					}
				}
			}
		}

		if time.Now().After(session.ExpiresAt) {
			return nil, fmt.Errorf("session expired")
		}
	}

	// Update last accessed time.
	session.LastAccessedAt = time.Now()
	if err := m.sessionStore.UpdateSession(ctx, session); err != nil {
		m.logger.Printf("[SSO] Failed to update session last accessed: %v", err)
	}

	// Get user.
	user, err := m.userStore.GetUserByID(ctx, session.UserID)
	if err != nil {
		return nil, fmt.Errorf("user not found: %w", err)
	}

	if !user.Active {
		return nil, fmt.Errorf("user account is disabled")
	}

	return user, nil
}

// Logout logs out a user by invalidating their session.
func (m *DefaultSSOManager) Logout(ctx context.Context, sessionID string) error {
	session, err := m.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session not found: %w", err)
	}

	// Revoke tokens at the provider.
	if session.AccessToken != "" {
		provider, err := m.GetProvider(session.Provider)
		if err == nil {
			if err := provider.RevokeToken(ctx, session.AccessToken); err != nil {
				m.logger.Printf("[SSO] Failed to revoke access token: %v", err)
			}
		}
	}

	if session.RefreshToken != "" {
		provider, err := m.GetProvider(session.Provider)
		if err == nil {
			if err := provider.RevokeToken(ctx, session.RefreshToken); err != nil {
				m.logger.Printf("[SSO] Failed to revoke refresh token: %v", err)
			}
		}
	}

	// Delete session.
	if err := m.sessionStore.DeleteSession(ctx, sessionID); err != nil {
		return fmt.Errorf("failed to delete session: %w", err)
	}

	m.logger.Printf("[SSO] Logged out session: %s", sessionID)
	return nil
}

// GetUser returns a user by ID.
func (m *DefaultSSOManager) GetUser(ctx context.Context, userID string) (*User, error) {
	return m.userStore.GetUserByID(ctx, userID)
}

// ListUsers lists all users.
func (m *DefaultSSOManager) ListUsers(ctx context.Context, opts ...ListOption) ([]*User, error) {
	return m.userStore.ListUsers(ctx, opts...)
}

// UpdateUser updates a user.
func (m *DefaultSSOManager) UpdateUser(ctx context.Context, user *User) error {
	return m.userStore.UpdateUser(ctx, user)
}

// DeleteUser deletes a user.
func (m *DefaultSSOManager) DeleteUser(ctx context.Context, userID string) error {
	// Delete all sessions for the user.
	if err := m.sessionStore.DeleteUserSessions(ctx, userID); err != nil {
		m.logger.Printf("[SSO] Failed to delete user sessions: %v", err)
	}

	return m.userStore.DeleteUser(ctx, userID)
}

// SyncGroups synchronizes group memberships from the SSO provider.
func (m *DefaultSSOManager) SyncGroups(ctx context.Context, providerType ProviderType) error {
	provider, err := m.GetProvider(providerType)
	if err != nil {
		return err
	}

	// Get all users from this provider.
	users, err := m.userStore.ListUsers(ctx, WithProvider(providerType))
	if err != nil {
		return fmt.Errorf("failed to list users: %w", err)
	}

	for _, user := range users {
		// Get user's groups from the provider.
		if entraProvider, ok := provider.(*EntraIDProvider); ok {
			groups, err := entraProvider.GetUserGroups(ctx, user.Metadata["access_token"])
			if err != nil {
				m.logger.Printf("[SSO] Failed to get groups for user %s: %v", user.Email, err)
				continue
			}

			user.Groups = groups
			user.UpdatedAt = time.Now()

			if err := m.userStore.UpdateUser(ctx, user); err != nil {
				m.logger.Printf("[SSO] Failed to update user groups %s: %v", user.Email, err)
			}
		}
	}

	m.logger.Printf("[SSO] Synced groups for %d users from %s", len(users), providerType)
	return nil
}

// GetProviders returns all configured SSO providers.
func (m *DefaultSSOManager) GetProviders() []SSOProvider {
	m.mu.RLock()
	defer m.mu.RUnlock()

	providers := make([]SSOProvider, 0, len(m.providers))
	for _, provider := range m.providers {
		providers = append(providers, provider)
	}

	return providers
}

// InMemoryUserStore implements UserStore using in-memory storage.
type InMemoryUserStore struct {
	mu    sync.RWMutex
	users map[string]*User
}

// NewInMemoryUserStore creates a new in-memory user store.
func NewInMemoryUserStore() *InMemoryUserStore {
	return &InMemoryUserStore{
		users: make(map[string]*User),
	}
}

// GetUserByID retrieves a user by ID.
func (s *InMemoryUserStore) GetUserByID(ctx context.Context, id string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	user, ok := s.users[id]
	if !ok {
		return nil, fmt.Errorf("user not found: %s", id)
	}

	return user, nil
}

// GetUserByEmail retrieves a user by email.
func (s *InMemoryUserStore) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, user := range s.users {
		if user.Email == email {
			return user, nil
		}
	}

	return nil, fmt.Errorf("user not found: %s", email)
}

// GetUserByProviderID retrieves a user by provider and provider user ID.
func (s *InMemoryUserStore) GetUserByProviderID(ctx context.Context, provider ProviderType, providerUserID string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, user := range s.users {
		if user.Provider == provider && user.ProviderUserID == providerUserID {
			return user, nil
		}
	}

	return nil, fmt.Errorf("user not found")
}

// CreateUser creates a new user.
func (s *InMemoryUserStore) CreateUser(ctx context.Context, user *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.users[user.ID]; exists {
		return fmt.Errorf("user already exists: %s", user.ID)
	}

	s.users[user.ID] = user
	return nil
}

// UpdateUser updates an existing user.
func (s *InMemoryUserStore) UpdateUser(ctx context.Context, user *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.users[user.ID]; !exists {
		return fmt.Errorf("user not found: %s", user.ID)
	}

	s.users[user.ID] = user
	return nil
}

// DeleteUser deletes a user.
func (s *InMemoryUserStore) DeleteUser(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.users[id]; !exists {
		return fmt.Errorf("user not found: %s", id)
	}

	delete(s.users, id)
	return nil
}

// ListUsers lists all users.
func (s *InMemoryUserStore) ListUsers(ctx context.Context, opts ...ListOption) ([]*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	options := &ListOptions{}
	for _, opt := range opts {
		opt(options)
	}

	users := make([]*User, 0, len(s.users))
	for _, user := range s.users {
		// Apply filters.
		if options.Provider != "" && user.Provider != options.Provider {
			continue
		}
		if options.Group != "" {
			found := false
			for _, group := range user.Groups {
				if group == options.Group {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if options.Role != "" {
			found := false
			for _, role := range user.Roles {
				if role == options.Role {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if options.Active != nil && user.Active != *options.Active {
			continue
		}

		users = append(users, user)
	}

	// Apply pagination.
	if options.Offset > 0 {
		if options.Offset >= len(users) {
			return []*User{}, nil
		}
		users = users[options.Offset:]
	}
	if options.Limit > 0 && options.Limit < len(users) {
		users = users[:options.Limit]
	}

	return users, nil
}

// GetUsersByGroup retrieves users in a group.
func (s *InMemoryUserStore) GetUsersByGroup(ctx context.Context, group string) ([]*User, error) {
	return s.ListUsers(ctx, WithGroup(group))
}

// GetUsersByRole retrieves users with a specific role.
func (s *InMemoryUserStore) GetUsersByRole(ctx context.Context, role string) ([]*User, error) {
	return s.ListUsers(ctx, WithRole(role))
}

// InMemorySessionStore implements SessionStore using in-memory storage.
type InMemorySessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewInMemorySessionStore creates a new in-memory session store.
func NewInMemorySessionStore() *InMemorySessionStore {
	return &InMemorySessionStore{
		sessions: make(map[string]*Session),
	}
}

// GetSession retrieves a session by ID.
func (s *InMemorySessionStore) GetSession(ctx context.Context, id string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session not found: %s", id)
	}

	return session, nil
}

// CreateSession creates a new session.
func (s *InMemorySessionStore) CreateSession(ctx context.Context, session *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.sessions[session.ID]; exists {
		return fmt.Errorf("session already exists: %s", session.ID)
	}

	s.sessions[session.ID] = session
	return nil
}

// UpdateSession updates an existing session.
func (s *InMemorySessionStore) UpdateSession(ctx context.Context, session *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.sessions[session.ID]; !exists {
		return fmt.Errorf("session not found: %s", session.ID)
	}

	s.sessions[session.ID] = session
	return nil
}

// DeleteSession deletes a session.
func (s *InMemorySessionStore) DeleteSession(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.sessions[id]; !exists {
		return fmt.Errorf("session not found: %s", id)
	}

	delete(s.sessions, id)
	return nil
}

// GetUserSessions retrieves all sessions for a user.
func (s *InMemorySessionStore) GetUserSessions(ctx context.Context, userID string) ([]*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sessions := make([]*Session, 0)
	for _, session := range s.sessions {
		if session.UserID == userID {
			sessions = append(sessions, session)
		}
	}

	return sessions, nil
}

// DeleteUserSessions deletes all sessions for a user.
func (s *InMemorySessionStore) DeleteUserSessions(ctx context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, session := range s.sessions {
		if session.UserID == userID {
			delete(s.sessions, id)
		}
	}

	return nil
}

// CleanupExpiredSessions removes expired sessions.
func (s *InMemorySessionStore) CleanupExpiredSessions(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for id, session := range s.sessions {
		if now.After(session.ExpiresAt) {
			delete(s.sessions, id)
		}
	}

	return nil
}
