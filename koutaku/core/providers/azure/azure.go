// Package azure implements the Azure provider.
package azure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/bytedance/sonic"
	"github.com/koutaku/koutaku/core/providers/anthropic"
	"github.com/koutaku/koutaku/core/providers/openai"
	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	schemas "github.com/koutaku/koutaku/core/schemas"

	"github.com/valyala/fasthttp"
)

// AzureAuthorizationTokenKey is the context key for the Azure authentication token.
const AzureAuthorizationTokenKey schemas.KoutakuContextKey = "azure-authorization-token"

// DefaultAzureScope is the default scope for Azure authentication.
const DefaultAzureScope = "https://cognitiveservices.azure.com/.default"

// AzureProvider implements the Provider interface for Azure's API.
type AzureProvider struct {
	logger          schemas.Logger        // Logger for provider operations
	client          *fasthttp.Client      // HTTP client for unary API requests (ReadTimeout bounds overall response)
	streamingClient *fasthttp.Client      // HTTP client for streaming API requests (no ReadTimeout; idle governed by NewIdleTimeoutReader)
	networkConfig   schemas.NetworkConfig // Network configuration including extra headers

	credentials         sync.Map // map of tenant ID:client ID to azcore.TokenCredential
	sendBackRawRequest  bool     // Whether to include raw request in KoutakuResponse
	sendBackRawResponse bool     // Whether to include raw response in KoutakuResponse
}

func (p *AzureProvider) getOrCreateAuth(
	tenantID, clientID, clientSecret string,
) (azcore.TokenCredential, error) {
	key := tenantID + ":" + clientID

	// Fast path
	if val, ok := p.credentials.Load(key); ok {
		return val.(azcore.TokenCredential), nil
	}

	// Slow path - create new credential
	cred, err := azidentity.NewClientSecretCredential(
		tenantID,
		clientID,
		clientSecret,
		nil,
	)
	if err != nil {
		return nil, err
	}

	actual, _ := p.credentials.LoadOrStore(key, cred)
	return actual.(azcore.TokenCredential), nil
}

// getOrCreateDefaultAzureCredential returns a DefaultAzureCredential, creating and caching it if needed.
// It automatically detects the auth environment: managed identity on Azure VMs/containers,
// workload identity in AKS, environment variables, Azure CLI, and more.
func (p *AzureProvider) getOrCreateDefaultAzureCredential() (azcore.TokenCredential, error) {
	const cacheKey = "default_azure_credential"

	if val, ok := p.credentials.Load(cacheKey); ok {
		return val.(azcore.TokenCredential), nil
	}

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}

	actual, _ := p.credentials.LoadOrStore(cacheKey, cred)
	return actual.(azcore.TokenCredential), nil
}

// getAzureAuthHeaders returns authentication headers based on priority:
// 1. Service Principal (client ID/secret/tenant ID) - Bearer token
// 2. Context token - Bearer token
// 3. API key - api-key or x-api-key header
func (provider *AzureProvider) getAzureAuthHeaders(ctx *schemas.KoutakuContext, key schemas.Key, isAnthropicModel bool) (map[string]string, *schemas.KoutakuError) {
	authHeader := make(map[string]string)

	// Service Principal authentication
	if key.AzureKeyConfig != nil && key.AzureKeyConfig.ClientID != nil &&
		key.AzureKeyConfig.ClientSecret != nil && key.AzureKeyConfig.TenantID != nil && key.AzureKeyConfig.ClientID.GetValue() != "" && key.AzureKeyConfig.ClientSecret.GetValue() != "" && key.AzureKeyConfig.TenantID.GetValue() != "" {
		cred, err := provider.getOrCreateAuth(key.AzureKeyConfig.TenantID.GetValue(), key.AzureKeyConfig.ClientID.GetValue(), key.AzureKeyConfig.ClientSecret.GetValue())
		if err != nil {
			return nil, providerUtils.NewKoutakuOperationError("failed to get or create Azure authentication", err)
		}

		scopes := getAzureScopes(key.AzureKeyConfig.Scopes)

		token, err := cred.GetToken(ctx, policy.TokenRequestOptions{
			Scopes: scopes,
		})
		if err != nil {
			return nil, providerUtils.NewKoutakuOperationError("failed to get Azure access token", err)
		}

		if token.Token == "" {
			return nil, providerUtils.NewKoutakuOperationError("Azure access token is empty", errors.New("token is empty"))
		}

		authHeader["Authorization"] = fmt.Sprintf("Bearer %s", token.Token)
		return authHeader, nil
	}

	// Context token authentication
	if authToken, ok := ctx.Value(AzureAuthorizationTokenKey).(string); ok && authToken != "" {
		authHeader["Authorization"] = fmt.Sprintf("Bearer %s", authToken)
		return authHeader, nil
	}

	value := key.Value.GetValue()
	if value == "" {
		// No explicit credentials provided - attempt DefaultAzureCredential auto-detection.
		// This covers managed identity on Azure VMs/containers, workload identity in AKS,
		// environment variables, Azure CLI, and more - with no config required.
		scopes := getAzureScopes(nil)
		if key.AzureKeyConfig != nil {
			scopes = getAzureScopes(key.AzureKeyConfig.Scopes)
		}

		cred, err := provider.getOrCreateDefaultAzureCredential()
		if err != nil {
			return nil, providerUtils.NewKoutakuOperationError("no credentials provided and DefaultAzureCredential unavailable", err)
		}

		token, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: scopes})
		if err != nil {
			return nil, providerUtils.NewKoutakuOperationError("no credentials provided and DefaultAzureCredential failed to get token", err)
		}

		if token.Token == "" {
			return nil, providerUtils.NewKoutakuOperationError("no credentials provided and DefaultAzureCredential returned empty token", errors.New("token is empty"))
		}

		authHeader["Authorization"] = fmt.Sprintf("Bearer %s", token.Token)
		return authHeader, nil
	}

	// API key authentication
	if isAnthropicModel {
		authHeader["x-api-key"] = value
	} else {
		authHeader["api-key"] = value
	}
	return authHeader, nil
}

// NewAzureProvider creates a new Azure provider instance.
// It initializes the HTTP client with the provided configuration and sets up response pools.
// The client is configured with timeouts, concurrency limits, and optional proxy settings.
func NewAzureProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*AzureProvider, error) {
	config.CheckAndSetDefaults()

	requestTimeout := time.Second * time.Duration(config.NetworkConfig.DefaultRequestTimeoutInSeconds)
	client := &fasthttp.Client{
		ReadTimeout:         requestTimeout,
		WriteTimeout:        requestTimeout,
		MaxConnsPerHost:     config.NetworkConfig.MaxConnsPerHost,
		MaxIdleConnDuration: 30 * time.Second,
		MaxConnWaitTimeout:  requestTimeout,
		MaxConnDuration:     time.Second * time.Duration(schemas.DefaultMaxConnDurationInSeconds),
		ConnPoolStrategy:    fasthttp.FIFO,
	}

	// Configure proxy and retry policy
	client = providerUtils.ConfigureProxy(client, config.ProxyConfig, logger)
	client = providerUtils.ConfigureDialer(client)
	client = providerUtils.ConfigureTLS(client, config.NetworkConfig, logger)
	streamingClient := providerUtils.BuildStreamingClient(client)
	return &AzureProvider{
		logger:              logger,
		client:              client,
		streamingClient:     streamingClient,
		networkConfig:       config.NetworkConfig,
		sendBackRawRequest:  config.SendBackRawRequest,
		sendBackRawResponse: config.SendBackRawResponse,
	}, nil
}

// GetProviderKey returns the provider identifier for Azure.
func (provider *AzureProvider) GetProviderKey() schemas.ModelProvider {
	return schemas.Azure
}

// completeRequest sends a request to Azure's API and handles the response.
// It constructs the API URL, sets up authentication, and processes the response.
// Returns the response body, request latency, or an error if the request fails.
func (provider *AzureProvider) completeRequest(
	ctx *schemas.KoutakuContext,
	jsonData []byte,
	path string,
	key schemas.Key,
	model string,
) ([]byte, time.Duration, map[string]string, *schemas.KoutakuError) {
	// Create the request with the JSON body
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	respOwned := true
	defer func() {
		if respOwned {
			fasthttp.ReleaseResponse(resp)
		}
	}()

	var url string
	isAnthropicModel := schemas.IsAnthropicModel(model)

	// Set any extra headers from network config.
	// For Anthropic models, exclude anthropic-beta — it is merged and filtered explicitly below.
	if isAnthropicModel {
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, []string{anthropic.AnthropicBetaHeader})
	} else {
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	}
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")

	// Get authentication headers
	authHeaders, koutakuErr := provider.getAzureAuthHeaders(ctx, key, isAnthropicModel)
	if koutakuErr != nil {
		return nil, 0, nil, koutakuErr
	}

	// Apply headers to request
	for k, v := range authHeaders {
		req.Header.Set(k, v)
	}

	endpoint := key.AzureKeyConfig.Endpoint.GetValue()
	if endpoint == "" {
		return nil, 0, nil, providerUtils.NewConfigurationError("endpoint not set")
	}

	if isAnthropicModel {
		req.Header.Set("anthropic-version", AzureAnthropicAPIVersionDefault)
		url = fmt.Sprintf("%s/%s", endpoint, path)

		// Merge ExtraHeaders + context anthropic-beta, filter for Azure, then set as HTTP header
		if betaHeaders := anthropic.FilterBetaHeadersForProvider(anthropic.MergeBetaHeaders(provider.networkConfig.ExtraHeaders, ctx), schemas.Azure, provider.networkConfig.BetaHeaderOverrides); len(betaHeaders) > 0 {
			req.Header.Set(anthropic.AnthropicBetaHeader, strings.Join(betaHeaders, ","))
		} else {
			req.Header.Del(anthropic.AnthropicBetaHeader)
		}
	} else {
		apiVersion := key.AzureKeyConfig.APIVersion
		if apiVersion == nil {
			apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
		}
		if path == "openai/v1/responses" {
			url = fmt.Sprintf("%s/%s?api-version=preview", endpoint, path)
		} else {
			url = fmt.Sprintf("%s/%s?api-version=%s", endpoint, path, apiVersion.GetValue())
		}
	}

	req.SetRequestURI(url)
	if !providerUtils.ApplyLargePayloadRequestBodyWithModelNormalization(ctx, req, schemas.OpenAI) {
		req.SetBody(jsonData)
	}

	// Send the request with optional large response streaming
	activeClient := providerUtils.PrepareResponseStreaming(ctx, provider.client, resp)
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, activeClient, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, latency, nil, koutakuErr
	}

	// Extract provider response headers before body is copied — do this before status check
	// so error responses also carry provider headers (rate-limit info, request IDs, etc.)
	providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		providerUtils.MaterializeStreamErrorBody(ctx, resp)
		rawErrBody := append([]byte(nil), resp.Body()...)
		return rawErrBody, latency, providerResponseHeaders, openai.ParseOpenAIError(resp)
	}

	body, isLargeResp, decodeErr := providerUtils.FinalizeResponseWithLargeDetection(ctx, resp, provider.logger)
	if decodeErr != nil {
		return nil, latency, providerResponseHeaders, decodeErr
	}
	if isLargeResp {
		respOwned = false
		return nil, latency, providerResponseHeaders, nil
	}

	return body, latency, providerResponseHeaders, nil
}

// listModelsByKey performs a list models request for a single key.
// Returns the response and latency, or an error if the request fails.
func (provider *AzureProvider) listModelsByKey(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuListModelsRequest) (*schemas.KoutakuListModelsResponse, *schemas.KoutakuError) {
	// Get API version
	apiVersion := key.AzureKeyConfig.APIVersion
	if apiVersion == nil {
		apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
	}

	// Create the request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Set any extra headers from network config
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)

	req.SetRequestURI(key.AzureKeyConfig.Endpoint.GetValue() + providerUtils.GetPathFromContext(ctx, fmt.Sprintf("/openai/models?api-version=%s", apiVersion.GetValue())))
	req.Header.SetMethod(http.MethodGet)
	req.Header.SetContentType("application/json")

	// Set Azure authentication
	authHeaders, koutakuErr := provider.getAzureAuthHeaders(ctx, key, false)
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	for k, v := range authHeaders {
		req.Header.Set(k, v)
	}

	// Send the request and measure latency
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Store provider response headers in context before status check so error responses also forward them
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, openai.ParseOpenAIError(resp)
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
	}

	// Read the response body and copy it before releasing the response
	// to avoid use-after-free since resp.Body() references fasthttp's internal buffer
	responseBody := append([]byte(nil), body...)

	// Parse Azure-specific response
	azureResponse := &AzureListModelsResponse{}
	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(responseBody, azureResponse, nil, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Convert to Koutaku response
	response := azureResponse.ToKoutakuListModelsResponse(key.Models, key.BlacklistedModels, key.Aliases, request.Unfiltered)
	if response == nil {
		return nil, providerUtils.NewKoutakuOperationError("failed to convert Azure model list response", nil)
	}

	response.ExtraFields.Latency = latency.Milliseconds()

	// Set raw request if enabled
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		response.ExtraFields.RawRequest = rawRequest
	}

	// Set raw response if enabled
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		response.ExtraFields.RawResponse = rawResponse
	}

	return response, nil
}

// ListModels performs a list models request to Azure's API.
// It retrieves all models accessible by the Azure resource
// Requests are made concurrently for improved performance.
func (provider *AzureProvider) ListModels(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuListModelsRequest) (*schemas.KoutakuListModelsResponse, *schemas.KoutakuError) {
	return providerUtils.HandleMultipleListModelsRequests(
		ctx,
		keys,
		request,
		provider.listModelsByKey,
	)
}

// TextCompletion performs a text completion request to Azure's API.
// It formats the request, sends it to Azure, and processes the response.
// Returns a KoutakuResponse containing the completion results or an error if the request fails.
func (provider *AzureProvider) TextCompletion(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuTextCompletionRequest) (*schemas.KoutakuTextCompletionResponse, *schemas.KoutakuError) {
	// Use centralized OpenAI text converter (Azure is OpenAI-compatible)
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return openai.ToOpenAITextCompletionRequest(request), nil
		})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	responseBody, latency, providerResponseHeaders, err := provider.completeRequest(
		ctx,
		jsonData,
		fmt.Sprintf("openai/deployments/%s/completions", request.Model),
		key,
		request.Model,
	)
	if providerResponseHeaders != nil {
		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)
	}
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Large response mode: return lightweight response with metadata only
	if isLargeResp, _ := ctx.Value(schemas.KoutakuContextKeyLargeResponseMode).(bool); isLargeResp {
		return &schemas.KoutakuTextCompletionResponse{
			Model: request.Model,
			ExtraFields: schemas.KoutakuResponseExtraFields{
				Latency:                 latency.Milliseconds(),
				ProviderResponseHeaders: providerResponseHeaders,
			},
		}, nil
	}

	response := &schemas.KoutakuTextCompletionResponse{}

	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(responseBody, response, jsonData, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	response.ExtraFields.Latency = latency.Milliseconds()
	response.ExtraFields.ProviderResponseHeaders = providerResponseHeaders

	// Set raw request if enabled
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		response.ExtraFields.RawRequest = rawRequest
	}

	// Set raw response if enabled
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		response.ExtraFields.RawResponse = rawResponse
	}

	return response, nil
}

// TextCompletionStream performs a streaming text completion request to Azure's API.
// It formats the request, sends it to Azure, and processes the response.
// Returns a channel of KoutakuStreamChunk objects or an error if the request fails.
func (provider *AzureProvider) TextCompletionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuTextCompletionRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	apiVersion := key.AzureKeyConfig.APIVersion
	if apiVersion == nil {
		apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
	}

	url := fmt.Sprintf("%s/openai/deployments/%s/completions?api-version=%s", key.AzureKeyConfig.Endpoint.GetValue(), request.Model, apiVersion.GetValue())

	// Get Azure authentication headers
	authHeader, err := provider.getAzureAuthHeaders(ctx, key, false)
	if err != nil {
		return nil, err
	}

	return openai.HandleOpenAITextCompletionStreaming(
		ctx,
		provider.streamingClient,
		url,
		request,
		authHeader,
		provider.networkConfig.ExtraHeaders,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.GetProviderKey(),
		nil,
		postHookRunner,
		nil,
		nil,
		provider.logger,
		postHookSpanFinalizer,
	)
}

// ChatCompletion performs a chat completion request to Azure's API.
// It formats the request, sends it to Azure, and processes the response.
// Returns a KoutakuResponse containing the completion results or an error if the request fails.
func (provider *AzureProvider) ChatCompletion(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuChatRequest) (*schemas.KoutakuChatResponse, *schemas.KoutakuError) {
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			if schemas.IsAnthropicModel(request.Model) {
				reqBody, err := anthropic.ToAnthropicChatRequest(ctx, request)
				if err != nil {
					return nil, err
				}
				if reqBody != nil {
					// Add provider-aware beta headers for Azure
					anthropic.AddMissingBetaHeadersToContext(ctx, reqBody, schemas.Azure)
				}
				return reqBody, nil
			} else {
				return openai.ToOpenAIChatRequest(ctx, request), nil
			}
		})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	var path string
	if schemas.IsAnthropicModel(request.Model) {
		path = "anthropic/v1/messages"
	} else {
		path = fmt.Sprintf("openai/deployments/%s/chat/completions", request.Model)
	}

	responseBody, latency, providerResponseHeaders, err := provider.completeRequest(
		ctx,
		jsonData,
		path,
		key,
		request.Model,
	)
	if providerResponseHeaders != nil {
		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)
	}
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Large response mode: return lightweight response with metadata only
	if isLargeResp, _ := ctx.Value(schemas.KoutakuContextKeyLargeResponseMode).(bool); isLargeResp {
		return &schemas.KoutakuChatResponse{
			Model: request.Model,
			ExtraFields: schemas.KoutakuResponseExtraFields{
				Latency:                 latency.Milliseconds(),
				ProviderResponseHeaders: providerResponseHeaders,
			},
		}, nil
	}

	response := &schemas.KoutakuChatResponse{}
	var rawRequest interface{}
	var rawResponse interface{}

	if schemas.IsAnthropicModel(request.Model) {
		anthropicResponse := anthropic.AcquireAnthropicMessageResponse()
		defer anthropic.ReleaseAnthropicMessageResponse(anthropicResponse)
		rawRequest, rawResponse, koutakuErr = providerUtils.HandleProviderResponse(responseBody, anthropicResponse, jsonData, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
		if koutakuErr != nil {
			return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse)
		}
		response = anthropicResponse.ToKoutakuChatResponse(ctx)
	} else {
		rawRequest, rawResponse, koutakuErr = providerUtils.HandleProviderResponse(responseBody, response, jsonData, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
		if koutakuErr != nil {
			return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse)
		}
	}

	response.ExtraFields.Latency = latency.Milliseconds()
	response.ExtraFields.ProviderResponseHeaders = providerResponseHeaders

	// Set raw request if enabled
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		response.ExtraFields.RawRequest = rawRequest
	}

	// Set raw response if enabled
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		response.ExtraFields.RawResponse = rawResponse
	}

	return response, nil
}

// ChatCompletionStream performs a streaming chat completion request to Azure's API.
// It supports real-time streaming of responses using Server-Sent Events (SSE).
// Uses Azure-specific URL construction with deployments and supports both api-key and Bearer token authentication.
// Returns a channel containing KoutakuResponse objects representing the stream or an error if the request fails.
func (provider *AzureProvider) ChatCompletionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuChatRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	var url string
	if schemas.IsAnthropicModel(request.Model) {
		authHeader, err := provider.getAzureAuthHeaders(ctx, key, true)
		if err != nil {
			return nil, err
		}
		authHeader["anthropic-version"] = AzureAnthropicAPIVersionDefault
		url = fmt.Sprintf("%s/anthropic/v1/messages", key.AzureKeyConfig.Endpoint.GetValue())

		jsonData, err := providerUtils.CheckContextAndGetRequestBody(
			ctx,
			request,
			func() (providerUtils.RequestBodyWithExtraParams, error) {
				reqBody, err := anthropic.ToAnthropicChatRequest(ctx, request)
				if err != nil {
					return nil, err
				}
				if reqBody != nil {
					reqBody.Stream = schemas.Ptr(true)
					// Add provider-aware beta headers for Azure
					anthropic.AddMissingBetaHeadersToContext(ctx, reqBody, schemas.Azure)
				}
				return reqBody, nil
			})
		if err != nil {
			return nil, err
		}

		// Use shared streaming logic from Anthropic
		return anthropic.HandleAnthropicChatCompletionStreaming(
			ctx,
			provider.streamingClient,
			url,
			jsonData,
			authHeader,
			provider.networkConfig.ExtraHeaders,
			provider.networkConfig.BetaHeaderOverrides,
			providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
			providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
			provider.GetProviderKey(),
			postHookRunner,
			nil,
			provider.logger,
			postHookSpanFinalizer,
		)
	} else {
		authHeader, err := provider.getAzureAuthHeaders(ctx, key, false)
		if err != nil {
			return nil, err
		}
		apiVersion := key.AzureKeyConfig.APIVersion
		if apiVersion == nil {
			apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
		}
		url = fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s", key.AzureKeyConfig.Endpoint.GetValue(), request.Model, apiVersion.GetValue())

		// Use shared streaming logic from OpenAI
		return openai.HandleOpenAIChatCompletionStreaming(
			ctx,
			provider.streamingClient,
			url,
			request,
			authHeader,
			provider.networkConfig.ExtraHeaders,
			providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
			providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
			provider.GetProviderKey(),
			postHookRunner,
			nil,
			nil,
			nil,
			nil,
			nil,
			provider.logger,
			postHookSpanFinalizer,
		)
	}
}

// Responses performs a responses request to Azure's API.
// It formats the request, sends it to Azure, and processes the response.
// Returns a KoutakuResponse containing the completion results or an error if the request fails.
func (provider *AzureProvider) Responses(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuResponsesRequest) (*schemas.KoutakuResponsesResponse, *schemas.KoutakuError) {
	var jsonData []byte
	var koutakuErr *schemas.KoutakuError
	if schemas.IsAnthropicModel(request.Model) {
		jsonData, koutakuErr = getRequestBodyForAnthropicResponses(ctx, request, request.Model, false)
	} else {
		jsonData, koutakuErr = providerUtils.CheckContextAndGetRequestBody(
			ctx,
			request,
			func() (providerUtils.RequestBodyWithExtraParams, error) {
				reqBody := openai.ToOpenAIResponsesRequest(request)
				return reqBody, nil
			})
	}
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	var path string
	if schemas.IsAnthropicModel(request.Model) {
		path = "anthropic/v1/messages"
	} else {
		path = "openai/v1/responses"
	}

	responseBody, latency, providerResponseHeaders, err := provider.completeRequest(
		ctx,
		jsonData,
		path,
		key,
		request.Model,
	)
	if providerResponseHeaders != nil {
		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)
	}
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Large response mode: return lightweight response with metadata only
	if isLargeResp, _ := ctx.Value(schemas.KoutakuContextKeyLargeResponseMode).(bool); isLargeResp {
		return &schemas.KoutakuResponsesResponse{
			Model: request.Model,
			ExtraFields: schemas.KoutakuResponseExtraFields{
				Latency:                 latency.Milliseconds(),
				ProviderResponseHeaders: providerResponseHeaders,
			},
		}, nil
	}

	response := &schemas.KoutakuResponsesResponse{}
	var rawRequest interface{}
	var rawResponse interface{}

	if schemas.IsAnthropicModel(request.Model) {
		anthropicResponse := anthropic.AcquireAnthropicMessageResponse()
		defer anthropic.ReleaseAnthropicMessageResponse(anthropicResponse)
		rawRequest, rawResponse, koutakuErr = providerUtils.HandleProviderResponse(responseBody, anthropicResponse, jsonData, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
		if koutakuErr != nil {
			return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse)
		}
		response = anthropicResponse.ToKoutakuResponsesResponse(ctx)
	} else {
		rawRequest, rawResponse, koutakuErr = providerUtils.HandleProviderResponse(responseBody, response, jsonData, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
		if koutakuErr != nil {
			return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse)
		}
	}

	response.ExtraFields.Latency = latency.Milliseconds()
	response.ExtraFields.ProviderResponseHeaders = providerResponseHeaders

	// Set raw request if enabled
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		response.ExtraFields.RawRequest = rawRequest
	}

	// Set raw response if enabled
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		response.ExtraFields.RawResponse = rawResponse
	}

	return response, nil
}

// ResponsesStream performs a streaming responses request to Azure's API.
func (provider *AzureProvider) ResponsesStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuResponsesRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	var url string
	if schemas.IsAnthropicModel(request.Model) {
		authHeader, err := provider.getAzureAuthHeaders(ctx, key, true)
		if err != nil {
			return nil, err
		}
		authHeader["anthropic-version"] = AzureAnthropicAPIVersionDefault
		url = fmt.Sprintf("%s/anthropic/v1/messages", key.AzureKeyConfig.Endpoint.GetValue())

		jsonData, koutakuErr := getRequestBodyForAnthropicResponses(ctx, request, request.Model, true)
		if koutakuErr != nil {
			return nil, koutakuErr
		}

		// Use shared streaming logic from Anthropic
		return anthropic.HandleAnthropicResponsesStream(
			ctx,
			provider.streamingClient,
			url,
			jsonData,
			authHeader,
			provider.networkConfig.ExtraHeaders,
			provider.networkConfig.BetaHeaderOverrides,
			providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
			providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
			provider.GetProviderKey(),
			postHookRunner,
			nil,
			provider.logger,
			postHookSpanFinalizer,
		)
	} else {
		authHeader, err := provider.getAzureAuthHeaders(ctx, key, false)
		if err != nil {
			return nil, err
		}
		url = fmt.Sprintf("%s/openai/v1/responses?api-version=preview", key.AzureKeyConfig.Endpoint.GetValue())

		// Use shared streaming logic from OpenAI
		return openai.HandleOpenAIResponsesStreaming(
			ctx,
			provider.streamingClient,
			url,
			request,
			authHeader,
			provider.networkConfig.ExtraHeaders,
			providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
			providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
			provider.GetProviderKey(),
			postHookRunner,
			nil,
			nil,
			nil,
			nil,
			provider.logger,
			postHookSpanFinalizer,
		)
	}
}

// Embedding generates embeddings for the given input text(s) using Azure.
// The input can be either a single string or a slice of strings for batch embedding.
// Returns a KoutakuResponse containing the embedding(s) and any error that occurred.
func (provider *AzureProvider) Embedding(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuEmbeddingRequest) (*schemas.KoutakuEmbeddingResponse, *schemas.KoutakuError) {
	// Use centralized converter
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return openai.ToOpenAIEmbeddingRequest(request), nil
		})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	responseBody, latency, providerResponseHeaders, err := provider.completeRequest(
		ctx,
		jsonData,
		fmt.Sprintf("openai/deployments/%s/embeddings", request.Model),
		key,
		request.Model,
	)
	if providerResponseHeaders != nil {
		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)
	}
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Large response mode: return lightweight response with metadata only
	if isLargeResp, _ := ctx.Value(schemas.KoutakuContextKeyLargeResponseMode).(bool); isLargeResp {
		return &schemas.KoutakuEmbeddingResponse{
			Model: request.Model,
			ExtraFields: schemas.KoutakuResponseExtraFields{
				Latency:                 latency.Milliseconds(),
				ProviderResponseHeaders: providerResponseHeaders,
			},
		}, nil
	}

	response := &schemas.KoutakuEmbeddingResponse{}

	// Use enhanced response handler with pre-allocated response
	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(responseBody, response, jsonData, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	response.ExtraFields.Latency = latency.Milliseconds()
	response.ExtraFields.ProviderResponseHeaders = providerResponseHeaders

	// Set raw request if enabled
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		response.ExtraFields.RawRequest = rawRequest
	}

	// Set raw response if enabled
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		response.ExtraFields.RawResponse = rawResponse
	}

	return response, nil
}

// Speech is not supported by the Azure provider.
func (provider *AzureProvider) Speech(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuSpeechRequest) (*schemas.KoutakuSpeechResponse, *schemas.KoutakuError) {
	apiVersion := key.AzureKeyConfig.APIVersion
	if apiVersion == nil {
		apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
	}

	endpoint := key.AzureKeyConfig.Endpoint.GetValue()
	if endpoint == "" {
		return nil, providerUtils.NewConfigurationError("endpoint not set")
	}

	url := fmt.Sprintf("%s/openai/deployments/%s/audio/speech?api-version=%s", endpoint, request.Model, apiVersion.GetValue())

	response, err := openai.HandleOpenAISpeechRequest(
		ctx,
		provider.client,
		url,
		request,
		key,
		provider.networkConfig.ExtraHeaders,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		nil,
		provider.logger,
	)

	if err != nil {
		return nil, err
	}

	return response, err
}

// Rerank is not supported by the Azure provider.
func (provider *AzureProvider) Rerank(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuRerankRequest) (*schemas.KoutakuRerankResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.RerankRequest, provider.GetProviderKey())
}

// OCR is not supported by the Azure provider.
func (provider *AzureProvider) OCR(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuOCRRequest) (*schemas.KoutakuOCRResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.OCRRequest, provider.GetProviderKey())
}

// SpeechStream handles streaming for speech synthesis with Azure.
// Azure sends raw binary audio bytes in SSE format, unlike OpenAI which sends JSON.
func (provider *AzureProvider) SpeechStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuSpeechRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	// Get Azure authentication headers
	authHeader, err := provider.getAzureAuthHeaders(ctx, key, false)
	if err != nil {
		return nil, err
	}

	apiVersion := key.AzureKeyConfig.APIVersion
	if apiVersion == nil {
		apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
	}
	url := fmt.Sprintf("%s/openai/deployments/%s/audio/speech?api-version=%s", key.AzureKeyConfig.Endpoint.GetValue(), request.Model, apiVersion.GetValue())

	// Create HTTP request for streaming
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	resp.StreamBody = true
	defer fasthttp.ReleaseRequest(req)

	// Prepare headers
	headers := map[string]string{
		"Content-Type":    "application/json",
		"Accept":          "text/event-stream",
		"Cache-Control":   "no-cache",
		"Accept-Encoding": "identity",
	}

	maps.Copy(headers, authHeader)

	req.Header.SetMethod(http.MethodPost)
	req.SetRequestURI(url)
	req.Header.SetContentType("application/json")

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)

	// Set headers
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	// Build request body
	jsonBody, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			reqBody := openai.ToOpenAISpeechRequest(request)
			if reqBody != nil {
				reqBody.StreamFormat = schemas.Ptr("sse")
			}
			return reqBody, nil
		})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	if !providerUtils.ApplyLargePayloadRequestBodyWithModelNormalization(ctx, req, schemas.OpenAI) {
		req.SetBody(jsonBody)
	}

	// Make the request
	requestErr := provider.client.Do(req, resp)
	if requestErr != nil {
		defer providerUtils.ReleaseStreamingResponse(resp)
		if errors.Is(requestErr, context.Canceled) {
			return nil, providerUtils.EnrichError(ctx, &schemas.KoutakuError{
				IsKoutakuError: false,
				Error: &schemas.ErrorField{
					Type:    schemas.Ptr(schemas.RequestCancelled),
					Message: schemas.ErrRequestCancelled,
					Error:   requestErr,
				},
			}, jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
		}
		if errors.Is(requestErr, fasthttp.ErrTimeout) || errors.Is(requestErr, context.DeadlineExceeded) {
			return nil, providerUtils.EnrichError(ctx, providerUtils.NewKoutakuTimeoutError(schemas.ErrProviderRequestTimedOut, requestErr), jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
		}
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewKoutakuOperationError(schemas.ErrProviderDoRequest, requestErr), jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
	}

	// Extract provider response headers before status check so error responses also forward them
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

	// Check for HTTP errors
	if resp.StatusCode() != fasthttp.StatusOK {
		defer providerUtils.ReleaseStreamingResponse(resp)
		return nil, providerUtils.EnrichError(ctx, openai.ParseOpenAIError(resp), jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
	}

	// Create response channel
	responseChan := make(chan *schemas.KoutakuStreamChunk, schemas.DefaultStreamBufferSize)

	providerUtils.SetStreamIdleTimeoutIfEmpty(ctx, provider.networkConfig.StreamIdleTimeoutInSeconds)

	// Start streaming in a goroutine
	go func() {
		defer providerUtils.EnsureStreamFinalizerCalled(ctx, postHookSpanFinalizer)
		defer func() {
			if ctx.Err() == context.Canceled {
				providerUtils.HandleStreamCancellation(ctx, postHookRunner, responseChan, provider.logger, postHookSpanFinalizer)
			} else if ctx.Err() == context.DeadlineExceeded {
				providerUtils.HandleStreamTimeout(ctx, postHookRunner, responseChan, provider.logger, postHookSpanFinalizer)
			}
			close(responseChan)
		}()
		// Always release response on exit; bodyStream close should prevent indefinite blocking.
		defer providerUtils.ReleaseStreamingResponse(resp)

		reader, releaseGzip := providerUtils.DecompressStreamBody(resp)
		defer releaseGzip()

		// Wrap reader with idle timeout to detect stalled streams (e.g., Azure TPM throttling
		// that stops sending data but keeps the TCP connection open).
		reader, stopIdleTimeout := providerUtils.NewIdleTimeoutReader(reader, resp.BodyStream(), providerUtils.GetStreamIdleTimeout(ctx))
		defer stopIdleTimeout()

		// Setup cancellation handler to close the raw network stream on ctx cancellation,
		// which immediately unblocks any in-progress read (including reads blocked inside a gzip decompression layer).
		stopCancellation := providerUtils.SetupStreamCancellation(ctx, resp.BodyStream(), provider.logger)
		defer stopCancellation()

		chunkIndex := -1
		startTime := time.Now()
		lastChunkTime := startTime

		// Read SSE events manually to handle binary data with embedded newlines
		// SSE format: "data: <content>\n\n" - events are separated by double newlines
		// We can't use bufio.Scanner because MP3 data contains 0x0a bytes which get interpreted as newlines
		readBuffer := make([]byte, 64*1024) // 64KB read chunks
		var accumulated []byte
		doneReceived := false

		for {
			// If context was cancelled/timed out, let defer handle it
			if ctx.Err() != nil {
				return
			}
			// Read from stream
			n, readErr := reader.Read(readBuffer)
			if n > 0 {
				accumulated = append(accumulated, readBuffer[:n]...)

				// Process complete SSE events (separated by \r\n\r\n or \n\n)
				for {
					// Find the next double-newline separator (try CRLF first, then LF)
					var idx int
					var separatorLen int
					idx = bytes.Index(accumulated, []byte("\r\n\r\n"))
					if idx != -1 {
						separatorLen = 4 // \r\n\r\n
					} else {
						idx = bytes.Index(accumulated, []byte("\n\n"))
						if idx != -1 {
							separatorLen = 2 // \n\n
						}
					}
					if idx == -1 {
						// No complete event yet, need more data
						break
					}

					// Extract the event (everything up to the separator)
					event := accumulated[:idx]
					accumulated = accumulated[idx+separatorLen:] // Skip the separator

					// Skip empty events and comments
					if len(event) == 0 || bytes.HasPrefix(event, []byte(":")) {
						continue
					}

					// Parse the SSE event
					var audioData []byte

					// Check if this has "data: " prefix (standard SSE format)
					if bytes.HasPrefix(event, []byte("data: ")) {
						audioData = event[6:] // Skip "data: " prefix
						// Check for [DONE] marker - break out of loops to send final response
						if bytes.Equal(audioData, []byte("[DONE]")) {
							doneReceived = true
							break
						}
					} else {
						// Raw data without prefix (shouldn't happen with Azure, but handle it)
						audioData = event
					}

					// Skip empty data
					if len(audioData) == 0 {
						continue
					}

					// Azure sends JSON-wrapped responses for speech streaming
					// Parse the JSON to extract the response type and audio data
					var response schemas.KoutakuSpeechStreamResponse
					if err := sonic.Unmarshal(audioData, &response); err != nil {
						// If JSON parsing fails, check if this might be an error response
						// Quick check for error field (allocation-free using sonic.Get)
						if errorNode, _ := sonic.Get(audioData, "error"); errorNode.Exists() {
							// Only unmarshal when we know there's an error
							var koutakuErr schemas.KoutakuError
							if errParseErr := sonic.Unmarshal(audioData, &koutakuErr); errParseErr == nil {
								if koutakuErr.Error != nil && koutakuErr.Error.Message != "" {
									ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
									providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, &koutakuErr, responseChan, provider.logger, postHookSpanFinalizer)
									return
								}
							}
						}
						// If it's not valid JSON, log and skip
						provider.logger.Warn("failed to parse speech stream response: %v", err)
						continue
					}

					// Check for completion event - skip if no audio data
					if response.Type == schemas.SpeechStreamResponseTypeDone || len(response.Audio) == 0 {
						// This is a control event or empty response - skip
						continue
					}

					chunkIndex++

					// Set extra fields for the response
					response.ExtraFields = schemas.KoutakuResponseExtraFields{
						ChunkIndex: chunkIndex,
						Latency:    time.Since(lastChunkTime).Milliseconds(),
					}
					lastChunkTime = time.Now()

					if sendBackRawResponse {
						response.ExtraFields.RawResponse = audioData
					}

					providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, nil, &response, nil, nil), responseChan, postHookSpanFinalizer)
				}

				// Check if we received [DONE] marker - break outer loop to send final response
				if doneReceived {
					break
				}
			}

			// Handle read errors
			if readErr != nil {
				// If context was cancelled/timed out, let defer handle it
				if ctx.Err() != nil {
					return
				}
				if readErr != io.EOF {
					// Non-EOF errors (e.g., connection reset by peer due to TPM throttling)
					// must be reported to the client instead of falling through to send
					// a fake "done" response with truncated audio.
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					provider.logger.Warn("Error reading stream: %v", readErr)
					providerUtils.ProcessAndSendError(ctx, postHookRunner, readErr, responseChan, provider.logger, postHookSpanFinalizer)
					return
				}
				break
			}
		}

		// Send final "done" response only if we received the [DONE] marker from the provider.
		// Without [DONE], the stream ended abnormally (e.g., clean EOF without proper SSE termination).
		if chunkIndex >= 0 && doneReceived {
			finalResponse := schemas.KoutakuSpeechStreamResponse{
				Type: schemas.SpeechStreamResponseTypeDone,
				ExtraFields: schemas.KoutakuResponseExtraFields{
					ChunkIndex: chunkIndex + 1,
					Latency:    time.Since(startTime).Milliseconds(),
				},
			}

			if sendBackRawRequest {
				providerUtils.ParseAndSetRawRequest(&finalResponse.ExtraFields, jsonBody)
			}

			finalResponse.BackfillParams(request)
			ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
			providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, nil, &finalResponse, nil, nil), responseChan, postHookSpanFinalizer)
		} else if chunkIndex >= 0 && !doneReceived {
			provider.logger.Warn("Stream ended without receiving [DONE] marker after %d chunks", chunkIndex+1)
		}

		// Response is released via deferred ReleaseStreamingResponse(resp) above.
	}()

	return responseChan, nil
}

// Transcription is not supported by the Azure provider.
func (provider *AzureProvider) Transcription(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuTranscriptionRequest) (*schemas.KoutakuTranscriptionResponse, *schemas.KoutakuError) {
	apiVersion := key.AzureKeyConfig.APIVersion
	if apiVersion == nil {
		apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
	}

	url := fmt.Sprintf("%s/openai/deployments/%s/audio/transcriptions?api-version=%s", key.AzureKeyConfig.Endpoint.GetValue(), request.Model, apiVersion.GetValue())

	response, err := openai.HandleOpenAITranscriptionRequest(
		ctx,
		provider.client,
		url,
		request,
		key,
		provider.networkConfig.ExtraHeaders,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		nil,
		provider.logger,
	)

	if err != nil {
		return nil, err
	}

	return response, err
}

// TranscriptionStream is not supported by the Azure provider.
func (provider *AzureProvider) TranscriptionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuTranscriptionRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TranscriptionStreamRequest, provider.GetProviderKey())
}

// ImageGeneration performs an Image Generation request to Azure's API.
// It formats the request, sends it to Azure, and processes the response.
// Returns a KoutakuResponse containing the koutaku response or an error if the request fails.
func (provider *AzureProvider) ImageGeneration(ctx *schemas.KoutakuContext, key schemas.Key,
	request *schemas.KoutakuImageGenerationRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	apiVersion := key.AzureKeyConfig.APIVersion
	if apiVersion == nil || apiVersion.GetValue() == "" {
		apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
	}

	endpoint := key.AzureKeyConfig.Endpoint.GetValue()
	if endpoint == "" {
		return nil, providerUtils.NewConfigurationError("endpoint not set")
	}

	response, err := openai.HandleOpenAIImageGenerationRequest(
		ctx,
		provider.client,
		fmt.Sprintf("%s/openai/deployments/%s/images/generations?api-version=%s", endpoint, request.Model, apiVersion.GetValue()),
		request,
		key,
		provider.networkConfig.ExtraHeaders,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.logger,
	)
	if err != nil {
		return nil, err
	}

	return response, err
}

// ImageGenerationStream performs a streaming image generation request to Azure's API.
// It formats the request, sends it to Azure, and processes the response.
// Returns a channel of KoutakuStreamChunk objects or an error if the request fails.
func (provider *AzureProvider) ImageGenerationStream(
	ctx *schemas.KoutakuContext,
	postHookRunner schemas.PostHookRunner,
	postHookSpanFinalizer func(context.Context),
	key schemas.Key,
	request *schemas.KoutakuImageGenerationRequest,
) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	apiVersion := key.AzureKeyConfig.APIVersion
	if apiVersion == nil || apiVersion.GetValue() == "" {
		apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
	}

	endpoint := key.AzureKeyConfig.Endpoint.GetValue()
	if endpoint == "" {
		return nil, providerUtils.NewConfigurationError("endpoint not set")
	}

	url := fmt.Sprintf("%s/openai/deployments/%s/images/generations?api-version=%s", endpoint, request.Model, apiVersion.GetValue())

	authHeader, err := provider.getAzureAuthHeaders(ctx, key, false)
	if err != nil {
		return nil, err
	}

	// Azure is OpenAI-compatible
	return openai.HandleOpenAIImageGenerationStreaming(
		ctx,
		provider.streamingClient,
		url,
		request,
		authHeader,
		provider.networkConfig.ExtraHeaders,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.GetProviderKey(),
		postHookRunner,
		nil,
		nil,
		nil,
		provider.logger,
		postHookSpanFinalizer,
	)

}

// ImageEdit performs an image edit request to Azure's API.
func (provider *AzureProvider) ImageEdit(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageEditRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	apiVersion := key.AzureKeyConfig.APIVersion
	if apiVersion == nil || apiVersion.GetValue() == "" {
		apiVersion = schemas.NewEnvVar(AzureAPIVersionImageEditDefault)
	}

	endpoint := key.AzureKeyConfig.Endpoint.GetValue()
	if endpoint == "" {
		return nil, providerUtils.NewConfigurationError("endpoint not set")
	}

	url := fmt.Sprintf("%s/openai/deployments/%s/images/edits?api-version=%s", endpoint, request.Model, apiVersion.GetValue())
	response, err := openai.HandleOpenAIImageEditRequest(
		ctx,
		provider.client,
		url,
		request,
		key,
		provider.networkConfig.ExtraHeaders,
		false,
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.GetProviderKey(),
		provider.logger,
	)
	if err != nil {
		return nil, err
	}

	return response, err
}

// ImageEditStream performs a streaming image edit request to Azure's API.
func (provider *AzureProvider) ImageEditStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuImageEditRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	apiVersion := key.AzureKeyConfig.APIVersion
	if apiVersion == nil || apiVersion.GetValue() == "" {
		apiVersion = schemas.NewEnvVar(AzureAPIVersionImageEditDefault)
	}

	endpoint := key.AzureKeyConfig.Endpoint.GetValue()
	if endpoint == "" {
		return nil, providerUtils.NewConfigurationError("endpoint not set")
	}

	url := fmt.Sprintf("%s/openai/deployments/%s/images/edits?api-version=%s", endpoint, request.Model, apiVersion.GetValue())

	authHeader, err := provider.getAzureAuthHeaders(ctx, key, false)
	if err != nil {
		return nil, err
	}

	// Azure is OpenAI-compatible
	return openai.HandleOpenAIImageEditStreamRequest(
		ctx,
		provider.streamingClient,
		url,
		request,
		authHeader,
		provider.networkConfig.ExtraHeaders,
		false,
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.GetProviderKey(),
		postHookRunner,
		nil,
		nil,
		nil,
		provider.logger,
		postHookSpanFinalizer,
	)

}

// ImageVariation is not supported by the Azure provider.
func (provider *AzureProvider) ImageVariation(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageVariationRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageVariationRequest, provider.GetProviderKey())
}

// VideoGeneration creates a video using Azure's OpenAI-compatible Sora API.
// This delegates to the OpenAI handler with Azure-specific URL and authentication.
func (provider *AzureProvider) VideoGeneration(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuVideoGenerationRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	endpoint := key.AzureKeyConfig.Endpoint.GetValue()
	if endpoint == "" {
		return nil, providerUtils.NewConfigurationError("endpoint not set")
	}

	// Build Azure URL for OpenAI-compatible video generation endpoint
	url := fmt.Sprintf("%s/openai/v1/videos", endpoint)

	response, koutakuErr := openai.HandleOpenAIVideoGenerationRequest(
		ctx,
		provider.client,
		url,
		request,
		key,
		provider.networkConfig.ExtraHeaders,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.logger,
	)
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	return response, nil
}

// VideoRetrieve retrieves the status of a video from Azure's OpenAI-compatible API.
func (provider *AzureProvider) VideoRetrieve(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuVideoRetrieveRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	providerName := provider.GetProviderKey()
	if request.ID == "" {
		return nil, providerUtils.NewKoutakuOperationError("video_id is required", nil)
	}
	videoID := providerUtils.StripVideoIDProviderSuffix(request.ID, providerName)

	endpoint := key.AzureKeyConfig.Endpoint.GetValue()
	if endpoint == "" {
		return nil, providerUtils.NewConfigurationError("endpoint not set")
	}

	authHeaders, koutakuErr := provider.getAzureAuthHeaders(ctx, key, false)
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	return openai.HandleOpenAIVideoRetrieveRequest(
		ctx,
		provider.client,
		fmt.Sprintf("%s/openai/v1/videos/%s", endpoint, videoID),
		request,
		key,
		provider.networkConfig.ExtraHeaders,
		authHeaders,
		providerName,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.VideoDownload,
		provider.logger,
	)
}

// VideoDownload downloads video content from Azure's OpenAI-compatible API.
func (provider *AzureProvider) VideoDownload(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuVideoDownloadRequest) (*schemas.KoutakuVideoDownloadResponse, *schemas.KoutakuError) {
	providerName := provider.GetProviderKey()

	if request.ID == "" {
		return nil, providerUtils.NewKoutakuOperationError("video_id is required", nil)
	}
	videoID := providerUtils.StripVideoIDProviderSuffix(request.ID, providerName)

	endpoint := key.AzureKeyConfig.Endpoint.GetValue()
	if endpoint == "" {
		return nil, providerUtils.NewConfigurationError("endpoint not set")
	}

	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Set headers
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)

	// Build Azure URL
	url := fmt.Sprintf("%s/openai/v1/videos/%s/content", endpoint, videoID)

	req.SetRequestURI(url)
	req.Header.SetMethod(http.MethodGet)

	// Get authentication headers
	authHeaders, koutakuErr := provider.getAzureAuthHeaders(ctx, key, false)
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	for k, v := range authHeaders {
		req.Header.Set(k, v)
	}

	// Make request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, openai.ParseOpenAIError(resp)
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
	}

	// Get content type from response
	contentType := string(resp.Header.ContentType())
	if contentType == "" {
		// Default to video/mp4 if not specified
		contentType = "video/mp4"
	}

	// Create response
	response := &schemas.KoutakuVideoDownloadResponse{
		VideoID:     request.ID,
		Content:     append([]byte(nil), body...),
		ContentType: contentType,
		ExtraFields: schemas.KoutakuResponseExtraFields{
			Latency: latency.Milliseconds(),
		},
	}

	return response, nil
}

// VideoDelete deletes a video from Azure's OpenAI-compatible API.
func (provider *AzureProvider) VideoDelete(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuVideoDeleteRequest) (*schemas.KoutakuVideoDeleteResponse, *schemas.KoutakuError) {
	providerName := provider.GetProviderKey()

	if request.ID == "" {
		return nil, providerUtils.NewKoutakuOperationError("video_id is required", nil)
	}
	videoID := providerUtils.StripVideoIDProviderSuffix(request.ID, providerName)

	endpoint := key.AzureKeyConfig.Endpoint.GetValue()
	if endpoint == "" {
		return nil, providerUtils.NewConfigurationError("endpoint not set")
	}

	// Build Azure URL
	url := fmt.Sprintf("%s/openai/v1/videos/%s", endpoint, videoID)

	response, koutakuErr := openai.HandleOpenAIVideoDeleteRequest(
		ctx,
		provider.client,
		url,
		videoID,
		key,
		provider.networkConfig.ExtraHeaders,
		providerName,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.logger,
	)
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	return response, nil
}

// VideoList lists videos from Azure's OpenAI-compatible API.
func (provider *AzureProvider) VideoList(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuVideoListRequest) (*schemas.KoutakuVideoListResponse, *schemas.KoutakuError) {
	endpoint := key.AzureKeyConfig.Endpoint.GetValue()
	if endpoint == "" {
		return nil, providerUtils.NewConfigurationError("endpoint not set")
	}

	// Build Azure URL
	baseURL := fmt.Sprintf("%s/openai/v1/videos", endpoint)

	response, koutakuErr := openai.HandleOpenAIVideoListRequest(
		ctx,
		provider.client,
		baseURL,
		request,
		key,
		provider.networkConfig.ExtraHeaders,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.logger,
	)
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	return response, nil
}

// VideoRemix is not supported by Azure provider.
func (provider *AzureProvider) VideoRemix(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoRemixRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoRemixRequest, provider.GetProviderKey())
}

// FileUpload uploads a file to Azure OpenAI.
func (provider *AzureProvider) FileUpload(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuFileUploadRequest) (*schemas.KoutakuFileUploadResponse, *schemas.KoutakuError) {
	if len(request.File) == 0 {
		return nil, providerUtils.NewKoutakuOperationError("file content is required", nil)
	}

	if request.Purpose == "" {
		return nil, providerUtils.NewKoutakuOperationError("purpose is required", nil)
	}

	// Get API version
	apiVersion := key.AzureKeyConfig.APIVersion
	if apiVersion == nil {
		apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
	}

	// Create multipart form data
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Add purpose field
	if err := writer.WriteField("purpose", string(request.Purpose)); err != nil {
		return nil, providerUtils.NewKoutakuOperationError("failed to write purpose field", err)
	}

	// Add file field
	filename := request.Filename
	if filename == "" {
		filename = "file.jsonl"
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, providerUtils.NewKoutakuOperationError("failed to create form file", err)
	}
	if _, err := part.Write(request.File); err != nil {
		return nil, providerUtils.NewKoutakuOperationError("failed to write file content", err)
	}

	if err := writer.Close(); err != nil {
		return nil, providerUtils.NewKoutakuOperationError("failed to close multipart writer", err)
	}

	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Build URL with query params
	baseURL := fmt.Sprintf("%s/openai/files", key.AzureKeyConfig.Endpoint.GetValue())
	values := url.Values{}
	values.Set("api-version", apiVersion.GetValue())
	requestURL := baseURL + "?" + values.Encode()

	// Set headers
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(requestURL)
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType(writer.FormDataContentType())

	// Set Azure authentication
	if err := provider.setAzureAuth(ctx, req, key); err != nil {
		return nil, err
	}

	req.SetBody(buf.Bytes())

	// Make request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK && resp.StatusCode() != fasthttp.StatusCreated {
		return nil, openai.ParseOpenAIError(resp)
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
	}

	var openAIResp openai.OpenAIFileResponse
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &openAIResp, nil, sendBackRawRequest, sendBackRawResponse)
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	return openAIResp.ToKoutakuFileUploadResponse(latency, sendBackRawRequest, sendBackRawResponse, rawRequest, rawResponse), nil
}

// FileList lists files from all provided Azure keys and aggregates results.
// FileList lists files using serial pagination across keys.
// Exhausts all pages from one key before moving to the next.
func (provider *AzureProvider) FileList(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuFileListRequest) (*schemas.KoutakuFileListResponse, *schemas.KoutakuError) {
	if len(keys) == 0 {
		return nil, providerUtils.NewConfigurationError("no Azure keys available for file list operation")
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	// Initialize serial pagination helper
	helper, err := providerUtils.NewSerialListHelper(keys, request.After, provider.logger)
	if err != nil {
		return nil, providerUtils.NewKoutakuOperationError("invalid pagination cursor", err)
	}

	// Get current key to query
	key, nativeCursor, ok := helper.GetCurrentKey()
	if !ok {
		// All keys exhausted
		return &schemas.KoutakuFileListResponse{
			Object:  "list",
			Data:    []schemas.FileObject{},
			HasMore: false,
		}, nil
	}

	// Get API version
	apiVersion := key.AzureKeyConfig.APIVersion
	if apiVersion == nil {
		apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
	}

	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Build URL with query params
	requestURL := fmt.Sprintf("%s/openai/files", key.AzureKeyConfig.Endpoint.GetValue())
	values := url.Values{}
	values.Set("api-version", apiVersion.GetValue())
	if request.Purpose != "" {
		values.Set("purpose", string(request.Purpose))
	}
	// Use native cursor from serial helper
	if nativeCursor != "" {
		values.Set("after", nativeCursor)
	}
	if encodedValues := values.Encode(); encodedValues != "" {
		requestURL += "?" + encodedValues
	}

	// Set headers
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(requestURL)
	req.Header.SetMethod(http.MethodGet)
	req.Header.SetContentType("application/json")

	// Set Azure authentication
	if err := provider.setAzureAuth(ctx, req, key); err != nil {
		return nil, err
	}

	// Make request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, openai.ParseOpenAIError(resp)
	}

	body, decodeErr := providerUtils.CheckAndDecodeBody(resp)
	if decodeErr != nil {
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, decodeErr)
	}

	var openAIResp openai.OpenAIFileListResponse
	_, _, koutakuErr = providerUtils.HandleProviderResponse(body, &openAIResp, nil, sendBackRawRequest, sendBackRawResponse)
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Convert files to Koutaku format
	files := make([]schemas.FileObject, 0, len(openAIResp.Data))
	var lastFileID string
	for _, file := range openAIResp.Data {
		files = append(files, schemas.FileObject{
			ID:            file.ID,
			Object:        file.Object,
			Bytes:         file.Bytes,
			CreatedAt:     file.CreatedAt,
			Filename:      file.Filename,
			Purpose:       schemas.FilePurpose(file.Purpose),
			Status:        openai.ToKoutakuFileStatus(file.Status),
			StatusDetails: file.StatusDetails,
		})
		lastFileID = file.ID
	}

	// Build cursor for next request
	nextCursor, hasMore := helper.BuildNextCursor(openAIResp.HasMore, lastFileID)

	// Convert to Koutaku response
	koutakuResp := &schemas.KoutakuFileListResponse{
		Object:  "list",
		Data:    files,
		HasMore: hasMore,
		ExtraFields: schemas.KoutakuResponseExtraFields{
			Latency: latency.Milliseconds(),
		},
	}
	if nextCursor != "" {
		koutakuResp.After = &nextCursor
	}

	return koutakuResp, nil
}

// FileRetrieve retrieves file metadata from Azure OpenAI by trying each key until found.
func (provider *AzureProvider) FileRetrieve(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuFileRetrieveRequest) (*schemas.KoutakuFileRetrieveResponse, *schemas.KoutakuError) {
	providerName := provider.GetProviderKey()

	if request.FileID == "" {
		return nil, providerUtils.NewKoutakuOperationError("file_id is required", nil)
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	var lastErr *schemas.KoutakuError
	for _, key := range keys {
		// Get API version
		apiVersion := key.AzureKeyConfig.APIVersion
		if apiVersion == nil {
			apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
		}

		// Create request
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		// Build URL with query params
		baseURL := fmt.Sprintf("%s/openai/files/%s", key.AzureKeyConfig.Endpoint.GetValue(), url.PathEscape(request.FileID))
		values := url.Values{}
		values.Set("api-version", apiVersion.GetValue())
		requestURL := baseURL + "?" + values.Encode()

		// Set headers
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(requestURL)
		req.Header.SetMethod(http.MethodGet)
		req.Header.SetContentType("application/json")

		// Set Azure authentication
		if authErr := provider.setAzureAuth(ctx, req, key); authErr != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = authErr
			continue
		}

		// Make request
		latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
		if koutakuErr != nil {
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = koutakuErr
			continue
		}

		// Handle error response
		if resp.StatusCode() != fasthttp.StatusOK {
			lastErr = openai.ParseOpenAIError(resp)
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			continue
		}

		body, err := providerUtils.CheckAndDecodeBody(resp)
		if err != nil {
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
			continue
		}

		var openAIResp openai.OpenAIFileResponse
		rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &openAIResp, nil, sendBackRawRequest, sendBackRawResponse)
		if koutakuErr != nil {
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = koutakuErr
			continue
		}

		wait()
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)

		return openAIResp.ToKoutakuFileRetrieveResponse(providerName, latency, sendBackRawRequest, sendBackRawResponse, rawRequest, rawResponse), nil
	}

	return nil, lastErr
}

// FileDelete deletes a file from Azure OpenAI by trying each key until successful.
func (provider *AzureProvider) FileDelete(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuFileDeleteRequest) (*schemas.KoutakuFileDeleteResponse, *schemas.KoutakuError) {
	if request.FileID == "" {
		return nil, providerUtils.NewKoutakuOperationError("file_id is required", nil)
	}

	if len(keys) == 0 {
		return nil, providerUtils.NewConfigurationError("no Azure keys available for file delete operation")
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	var lastErr *schemas.KoutakuError
	for _, key := range keys {
		// Get API version
		apiVersion := key.AzureKeyConfig.APIVersion
		if apiVersion == nil {
			apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
		}

		// Create request
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		// Build URL with query params
		baseURL := fmt.Sprintf("%s/openai/files/%s", key.AzureKeyConfig.Endpoint.GetValue(), url.PathEscape(request.FileID))
		values := url.Values{}
		values.Set("api-version", apiVersion.GetValue())
		requestURL := baseURL + "?" + values.Encode()

		// Set headers
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(requestURL)
		req.Header.SetMethod(http.MethodDelete)
		req.Header.SetContentType("application/json")

		// Set Azure authentication
		if authErr := provider.setAzureAuth(ctx, req, key); authErr != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = authErr
			continue
		}

		// Make request
		latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
		if koutakuErr != nil {
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = koutakuErr
			continue
		}

		// Handle error response
		if resp.StatusCode() != fasthttp.StatusOK && resp.StatusCode() != fasthttp.StatusNoContent {
			lastErr = openai.ParseOpenAIError(resp)
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			continue
		}

		if resp.StatusCode() == fasthttp.StatusNoContent {
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			return &schemas.KoutakuFileDeleteResponse{
				ID:      request.FileID,
				Object:  "file",
				Deleted: true,
				ExtraFields: schemas.KoutakuResponseExtraFields{
					Latency: latency.Milliseconds(),
				},
			}, nil
		}

		body, err := providerUtils.CheckAndDecodeBody(resp)
		if err != nil {
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
			continue
		}

		var openAIResp openai.OpenAIFileDeleteResponse
		rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &openAIResp, nil, sendBackRawRequest, sendBackRawResponse)
		if koutakuErr != nil {
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = koutakuErr
			continue
		}

		wait()
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)

		result := &schemas.KoutakuFileDeleteResponse{
			ID:      openAIResp.ID,
			Object:  openAIResp.Object,
			Deleted: openAIResp.Deleted,
			ExtraFields: schemas.KoutakuResponseExtraFields{
				Latency: latency.Milliseconds(),
			},
		}

		if sendBackRawRequest {
			result.ExtraFields.RawRequest = rawRequest
		}

		if sendBackRawResponse {
			result.ExtraFields.RawResponse = rawResponse
		}

		return result, nil
	}

	return nil, lastErr
}

// FileContent downloads file content from Azure OpenAI by trying each key until found.
func (provider *AzureProvider) FileContent(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuFileContentRequest) (*schemas.KoutakuFileContentResponse, *schemas.KoutakuError) {
	if request.FileID == "" {
		return nil, providerUtils.NewKoutakuOperationError("file_id is required", nil)
	}

	if len(keys) == 0 {
		return nil, providerUtils.NewConfigurationError("no Azure keys available for file content operation")
	}

	var lastErr *schemas.KoutakuError

	for _, key := range keys {
		// Get API version
		apiVersion := key.AzureKeyConfig.APIVersion
		if apiVersion == nil {
			apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
		}

		// Create request
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		// Build URL with query params
		baseURL := fmt.Sprintf("%s/openai/files/%s/content", key.AzureKeyConfig.Endpoint.GetValue(), url.PathEscape(request.FileID))
		values := url.Values{}
		values.Set("api-version", apiVersion.GetValue())
		requestURL := baseURL + "?" + values.Encode()

		// Set headers
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(requestURL)
		req.Header.SetMethod(http.MethodGet)

		// Set Azure authentication
		if authErr := provider.setAzureAuth(ctx, req, key); authErr != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = authErr
			continue
		}

		// Make request
		latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
		if koutakuErr != nil {
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = koutakuErr
			continue
		}

		// Handle error response
		if resp.StatusCode() != fasthttp.StatusOK {
			lastErr = openai.ParseOpenAIError(resp)
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			continue
		}

		body, err := providerUtils.CheckAndDecodeBody(resp)
		if err != nil {
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
			continue
		}

		// Get content type from response
		contentType := string(resp.Header.ContentType())
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		content := append([]byte(nil), body...)

		wait()
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)

		return &schemas.KoutakuFileContentResponse{
			FileID:      request.FileID,
			Content:     content,
			ContentType: contentType,
			ExtraFields: schemas.KoutakuResponseExtraFields{
				Latency: latency.Milliseconds(),
			},
		}, nil
	}

	return nil, lastErr
}

// BatchCreate creates a new batch job on Azure OpenAI.
// Azure Batch API uses the same format as OpenAI but with Azure-specific URL patterns.
func (provider *AzureProvider) BatchCreate(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuBatchCreateRequest) (*schemas.KoutakuBatchCreateResponse, *schemas.KoutakuError) {
	inputFileID := request.InputFileID

	// If no file_id provided but inline requests are available, upload them first
	if inputFileID == "" && len(request.Requests) > 0 {
		// Convert inline requests to JSONL format
		jsonlData, err := openai.ConvertRequestsToJSONL(request.Requests)
		if err != nil {
			return nil, providerUtils.NewKoutakuOperationError("failed to convert requests to JSONL", err)
		}

		// Upload the file with purpose "batch"
		uploadResp, koutakuErr := provider.FileUpload(ctx, key, &schemas.KoutakuFileUploadRequest{
			File:     jsonlData,
			Filename: "batch_requests.jsonl",
			Purpose:  "batch",
		})
		if koutakuErr != nil {
			return nil, koutakuErr
		}

		inputFileID = uploadResp.ID
	}

	// Validate that we have a file ID (either provided or uploaded)
	if inputFileID == "" {
		return nil, providerUtils.NewKoutakuOperationError("either input_file_id or requests array is required for Azure batch API", nil)
	}

	// Get API version
	apiVersion := key.AzureKeyConfig.APIVersion
	if apiVersion == nil {
		apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
	}

	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Build URL with query params
	baseURL := fmt.Sprintf("%s/openai/batches", key.AzureKeyConfig.Endpoint.GetValue())
	values := url.Values{}
	values.Set("api-version", apiVersion.GetValue())
	requestURL := baseURL + "?" + values.Encode()

	// Set headers
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(requestURL)
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")

	// Set Azure authentication
	if err := provider.setAzureAuth(ctx, req, key); err != nil {
		return nil, err
	}

	// Build request body
	openAIReq := &openai.OpenAIBatchRequest{
		InputFileID:      inputFileID,
		Endpoint:         string(request.Endpoint),
		CompletionWindow: request.CompletionWindow,
		Metadata:         request.Metadata,
	}

	// Set default completion window if not provided
	if openAIReq.CompletionWindow == "" {
		openAIReq.CompletionWindow = "24h"
	}

	jsonData, err := providerUtils.MarshalSorted(openAIReq)
	if err != nil {
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderRequestMarshal, err)
	}
	req.SetBody(jsonData)

	// Make request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK && resp.StatusCode() != fasthttp.StatusCreated {
		return nil, openai.ParseOpenAIError(resp)
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err), jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	var openAIResp openai.OpenAIBatchResponse
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &openAIResp, jsonData, sendBackRawRequest, sendBackRawResponse)
	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, body, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	return openAIResp.ToKoutakuBatchCreateResponse(latency, sendBackRawRequest, sendBackRawResponse, rawRequest, rawResponse), nil
}

// BatchList lists batch jobs from all provided Azure keys and aggregates results.
// BatchList lists batch jobs using serial pagination across keys.
// Exhausts all pages from one key before moving to the next.
func (provider *AzureProvider) BatchList(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuBatchListRequest) (*schemas.KoutakuBatchListResponse, *schemas.KoutakuError) {
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	if len(keys) == 0 {
		return nil, providerUtils.NewConfigurationError("no Azure keys available for batch list operation")
	}

	// Initialize serial pagination helper
	helper, err := providerUtils.NewSerialListHelper(keys, request.After, provider.logger)
	if err != nil {
		return nil, providerUtils.NewKoutakuOperationError("invalid pagination cursor", err)
	}

	// Get current key to query
	key, nativeCursor, ok := helper.GetCurrentKey()
	if !ok {
		// All keys exhausted
		return &schemas.KoutakuBatchListResponse{
			Object:  "list",
			Data:    []schemas.KoutakuBatchRetrieveResponse{},
			HasMore: false,
		}, nil
	}

	// Get API version
	apiVersion := key.AzureKeyConfig.APIVersion
	if apiVersion == nil {
		apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
	}

	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Build URL with query params
	baseURL := fmt.Sprintf("%s/openai/batches", key.AzureKeyConfig.Endpoint.GetValue())
	values := url.Values{}
	values.Set("api-version", apiVersion.GetValue())
	if request.Limit > 0 {
		values.Set("limit", fmt.Sprintf("%d", request.Limit))
	}
	// Use native cursor from serial helper
	if nativeCursor != "" {
		values.Set("after", nativeCursor)
	}
	requestURL := baseURL + "?" + values.Encode()

	// Set headers
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(requestURL)
	req.Header.SetMethod(http.MethodGet)
	req.Header.SetContentType("application/json")

	// Set Azure authentication
	if err := provider.setAzureAuth(ctx, req, key); err != nil {
		return nil, err
	}

	// Make request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, openai.ParseOpenAIError(resp)
	}

	body, decodeErr := providerUtils.CheckAndDecodeBody(resp)
	if decodeErr != nil {
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, decodeErr)
	}

	var openAIResp openai.OpenAIBatchListResponse
	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &openAIResp, nil, sendBackRawRequest, sendBackRawResponse)
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Convert batches to Koutaku format
	batches := make([]schemas.KoutakuBatchRetrieveResponse, 0, len(openAIResp.Data))
	var lastBatchID string
	for _, batch := range openAIResp.Data {
		batches = append(batches, *batch.ToKoutakuBatchRetrieveResponse(latency, sendBackRawRequest, sendBackRawResponse, rawRequest, rawResponse))
		lastBatchID = batch.ID
	}

	// Build cursor for next request
	nextCursor, hasMore := helper.BuildNextCursor(openAIResp.HasMore, lastBatchID)

	// Convert to Koutaku response
	koutakuResp := &schemas.KoutakuBatchListResponse{
		Object:  "list",
		Data:    batches,
		HasMore: hasMore,
		ExtraFields: schemas.KoutakuResponseExtraFields{
			Latency: latency.Milliseconds(),
		},
	}
	if nextCursor != "" {
		koutakuResp.NextCursor = &nextCursor
	}

	return koutakuResp, nil
}

// BatchRetrieve retrieves a specific batch job from Azure OpenAI by trying each key until found.
func (provider *AzureProvider) BatchRetrieve(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuBatchRetrieveRequest) (*schemas.KoutakuBatchRetrieveResponse, *schemas.KoutakuError) {
	if request.BatchID == "" {
		return nil, providerUtils.NewKoutakuOperationError("batch_id is required", nil)
	}

	if len(keys) == 0 {
		return nil, providerUtils.NewConfigurationError("no Azure keys available for batch retrieve operation")
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	var lastErr *schemas.KoutakuError
	for _, key := range keys {
		// Get API version
		apiVersion := key.AzureKeyConfig.APIVersion
		if apiVersion == nil {
			apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
		}

		// Create request
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		// Build URL with query params
		baseURL := fmt.Sprintf("%s/openai/batches/%s", key.AzureKeyConfig.Endpoint.GetValue(), url.PathEscape(request.BatchID))
		values := url.Values{}
		values.Set("api-version", apiVersion.GetValue())
		requestURL := baseURL + "?" + values.Encode()

		// Set headers
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(requestURL)
		req.Header.SetMethod(http.MethodGet)
		req.Header.SetContentType("application/json")

		// Set Azure authentication
		if authErr := provider.setAzureAuth(ctx, req, key); authErr != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = authErr
			continue
		}

		// Make request
		latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
		if koutakuErr != nil {
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = koutakuErr
			continue
		}

		// Handle error response
		if resp.StatusCode() != fasthttp.StatusOK {
			lastErr = openai.ParseOpenAIError(resp)
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			continue
		}

		body, err := providerUtils.CheckAndDecodeBody(resp)
		if err != nil {
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
			continue
		}

		var openAIResp openai.OpenAIBatchResponse
		rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &openAIResp, nil, sendBackRawRequest, sendBackRawResponse)
		if koutakuErr != nil {
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = koutakuErr
			continue
		}

		wait()
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)

		result := openAIResp.ToKoutakuBatchRetrieveResponse(latency, sendBackRawRequest, sendBackRawResponse, rawRequest, rawResponse)
		return result, nil
	}

	return nil, lastErr
}

// BatchCancel cancels a batch job on Azure OpenAI by trying each key until successful.
func (provider *AzureProvider) BatchCancel(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuBatchCancelRequest) (*schemas.KoutakuBatchCancelResponse, *schemas.KoutakuError) {
	if request.BatchID == "" {
		return nil, providerUtils.NewKoutakuOperationError("batch_id is required", nil)
	}

	if len(keys) == 0 {
		return nil, providerUtils.NewConfigurationError("no Azure keys available for batch cancel operation")
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	var lastErr *schemas.KoutakuError
	for _, key := range keys {
		// Get API version
		apiVersion := key.AzureKeyConfig.APIVersion
		if apiVersion == nil {
			apiVersion = schemas.NewEnvVar(AzureAPIVersionDefault)
		}

		// Create request
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		// Build URL with query params
		baseURL := fmt.Sprintf("%s/openai/batches/%s/cancel", key.AzureKeyConfig.Endpoint.GetValue(), url.PathEscape(request.BatchID))
		values := url.Values{}
		values.Set("api-version", apiVersion.GetValue())
		requestURL := baseURL + "?" + values.Encode()

		// Set headers
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(requestURL)
		req.Header.SetMethod(http.MethodPost)
		req.Header.SetContentType("application/json")

		// Set Azure authentication
		if authErr := provider.setAzureAuth(ctx, req, key); authErr != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = authErr
			continue
		}

		// Make request
		latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
		if koutakuErr != nil {
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = koutakuErr
			continue
		}

		// Handle error response
		if resp.StatusCode() != fasthttp.StatusOK {
			lastErr = openai.ParseOpenAIError(resp)
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			continue
		}

		body, err := providerUtils.CheckAndDecodeBody(resp)
		if err != nil {
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
			continue
		}

		var openAIResp openai.OpenAIBatchResponse
		rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &openAIResp, nil, sendBackRawRequest, sendBackRawResponse)
		if koutakuErr != nil {
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = koutakuErr
			continue
		}

		wait()
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)

		result := &schemas.KoutakuBatchCancelResponse{
			ID:           openAIResp.ID,
			Object:       openAIResp.Object,
			Status:       openai.ToKoutakuBatchStatus(openAIResp.Status),
			CancellingAt: openAIResp.CancellingAt,
			CancelledAt:  openAIResp.CancelledAt,
			ExtraFields: schemas.KoutakuResponseExtraFields{
				Latency: latency.Milliseconds(),
			},
		}

		if openAIResp.RequestCounts != nil {
			result.RequestCounts = schemas.BatchRequestCounts{
				Total:     openAIResp.RequestCounts.Total,
				Completed: openAIResp.RequestCounts.Completed,
				Failed:    openAIResp.RequestCounts.Failed,
			}
		}

		if sendBackRawRequest {
			result.ExtraFields.RawRequest = rawRequest
		}

		if sendBackRawResponse {
			result.ExtraFields.RawResponse = rawResponse
		}

		return result, nil
	}

	return nil, lastErr
}

// BatchDelete is not supported by the Azure provider.
func (provider *AzureProvider) BatchDelete(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuBatchDeleteRequest) (*schemas.KoutakuBatchDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchDeleteRequest, schemas.Azure)
}

// BatchResults retrieves batch results from Azure OpenAI by trying each key until successful.
// For Azure (like OpenAI), batch results are obtained by downloading the output_file_id.
func (provider *AzureProvider) BatchResults(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuBatchResultsRequest) (*schemas.KoutakuBatchResultsResponse, *schemas.KoutakuError) {
	// First, retrieve the batch to get the output_file_id (using all keys)
	batchResp, koutakuErr := provider.BatchRetrieve(ctx, keys, &schemas.KoutakuBatchRetrieveRequest{
		Provider: request.Provider,
		BatchID:  request.BatchID,
	})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	if batchResp.OutputFileID == nil || *batchResp.OutputFileID == "" {
		return nil, providerUtils.NewKoutakuOperationError("batch results not available: output_file_id is empty (batch may not be completed)", nil)
	}

	// Download the output file content (using all keys)
	fileContentResp, koutakuErr := provider.FileContent(ctx, keys, &schemas.KoutakuFileContentRequest{
		Provider: request.Provider,
		FileID:   *batchResp.OutputFileID,
	})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Parse JSONL content - each line is a separate result
	var results []schemas.BatchResultItem

	parseResult := providerUtils.ParseJSONL(fileContentResp.Content, func(line []byte) error {
		var resultItem schemas.BatchResultItem
		if err := sonic.Unmarshal(line, &resultItem); err != nil {
			provider.logger.Warn("failed to parse batch result line: %v", err)
			return err
		}
		results = append(results, resultItem)
		return nil
	})

	batchResultsResp := &schemas.KoutakuBatchResultsResponse{
		BatchID: request.BatchID,
		Results: results,
		ExtraFields: schemas.KoutakuResponseExtraFields{
			Latency: fileContentResp.ExtraFields.Latency,
		},
	}

	if len(parseResult.Errors) > 0 {
		batchResultsResp.ExtraFields.ParseErrors = parseResult.Errors
	}

	return batchResultsResp, nil
}

// CountTokens is not supported by the Azure provider.
func (provider *AzureProvider) CountTokens(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuResponsesRequest) (*schemas.KoutakuCountTokensResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.CountTokensRequest, provider.GetProviderKey())
}

// ContainerCreate is not supported by the Azure provider.
func (provider *AzureProvider) ContainerCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuContainerCreateRequest) (*schemas.KoutakuContainerCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerCreateRequest, provider.GetProviderKey())
}

// ContainerList is not supported by the Azure provider.
func (provider *AzureProvider) ContainerList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerListRequest) (*schemas.KoutakuContainerListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerListRequest, provider.GetProviderKey())
}

// ContainerRetrieve is not supported by the Azure provider.
func (provider *AzureProvider) ContainerRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerRetrieveRequest) (*schemas.KoutakuContainerRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerRetrieveRequest, provider.GetProviderKey())
}

// ContainerDelete is not supported by the Azure provider.
func (provider *AzureProvider) ContainerDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerDeleteRequest) (*schemas.KoutakuContainerDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerDeleteRequest, provider.GetProviderKey())
}

// ContainerFileCreate is not supported by the Azure provider.
func (provider *AzureProvider) ContainerFileCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuContainerFileCreateRequest) (*schemas.KoutakuContainerFileCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileCreateRequest, provider.GetProviderKey())
}

// ContainerFileList is not supported by the Azure provider.
func (provider *AzureProvider) ContainerFileList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileListRequest) (*schemas.KoutakuContainerFileListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileListRequest, provider.GetProviderKey())
}

// ContainerFileRetrieve is not supported by the Azure provider.
func (provider *AzureProvider) ContainerFileRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileRetrieveRequest) (*schemas.KoutakuContainerFileRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileRetrieveRequest, provider.GetProviderKey())
}

// ContainerFileContent is not supported by the Azure provider.
func (provider *AzureProvider) ContainerFileContent(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileContentRequest) (*schemas.KoutakuContainerFileContentResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileContentRequest, provider.GetProviderKey())
}

// ContainerFileDelete is not supported by the Azure provider.
func (provider *AzureProvider) ContainerFileDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileDeleteRequest) (*schemas.KoutakuContainerFileDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileDeleteRequest, provider.GetProviderKey())
}

// Passthrough forwards a raw request to Azure's API without any transformation.
func (provider *AzureProvider) Passthrough(
	ctx *schemas.KoutakuContext,
	key schemas.Key,
	req *schemas.KoutakuPassthroughRequest,
) (*schemas.KoutakuPassthroughResponse, *schemas.KoutakuError) {
	url := provider.buildPassthroughURL(key, req.Path, req.RawQuery)

	fasthttpReq := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)
	defer fasthttp.ReleaseRequest(fasthttpReq)

	fasthttpReq.Header.SetMethod(req.Method)
	fasthttpReq.SetRequestURI(url)

	providerUtils.SetExtraHeaders(ctx, fasthttpReq, provider.networkConfig.ExtraHeaders, nil)

	for k, v := range req.SafeHeaders {
		fasthttpReq.Header.Set(k, v)
	}

	authHeaders, koutakuErr := provider.getAzureAuthHeaders(ctx, key, schemas.IsAnthropicModel(req.Model))
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	for k, v := range authHeaders {
		fasthttpReq.Header.Set(k, v)
	}

	fasthttpReq.SetBody(req.Body)

	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, fasthttpReq, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	headers := providerUtils.ExtractProviderResponseHeaders(resp)

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewKoutakuOperationError("failed to decode response body", err)
	}

	// Remove wire-level encoding headers after decoding; downstream should recalculate them for the buffered body.
	for k := range headers {
		if strings.EqualFold(k, "Content-Encoding") || strings.EqualFold(k, "Content-Length") {
			delete(headers, k)
		}
	}

	koutakuResponse := &schemas.KoutakuPassthroughResponse{
		StatusCode: resp.StatusCode(),
		Headers:    headers,
		Body:       body,
	}

	koutakuResponse.ExtraFields.Latency = latency.Milliseconds()

	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		providerUtils.ParseAndSetRawRequestIfJSON(fasthttpReq, &koutakuResponse.ExtraFields)
	}

	return koutakuResponse, nil
}

// PassthroughStream forwards a raw streaming request to Azure's API without any transformation.
// Chunks are piped back as raw bytes, preserving the upstream SSE or binary stream format.
func (provider *AzureProvider) PassthroughStream(
	ctx *schemas.KoutakuContext,
	postHookRunner schemas.PostHookRunner,
	postHookSpanFinalizer func(context.Context),
	key schemas.Key,
	req *schemas.KoutakuPassthroughRequest,
) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	url := provider.buildPassthroughURL(key, req.Path, req.RawQuery)

	fasthttpReq := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	resp.StreamBody = true
	defer fasthttp.ReleaseRequest(fasthttpReq)

	fasthttpReq.Header.SetMethod(req.Method)
	fasthttpReq.SetRequestURI(url)

	providerUtils.SetExtraHeaders(ctx, fasthttpReq, provider.networkConfig.ExtraHeaders, nil)

	for k, v := range req.SafeHeaders {
		fasthttpReq.Header.Set(k, v)
	}

	fasthttpReq.Header.Set("Connection", "close")

	authHeaders, koutakuErr := provider.getAzureAuthHeaders(ctx, key, schemas.IsAnthropicModel(req.Model))
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	for k, v := range authHeaders {
		fasthttpReq.Header.Set(k, v)
	}

	fasthttpReq.SetBody(req.Body)

	activeClient := providerUtils.PrepareResponseStreaming(ctx, provider.streamingClient, resp)
	providerUtils.SetStreamIdleTimeoutIfEmpty(ctx, provider.networkConfig.StreamIdleTimeoutInSeconds)

	startTime := time.Now()

	if err := activeClient.Do(fasthttpReq, resp); err != nil {
		providerUtils.ReleaseStreamingResponse(resp)
		if errors.Is(err, context.Canceled) {
			return nil, &schemas.KoutakuError{
				IsKoutakuError: false,
				Error: &schemas.ErrorField{
					Type:    schemas.Ptr(schemas.RequestCancelled),
					Message: schemas.ErrRequestCancelled,
					Error:   err,
				},
			}
		}
		if errors.Is(err, fasthttp.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
			return nil, providerUtils.NewKoutakuTimeoutError(schemas.ErrProviderRequestTimedOut, err)
		}
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderDoRequest, err)
	}

	headers := providerUtils.ExtractProviderResponseHeaders(resp)

	rawBodyStream := resp.BodyStream()
	if rawBodyStream == nil {
		providerUtils.ReleaseStreamingResponse(resp)
		return nil, providerUtils.NewKoutakuOperationError("provider returned an empty stream body", fmt.Errorf("provider returned an empty stream body"))
	}

	bodyStream, stopIdleTimeout := providerUtils.NewIdleTimeoutReader(rawBodyStream, rawBodyStream, providerUtils.GetStreamIdleTimeout(ctx))
	stopCancellation := providerUtils.SetupStreamCancellation(ctx, rawBodyStream, provider.logger)

	extraFields := schemas.KoutakuResponseExtraFields{}
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		providerUtils.ParseAndSetRawRequestIfJSON(fasthttpReq, &extraFields)
	}
	statusCode := resp.StatusCode()

	ch := make(chan *schemas.KoutakuStreamChunk, schemas.DefaultStreamBufferSize)
	go func() {
		defer providerUtils.EnsureStreamFinalizerCalled(ctx, postHookSpanFinalizer)
		defer func() {
			if ctx.Err() == context.Canceled {
				providerUtils.HandleStreamCancellation(ctx, postHookRunner, ch, provider.logger, postHookSpanFinalizer)
			} else if ctx.Err() == context.DeadlineExceeded {
				providerUtils.HandleStreamTimeout(ctx, postHookRunner, ch, provider.logger, postHookSpanFinalizer)
			}
			close(ch)
		}()
		defer providerUtils.ReleaseStreamingResponse(resp)
		defer stopIdleTimeout()
		defer stopCancellation()

		buf := make([]byte, 4096)
		for {
			n, readErr := bodyStream.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				select {
				case ch <- &schemas.KoutakuStreamChunk{
					KoutakuPassthroughResponse: &schemas.KoutakuPassthroughResponse{
						StatusCode:  statusCode,
						Headers:     headers,
						Body:        chunk,
						ExtraFields: extraFields,
					},
				}:
				case <-ctx.Done():
					return
				}
			}
			if readErr == io.EOF {
				ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
				extraFields.Latency = time.Since(startTime).Milliseconds()
				finalResp := &schemas.KoutakuResponse{
					PassthroughResponse: &schemas.KoutakuPassthroughResponse{
						StatusCode:  statusCode,
						Headers:     headers,
						ExtraFields: extraFields,
					},
				}
				postHookRunner(ctx, finalResp, nil)
				if postHookSpanFinalizer != nil {
					postHookSpanFinalizer(ctx)
				}
				return
			}
			if readErr != nil {
				if ctx.Err() != nil {
					return
				}
				ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
				extraFields.Latency = time.Since(startTime).Milliseconds()
				providerUtils.ProcessAndSendError(ctx, postHookRunner, readErr, ch, provider.logger, postHookSpanFinalizer)
				return
			}
		}
	}()
	return ch, nil
}

// buildPassthroughURL constructs the full Azure URL for a passthrough request.
func (provider *AzureProvider) buildPassthroughURL(key schemas.Key, path, rawQuery string) string {
	endpoint := key.AzureKeyConfig.Endpoint.GetValue()

	// Normalise the responses path emitted by the Azure SDK.
	path = strings.Replace(path, "/openai/responses", "/openai/v1/responses", 1)

	path = strings.Replace(path, "/openai/videos", "/openai/v1/videos", 1)

	var apiVersion string
	switch {
	case strings.Contains(path, "openai/v1/responses"):
		apiVersion = AzureAPIVersionPreview
	case strings.HasPrefix(path, "/anthropic/"),
		strings.HasPrefix(path, "/openai/v1/videos"):
		apiVersion = ""
	default:
		if key.AzureKeyConfig.APIVersion != nil {
			apiVersion = key.AzureKeyConfig.APIVersion.GetValue()
		}
		if apiVersion == "" {
			if values, err := url.ParseQuery(rawQuery); err == nil {
				apiVersion = values.Get("api-version")
			}
		}
	}

	// Strip api-version from the client query to avoid duplicates.
	queryValues, _ := url.ParseQuery(rawQuery)
	queryValues.Del("api-version")
	cleanQuery := queryValues.Encode()

	fullURL := endpoint + path
	switch {
	case cleanQuery != "" && apiVersion != "":
		fullURL += "?" + cleanQuery + "&api-version=" + apiVersion
	case cleanQuery != "":
		fullURL += "?" + cleanQuery
	case apiVersion != "":
		fullURL += "?api-version=" + apiVersion
	}
	return fullURL
}
