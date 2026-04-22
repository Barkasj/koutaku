package azure

import (
	"context"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/koutaku/koutaku/core/providers/openai"
	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

// setAzureAuth sets the Azure authentication header on the request for OpenAI models.
// It handles authentication in order of priority:
// 1. Service Principal (client ID/secret/tenant ID) - uses Bearer token
// 2. Context token - uses Bearer token
// 3. API key - uses api-key header
// 4. DefaultAzureCredential auto-detection (managed identity, workload identity, env vars, CLI)
func (provider *AzureProvider) setAzureAuth(ctx context.Context, req *fasthttp.Request, key schemas.Key) *schemas.KoutakuError {
	// Service Principal authentication
	if key.AzureKeyConfig != nil && key.AzureKeyConfig.ClientID != nil &&
		key.AzureKeyConfig.ClientSecret != nil && key.AzureKeyConfig.TenantID != nil && key.AzureKeyConfig.ClientID.GetValue() != "" && key.AzureKeyConfig.ClientSecret.GetValue() != "" && key.AzureKeyConfig.TenantID.GetValue() != "" {
		cred, err := provider.getOrCreateAuth(key.AzureKeyConfig.TenantID.GetValue(), key.AzureKeyConfig.ClientID.GetValue(), key.AzureKeyConfig.ClientSecret.GetValue())
		if err != nil {
			return providerUtils.NewKoutakuOperationError("failed to get or create Azure authentication", err)
		}

		scopes := getAzureScopes(key.AzureKeyConfig.Scopes)

		token, err := cred.GetToken(ctx, policy.TokenRequestOptions{
			Scopes: scopes,
		})
		if err != nil {
			return providerUtils.NewKoutakuOperationError("failed to get Azure access token", err)
		}

		if token.Token == "" {
			return providerUtils.NewKoutakuOperationError("azure access token is empty", fmt.Errorf("token is empty"))
		}

		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token.Token))
		req.Header.Del("api-key")
		return nil
	}

	// Context token authentication
	if authToken, ok := ctx.Value(AzureAuthorizationTokenKey).(string); ok && authToken != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", authToken))
		req.Header.Del("api-key")
		return nil
	}

	// API key authentication
	value := key.Value.GetValue()
	if value != "" {
		req.Header.Del("Authorization")
		req.Header.Set("api-key", value)
		return nil
	}

	// No explicit credentials - attempt DefaultAzureCredential auto-detection.
	scopes := getAzureScopes(nil)
	if key.AzureKeyConfig != nil {
		scopes = getAzureScopes(key.AzureKeyConfig.Scopes)
	}

	cred, err := provider.getOrCreateDefaultAzureCredential()
	if err != nil {
		return providerUtils.NewKoutakuOperationError("no credentials provided and DefaultAzureCredential unavailable", err)
	}

	token, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: scopes})
	if err != nil {
		return providerUtils.NewKoutakuOperationError("no credentials provided and DefaultAzureCredential failed to get token", err)
	}

	if token.Token == "" {
		return providerUtils.NewKoutakuOperationError("no credentials provided and DefaultAzureCredential returned empty token", fmt.Errorf("token is empty"))
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token.Token))
	req.Header.Del("api-key")
	return nil
}

// AzureFileResponse represents an Azure file response (same as OpenAI).
type AzureFileResponse struct {
	ID            string              `json:"id"`
	Object        string              `json:"object"`
	Bytes         int64               `json:"bytes"`
	CreatedAt     int64               `json:"created_at"`
	Filename      string              `json:"filename"`
	Purpose       schemas.FilePurpose `json:"purpose"`
	Status        string              `json:"status,omitempty"`
	StatusDetails *string             `json:"status_details,omitempty"`
}

// ToKoutakuFileUploadResponse converts Azure file response to Koutaku response.
func (r *AzureFileResponse) ToKoutakuFileUploadResponse(providerName schemas.ModelProvider, latency time.Duration, sendBackRawResponse bool, rawResponse interface{}) *schemas.KoutakuFileUploadResponse {
	resp := &schemas.KoutakuFileUploadResponse{
		ID:             r.ID,
		Object:         r.Object,
		Bytes:          r.Bytes,
		CreatedAt:      r.CreatedAt,
		Filename:       r.Filename,
		Purpose:        r.Purpose,
		Status:         openai.ToKoutakuFileStatus(r.Status),
		StatusDetails:  r.StatusDetails,
		StorageBackend: schemas.FileStorageAPI,
		ExtraFields: schemas.KoutakuResponseExtraFields{
			Latency: latency.Milliseconds(),
		},
	}

	if sendBackRawResponse {
		resp.ExtraFields.RawResponse = rawResponse
	}

	return resp
}
