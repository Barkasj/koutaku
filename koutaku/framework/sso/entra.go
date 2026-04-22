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

// EntraIDProvider implements SSOProvider for Microsoft Entra ID (Azure AD).
type EntraIDProvider struct {
	config     ProviderConfig
	httpClient *http.Client
}

// NewEntraIDProvider creates a new Entra ID SSO provider.
func NewEntraIDProvider(config ProviderConfig) *EntraIDProvider {
	return &EntraIDProvider{
		config: config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// GetType returns the provider type.
func (p *EntraIDProvider) GetType() ProviderType {
	return ProviderTypeEntraID
}

// GetName returns the provider name.
func (p *EntraIDProvider) GetName() string {
	return p.config.Name
}

// GetAuthority returns the authority URL.
func (p *EntraIDProvider) GetAuthority() string {
	if p.config.EntraID != nil && p.config.EntraID.Authority != "" {
		return p.config.EntraID.Authority
	}

	tenantID := "common"
	if p.config.EntraID != nil && p.config.EntraID.TenantID != "" {
		tenantID = p.config.EntraID.TenantID
	}

	return fmt.Sprintf("https://login.microsoftonline.com/%s", tenantID)
}

// GetAuthURL returns the URL to redirect users for authentication.
func (p *EntraIDProvider) GetAuthURL(state string, opts ...AuthOption) (string, error) {
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
	params.Set("response_mode", "query")

	if authOpts.Prompt != "" {
		params.Set("prompt", authOpts.Prompt)
	}

	if authOpts.LoginHint != "" {
		params.Set("login_hint", authOpts.LoginHint)
	}

	authURL := fmt.Sprintf("%s/oauth2/v2.0/authorize?%s", p.GetAuthority(), params.Encode())
	return authURL, nil
}

// Exchange exchanges an authorization code for tokens.
func (p *EntraIDProvider) Exchange(ctx context.Context, code string) (*TokenResponse, error) {
	tokenURL := fmt.Sprintf("%s/oauth2/v2.0/token", p.GetAuthority())

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
func (p *EntraIDProvider) GetUserInfo(ctx context.Context, accessToken string) (*User, error) {
	// Microsoft Graph API endpoint for user info.
	userInfoURL := "https://graph.microsoft.com/v1.0/me"

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

	var graphUser struct {
		ID                string `json:"id"`
		DisplayName       string `json:"displayName"`
		GivenName         string `json:"givenName"`
		Surname           string `json:"surname"`
		Mail              string `json:"mail"`
		UserPrincipalName string `json:"userPrincipalName"`
		JobTitle          string `json:"jobTitle"`
		Department        string `json:"department"`
		OfficeLocation    string `json:"officeLocation"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&graphUser); err != nil {
		return nil, fmt.Errorf("failed to decode user info: %w", err)
	}

	email := graphUser.Mail
	if email == "" {
		email = graphUser.UserPrincipalName
	}

	user := &User{
		ID:             graphUser.ID,
		Email:          email,
		Name:           graphUser.DisplayName,
		FirstName:      graphUser.GivenName,
		LastName:       graphUser.Surname,
		Provider:       ProviderTypeEntraID,
		ProviderUserID: graphUser.ID,
		Metadata: map[string]string{
			"job_title":       graphUser.JobTitle,
			"department":      graphUser.Department,
			"office_location": graphUser.OfficeLocation,
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Active:    true,
	}

	return user, nil
}

// RefreshToken refreshes an access token using a refresh token.
func (p *EntraIDProvider) RefreshToken(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	tokenURL := fmt.Sprintf("%s/oauth2/v2.0/token", p.GetAuthority())

	data := url.Values{}
	data.Set("client_id", p.config.ClientID)
	data.Set("client_secret", p.config.ClientSecret)
	data.Set("refresh_token", refreshToken)
	data.Set("grant_type", "refresh_token")
	data.Set("scope", strings.Join(append([]string{"openid", "profile", "email"}, p.config.Scopes...), " "))

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
func (p *EntraIDProvider) RevokeToken(ctx context.Context, token string) error {
	// Entra ID doesn't have a standard revocation endpoint.
	// Sessions are managed via Microsoft Graph API.
	// For now, we'll return nil as token revocation is handled by session management.
	return nil
}

// ValidateToken validates an access token.
func (p *EntraIDProvider) ValidateToken(ctx context.Context, token string) (*User, error) {
	// Use Microsoft Graph API to validate the token and get user info.
	return p.GetUserInfo(ctx, token)
}

// GetJWKS returns the JSON Web Key Set for token validation.
func (p *EntraIDProvider) GetJWKS(ctx context.Context) (interface{}, error) {
	jwksURL := fmt.Sprintf("%s/discovery/v2.0/keys", p.GetAuthority())

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

// GetUserGroups retrieves the user's group memberships from Entra ID.
func (p *EntraIDProvider) GetUserGroups(ctx context.Context, accessToken string) ([]string, error) {
	groupsURL := "https://graph.microsoft.com/v1.0/me/transitiveMemberOf/microsoft.graph.group?$select=id,displayName"

	req, err := http.NewRequestWithContext(ctx, "GET", groupsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get user groups: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get user groups failed with status %d: %s", resp.StatusCode, string(body))
	}

	var groupsResp struct {
		Value []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&groupsResp); err != nil {
		return nil, fmt.Errorf("failed to decode groups: %w", err)
	}

	groups := make([]string, len(groupsResp.Value))
	for i, group := range groupsResp.Value {
		groups[i] = group.DisplayName
	}

	return groups, nil
}
