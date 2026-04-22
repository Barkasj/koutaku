package sso

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// KeycloakProvider implements SSOProvider for Keycloak.
type KeycloakProvider struct {
	config     ProviderConfig
	httpClient *http.Client
}

// NewKeycloakProvider creates a new Keycloak SSO provider.
func NewKeycloakProvider(config ProviderConfig) *KeycloakProvider {
	return &KeycloakProvider{
		config: config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// GetType returns the provider type.
func (p *KeycloakProvider) GetType() ProviderType {
	return ProviderTypeKeycloak
}

// GetName returns the provider name.
func (p *KeycloakProvider) GetName() string {
	return p.config.Name
}

// GetBaseURL returns the Keycloak base URL.
func (p *KeycloakProvider) GetBaseURL() string {
	if p.config.Keycloak != nil && p.config.Keycloak.BaseURL != "" {
		return p.config.Keycloak.BaseURL
	}
	return p.config.Issuer
}

// GetRealm returns the Keycloak realm.
func (p *KeycloakProvider) GetRealm() string {
	if p.config.Keycloak != nil && p.config.Keycloak.Realm != "" {
		return p.config.Keycloak.Realm
	}
	return "master"
}

// GetRealmURL returns the realm-specific URL.
func (p *KeycloakProvider) GetRealmURL() string {
	return fmt.Sprintf("%s/realms/%s", p.GetBaseURL(), p.GetRealm())
}

// GetAuthURL returns the URL to redirect users for authentication.
func (p *KeycloakProvider) GetAuthURL(state string, opts ...AuthOption) (string, error) {
	authOpts := &AuthOptions{}
	for _, opt := range opts {
		opt(authOpts)
	}

	scopes := []string{"openid", "profile", "email"}
	scopes = append(scopes, authOpts.Scopes...)
	scopes = append(scopes, p.config.Scopes...)

	params := url.Values{}
	params.Set("client_id", p.config.ClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", p.config.RedirectURI)
	params.Set("scope", strings.Join(scopes, " "))
	params.Set("state", state)

	if authOpts.Prompt != "" {
		params.Set("prompt", authOpts.Prompt)
	}

	if authOpts.LoginHint != "" {
		params.Set("login_hint", authOpts.LoginHint)
	}

	authURL := fmt.Sprintf("%s/protocol/openid-connect/auth?%s", p.GetRealmURL(), params.Encode())
	return authURL, nil
}

// Exchange exchanges an authorization code for tokens.
func (p *KeycloakProvider) Exchange(ctx context.Context, code string) (*TokenResponse, error) {
	tokenURL := fmt.Sprintf("%s/protocol/openid-connect/token", p.GetRealmURL())

	data := url.Values{}
	data.Set("client_id", p.config.ClientID)
	data.Set("client_secret", p.config.ClientSecret)
	data.Set("code", code)
	data.Set("redirect_uri", p.config.RedirectURI)
	data.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange code: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	return &tokenResp, nil
}

// GetUserInfo retrieves user information using an access token.
func (p *KeycloakProvider) GetUserInfo(ctx context.Context, accessToken string) (*User, error) {
	userInfoURL := fmt.Sprintf("%s/protocol/openid-connect/userinfo", p.GetRealmURL())

	req, err := http.NewRequestWithContext(ctx, "GET", userInfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get user info failed with status %d: %s", resp.StatusCode, string(body))
	}

	var kcUser struct {
		Sub               string   `json:"sub"`
		Email             string   `json:"email"`
		EmailVerified     bool     `json:"email_verified"`
		Name              string   `json:"name"`
		PreferredUsername string   `json:"preferred_username"`
		GivenName         string   `json:"given_name"`
		FamilyName        string   `json:"family_name"`
		Picture           string   `json:"picture"`
		Groups            []string `json:"groups"`
		RealmAccess       struct {
			Roles []string `json:"roles"`
		} `json:"realm_access"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&kcUser); err != nil {
		return nil, fmt.Errorf("failed to decode user info: %w", err)
	}

	user := &User{
		ID:             kcUser.Sub,
		Email:          kcUser.Email,
		Name:           kcUser.Name,
		FirstName:      kcUser.GivenName,
		LastName:       kcUser.FamilyName,
		Picture:        kcUser.Picture,
		Provider:       ProviderTypeKeycloak,
		ProviderUserID: kcUser.Sub,
		Groups:         kcUser.Groups,
		Roles:          kcUser.RealmAccess.Roles,
		Metadata: map[string]string{
			"preferred_username": kcUser.PreferredUsername,
			"email_verified":     fmt.Sprintf("%t", kcUser.EmailVerified),
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Active:    true,
	}

	return user, nil
}

// RefreshToken refreshes an access token using a refresh token.
func (p *KeycloakProvider) RefreshToken(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	tokenURL := fmt.Sprintf("%s/protocol/openid-connect/token", p.GetRealmURL())

	data := url.Values{}
	data.Set("client_id", p.config.ClientID)
	data.Set("client_secret", p.config.ClientSecret)
	data.Set("refresh_token", refreshToken)
	data.Set("grant_type", "refresh_token")

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to refresh token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token refresh failed with status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	return &tokenResp, nil
}

// RevokeToken revokes an access or refresh token.
func (p *KeycloakProvider) RevokeToken(ctx context.Context, token string) error {
	revokeURL := fmt.Sprintf("%s/protocol/openid-connect/revoke", p.GetRealmURL())

	data := url.Values{}
	data.Set("client_id", p.config.ClientID)
	data.Set("client_secret", p.config.ClientSecret)
	data.Set("token", token)

	req, err := http.NewRequestWithContext(ctx, "POST", revokeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to revoke token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("token revocation failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// ValidateToken validates an access token.
func (p *KeycloakProvider) ValidateToken(ctx context.Context, token string) (*User, error) {
	// Use Keycloak's token introspection endpoint.
	introspectURL := fmt.Sprintf("%s/protocol/openid-connect/token/introspect", p.GetRealmURL())

	data := url.Values{}
	data.Set("client_id", p.config.ClientID)
	data.Set("client_secret", p.config.ClientSecret)
	data.Set("token", token)

	req, err := http.NewRequestWithContext(ctx, "POST", introspectURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to validate token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token validation failed with status %d: %s", resp.StatusCode, string(body))
	}

	var introspectResult struct {
		Active    bool   `json:"active"`
		Sub       string `json:"sub"`
		Email     string `json:"email"`
		Name      string `json:"name"`
		Username  string `json:"username"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&introspectResult); err != nil {
		return nil, fmt.Errorf("failed to decode introspection result: %w", err)
	}

	if !introspectResult.Active {
		return nil, fmt.Errorf("token is not active")
	}

	user := &User{
		ID:             introspectResult.Sub,
		Email:          introspectResult.Email,
		Name:           introspectResult.Name,
		Provider:       ProviderTypeKeycloak,
		ProviderUserID: introspectResult.Sub,
		Active:         true,
	}

	return user, nil
}

// GetJWKS returns the JSON Web Key Set for token validation.
func (p *KeycloakProvider) GetJWKS(ctx context.Context) (interface{}, error) {
	jwksURL := fmt.Sprintf("%s/protocol/openid-connect/certs", p.GetRealmURL())

	req, err := http.NewRequestWithContext(ctx, "GET", jwksURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get JWKS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get JWKS failed with status %d: %s", resp.StatusCode, string(body))
	}

	var jwks interface{}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return nil, fmt.Errorf("failed to decode JWKS: %w", err)
	}

	return jwks, nil
}
