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

// ZitadelProvider implements SSOProvider for Zitadel.
type ZitadelProvider struct {
	config     ProviderConfig
	httpClient *http.Client
}

// NewZitadelProvider creates a new Zitadel SSO provider.
func NewZitadelProvider(config ProviderConfig) *ZitadelProvider {
	return &ZitadelProvider{
		config: config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// GetType returns the provider type.
func (p *ZitadelProvider) GetType() ProviderType {
	return ProviderTypeZitadel
}

// GetName returns the provider name.
func (p *ZitadelProvider) GetName() string {
	return p.config.Name
}

// GetInstanceURL returns the Zitadel instance URL.
func (p *ZitadelProvider) GetInstanceURL() string {
	if p.config.Zitadel != nil && p.config.Zitadel.InstanceURL != "" {
		return p.config.Zitadel.InstanceURL
	}
	return p.config.Issuer
}

// GetAuthURL returns the URL to redirect users for authentication.
func (p *ZitadelProvider) GetAuthURL(state string, opts ...AuthOption) (string, error) {
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

	authURL := fmt.Sprintf("%s/oauth/v2/authorize?%s", p.GetInstanceURL(), params.Encode())
	return authURL, nil
}

// Exchange exchanges an authorization code for tokens.
func (p *ZitadelProvider) Exchange(ctx context.Context, code string) (*TokenResponse, error) {
	tokenURL := fmt.Sprintf("%s/oauth/v2/token", p.GetInstanceURL())

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
func (p *ZitadelProvider) GetUserInfo(ctx context.Context, accessToken string) (*User, error) {
	userInfoURL := fmt.Sprintf("%s/oidc/v1/userinfo", p.GetInstanceURL())

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

	var zitadelUser struct {
		Sub               string `json:"sub"`
		Name              string `json:"name"`
		GivenName         string `json:"given_name"`
		FamilyName        string `json:"family_name`
		Email             string `json:"email"`
		EmailVerified     bool   `json:"email_verified"`
		PreferredUsername string `json:"preferred_username"`
		Locale            string `json:"locale"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&zitadelUser); err != nil {
		return nil, fmt.Errorf("failed to decode user info: %w", err)
	}

	user := &User{
		ID:             zitadelUser.Sub,
		Email:          zitadelUser.Email,
		Name:           zitadelUser.Name,
		FirstName:      zitadelUser.GivenName,
		LastName:       zitadelUser.FamilyName,
		Provider:       ProviderTypeZitadel,
		ProviderUserID: zitadelUser.Sub,
		Metadata: map[string]string{
			"preferred_username": zitadelUser.PreferredUsername,
			"email_verified":     fmt.Sprintf("%t", zitadelUser.EmailVerified),
			"locale":             zitadelUser.Locale,
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Active:    true,
	}

	return user, nil
}

// RefreshToken refreshes an access token using a refresh token.
func (p *ZitadelProvider) RefreshToken(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	tokenURL := fmt.Sprintf("%s/oauth/v2/token", p.GetInstanceURL())

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
func (p *ZitadelProvider) RevokeToken(ctx context.Context, token string) error {
	revokeURL := fmt.Sprintf("%s/oauth/v2/revoke", p.GetInstanceURL())

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
func (p *ZitadelProvider) ValidateToken(ctx context.Context, token string) (*User, error) {
	// Use Zitadel's token introspection endpoint.
	introspectURL := fmt.Sprintf("%s/oauth/v2/introspect", p.GetInstanceURL())

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
		Active   bool   `json:"active"`
		Sub      string `json:"sub"`
		Email    string `json:"email"`
		Name     string `json:"name"`
		Username string `json:"username"`
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
		Provider:       ProviderTypeZitadel,
		ProviderUserID: introspectResult.Sub,
		Active:         true,
	}

	return user, nil
}

// GetJWKS returns the JSON Web Key Set for token validation.
func (p *ZitadelProvider) GetJWKS(ctx context.Context) (interface{}, error) {
	jwksURL := fmt.Sprintf("%s/oauth/v2/keys", p.GetInstanceURL())

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
