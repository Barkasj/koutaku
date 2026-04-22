package sso

import (
	"context"
	"log"
	"os"
	"testing"
	"time"
)

func TestNewSSOManager(t *testing.T) {
	config := SSOConfig{
		Enabled: true,
		Providers: []ProviderConfig{
			{
				Type:         ProviderTypeGoogle,
				Name:         "Google",
				ClientID:     "test-client-id",
				ClientSecret: "test-client-secret",
				Issuer:       "https://accounts.google.com",
				RedirectURI:  "http://localhost:8080/callback",
				Enabled:      true,
			},
		},
		AutoCreateUsers: true,
		SessionDuration: 24 * time.Hour,
	}

	userStore := NewInMemoryUserStore()
	sessionStore := NewInMemorySessionStore()
	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	manager, err := NewSSOManager(config, userStore, sessionStore, logger)
	if err != nil {
		t.Fatalf("Failed to create SSO manager: %v", err)
	}

	if manager == nil {
		t.Fatal("Expected non-nil manager")
	}

	providers := manager.GetProviders()
	if len(providers) != 1 {
		t.Errorf("Expected 1 provider, got %d", len(providers))
	}
}

func TestSSOManagerGetProvider(t *testing.T) {
	config := SSOConfig{
		Enabled: true,
		Providers: []ProviderConfig{
			{
				Type:         ProviderTypeGoogle,
				Name:         "Google",
				ClientID:     "test-client-id",
				ClientSecret: "test-client-secret",
				Issuer:       "https://accounts.google.com",
				RedirectURI:  "http://localhost:8080/callback",
				Enabled:      true,
			},
		},
	}

	userStore := NewInMemoryUserStore()
	sessionStore := NewInMemorySessionStore()
	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	manager, err := NewSSOManager(config, userStore, sessionStore, logger)
	if err != nil {
		t.Fatalf("Failed to create SSO manager: %v", err)
	}

	// Get existing provider.
	provider, err := manager.GetProvider(ProviderTypeGoogle)
	if err != nil {
		t.Fatalf("Failed to get provider: %v", err)
	}

	if provider.GetType() != ProviderTypeGoogle {
		t.Errorf("Expected provider type 'google', got '%s'", provider.GetType())
	}

	// Get non-existing provider.
	_, err = manager.GetProvider(ProviderTypeOkta)
	if err == nil {
		t.Error("Expected error for non-existing provider")
	}
}

func TestSSOManagerGetAuthURL(t *testing.T) {
	config := SSOConfig{
		Enabled: true,
		Providers: []ProviderConfig{
			{
				Type:         ProviderTypeGoogle,
				Name:         "Google",
				ClientID:     "test-client-id",
				ClientSecret: "test-client-secret",
				Issuer:       "https://accounts.google.com",
				RedirectURI:  "http://localhost:8080/callback",
				Enabled:      true,
			},
		},
	}

	userStore := NewInMemoryUserStore()
	sessionStore := NewInMemorySessionStore()
	logger := log.New(os.Stdout, "[Test] ", log.LstdFlags)

	manager, err := NewSSOManager(config, userStore, sessionStore, logger)
	if err != nil {
		t.Fatalf("Failed to create SSO manager: %v", err)
	}

	authURL, err := manager.GetAuthURL(ProviderTypeGoogle, "test-state")
	if err != nil {
		t.Fatalf("Failed to get auth URL: %v", err)
	}

	if authURL == "" {
		t.Error("Expected non-empty auth URL")
	}

	// Check that the URL contains the client ID.
	if !contains(authURL, "test-client-id") {
		t.Error("Expected auth URL to contain client ID")
	}

	// Check that the URL contains the state.
	if !contains(authURL, "test-state") {
		t.Error("Expected auth URL to contain state")
	}
}

func TestInMemoryUserStore(t *testing.T) {
	store := NewInMemoryUserStore()
	ctx := context.Background()

	// Create user.
	user := &User{
		ID:             "user-1",
		Email:          "test@example.com",
		Name:           "Test User",
		Provider:       ProviderTypeGoogle,
		ProviderUserID: "google-123",
		Active:         true,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}

	if err := store.CreateUser(ctx, user); err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	// Get user by ID.
	fetchedUser, err := store.GetUserByID(ctx, "user-1")
	if err != nil {
		t.Fatalf("Failed to get user by ID: %v", err)
	}

	if fetchedUser.Email != "test@example.com" {
		t.Errorf("Expected email 'test@example.com', got '%s'", fetchedUser.Email)
	}

	// Get user by email.
	fetchedUser, err = store.GetUserByEmail(ctx, "test@example.com")
	if err != nil {
		t.Fatalf("Failed to get user by email: %v", err)
	}

	if fetchedUser.ID != "user-1" {
		t.Errorf("Expected ID 'user-1', got '%s'", fetchedUser.ID)
	}

	// Get user by provider ID.
	fetchedUser, err = store.GetUserByProviderID(ctx, ProviderTypeGoogle, "google-123")
	if err != nil {
		t.Fatalf("Failed to get user by provider ID: %v", err)
	}

	if fetchedUser.ID != "user-1" {
		t.Errorf("Expected ID 'user-1', got '%s'", fetchedUser.ID)
	}

	// Update user.
	user.Name = "Updated User"
	if err := store.UpdateUser(ctx, user); err != nil {
		t.Fatalf("Failed to update user: %v", err)
	}

	fetchedUser, err = store.GetUserByID(ctx, "user-1")
	if err != nil {
		t.Fatalf("Failed to get updated user: %v", err)
	}

	if fetchedUser.Name != "Updated User" {
		t.Errorf("Expected name 'Updated User', got '%s'", fetchedUser.Name)
	}

	// List users.
	users, err := store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("Failed to list users: %v", err)
	}

	if len(users) != 1 {
		t.Errorf("Expected 1 user, got %d", len(users))
	}

	// Delete user.
	if err := store.DeleteUser(ctx, "user-1"); err != nil {
		t.Fatalf("Failed to delete user: %v", err)
	}

	_, err = store.GetUserByID(ctx, "user-1")
	if err == nil {
		t.Error("Expected error for deleted user")
	}
}

func TestInMemorySessionStore(t *testing.T) {
	store := NewInMemorySessionStore()
	ctx := context.Background()

	// Create session.
	session := &Session{
		ID:             "session-1",
		UserID:         "user-1",
		Provider:       ProviderTypeGoogle,
		AccessToken:    "access-token",
		RefreshToken:   "refresh-token",
		ExpiresAt:      time.Now().Add(1 * time.Hour),
		CreatedAt:      time.Now(),
		LastAccessedAt: time.Now(),
	}

	if err := store.CreateSession(ctx, session); err != nil {
		t.Fatalf("Failed to create session: %v", err)
	}

	// Get session.
	fetchedSession, err := store.GetSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("Failed to get session: %v", err)
	}

	if fetchedSession.UserID != "user-1" {
		t.Errorf("Expected user ID 'user-1', got '%s'", fetchedSession.UserID)
	}

	// Update session.
	session.AccessToken = "new-access-token"
	if err := store.UpdateSession(ctx, session); err != nil {
		t.Fatalf("Failed to update session: %v", err)
	}

	fetchedSession, err = store.GetSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("Failed to get updated session: %v", err)
	}

	if fetchedSession.AccessToken != "new-access-token" {
		t.Errorf("Expected access token 'new-access-token', got '%s'", fetchedSession.AccessToken)
	}

	// Get user sessions.
	sessions, err := store.GetUserSessions(ctx, "user-1")
	if err != nil {
		t.Fatalf("Failed to get user sessions: %v", err)
	}

	if len(sessions) != 1 {
		t.Errorf("Expected 1 session, got %d", len(sessions))
	}

	// Delete session.
	if err := store.DeleteSession(ctx, "session-1"); err != nil {
		t.Fatalf("Failed to delete session: %v", err)
	}

	_, err = store.GetSession(ctx, "session-1")
	if err == nil {
		t.Error("Expected error for deleted session")
	}
}

func TestInMemorySessionStoreCleanup(t *testing.T) {
	store := NewInMemorySessionStore()
	ctx := context.Background()

	// Create expired session.
	expiredSession := &Session{
		ID:          "expired-session",
		UserID:      "user-1",
		Provider:    ProviderTypeGoogle,
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Add(-1 * time.Hour), // Expired 1 hour ago.
		CreatedAt:   time.Now().Add(-2 * time.Hour),
	}

	if err := store.CreateSession(ctx, expiredSession); err != nil {
		t.Fatalf("Failed to create expired session: %v", err)
	}

	// Create valid session.
	validSession := &Session{
		ID:          "valid-session",
		UserID:      "user-1",
		Provider:    ProviderTypeGoogle,
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
		CreatedAt:   time.Now(),
	}

	if err := store.CreateSession(ctx, validSession); err != nil {
		t.Fatalf("Failed to create valid session: %v", err)
	}

	// Cleanup expired sessions.
	if err := store.CleanupExpiredSessions(ctx); err != nil {
		t.Fatalf("Failed to cleanup expired sessions: %v", err)
	}

	// Check that expired session is gone.
	_, err := store.GetSession(ctx, "expired-session")
	if err == nil {
		t.Error("Expected error for expired session")
	}

	// Check that valid session still exists.
	_, err = store.GetSession(ctx, "valid-session")
	if err != nil {
		t.Fatalf("Failed to get valid session: %v", err)
	}
}

func TestProviderTypes(t *testing.T) {
	tests := []struct {
		providerType ProviderType
		expected     string
	}{
		{ProviderTypeEntraID, "entra_id"},
		{ProviderTypeGoogle, "google"},
		{ProviderTypeKeycloak, "keycloak"},
		{ProviderTypeOkta, "okta"},
		{ProviderTypeZitadel, "zitadel"},
	}

	for _, tt := range tests {
		if string(tt.providerType) != tt.expected {
			t.Errorf("Expected provider type '%s', got '%s'", tt.expected, string(tt.providerType))
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && (s[0:len(substr)] == substr || contains(s[1:], substr)))
}
