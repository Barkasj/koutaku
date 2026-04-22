// Package anthropic implements the Anthropic provider for the Koutaku API.
package anthropic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	schemas "github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

// AnthropicProvider implements the Provider interface for Anthropic's Claude API.
type AnthropicProvider struct {
	logger               schemas.Logger                // Logger for provider operations
	client               *fasthttp.Client              // HTTP client for unary API requests (ReadTimeout bounds overall response)
	streamingClient      *fasthttp.Client              // HTTP client for streaming API requests (no ReadTimeout; idle governed by NewIdleTimeoutReader)
	apiVersion           string                        // API version for the provider
	networkConfig        schemas.NetworkConfig         // Network configuration including extra headers
	sendBackRawRequest   bool                          // Whether to include raw request in KoutakuResponse
	sendBackRawResponse  bool                          // Whether to include raw response in KoutakuResponse
	customProviderConfig *schemas.CustomProviderConfig // Custom provider config
}

// anthropicMessageResponsePool provides a pool for Anthropic chat response objects.
var anthropicMessageResponsePool = sync.Pool{
	New: func() interface{} {
		return &AnthropicMessageResponse{}
	},
}

// anthropicTextResponsePool provides a pool for Anthropic text response objects.
var anthropicTextResponsePool = sync.Pool{
	New: func() interface{} {
		return &AnthropicTextResponse{}
	},
}

// AcquireAnthropicMessageResponse gets an Anthropic chat response from the pool.
func AcquireAnthropicMessageResponse() *AnthropicMessageResponse {
	resp := anthropicMessageResponsePool.Get().(*AnthropicMessageResponse)
	*resp = AnthropicMessageResponse{} // Reset the struct
	return resp
}

// ReleaseAnthropicMessageResponse returns an Anthropic chat response to the pool.
func ReleaseAnthropicMessageResponse(resp *AnthropicMessageResponse) {
	if resp != nil {
		anthropicMessageResponsePool.Put(resp)
	}
}

// acquireAnthropicTextResponse gets an Anthropic text response from the pool.
func acquireAnthropicTextResponse() *AnthropicTextResponse {
	resp := anthropicTextResponsePool.Get().(*AnthropicTextResponse)
	*resp = AnthropicTextResponse{} // Reset the struct
	return resp
}

// releaseAnthropicTextResponse returns an Anthropic text response to the pool.
func releaseAnthropicTextResponse(resp *AnthropicTextResponse) {
	if resp != nil {
		anthropicTextResponsePool.Put(resp)
	}
}

// NewAnthropicProvider creates a new Anthropic provider instance.
// It initializes the HTTP client with the provided configuration and sets up response pools.
// The client is configured with timeouts, concurrency limits, and optional proxy settings.
func NewAnthropicProvider(config *schemas.ProviderConfig, logger schemas.Logger) *AnthropicProvider {
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

	// Pre-warm response pools
	for i := 0; i < config.ConcurrencyAndBufferSize.Concurrency; i++ {
		anthropicTextResponsePool.Put(&AnthropicTextResponse{})
		anthropicMessageResponsePool.Put(&AnthropicMessageResponse{})
	}

	// Configure proxy and retry policy
	client = providerUtils.ConfigureProxy(client, config.ProxyConfig, logger)
	client = providerUtils.ConfigureDialer(client)
	client = providerUtils.ConfigureTLS(client, config.NetworkConfig, logger)
	streamingClient := providerUtils.BuildStreamingClient(client)
	// Set default BaseURL if not provided
	if config.NetworkConfig.BaseURL == "" {
		config.NetworkConfig.BaseURL = "https://api.anthropic.com"
	}
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")

	return &AnthropicProvider{
		logger:               logger,
		client:               client,
		streamingClient:      streamingClient,
		apiVersion:           "2023-06-01",
		networkConfig:        config.NetworkConfig,
		sendBackRawRequest:   config.SendBackRawRequest,
		sendBackRawResponse:  config.SendBackRawResponse,
		customProviderConfig: config.CustomProviderConfig,
	}
}

// GetProviderKey returns the provider identifier for Anthropic.
func (provider *AnthropicProvider) GetProviderKey() schemas.ModelProvider {
	return providerUtils.GetProviderName(schemas.Anthropic, provider.customProviderConfig)
}

// buildRequestURL constructs the full request URL using the provider's configuration.
func (provider *AnthropicProvider) buildRequestURL(ctx *schemas.KoutakuContext, defaultPath string, requestType schemas.RequestType) string {
	path, isCompleteURL := providerUtils.GetRequestPath(ctx, defaultPath, provider.customProviderConfig, requestType)
	if isCompleteURL {
		return path
	}
	return provider.networkConfig.BaseURL + path
}

func setAnthropicRequestBody(ctx *schemas.KoutakuContext, req *fasthttp.Request, body []byte) bool {
	// Keep one request-body path for both modes:
	// - normal mode: send converted JSON/multipart bytes
	// - large payload mode: stream original client body reader
	// Example failure prevented: duplicating large uploads in memory after passthrough
	// was already activated at transport layer.
	usedLargePayloadBody := providerUtils.ApplyLargePayloadRequestBodyWithModelNormalization(ctx, req, schemas.Anthropic)
	if !usedLargePayloadBody {
		req.SetBody(body)
	}
	return usedLargePayloadBody
}

func extractAnthropicResponsesUsageFromPrefetch(data []byte) *schemas.ResponsesResponseUsage {
	node, err := sonic.Get(data, "usage")
	if err != nil {
		return nil
	}
	raw, _ := node.Raw()
	if raw == "" {
		return nil
	}
	var usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	if err := sonic.UnmarshalString(raw, &usage); err != nil {
		return nil
	}
	return &schemas.ResponsesResponseUsage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.InputTokens + usage.OutputTokens,
	}
}

// completeRequest sends a request to Anthropic's API and handles the response.
// It constructs the API URL, sets up authentication, and processes the response.
// Returns the response body or an error if the request fails.
// When large response streaming is activated (KoutakuContextKeyLargeResponseMode set in ctx),
// returns (nil, latency, nil) — callers must check the context flag.
func (provider *AnthropicProvider) completeRequest(ctx *schemas.KoutakuContext, jsonData []byte, url string, key string, requestType schemas.RequestType) ([]byte, time.Duration, map[string]string, *schemas.KoutakuError) {
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

	// Set any extra headers from network config
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(url)
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")

	// Can be empty in case of passthrough or keyless custom provider
	// Here we can avoid this - in case of passthrough completely
	if key != "" && !IsClaudeCodeMaxMode(ctx) {
		req.Header.Set("x-api-key", key)
	}
	req.Header.Set("anthropic-version", provider.apiVersion)

	if betaHeaders := FilterBetaHeadersForProvider(MergeBetaHeaders(provider.networkConfig.ExtraHeaders, ctx), schemas.Anthropic, provider.networkConfig.BetaHeaderOverrides); len(betaHeaders) > 0 {
		req.Header.Set(AnthropicBetaHeader, strings.Join(betaHeaders, ","))
	} else {
		req.Header.Del(AnthropicBetaHeader)
	}

	usedLargePayloadBody := setAnthropicRequestBody(ctx, req, jsonData)

	requestClient := provider.client
	responseThreshold, _ := ctx.Value(schemas.KoutakuContextKeyLargeResponseThreshold).(int64)
	isCountTokens := requestType == schemas.CountTokensRequest
	// CountTokens responses are always tiny — skip streaming client so the response
	// is buffered normally (same approach as OpenAI and Gemini count_tokens handlers).
	if responseThreshold > 0 && !isCountTokens {
		resp.StreamBody = true
		requestClient = providerUtils.BuildLargeResponseClient(provider.client, responseThreshold)
	}

	// Send the request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, requestClient, req, resp)
	defer wait()
	if usedLargePayloadBody {
		providerUtils.DrainLargePayloadRemainder(ctx)
	}
	if koutakuErr != nil {
		return nil, latency, nil, koutakuErr
	}

	// Extract provider response headers before status check so error responses also forward them
	providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)

	// Handle error response — materialize stream body for error parsing
	if resp.StatusCode() != fasthttp.StatusOK {
		providerUtils.MaterializeStreamErrorBody(ctx, resp)
		provider.logger.Debug("error from %s provider: %s", provider.GetProviderKey(), string(resp.Body()))
		return nil, latency, providerResponseHeaders, parseAnthropicError(resp)
	}

	// CountTokens uses buffered response (streaming skipped above) — decode directly.
	if isCountTokens {
		body, err := providerUtils.CheckAndDecodeBody(resp)
		if err != nil {
			return nil, latency, providerResponseHeaders, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
		}
		return body, latency, providerResponseHeaders, nil
	}

	// Delegate large response detection + normal buffered path to shared utility
	body, isLarge, respErr := providerUtils.FinalizeResponseWithLargeDetection(ctx, resp, provider.logger)
	if respErr != nil {
		return nil, latency, providerResponseHeaders, respErr
	}
	if isLarge {
		respOwned = false
		return nil, latency, providerResponseHeaders, nil
	}
	return body, latency, providerResponseHeaders, nil
}

// listModelsByKey performs a list models request for a single key.
// Returns the response and latency, or an error if the request fails.
func (provider *AnthropicProvider) listModelsByKey(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuListModelsRequest) (*schemas.KoutakuListModelsResponse, *schemas.KoutakuError) {
	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Set any extra headers from network config
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)

	// Build URL using centralized URL construction
	req.SetRequestURI(provider.buildRequestURL(ctx, fmt.Sprintf("/v1/models?limit=%d", schemas.DefaultPageSize), schemas.ListModelsRequest))
	req.Header.SetMethod(http.MethodGet)
	req.Header.SetContentType("application/json")
	if key.Value.GetValue() != "" {
		req.Header.Set("x-api-key", key.Value.GetValue())
	}
	req.Header.Set("anthropic-version", provider.apiVersion)

	// Make request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Store provider response headers in context before status check so error responses also forward them
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, parseAnthropicError(resp)
	}

	// Parse Anthropic's response
	var anthropicResponse AnthropicListModelsResponse
	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(resp.Body(), &anthropicResponse, nil, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Create final response
	response := anthropicResponse.ToKoutakuListModelsResponse(provider.GetProviderKey(), key.Models, key.BlacklistedModels, key.Aliases, request.Unfiltered)
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

// ListModels performs a list models request to Anthropic's API.
// It fetches models using all provided keys and aggregates the results.
// Uses a best-effort approach: continues with remaining keys even if some fail.
// Requests are made concurrently for improved performance.
func (provider *AnthropicProvider) ListModels(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuListModelsRequest) (*schemas.KoutakuListModelsResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.ListModelsRequest); err != nil {
		return nil, err
	}
	if provider.customProviderConfig != nil && provider.customProviderConfig.IsKeyLess {
		return providerUtils.HandleKeylessListModelsRequest(schemas.Anthropic, func() (*schemas.KoutakuListModelsResponse, *schemas.KoutakuError) {
			return provider.listModelsByKey(ctx, schemas.Key{}, request)
		})
	}
	return providerUtils.HandleMultipleListModelsRequests(
		ctx,
		keys,
		request,
		provider.listModelsByKey,
	)
}

// TextCompletion performs a text completion request to Anthropic's API.
// It formats the request, sends it to Anthropic, and processes the response.
// Returns a KoutakuResponse containing the completion results or an error if the request fails.
func (provider *AnthropicProvider) TextCompletion(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuTextCompletionRequest) (*schemas.KoutakuTextCompletionResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.TextCompletionRequest); err != nil {
		return nil, err
	}

	// Convert to Anthropic format using the centralized converter
	jsonData, err := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToAnthropicTextCompletionRequest(request), nil
		})
	if err != nil {
		return nil, err
	}

	// Use struct directly for JSON marshaling (no beta headers for text completion)
	responseBody, latency, providerResponseHeaders, err := provider.completeRequest(ctx, jsonData, provider.buildRequestURL(ctx, "/v1/complete", schemas.TextCompletionRequest), key.Value.GetValue(), schemas.TextCompletionRequest)
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

	// Create response object from pool
	response := acquireAnthropicTextResponse()
	defer releaseAnthropicTextResponse(response)

	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(responseBody, response, jsonData, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	koutakuResponse := response.ToKoutakuTextCompletionResponse()

	// Set ExtraFields
	koutakuResponse.ExtraFields.Latency = latency.Milliseconds()
	koutakuResponse.ExtraFields.ProviderResponseHeaders = providerResponseHeaders

	// Set raw request if enabled
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		koutakuResponse.ExtraFields.RawRequest = rawRequest
	}

	// Set raw response if enabled
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		koutakuResponse.ExtraFields.RawResponse = rawResponse
	}

	return koutakuResponse, nil
}

// TextCompletionStream performs a streaming text completion request to Anthropic's API.
// It formats the request, sends it to Anthropic, and processes the response.
// Returns a channel of KoutakuStreamChunk objects or an error if the request fails.
func (provider *AnthropicProvider) TextCompletionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuTextCompletionRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TextCompletionStreamRequest, provider.GetProviderKey())
}

// ChatCompletion performs a chat completion request to Anthropic's API.
// It formats the request, sends it to Anthropic, and processes the response.
// Returns a KoutakuResponse containing the completion results or an error if the request fails.
func (provider *AnthropicProvider) ChatCompletion(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuChatRequest) (*schemas.KoutakuChatResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.ChatCompletionRequest); err != nil {
		return nil, err
	}
	// Convert to Anthropic format and get required beta headers
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			anthropicReq, convErr := ToAnthropicChatRequest(ctx, request)
			if convErr != nil {
				return nil, convErr
			}
			AddMissingBetaHeadersToContext(ctx, anthropicReq, schemas.Anthropic)
			return anthropicReq, nil
		})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// On the raw-body passthrough path, the typed-struct StripUnsupportedAnthropicFields
	// was not invoked. Apply the JSON-level sanitizer for behavioural parity so
	// unsupported request-level and tool-level fields don't leak to providers that
	// would reject them.
	if useRawBody, ok := ctx.Value(schemas.KoutakuContextKeyUseRawRequestBody).(bool); ok && useRawBody {
		// Feature gating keyed to schemas.Anthropic (not provider.GetProviderKey())
		// so custom Anthropic aliases get the same feature lookup as the typed
		// path above (line 445), keeping raw and typed behavior in lockstep.
		sanitized, rawErr := stripUnsupportedFieldsFromRawBody(jsonData, schemas.Anthropic, request.Model)
		if rawErr != nil {
			return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderRequestMarshal, rawErr)
		}
		jsonData = sanitized
		// Auto-inject matching anthropic-beta headers for fields the sanitizer
		// preserved. Probe-unmarshal reuses the typed path's header walker so
		// the two paths stay in lockstep.
		var probe AnthropicMessageRequest
		if err := schemas.Unmarshal(jsonData, &probe); err == nil {
			AddMissingBetaHeadersToContext(ctx, &probe, schemas.Anthropic)
		}
	}

	// Use struct directly for JSON marshaling
	responseBody, latency, providerResponseHeaders, err := provider.completeRequest(ctx, jsonData, provider.buildRequestURL(ctx, "/v1/messages", schemas.ChatCompletionRequest), key.Value.GetValue(), schemas.ChatCompletionRequest)
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

	// Create response object from pool
	response := AcquireAnthropicMessageResponse()
	defer ReleaseAnthropicMessageResponse(response)

	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(responseBody, response, jsonData, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}
	// Create final response
	koutakuResponse := response.ToKoutakuChatResponse(ctx)

	// Set ExtraFields
	koutakuResponse.ExtraFields.Latency = latency.Milliseconds()
	koutakuResponse.ExtraFields.ProviderResponseHeaders = providerResponseHeaders

	// Set raw request if enabled
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		koutakuResponse.ExtraFields.RawRequest = rawRequest
	}

	// Set raw response if enabled
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		koutakuResponse.ExtraFields.RawResponse = rawResponse
	}

	return koutakuResponse, nil
}

// ChatCompletionStream performs a streaming chat completion request to the Anthropic API.
// It supports real-time streaming of responses using Server-Sent Events (SSE).
// Returns a channel containing KoutakuStreamChunk objects representing the stream or an error if the request fails.
func (provider *AnthropicProvider) ChatCompletionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuChatRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.ChatCompletionStreamRequest); err != nil {
		return nil, err
	}

	// Convert to Anthropic format and get required beta headers
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			anthropicReq, convErr := ToAnthropicChatRequest(ctx, request)
			if convErr != nil {
				return nil, convErr
			}
			anthropicReq.Stream = schemas.Ptr(true)
			AddMissingBetaHeadersToContext(ctx, anthropicReq, schemas.Anthropic)
			return anthropicReq, nil
		})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// On the raw-body passthrough path, the typed-struct StripUnsupportedAnthropicFields
	// was not invoked. Apply the JSON-level sanitizer for behavioural parity.
	if useRawBody, ok := ctx.Value(schemas.KoutakuContextKeyUseRawRequestBody).(bool); ok && useRawBody {
		// Feature gating keyed to schemas.Anthropic (not provider.GetProviderKey())
		// to keep raw and typed paths in lockstep on custom aliases — mirrors
		// the typed path's hardcoded schemas.Anthropic at line 548.
		sanitized, rawErr := stripUnsupportedFieldsFromRawBody(jsonData, schemas.Anthropic, request.Model)
		if rawErr != nil {
			return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderRequestMarshal, rawErr)
		}
		jsonData = sanitized
		// Auto-inject matching anthropic-beta headers for fields the sanitizer
		// preserved. Probe-unmarshal reuses the typed path's header walker.
		var probe AnthropicMessageRequest
		if err := schemas.Unmarshal(jsonData, &probe); err == nil {
			AddMissingBetaHeadersToContext(ctx, &probe, schemas.Anthropic)
		}
	}

	// Prepare Anthropic headers
	headers := map[string]string{
		"Content-Type":      "application/json",
		"anthropic-version": provider.apiVersion,
		"Accept":            "text/event-stream",
		"Cache-Control":     "no-cache",
	}

	if key.Value.GetValue() != "" && !IsClaudeCodeMaxMode(ctx) {
		headers["x-api-key"] = key.Value.GetValue()
	}

	providerUtils.SetStreamIdleTimeoutIfEmpty(ctx, provider.networkConfig.StreamIdleTimeoutInSeconds)

	// Use shared Anthropic streaming logic
	return HandleAnthropicChatCompletionStreaming(
		ctx,
		provider.streamingClient,
		provider.buildRequestURL(ctx, "/v1/messages", schemas.ChatCompletionStreamRequest),
		jsonData,
		headers,
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
}

// HandleAnthropicChatCompletionStreaming handles streaming for Anthropic-compatible APIs.
// This shared function reduces code duplication between providers that use the same SSE event format.
func HandleAnthropicChatCompletionStreaming(
	ctx *schemas.KoutakuContext,
	client *fasthttp.Client,
	url string,
	jsonBody []byte,
	headers map[string]string,
	extraHeaders map[string]string,
	betaHeaderOverrides map[string]bool,
	sendBackRawRequest bool,
	sendBackRawResponse bool,
	providerName schemas.ModelProvider,
	postHookRunner schemas.PostHookRunner,
	postResponseConverter func(*schemas.KoutakuChatResponse) *schemas.KoutakuChatResponse,
	logger schemas.Logger,
	postHookSpanFinalizer func(context.Context),
) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	resp.StreamBody = true // Initialize for streaming
	defer fasthttp.ReleaseRequest(req)

	req.Header.SetMethod(http.MethodPost)
	req.SetRequestURI(url)
	req.Header.SetContentType("application/json")

	providerUtils.SetExtraHeaders(ctx, req, extraHeaders, []string{AnthropicBetaHeader})

	if betaHeaders := FilterBetaHeadersForProvider(MergeBetaHeaders(extraHeaders, ctx), providerName, betaHeaderOverrides); len(betaHeaders) > 0 {
		req.Header.Set(AnthropicBetaHeader, strings.Join(betaHeaders, ","))
	} else {
		req.Header.Del(AnthropicBetaHeader)
	}

	for key, value := range headers {
		req.Header.Set(key, value)
	}

	usedLargePayloadBody := setAnthropicRequestBody(ctx, req, jsonBody)

	// Use streaming-aware client when large payload optimization is active — ensures
	// MaxResponseBodySize > 0 so ErrBodyTooLarge triggers StreamBody for Content-Length responses.
	activeClient := providerUtils.PrepareResponseStreaming(ctx, client, resp)

	// Make the request
	err := activeClient.Do(req, resp)
	if usedLargePayloadBody {
		providerUtils.DrainLargePayloadRemainder(ctx)
	}
	if err != nil {
		defer providerUtils.ReleaseStreamingResponse(resp)
		if errors.Is(err, context.Canceled) {
			return nil, providerUtils.EnrichError(ctx, &schemas.KoutakuError{
				IsKoutakuError: false,
				Error: &schemas.ErrorField{
					Type:    schemas.Ptr(schemas.RequestCancelled),
					Message: schemas.ErrRequestCancelled,
					Error:   err,
				},
			}, jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
		}
		if errors.Is(err, fasthttp.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
			return nil, providerUtils.EnrichError(ctx, providerUtils.NewKoutakuTimeoutError(schemas.ErrProviderRequestTimedOut, err), jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
		}
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewKoutakuOperationError(schemas.ErrProviderDoRequest, err), jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
	}

	// Store provider response headers in context before status check so error responses also forward them
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

	// Check for HTTP errors
	if resp.StatusCode() != fasthttp.StatusOK {
		defer providerUtils.ReleaseStreamingResponse(resp)
		return nil, providerUtils.EnrichError(ctx, parseAnthropicError(resp), jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
	}

	// Large payload streaming passthrough — pipe raw upstream SSE to client
	if providerUtils.SetupStreamingPassthrough(ctx, resp) {
		responseChan := make(chan *schemas.KoutakuStreamChunk)
		close(responseChan)
		return responseChan, nil
	}

	// Create response channel
	responseChan := make(chan *schemas.KoutakuStreamChunk, schemas.DefaultStreamBufferSize)

	// Start streaming in a goroutine
	go func() {
		defer providerUtils.EnsureStreamFinalizerCalled(ctx, postHookSpanFinalizer)
		defer func() {
			if ctx.Err() == context.Canceled {
				providerUtils.HandleStreamCancellation(ctx, postHookRunner, responseChan, logger, postHookSpanFinalizer)
			} else if ctx.Err() == context.DeadlineExceeded {
				providerUtils.HandleStreamTimeout(ctx, postHookRunner, responseChan, logger, postHookSpanFinalizer)
			}
			close(responseChan)
		}()
		defer providerUtils.ReleaseStreamingResponse(resp)

		if resp.BodyStream() == nil {
			koutakuErr := providerUtils.NewKoutakuOperationError(
				"Provider returned an empty response",
				fmt.Errorf("provider returned an empty response"),
			)
			ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
			providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, providerUtils.EnrichError(ctx, koutakuErr, jsonBody, nil, sendBackRawRequest, sendBackRawResponse), responseChan, logger, postHookSpanFinalizer)
			return
		}

		// Decompress gzip-encoded streams transparently (no-op for non-gzip)
		reader, releaseGzip := providerUtils.DecompressStreamBody(resp)
		defer releaseGzip()

		// Wrap reader with idle timeout to detect stalled streams.
		reader, stopIdleTimeout := providerUtils.NewIdleTimeoutReader(reader, resp.BodyStream(), providerUtils.GetStreamIdleTimeout(ctx))
		defer stopIdleTimeout()

		// Setup cancellation handler to close the raw network stream on ctx cancellation,
		// which immediately unblocks any in-progress read (including reads blocked inside a gzip decompression layer).
		stopCancellation := providerUtils.SetupStreamCancellation(ctx, resp.BodyStream(), logger)
		defer stopCancellation()

		sseReader := providerUtils.GetSSEEventReader(ctx, reader)

		chunkIndex := 0

		startTime := time.Now()
		lastChunkTime := startTime

		// Track minimal state needed for response format
		var messageID string
		var modelName string
		var finishReason *string

		usage := &schemas.KoutakuLLMUsage{}

		// Check for structured output tool name and track state
		var structuredOutputToolName string
		var isAccumulatingStructuredOutput bool
		if toolName, ok := ctx.Value(schemas.KoutakuContextKeyStructuredOutputToolName).(string); ok {
			structuredOutputToolName = toolName
		}

		// Per-response tool-call index state
		streamState := NewAnthropicStreamState()

		for {
			// If context was cancelled/timed out, let defer handle it
			if ctx.Err() != nil {
				return
			}
			eventType, eventDataBytes, readErr := sseReader.ReadEvent()
			if readErr != nil {
				if readErr != io.EOF {
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					logger.Warn("Error reading %s stream: %v", providerName, readErr)
					providerUtils.ProcessAndSendError(ctx, postHookRunner, readErr, responseChan, logger, postHookSpanFinalizer)
					return
				}
				break
			}
			eventData := string(eventDataBytes)
			if eventType == "" || eventData == "" {
				continue
			}
			var event AnthropicStreamEvent
			if err := sonic.Unmarshal([]byte(eventData), &event); err != nil {
				logger.Warn("Failed to parse message_start event: %v", err)
				continue
			}
			if event.Type == AnthropicStreamEventTypeMessageStart && event.Message != nil && event.Message.ID != "" {
				messageID = event.Message.ID
			}
			// Check for usage in both top-level event.Usage and nested event.Message.Usage
			// message_start events have usage nested in message.usage, while message_delta has it at top level
			var usageToProcess *AnthropicUsage
			if event.Usage != nil {
				usageToProcess = event.Usage
			} else if event.Message != nil && event.Message.Usage != nil {
				usageToProcess = event.Message.Usage
			}
			if usageToProcess != nil {
				// Collect usage information and send at the end of the stream
				// Here in some cases usage comes before final message
				// So we need to check if the response.Usage is nil and then if usage != nil
				// then add up all tokens
				if usageToProcess.InputTokens > usage.PromptTokens {
					usage.PromptTokens = usageToProcess.InputTokens
				}
				if usageToProcess.OutputTokens > usage.CompletionTokens {
					usage.CompletionTokens = usageToProcess.OutputTokens
				}
				calculatedTotal := usage.PromptTokens + usage.CompletionTokens
				if calculatedTotal > usage.TotalTokens {
					usage.TotalTokens = calculatedTotal
				}
				// Handle cached tokens if present
				if usageToProcess.CacheReadInputTokens > 0 {
					if usage.PromptTokensDetails == nil {
						usage.PromptTokensDetails = &schemas.ChatPromptTokensDetails{}
					}
					if usageToProcess.CacheReadInputTokens > usage.PromptTokensDetails.CachedReadTokens {
						usage.PromptTokensDetails.CachedReadTokens = usageToProcess.CacheReadInputTokens
					}
				}
				if usageToProcess.CacheCreationInputTokens > 0 {
					if usage.PromptTokensDetails == nil {
						usage.PromptTokensDetails = &schemas.ChatPromptTokensDetails{}
					}
					if usageToProcess.CacheCreationInputTokens > usage.PromptTokensDetails.CachedWriteTokens {
						usage.PromptTokensDetails.CachedWriteTokens = usageToProcess.CacheCreationInputTokens
					}
				}
			}
			if event.Message != nil {
				modelName = event.Message.Model
			}

			// Extract finish reason from event delta
			if event.Delta != nil && event.Delta.StopReason != nil {
				mappedReason := ConvertAnthropicFinishReasonToKoutaku(*event.Delta.StopReason)
				finishReason = &mappedReason

				// Override finish reason for structured output
				// When structured output is used, tool_use stop reason should appear as "stop" to the client
				if structuredOutputToolName != "" && *finishReason == string(schemas.KoutakuFinishReasonToolCalls) {
					stopReason := string(schemas.KoutakuFinishReasonStop)
					finishReason = &stopReason
				}
			}

			// Handle structured output: intercept tool calls for the structured output tool
			// and convert them to content instead of forwarding as tool calls
			if structuredOutputToolName != "" {
				// Check for tool use start event
				if event.Type == AnthropicStreamEventTypeContentBlockStart {
					if event.ContentBlock != nil && event.ContentBlock.Type == AnthropicContentBlockTypeToolUse {
						if event.ContentBlock.Name != nil && *event.ContentBlock.Name == structuredOutputToolName {
							isAccumulatingStructuredOutput = true
							continue
						}
					}
				}

				// Check for tool use delta event
				if event.Type == AnthropicStreamEventTypeContentBlockDelta && isAccumulatingStructuredOutput {
					if event.Delta != nil && event.Delta.Type == AnthropicStreamDeltaTypeInputJSON && event.Delta.PartialJSON != nil {
						// Convert tool use delta to content delta
						content := *event.Delta.PartialJSON
						response := &schemas.KoutakuChatResponse{
							ID:     messageID,
							Object: "chat.completion.chunk",
							Choices: []schemas.KoutakuResponseChoice{
								{
									Index: 0,
									ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
										Delta: &schemas.ChatStreamResponseChoiceDelta{
											Content: &content,
										},
									},
								},
							},
							ExtraFields: schemas.KoutakuResponseExtraFields{
								ChunkIndex: chunkIndex,
								Latency:    time.Since(lastChunkTime).Milliseconds(),
							},
						}
						lastChunkTime = time.Now()
						chunkIndex++

						if sendBackRawResponse {
							response.ExtraFields.RawResponse = eventData
						}

						providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetKoutakuResponseForStreamResponse(nil, response, nil, nil, nil, nil), responseChan, postHookSpanFinalizer)
						continue
					}
				}

				// Check for content block stop
				if event.Type == AnthropicStreamEventTypeContentBlockStop && isAccumulatingStructuredOutput {
					isAccumulatingStructuredOutput = false
					continue
				}
			}

			response, koutakuErr, isLastChunk := event.ToKoutakuChatCompletionStream(ctx, structuredOutputToolName, streamState)
			if koutakuErr != nil {
				ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
				providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, koutakuErr, responseChan, logger, postHookSpanFinalizer)
				break
			}
			if response != nil {
				response.ExtraFields = schemas.KoutakuResponseExtraFields{
					ChunkIndex: chunkIndex,
					Latency:    time.Since(lastChunkTime).Milliseconds(),
				}
				if postResponseConverter != nil {
					response = postResponseConverter(response)
					if response == nil {
						logger.Warn("postResponseConverter returned nil; skipping chunk")
						continue
					}
				}
				response.ID = messageID
				lastChunkTime = time.Now()
				chunkIndex++

				if sendBackRawResponse {
					response.ExtraFields.RawResponse = eventData
				}

				providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetKoutakuResponseForStreamResponse(nil, response, nil, nil, nil, nil), responseChan, postHookSpanFinalizer)
			}
			if isLastChunk {
				break
			}
		}
		if usage.PromptTokensDetails != nil {
			usage.PromptTokens = usage.PromptTokens + usage.PromptTokensDetails.CachedReadTokens + usage.PromptTokensDetails.CachedWriteTokens
			usage.TotalTokens = usage.TotalTokens + usage.PromptTokensDetails.CachedReadTokens + usage.PromptTokensDetails.CachedWriteTokens
		}
		response := providerUtils.CreateKoutakuChatCompletionChunkResponse(messageID, usage, finishReason, chunkIndex, modelName, 0)
		if postResponseConverter != nil {
			response = postResponseConverter(response)
			if response == nil {
				logger.Warn("postResponseConverter returned nil; skipping chunk")
				// Setting error on the context to signal to the defer that we need to close the stream
				ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
				return
			}
		}
		// Set raw request if enabled
		if sendBackRawRequest {
			providerUtils.ParseAndSetRawRequest(&response.ExtraFields, jsonBody)
		}
		response.ExtraFields.Latency = time.Since(startTime).Milliseconds()
		ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
		providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetKoutakuResponseForStreamResponse(nil, response, nil, nil, nil, nil), responseChan, postHookSpanFinalizer)
	}()

	return responseChan, nil
}

// Responses performs a chat completion request to Anthropic's API.
// It formats the request, sends it to Anthropic, and processes the response.
// Returns a KoutakuResponse containing the completion results or an error if the request fails.
func (provider *AnthropicProvider) Responses(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuResponsesRequest) (*schemas.KoutakuResponsesResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.ResponsesRequest); err != nil {
		return nil, err
	}
	jsonBody, err := getRequestBodyForResponses(ctx, request, false, nil)
	if err != nil {
		return nil, err
	}

	responseBody, latency, providerResponseHeaders, err := provider.completeRequest(ctx, jsonBody, provider.buildRequestURL(ctx, "/v1/messages", schemas.ResponsesRequest), key.Value.GetValue(), schemas.ResponsesRequest)
	if providerResponseHeaders != nil {
		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)
	}
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonBody, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Large response mode: return lightweight response with usage from preview for plugin pipeline.
	if isLargeResp, _ := ctx.Value(schemas.KoutakuContextKeyLargeResponseMode).(bool); isLargeResp {
		preview, _ := ctx.Value(schemas.KoutakuContextKeyLargePayloadResponsePreview).(string)
		return &schemas.KoutakuResponsesResponse{
			ID:        schemas.Ptr("resp_" + providerUtils.GetRandomString(50)),
			Object:    "response",
			CreatedAt: int(time.Now().Unix()),
			Model:     request.Model,
			Usage:     extractAnthropicResponsesUsageFromPrefetch([]byte(preview)),
			ExtraFields: schemas.KoutakuResponseExtraFields{
				Latency:                 latency.Milliseconds(),
				ProviderResponseHeaders: providerResponseHeaders,
			},
		}, nil
	}

	// Create response object from pool
	response := AcquireAnthropicMessageResponse()
	defer ReleaseAnthropicMessageResponse(response)

	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(responseBody, response, jsonBody, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonBody, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Create final response
	koutakuResponse := response.ToKoutakuResponsesResponse(ctx)

	// Set ExtraFields
	koutakuResponse.ExtraFields.Latency = latency.Milliseconds()
	koutakuResponse.ExtraFields.ProviderResponseHeaders = providerResponseHeaders

	// Set raw request if enabled
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		koutakuResponse.ExtraFields.RawRequest = rawRequest
	}

	// Set raw response if enabled
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		koutakuResponse.ExtraFields.RawResponse = rawResponse
	}

	return koutakuResponse, nil
}

// ResponsesStream performs a streaming responses request to the Anthropic API.
func (provider *AnthropicProvider) ResponsesStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuResponsesRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.ResponsesStreamRequest); err != nil {
		return nil, err
	}

	// Convert to Anthropic format using the centralized converter
	jsonBody, err := getRequestBodyForResponses(ctx, request, true, nil)
	if err != nil {
		return nil, err
	}

	// Prepare Anthropic headers
	headers := map[string]string{
		"Content-Type":      "application/json",
		"anthropic-version": provider.apiVersion,
		"Accept":            "text/event-stream",
		"Cache-Control":     "no-cache",
	}

	if key.Value.GetValue() != "" && !IsClaudeCodeMaxMode(ctx) {
		headers["x-api-key"] = key.Value.GetValue()
	}

	providerUtils.SetStreamIdleTimeoutIfEmpty(ctx, provider.networkConfig.StreamIdleTimeoutInSeconds)

	return HandleAnthropicResponsesStream(
		ctx,
		provider.streamingClient,
		provider.buildRequestURL(ctx, "/v1/messages", schemas.ResponsesStreamRequest),
		jsonBody,
		headers,
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
}

// HandleAnthropicResponsesStream handles streaming for Anthropic-compatible APIs.
// This shared function reduces code duplication between providers that use the same SSE event format.
func HandleAnthropicResponsesStream(
	ctx *schemas.KoutakuContext,
	client *fasthttp.Client,
	url string,
	jsonBody []byte,
	headers map[string]string,
	extraHeaders map[string]string,
	betaHeaderOverrides map[string]bool,
	sendBackRawRequest bool,
	sendBackRawResponse bool,
	providerName schemas.ModelProvider,
	postHookRunner schemas.PostHookRunner,
	postResponseConverter func(*schemas.KoutakuResponsesStreamResponse) *schemas.KoutakuResponsesStreamResponse,
	logger schemas.Logger,
	postHookSpanFinalizer func(context.Context),
) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	resp.StreamBody = true
	defer fasthttp.ReleaseRequest(req)

	req.Header.SetMethod(http.MethodPost)
	req.SetRequestURI(url)
	req.Header.SetContentType("application/json")

	providerUtils.SetExtraHeaders(ctx, req, extraHeaders, []string{AnthropicBetaHeader})

	if betaHeaders := FilterBetaHeadersForProvider(MergeBetaHeaders(extraHeaders, ctx), providerName, betaHeaderOverrides); len(betaHeaders) > 0 {
		req.Header.Set(AnthropicBetaHeader, strings.Join(betaHeaders, ","))
	} else {
		req.Header.Del(AnthropicBetaHeader)
	}

	// Set auth/static headers, applied after extra headers so they always win
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	// Set body
	usedLargePayloadBody := setAnthropicRequestBody(ctx, req, jsonBody)

	// Use streaming-aware client when large payload optimization is active — ensures
	// MaxResponseBodySize > 0 so ErrBodyTooLarge triggers StreamBody for Content-Length responses.
	activeClient := providerUtils.PrepareResponseStreaming(ctx, client, resp)

	// Make the request
	err := activeClient.Do(req, resp)
	if usedLargePayloadBody {
		providerUtils.DrainLargePayloadRemainder(ctx)
	}
	if err != nil {
		defer providerUtils.ReleaseStreamingResponse(resp)
		if errors.Is(err, context.Canceled) {
			return nil, providerUtils.EnrichError(ctx, &schemas.KoutakuError{
				IsKoutakuError: false,
				Error: &schemas.ErrorField{
					Type:    schemas.Ptr(schemas.RequestCancelled),
					Message: schemas.ErrRequestCancelled,
					Error:   err,
				},
			}, jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
		}
		if errors.Is(err, fasthttp.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
			return nil, providerUtils.EnrichError(ctx, providerUtils.NewKoutakuTimeoutError(schemas.ErrProviderRequestTimedOut, err), jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
		}
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewKoutakuOperationError(schemas.ErrProviderDoRequest, err), jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
	}

	// Store provider response headers in context before status check so error responses also forward them
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

	// Check for HTTP errors
	if resp.StatusCode() != fasthttp.StatusOK {
		defer providerUtils.ReleaseStreamingResponse(resp)
		return nil, providerUtils.EnrichError(ctx, parseAnthropicError(resp), jsonBody, nil, sendBackRawRequest, sendBackRawResponse)
	}

	// Large payload streaming passthrough — pipe raw upstream SSE to client
	if providerUtils.SetupStreamingPassthrough(ctx, resp) {
		responseChan := make(chan *schemas.KoutakuStreamChunk)
		close(responseChan)
		return responseChan, nil
	}

	// Create response channel
	responseChan := make(chan *schemas.KoutakuStreamChunk, schemas.DefaultStreamBufferSize)

	// Start streaming in a goroutine
	go func() {
		defer providerUtils.EnsureStreamFinalizerCalled(ctx, postHookSpanFinalizer)
		defer func() {
			if ctx.Err() == context.Canceled {
				providerUtils.HandleStreamCancellation(ctx, postHookRunner, responseChan, logger, postHookSpanFinalizer)
			} else if ctx.Err() == context.DeadlineExceeded {
				providerUtils.HandleStreamTimeout(ctx, postHookRunner, responseChan, logger, postHookSpanFinalizer)
			}
			close(responseChan)
		}()
		defer providerUtils.ReleaseStreamingResponse(resp)
		// If body stream is nil, return an error
		if resp.BodyStream() == nil {
			koutakuErr := providerUtils.NewKoutakuOperationError(
				"Provider returned an empty response",
				fmt.Errorf("provider returned an empty response"),
			)
			ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
			providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, providerUtils.EnrichError(ctx, koutakuErr, jsonBody, nil, sendBackRawRequest, sendBackRawResponse), responseChan, logger, postHookSpanFinalizer)
			return
		}

		// Decompress gzip-encoded streams on-the-fly. Returns a reader that is either
		// the gzip reader (if gzip-encoded) or the original body stream. Does NOT modify
		// resp, so ReleaseStreamingResponse can properly drain the underlying connection.
		reader, releaseGzip := providerUtils.DecompressStreamBody(resp)
		defer releaseGzip()

		// Wrap reader with idle timeout to detect stalled streams.
		reader, stopIdleTimeout := providerUtils.NewIdleTimeoutReader(reader, resp.BodyStream(), providerUtils.GetStreamIdleTimeout(ctx))
		defer stopIdleTimeout()

		// Setup cancellation handler to close the raw network stream on ctx cancellation,
		// which immediately unblocks any in-progress read (including reads blocked inside a gzip decompression layer).
		stopCancellation := providerUtils.SetupStreamCancellation(ctx, resp.BodyStream(), logger)
		defer stopCancellation()

		sseReader := providerUtils.GetSSEEventReader(ctx, reader)
		chunkIndex := 0

		startTime := time.Now()
		lastChunkTime := startTime

		// Track minimal state needed for response format
		usage := &schemas.ResponsesResponseUsage{}

		// Create stream state for stateful conversions
		streamState := acquireAnthropicResponsesStreamState()
		defer releaseAnthropicResponsesStreamState(streamState)

		// Set structured output tool name if present
		if toolName, ok := ctx.Value(schemas.KoutakuContextKeyStructuredOutputToolName).(string); ok {
			streamState.StructuredOutputToolName = toolName
		}

		var modelName string

		for {
			// If context was cancelled/timed out, let defer handle it
			if ctx.Err() != nil {
				return
			}
			eventType, eventDataBytes, readErr := sseReader.ReadEvent()
			if readErr != nil {
				if readErr != io.EOF {
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					logger.Warn("Error reading %s stream: %v", providerName, readErr)
					providerUtils.ProcessAndSendError(ctx, postHookRunner, readErr, responseChan, logger, postHookSpanFinalizer)
				}
				break
			}
			eventData := string(eventDataBytes)
			if eventType == "" || eventData == "" {
				continue
			}
			var event AnthropicStreamEvent
			if err := sonic.Unmarshal([]byte(eventData), &event); err != nil {
				logger.Warn("Failed to parse message_start event: %v", err)
				continue
			}
			if event.Message != nil && modelName == "" {
				modelName = event.Message.Model
			}
			// Note: response.created and response.in_progress are now emitted by ToKoutakuResponsesStream
			// from the message_start event, so we don't need to call them manually here

			// Check for usage in both top-level event.Usage and nested event.Message.Usage
			// message_start events have usage nested in message.usage, while message_delta has it at top level
			var usageToProcess *AnthropicUsage
			if event.Usage != nil {
				usageToProcess = event.Usage
			} else if event.Message != nil && event.Message.Usage != nil {
				usageToProcess = event.Message.Usage
			}

			if usageToProcess != nil {
				// Collect usage information and send at the end of the stream
				// Here in some cases usage comes before final message
				// So we need to check if the response.Usage is nil and then if usage != nil
				// then add up all tokens
				if usageToProcess.InputTokens > usage.InputTokens {
					usage.InputTokens = usageToProcess.InputTokens
				}
				if usageToProcess.OutputTokens > usage.OutputTokens {
					usage.OutputTokens = usageToProcess.OutputTokens
				}
				calculatedTotal := usage.InputTokens + usage.OutputTokens
				if calculatedTotal > usage.TotalTokens {
					usage.TotalTokens = calculatedTotal
				}
				// Handle cached tokens if present
				if usageToProcess.CacheReadInputTokens > 0 {
					if usage.InputTokensDetails == nil {
						usage.InputTokensDetails = &schemas.ResponsesResponseInputTokens{}
					}
					if usageToProcess.CacheReadInputTokens > usage.InputTokensDetails.CachedReadTokens {
						usage.InputTokensDetails.CachedReadTokens = usageToProcess.CacheReadInputTokens
					}
				}
				// Handle cached tokens if present
				if usageToProcess.CacheCreationInputTokens > 0 {
					if usage.InputTokensDetails == nil {
						usage.InputTokensDetails = &schemas.ResponsesResponseInputTokens{}
					}
					if usageToProcess.CacheCreationInputTokens > usage.InputTokensDetails.CachedWriteTokens {
						usage.InputTokensDetails.CachedWriteTokens = usageToProcess.CacheCreationInputTokens
					}
				}
			}

			responses, koutakuErr, isLastChunk := event.ToKoutakuResponsesStream(ctx, chunkIndex, streamState)
			// Propagate message_delta emission flag to context so the output converter
			// (ToAnthropicResponsesStreamResponse) can skip synthesizing a duplicate.
			if streamState.HasEmittedMessageDelta {
				ctx.SetValue(schemas.KoutakuContextKeyHasEmittedMessageDelta, true)
			}
			if koutakuErr != nil {
				// If context was cancelled/timed out, let defer handle it
				if ctx.Err() != nil {
					return
				}
				ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
				providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, koutakuErr, responseChan, logger, postHookSpanFinalizer)
				break
			}
			// Passthrough: when conversion returns no responses but we need to forward raw events,
			// create a minimal pass-through response to carry the raw event data.
			// This ensures events like compaction content_block_start/stop are not silently dropped.
			if len(responses) == 0 && sendBackRawResponse {
				passthroughResp := &schemas.KoutakuResponsesStreamResponse{
					Type:           schemas.ResponsesStreamResponseType(eventType),
					SequenceNumber: chunkIndex,
					ExtraFields: schemas.KoutakuResponseExtraFields{
						ChunkIndex:  chunkIndex,
						Latency:     time.Since(lastChunkTime).Milliseconds(),
						RawResponse: eventData,
					},
				}
				lastChunkTime = time.Now()
				chunkIndex++
				providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
					providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, passthroughResp, nil, nil, nil),
					responseChan, postHookSpanFinalizer)
				continue
			}
			// Handle each response in the slice
			for i, response := range responses {
				if response != nil {
					response.ExtraFields = schemas.KoutakuResponseExtraFields{
						ChunkIndex: chunkIndex,
						Latency:    time.Since(lastChunkTime).Milliseconds(),
					}
					if postResponseConverter != nil {
						response = postResponseConverter(response)
						if response == nil {
							logger.Warn("postResponseConverter returned nil; skipping chunk")
							continue
						}
					}
					lastChunkTime = time.Now()
					chunkIndex++

					// Only add raw response to the last chunk of the incoming event
					if providerUtils.ShouldSendBackRawResponse(ctx, sendBackRawResponse) && i == len(responses)-1 {
						response.ExtraFields.RawResponse = eventData
					}

					if isLastChunk && i == len(responses)-1 {
						if response.Response == nil {
							response.Response = &schemas.KoutakuResponsesResponse{}
						}
						if usage.InputTokensDetails != nil {
							usage.InputTokens = usage.InputTokens + usage.InputTokensDetails.CachedReadTokens + usage.InputTokensDetails.CachedWriteTokens
							usage.TotalTokens = usage.TotalTokens + usage.InputTokensDetails.CachedReadTokens + usage.InputTokensDetails.CachedWriteTokens
						}
						response.Response.Usage = usage
						// Set raw request if enabled
						if sendBackRawRequest {
							providerUtils.ParseAndSetRawRequest(&response.ExtraFields, jsonBody)
						}
						response.ExtraFields.Latency = time.Since(startTime).Milliseconds()
						ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
						providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, response, nil, nil, nil), responseChan, postHookSpanFinalizer)
						return
					}
					providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, response, nil, nil, nil), responseChan, postHookSpanFinalizer)
				}
			}

		}
	}()

	return responseChan, nil
}

// BatchCreate creates a new batch job.
func (provider *AnthropicProvider) BatchCreate(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuBatchCreateRequest) (*schemas.KoutakuBatchCreateResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.BatchCreateRequest); err != nil {
		return nil, err
	}

	providerName := provider.GetProviderKey()

	if len(request.Requests) == 0 {
		return nil, providerUtils.NewKoutakuOperationError("requests array is required for Anthropic batch API", nil)
	}

	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Set headers
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.buildRequestURL(ctx, "/v1/messages/batches", schemas.BatchCreateRequest))
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")

	if key.Value.GetValue() != "" {
		req.Header.Set("x-api-key", key.Value.GetValue())
	}
	req.Header.Set("anthropic-version", provider.apiVersion)

	// Build request body
	anthropicReq := &AnthropicBatchCreateRequest{
		Requests: make([]AnthropicBatchRequestItem, len(request.Requests)),
	}

	for i, r := range request.Requests {
		anthropicReq.Requests[i] = AnthropicBatchRequestItem{
			CustomID: r.CustomID,
			Params:   r.Params,
		}
		// Use Body if Params is empty
		if anthropicReq.Requests[i].Params == nil && r.Body != nil {
			anthropicReq.Requests[i].Params = r.Body
		}
	}

	jsonData, err := providerUtils.MarshalSorted(anthropicReq)
	if err != nil {
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderRequestMarshal, err)
	}
	usedLargePayloadBody := setAnthropicRequestBody(ctx, req, jsonData)

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	// Make request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if usedLargePayloadBody {
		providerUtils.DrainLargePayloadRemainder(ctx)
	}
	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, sendBackRawRequest, sendBackRawResponse)
	}

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		provider.logger.Debug("error from %s provider: %s", providerName, string(resp.Body()))
		return nil, parseAnthropicError(resp)
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err), jsonData, nil, sendBackRawRequest, sendBackRawResponse)
	}

	var anthropicResp AnthropicBatchResponse
	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &anthropicResp, jsonData, sendBackRawRequest, sendBackRawResponse)
	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, body, sendBackRawRequest, sendBackRawResponse)
	}

	return anthropicResp.ToKoutakuBatchCreateResponse(latency, sendBackRawRequest, sendBackRawResponse, rawRequest, rawResponse), nil
}

// BatchList lists batch jobs using serial pagination across keys.
// Exhausts all pages from one key before moving to the next.
func (provider *AnthropicProvider) BatchList(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuBatchListRequest) (*schemas.KoutakuBatchListResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.BatchListRequest); err != nil {
		return nil, err
	}

	providerName := provider.GetProviderKey()
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	// Initialize serial pagination helper (Anthropic uses AfterID for pagination)
	helper, err := providerUtils.NewSerialListHelper(keys, request.AfterID, provider.logger)
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

	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Build URL with query params
	baseURL := provider.buildRequestURL(ctx, "/v1/messages/batches", schemas.BatchListRequest)
	values := url.Values{}
	if request.Limit > 0 {
		values.Set("limit", fmt.Sprintf("%d", request.Limit))
	}
	if request.BeforeID != nil && *request.BeforeID != "" {
		values.Set("before_id", *request.BeforeID)
	}
	// Use native cursor from serial helper instead of request.AfterID
	if nativeCursor != "" {
		values.Set("after_id", nativeCursor)
	}
	requestURL := baseURL
	if encodedValues := values.Encode(); encodedValues != "" {
		requestURL += "?" + encodedValues
	}

	// Set headers
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(requestURL)
	req.Header.SetMethod(http.MethodGet)
	req.Header.SetContentType("application/json")

	if key.Value.GetValue() != "" {
		req.Header.Set("x-api-key", key.Value.GetValue())
	}
	req.Header.Set("anthropic-version", provider.apiVersion)

	// Make request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		provider.logger.Debug("error from %s provider: %s", providerName, string(resp.Body()))
		return nil, parseAnthropicError(resp)
	}

	body, decodeErr := providerUtils.CheckAndDecodeBody(resp)
	if decodeErr != nil {
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, decodeErr)
	}

	var anthropicResp AnthropicBatchListResponse
	_, _, koutakuErr = providerUtils.HandleProviderResponse(body, &anthropicResp, nil, false, sendBackRawResponse)
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Convert batches to Koutaku format
	batches := make([]schemas.KoutakuBatchRetrieveResponse, 0, len(anthropicResp.Data))
	var lastBatchID string
	for _, batch := range anthropicResp.Data {
		batches = append(batches, *batch.ToKoutakuBatchRetrieveResponse(latency, false, false, nil, nil))
		lastBatchID = batch.ID
	}

	// Build cursor for next request
	// Anthropic uses LastID as the cursor for pagination
	nextCursor, hasMore := helper.BuildNextCursor(anthropicResp.HasMore, lastBatchID)

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

// BatchRetrieve retrieves a specific batch job by trying each key until found.
func (provider *AnthropicProvider) BatchRetrieve(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuBatchRetrieveRequest) (*schemas.KoutakuBatchRetrieveResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.BatchRetrieveRequest); err != nil {
		return nil, err
	}

	// batch id is required
	if request.BatchID == "" {
		return nil, providerUtils.NewKoutakuOperationError("batch_id is required", nil)
	}

	providerName := provider.GetProviderKey()
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	var lastErr *schemas.KoutakuError
	for _, key := range keys {
		// Create request
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		// Set headers
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(provider.buildRequestURL(
			ctx,
			"/v1/messages/batches/"+url.PathEscape(request.BatchID),
			schemas.BatchRetrieveRequest,
		))
		req.Header.SetMethod(http.MethodGet)
		req.Header.SetContentType("application/json")

		if key.Value.GetValue() != "" {
			req.Header.Set("x-api-key", key.Value.GetValue())
		}
		req.Header.Set("anthropic-version", provider.apiVersion)

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
			provider.logger.Debug("error from %s provider: %s", providerName, string(resp.Body()))
			lastErr = parseAnthropicError(resp)
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

		var anthropicResp AnthropicBatchResponse
		rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &anthropicResp, nil, sendBackRawRequest, sendBackRawResponse)
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

		result := anthropicResp.ToKoutakuBatchRetrieveResponse(latency, sendBackRawRequest, sendBackRawResponse, rawRequest, rawResponse)
		return result, nil
	}

	return nil, lastErr
}

// BatchCancel cancels a batch job by trying each key until successful.
func (provider *AnthropicProvider) BatchCancel(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuBatchCancelRequest) (*schemas.KoutakuBatchCancelResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.BatchCancelRequest); err != nil {
		return nil, err
	}

	// batch id is required
	if request.BatchID == "" {
		return nil, providerUtils.NewKoutakuOperationError("batch_id is required", nil)
	}

	providerName := provider.GetProviderKey()
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	var lastErr *schemas.KoutakuError
	for _, key := range keys {
		// Create request
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		// Set headers
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(provider.networkConfig.BaseURL + "/v1/messages/batches/" + request.BatchID + "/cancel")
		req.Header.SetMethod(http.MethodPost)
		req.Header.SetContentType("application/json")

		if key.Value.GetValue() != "" {
			req.Header.Set("x-api-key", key.Value.GetValue())
		}
		req.Header.Set("anthropic-version", provider.apiVersion)

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
			provider.logger.Debug("error from %s provider: %s", providerName, string(resp.Body()))
			lastErr = parseAnthropicError(resp)
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

		var anthropicResp AnthropicBatchResponse
		rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &anthropicResp, nil, sendBackRawRequest, sendBackRawResponse)
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
			ID:     anthropicResp.ID,
			Object: anthropicResp.Type,
			Status: ToKoutakuBatchStatus(anthropicResp.ProcessingStatus),
			ExtraFields: schemas.KoutakuResponseExtraFields{
				Latency: latency.Milliseconds(),
			},
		}

		if sendBackRawRequest {
			result.ExtraFields.RawRequest = rawRequest
		}

		if anthropicResp.CancelInitiatedAt != nil {
			cancellingAt := parseAnthropicTimestamp(*anthropicResp.CancelInitiatedAt)
			result.CancellingAt = &cancellingAt
		}

		if anthropicResp.RequestCounts != nil {
			result.RequestCounts = schemas.BatchRequestCounts{
				Total:     anthropicResp.RequestCounts.Processing + anthropicResp.RequestCounts.Succeeded + anthropicResp.RequestCounts.Errored + anthropicResp.RequestCounts.Canceled + anthropicResp.RequestCounts.Expired,
				Completed: anthropicResp.RequestCounts.Succeeded,
				Failed:    anthropicResp.RequestCounts.Errored,
			}
		}

		if sendBackRawResponse {
			result.ExtraFields.RawResponse = rawResponse
		}

		return result, nil
	}

	return nil, lastErr
}

// BatchDelete is not supported by the Anthropic provider.
func (provider *AnthropicProvider) BatchDelete(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuBatchDeleteRequest) (*schemas.KoutakuBatchDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchDeleteRequest, provider.GetProviderKey())
}

// BatchResults retrieves batch results by trying each key until found.
func (provider *AnthropicProvider) BatchResults(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuBatchResultsRequest) (*schemas.KoutakuBatchResultsResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.BatchResultsRequest); err != nil {
		return nil, err
	}

	if request.BatchID == "" {
		return nil, providerUtils.NewKoutakuOperationError("batch_id is required", nil)
	}

	providerName := provider.GetProviderKey()

	var lastErr *schemas.KoutakuError
	for _, key := range keys {
		// Create request
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		// Set headers
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(provider.networkConfig.BaseURL + "/v1/messages/batches/" + request.BatchID + "/results")
		req.Header.SetMethod(http.MethodGet)

		if key.Value.GetValue() != "" {
			req.Header.Set("x-api-key", key.Value.GetValue())
		}
		req.Header.Set("anthropic-version", provider.apiVersion)

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
			provider.logger.Debug("error from %s provider: %s", providerName, string(resp.Body()))
			lastErr = parseAnthropicError(resp)
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

		wait()
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)

		// Parse JSONL content - each line is a separate result
		var results []schemas.BatchResultItem

		parseResult := providerUtils.ParseJSONL(body, func(line []byte) error {
			var anthropicResult AnthropicBatchResultItem
			if err := sonic.Unmarshal(line, &anthropicResult); err != nil {
				provider.logger.Warn("failed to parse batch result line: %v", err)
				return err
			}

			// Convert to Koutaku format
			resultItem := schemas.BatchResultItem{
				CustomID: anthropicResult.CustomID,
				Result: &schemas.BatchResultData{
					Type:    anthropicResult.Result.Type,
					Message: anthropicResult.Result.Message,
				},
			}

			if anthropicResult.Result.Error != nil {
				resultItem.Error = &schemas.BatchResultError{
					Code:    anthropicResult.Result.Error.Type,
					Message: anthropicResult.Result.Error.Message,
				}
			}

			results = append(results, resultItem)
			return nil
		})

		batchResultsResp := &schemas.KoutakuBatchResultsResponse{
			BatchID: request.BatchID,
			Results: results,
			ExtraFields: schemas.KoutakuResponseExtraFields{
				Latency: latency.Milliseconds(),
			},
		}

		if len(parseResult.Errors) > 0 {
			batchResultsResp.ExtraFields.ParseErrors = parseResult.Errors
		}

		return batchResultsResp, nil
	}

	return nil, lastErr
}

// Embedding is not supported by the Anthropic provider.
func (provider *AnthropicProvider) Embedding(ctx *schemas.KoutakuContext, key schemas.Key, input *schemas.KoutakuEmbeddingRequest) (*schemas.KoutakuEmbeddingResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.EmbeddingRequest, provider.GetProviderKey())
}

// Speech is not supported by the Anthropic provider.
func (provider *AnthropicProvider) Speech(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuSpeechRequest) (*schemas.KoutakuSpeechResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.SpeechRequest, provider.GetProviderKey())
}

// SpeechStream is not supported by the Anthropic provider.
func (provider *AnthropicProvider) SpeechStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuSpeechRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.SpeechStreamRequest, provider.GetProviderKey())
}

// Transcription is not supported by the Anthropic provider.
func (provider *AnthropicProvider) Transcription(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuTranscriptionRequest) (*schemas.KoutakuTranscriptionResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TranscriptionRequest, provider.GetProviderKey())
}

// TranscriptionStream is not supported by the Anthropic provider.
func (provider *AnthropicProvider) TranscriptionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuTranscriptionRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TranscriptionStreamRequest, provider.GetProviderKey())
}

// ImageGeneration is not supported by the Anthropic provider.
func (provider *AnthropicProvider) ImageGeneration(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageGenerationRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageGenerationRequest, provider.GetProviderKey())
}

// ImageGenerationStream is not supported by the Anthropic provider.
func (provider *AnthropicProvider) ImageGenerationStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuImageGenerationRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageGenerationStreamRequest, provider.GetProviderKey())
}

// ImageEdit is not supported by the Anthropic provider.
func (provider *AnthropicProvider) ImageEdit(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageEditRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageEditRequest, provider.GetProviderKey())
}

// ImageEditStream is not supported by the Anthropic provider.
func (provider *AnthropicProvider) ImageEditStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuImageEditRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageEditStreamRequest, provider.GetProviderKey())
}

// ImageVariation is not supported by the Anthropic provider.
func (provider *AnthropicProvider) ImageVariation(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageVariationRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageVariationRequest, provider.GetProviderKey())
}

// Rerank is not supported by the Anthropic provider.
func (provider *AnthropicProvider) Rerank(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuRerankRequest) (*schemas.KoutakuRerankResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.RerankRequest, provider.GetProviderKey())
}

// OCR is not supported by the Anthropic provider.
func (provider *AnthropicProvider) OCR(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuOCRRequest) (*schemas.KoutakuOCRResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.OCRRequest, provider.GetProviderKey())
}

// FileUpload uploads a file to Anthropic's Files API.
func (provider *AnthropicProvider) FileUpload(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuFileUploadRequest) (*schemas.KoutakuFileUploadResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.FileUploadRequest); err != nil {
		return nil, err
	}

	providerName := provider.GetProviderKey()

	if len(request.File) == 0 {
		return nil, providerUtils.NewKoutakuOperationError("file content is required", nil)
	}

	// Create multipart form data
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Add file field
	filename := request.Filename
	if filename == "" {
		filename = "file"
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

	// Set headers
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(provider.buildRequestURL(ctx, "/v1/files", schemas.FileUploadRequest))
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType(writer.FormDataContentType())

	if key.Value.GetValue() != "" {
		req.Header.Set("x-api-key", key.Value.GetValue())
	}
	req.Header.Set("anthropic-version", provider.apiVersion)
	appendBetaHeader(req, AnthropicFilesAPIBetaHeader)
	req.SetBody(buf.Bytes())

	// Make request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK && resp.StatusCode() != fasthttp.StatusCreated {
		provider.logger.Debug("error from %s provider: %s", providerName, string(resp.Body()))
		return nil, parseAnthropicError(resp)
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
	}

	var anthropicResp AnthropicFileResponse
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &anthropicResp, nil, sendBackRawRequest, sendBackRawResponse)
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	return anthropicResp.ToKoutakuFileUploadResponse(latency, sendBackRawRequest, sendBackRawResponse, rawRequest, rawResponse), nil
}

// FileList lists files from all provided keys and aggregates results.
// FileList lists files using serial pagination across keys.
// Exhausts all pages from one key before moving to the next.
func (provider *AnthropicProvider) FileList(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuFileListRequest) (*schemas.KoutakuFileListResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.FileListRequest); err != nil {
		return nil, err
	}

	providerName := provider.GetProviderKey()
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

	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Build URL with query params
	requestURL := provider.buildRequestURL(ctx, "/v1/files", schemas.FileListRequest)
	values := url.Values{}
	if request.Limit > 0 {
		values.Set("limit", fmt.Sprintf("%d", request.Limit))
	}
	// Use native cursor from serial helper instead of request.After
	if nativeCursor != "" {
		values.Set("after_id", nativeCursor)
	}
	if encodedValues := values.Encode(); encodedValues != "" {
		requestURL += "?" + encodedValues
	}

	// Set headers
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(requestURL)
	req.Header.SetMethod(http.MethodGet)
	req.Header.SetContentType("application/json")

	if key.Value.GetValue() != "" {
		req.Header.Set("x-api-key", key.Value.GetValue())
	}
	req.Header.Set("anthropic-version", provider.apiVersion)
	appendBetaHeader(req, AnthropicFilesAPIBetaHeader)

	// Make request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		provider.logger.Debug("error from %s provider: %s", providerName, string(resp.Body()))
		return nil, parseAnthropicError(resp)
	}

	body, decodeErr := providerUtils.CheckAndDecodeBody(resp)
	if decodeErr != nil {
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, decodeErr)
	}

	var anthropicResp AnthropicFileListResponse
	_, _, koutakuErr = providerUtils.HandleProviderResponse(body, &anthropicResp, nil, sendBackRawRequest, sendBackRawResponse)
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Convert files to Koutaku format
	files := make([]schemas.FileObject, 0, len(anthropicResp.Data))
	var lastFileID string
	for _, file := range anthropicResp.Data {
		files = append(files, schemas.FileObject{
			ID:        file.ID,
			Object:    file.Type,
			Bytes:     file.SizeBytes,
			CreatedAt: parseAnthropicFileTimestamp(file.CreatedAt),
			Filename:  file.Filename,
			Purpose:   schemas.FilePurposeBatch,
			Status:    schemas.FileStatusProcessed,
		})
		lastFileID = file.ID
	}

	// Build cursor for next request
	// Anthropic uses LastID as the cursor for pagination
	nextCursor, hasMore := helper.BuildNextCursor(anthropicResp.HasMore, lastFileID)

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

// FileRetrieve retrieves file metadata from Anthropic's Files API by trying each key until found.
func (provider *AnthropicProvider) FileRetrieve(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuFileRetrieveRequest) (*schemas.KoutakuFileRetrieveResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.FileRetrieveRequest); err != nil {
		return nil, err
	}

	providerName := provider.GetProviderKey()

	if request.FileID == "" {
		return nil, providerUtils.NewKoutakuOperationError("file_id is required", nil)
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	var lastErr *schemas.KoutakuError
	for _, key := range keys {
		// Create request
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		// Set headers
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(provider.buildRequestURL(
			ctx,
			"/v1/files/"+url.PathEscape(request.FileID),
			schemas.FileRetrieveRequest,
		))
		req.Header.SetMethod(http.MethodGet)
		req.Header.SetContentType("application/json")

		if key.Value.GetValue() != "" {
			req.Header.Set("x-api-key", key.Value.GetValue())
		}
		req.Header.Set("anthropic-version", provider.apiVersion)
		appendBetaHeader(req, AnthropicFilesAPIBetaHeader)

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
			provider.logger.Debug("error from %s provider: %s", providerName, string(resp.Body()))
			lastErr = parseAnthropicError(resp)
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

		var anthropicResp AnthropicFileResponse
		rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &anthropicResp, nil, sendBackRawRequest, sendBackRawResponse)
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

		return anthropicResp.ToKoutakuFileRetrieveResponse(latency, sendBackRawRequest, sendBackRawResponse, rawRequest, rawResponse), nil
	}

	return nil, lastErr
}

// FileDelete deletes a file from Anthropic's Files API by trying each key until successful.
func (provider *AnthropicProvider) FileDelete(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuFileDeleteRequest) (*schemas.KoutakuFileDeleteResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.FileDeleteRequest); err != nil {
		return nil, err
	}

	providerName := provider.GetProviderKey()

	if request.FileID == "" {
		return nil, providerUtils.NewKoutakuOperationError("file_id is required", nil)
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	var lastErr *schemas.KoutakuError
	for _, key := range keys {
		// Create request
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		// Set headers
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(provider.networkConfig.BaseURL + "/v1/files/" + request.FileID)
		req.Header.SetMethod(http.MethodDelete)
		req.Header.SetContentType("application/json")

		if key.Value.GetValue() != "" {
			req.Header.Set("x-api-key", key.Value.GetValue())
		}
		req.Header.Set("anthropic-version", provider.apiVersion)
		appendBetaHeader(req, AnthropicFilesAPIBetaHeader)

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
			provider.logger.Debug("error from %s provider: %s", providerName, string(resp.Body()))
			lastErr = parseAnthropicError(resp)
			wait()
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			continue
		}

		// For 204 No Content, return success without parsing body
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

		var anthropicResp AnthropicFileDeleteResponse
		rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &anthropicResp, nil, sendBackRawRequest, sendBackRawResponse)
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
			ID:      anthropicResp.ID,
			Object:  "file",
			Deleted: anthropicResp.Type == "file_deleted",
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

// FileContent downloads file content from Anthropic's Files API by trying each key until found.
// Note: Only files created by skills or the code execution tool can be downloaded.
func (provider *AnthropicProvider) FileContent(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuFileContentRequest) (*schemas.KoutakuFileContentResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.FileContentRequest); err != nil {
		return nil, err
	}

	providerName := provider.GetProviderKey()

	if request.FileID == "" {
		return nil, providerUtils.NewKoutakuOperationError("file_id is required", nil)
	}

	var lastErr *schemas.KoutakuError
	for _, key := range keys {
		// Create request
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		// Set headers
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
		req.SetRequestURI(provider.networkConfig.BaseURL + "/v1/files/" + request.FileID + "/content")
		req.Header.SetMethod(http.MethodGet)

		if key.Value.GetValue() != "" {
			req.Header.Set("x-api-key", key.Value.GetValue())
		}
		req.Header.Set("anthropic-version", provider.apiVersion)
		appendBetaHeader(req, AnthropicFilesAPIBetaHeader)
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
			provider.logger.Debug("error from %s provider: %s", providerName, string(resp.Body()))
			lastErr = parseAnthropicError(resp)
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

// CountTokens counts tokens for a given request using Anthropic's API.
func (provider *AnthropicProvider) CountTokens(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuResponsesRequest) (*schemas.KoutakuCountTokensResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.CountTokensRequest); err != nil {
		return nil, err
	}
	jsonBody, err := getRequestBodyForResponses(ctx, request, false, []string{"max_tokens", "temperature"})
	if err != nil {
		return nil, err
	}

	responseBody, latency, providerResponseHeaders, koutakuErr := provider.completeRequest(ctx, jsonBody, provider.buildRequestURL(ctx, "/v1/messages/count_tokens", schemas.CountTokensRequest), key.Value.GetValue(), schemas.CountTokensRequest)
	if providerResponseHeaders != nil {
		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)
	}
	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonBody, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	anthropicResponse := &AnthropicCountTokensResponse{}
	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(
		responseBody,
		anthropicResponse,
		jsonBody,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
	)

	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonBody, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	response := anthropicResponse.ToKoutakuCountTokensResponse(request.Model)
	response.Model = request.Model

	response.ExtraFields.Latency = latency.Milliseconds()
	response.ExtraFields.ProviderResponseHeaders = providerResponseHeaders

	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		response.ExtraFields.RawRequest = rawRequest
	}

	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		response.ExtraFields.RawResponse = rawResponse
	}

	return response, nil
}

// VideoGeneration is not supported by the Anthropic provider.
func (provider *AnthropicProvider) VideoGeneration(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoGenerationRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoGenerationRequest, provider.GetProviderKey())
}

// VideoRetrieve is not supported by the Anthropic provider.
func (provider *AnthropicProvider) VideoRetrieve(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoRetrieveRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoRetrieveRequest, provider.GetProviderKey())
}

// VideoDownload is not supported by the Anthropic provider.
func (provider *AnthropicProvider) VideoDownload(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoDownloadRequest) (*schemas.KoutakuVideoDownloadResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoDownloadRequest, provider.GetProviderKey())
}

// VideoDelete is not supported by the Anthropic provider.
func (provider *AnthropicProvider) VideoDelete(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoDeleteRequest) (*schemas.KoutakuVideoDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoDeleteRequest, provider.GetProviderKey())
}

// VideoList is not supported by the Anthropic provider.
func (provider *AnthropicProvider) VideoList(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoListRequest) (*schemas.KoutakuVideoListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoListRequest, provider.GetProviderKey())
}

// VideoRemix is not supported by the Anthropic provider.
func (provider *AnthropicProvider) VideoRemix(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoRemixRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoRemixRequest, provider.GetProviderKey())
}

// ContainerCreate is not supported by the Anthropic provider.
func (provider *AnthropicProvider) ContainerCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuContainerCreateRequest) (*schemas.KoutakuContainerCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerCreateRequest, provider.GetProviderKey())
}

// ContainerList is not supported by the Anthropic provider.
func (provider *AnthropicProvider) ContainerList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerListRequest) (*schemas.KoutakuContainerListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerListRequest, provider.GetProviderKey())
}

// ContainerRetrieve is not supported by the Anthropic provider.
func (provider *AnthropicProvider) ContainerRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerRetrieveRequest) (*schemas.KoutakuContainerRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerRetrieveRequest, provider.GetProviderKey())
}

// ContainerDelete is not supported by the Anthropic provider.
func (provider *AnthropicProvider) ContainerDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerDeleteRequest) (*schemas.KoutakuContainerDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerDeleteRequest, provider.GetProviderKey())
}

// ContainerFileCreate is not supported by the Anthropic provider.
func (provider *AnthropicProvider) ContainerFileCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuContainerFileCreateRequest) (*schemas.KoutakuContainerFileCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileCreateRequest, provider.GetProviderKey())
}

// ContainerFileList is not supported by the Anthropic provider.
func (provider *AnthropicProvider) ContainerFileList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileListRequest) (*schemas.KoutakuContainerFileListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileListRequest, provider.GetProviderKey())
}

// ContainerFileRetrieve is not supported by the Anthropic provider.
func (provider *AnthropicProvider) ContainerFileRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileRetrieveRequest) (*schemas.KoutakuContainerFileRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileRetrieveRequest, provider.GetProviderKey())
}

// ContainerFileContent is not supported by the Anthropic provider.
func (provider *AnthropicProvider) ContainerFileContent(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileContentRequest) (*schemas.KoutakuContainerFileContentResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileContentRequest, provider.GetProviderKey())
}

// ContainerFileDelete is not supported by the Anthropic provider.
func (provider *AnthropicProvider) ContainerFileDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileDeleteRequest) (*schemas.KoutakuContainerFileDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileDeleteRequest, provider.GetProviderKey())
}

func (provider *AnthropicProvider) Passthrough(
	ctx *schemas.KoutakuContext,
	key schemas.Key,
	req *schemas.KoutakuPassthroughRequest,
) (*schemas.KoutakuPassthroughResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.PassthroughRequest); err != nil {
		return nil, err
	}

	url := provider.networkConfig.BaseURL + req.Path
	if req.RawQuery != "" {
		url += "?" + req.RawQuery
	}

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

	if key.Value.GetValue() != "" {
		fasthttpReq.Header.Set("x-api-key", key.Value.GetValue())
	}
	fasthttpReq.Header.Set("anthropic-version", provider.apiVersion)

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

func (provider *AnthropicProvider) PassthroughStream(
	ctx *schemas.KoutakuContext,
	postHookRunner schemas.PostHookRunner,
	postHookSpanFinalizer func(context.Context),
	key schemas.Key,
	req *schemas.KoutakuPassthroughRequest,
) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Anthropic, provider.customProviderConfig, schemas.PassthroughStreamRequest); err != nil {
		return nil, err
	}

	url := provider.networkConfig.BaseURL + req.Path
	if req.RawQuery != "" {
		url += "?" + req.RawQuery
	}

	startTime := time.Now()

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

	if key.Value.GetValue() != "" {
		fasthttpReq.Header.Set("x-api-key", key.Value.GetValue())
	}
	fasthttpReq.Header.Set("anthropic-version", provider.apiVersion)

	fasthttpReq.SetBody(req.Body)

	activeClient := providerUtils.PrepareResponseStreaming(ctx, provider.streamingClient, resp)
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

	bodyStream := resp.BodyStream()
	if bodyStream == nil {
		providerUtils.ReleaseStreamingResponse(resp)
		return nil, providerUtils.NewKoutakuOperationError(
			"provider returned an empty stream body",
			fmt.Errorf("provider returned an empty stream body"),
		)
	}

	// Wrap reader with idle timeout to detect stalled streams.
	providerUtils.SetStreamIdleTimeoutIfEmpty(ctx, provider.networkConfig.StreamIdleTimeoutInSeconds)
	rawBodyStream := bodyStream
	bodyStream, stopIdleTimeout := providerUtils.NewIdleTimeoutReader(bodyStream, rawBodyStream, providerUtils.GetStreamIdleTimeout(ctx))

	// Cancellation must close the raw stream to unblock reads.
	stopCancellation := providerUtils.SetupStreamCancellation(ctx, rawBodyStream, provider.logger)

	extraFields := schemas.KoutakuResponseExtraFields{}
	statusCode := resp.StatusCode()

	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		providerUtils.ParseAndSetRawRequestIfJSON(fasthttpReq, &extraFields)
	}

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
					return // let defer handle cancel/timeout
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
