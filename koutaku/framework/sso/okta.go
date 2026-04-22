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

// OktaProvider implements SSOProvider for Okta.
type OktaProvider struct {
	config     ProviderConfig
	httpClient *http.Client
}

// NewOktaProvider creates a new Okta SSO provider.
func NewOktaProvider(config ProviderConfig) *OktaProvider {
	return &OktaProvider{
		config: config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// GetType returns the provider type.
func (p *OktaProvider) GetType() ProviderType {
	return ProviderTypeOkta
}

// GetName returns the provider name.
func (p *OktaProvider) GetName() string {
	return p.config.Name
}

// GetDomain returns the Okta domain.
func (p *OktaProvider) GetDomain() string {
	if p.config.Okta != nil && p.config.Okta.Domain != "" {
		return p.config.Okta.Domain
	}
	return p.config.Issuer
}

// GetAuthServer returns the authorization server ID.
func (p *OktaProvider) GetAuthServer() string {
	if p.config.Okta != nil && p.config.Okta.AuthorizationServer != "" {
		return p.config.Okta.AuthorizationServer
	}
	return "default"
}

// GetBaseURL returns the Okta base URL.
func (p *OktaProvider) GetBaseURL() string {
	return fmt.Sprintf("https://%s", p.GetDomain())
}

// GetAuthURL returns the URL to redirect users for authentication.
func (p *OktaProvider) GetAuthURL(state string, opts ...AuthOption) (string, error) {
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

	authServer := p.GetAuthServer()
	authURL := fmt.Sprintf("%s/oauth2/%s/v1/authorize?%s", p.GetBaseURL(), authServer, params.Encode())
	return authURL, nil
}

// Exchange exchanges an authorization code for tokens.
func (p *OktaProvider) Exchange(ctx context.Context, code string) (*TokenResponse, error) {
	authServer := p.GetAuthServer()
	tokenURL := fmt.Sprintf("%s/oauth2/%s/v1/token", p.GetBaseURL(), authServer)

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
func (p *OktaProvider) GetUserInfo(ctx context.Context, accessToken string) (*User, error) {
	authServer := p.GetAuthServer()
	userInfoURL := fmt.Sprintf("%s/oauth2/%s/v1/userinfo", p.GetBaseURL(), authServer)

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

	var oktaUser struct {
		Sub        string `json:"sub"`
		Name       string `json:"name"`
		Email      string `json:"email"`
		GivenName  string `json:"given_name"`
		FamilyName string `json:"family_name"`
		PreferredUsername string `json:"preferred_username"`
		ZoneInfo   string `json:"zone_info"`
		Locale     string `json:"locale"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&oktaUser); err != nil {
		return nil, fmt.Errorf("failed to decode user info: %w", err)
	}

	user := &User{
		ID:             oktaUser.Sub,
		Email:          oktaUser.Email,
		Name:           oktaUser.Name,
		FirstName:      oktaUser.GivenName,
		LastName:       oktaUser.FamilyName,
		Provider:       ProviderTypeOkta,
		ProviderUserID: oktaUser.Sub,
		Metadata: map[string]string{
			"preferred_username": oktaUser.PreferredUsername,
			"zone_info":          oktaUser.ZoneInfo,
			"locale":             oktaUser.Locale,
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Active:    true,
	}

	return user, nil
}

// RefreshToken refreshes an access token using a refresh token.
func (p *OktaProvider) RefreshToken(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	authServer := p.GetAuthServer()
	tokenURL := fmt.Sprintf("%s/oauth2/%s/v1/token", p.GetBaseURL(), authServer)

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
func (p *OktaProvider) RevokeToken(ctx context.Context, token string) error {
	authServer := p.GetAuthServer()
	revokeURL := fmt.Sprintf("%s/oauth2/%s/v1/revoke", p.GetBaseURL(), authServer)

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
func (p *OktaProvider) ValidateToken(ctx context.Context, token string) (*User, error) {
	// Use Okta's introspection endpoint.
	authServer := p.GetAuthServer()
	introspectURL := fmt.Sprintf("%s/oauth2/%s/v1/introspect", p.GetBaseURL(), authServer)

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
		Provider:       ProviderTypeOkta,
		ProviderUserID: introspectResult.Sub,
		Active:         true,
	}

	return user, nil
}

// GetJWKS returns the JSON Web Key Set for token validation.
func (p *OktaProvider) GetJWKS(ctx context.Context) (interface{}, error) {
	authServer := p.GetAuthServer()
	jwksURL := fmt.Sprintf("%s/oauth2/%s/v1/keys", p.GetBaseURL(), authServer)

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
