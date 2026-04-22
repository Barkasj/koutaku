// Package providers implements various LLM providers and their utility functions.
// This file contains the Perplexity provider implementation.
package perplexity

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/koutaku/koutaku/core/providers/openai"
	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	schemas "github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

// PerplexityProvider implements the Provider interface for Perplexity's API.
type PerplexityProvider struct {
	logger              schemas.Logger        // Logger for provider operations
	client              *fasthttp.Client      // HTTP client for unary API requests (ReadTimeout bounds overall response)
	streamingClient     *fasthttp.Client      // HTTP client for streaming API requests (no ReadTimeout; idle governed by NewIdleTimeoutReader)
	networkConfig       schemas.NetworkConfig // Network configuration including extra headers
	sendBackRawRequest  bool                  // Whether to include raw request in KoutakuResponse
	sendBackRawResponse bool                  // Whether to include raw response in KoutakuResponse
}

// NewPerplexityProvider creates a new Perplexity provider instance.
// It initializes the HTTP client with the provided configuration and sets up response pools.
// The client is configured with timeouts, concurrency limits, and optional proxy settings.
func NewPerplexityProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*PerplexityProvider, error) {
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
	// Set default BaseURL if not provided
	if config.NetworkConfig.BaseURL == "" {
		config.NetworkConfig.BaseURL = "https://api.perplexity.ai"
	}
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")

	return &PerplexityProvider{
		logger:              logger,
		client:              client,
		streamingClient:     streamingClient,
		networkConfig:       config.NetworkConfig,
		sendBackRawRequest:  config.SendBackRawRequest,
		sendBackRawResponse: config.SendBackRawResponse,
	}, nil
}

// GetProviderKey returns the provider identifier for Perplexity.
func (provider *PerplexityProvider) GetProviderKey() schemas.ModelProvider {
	return schemas.Perplexity
}

// completeRequest sends a request to Perplexity's API and handles the response.
// It constructs the API URL, sets up authentication, and processes the response.
// Returns the response body or an error if the request fails.
func (provider *PerplexityProvider) completeRequest(ctx *schemas.KoutakuContext, jsonData []byte, url string, key string, model string) ([]byte, time.Duration, map[string]string, *schemas.KoutakuError) {
	// Create the request with the JSON body
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Set any extra headers from network config
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)

	req.SetRequestURI(url)
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	if !providerUtils.ApplyLargePayloadRequestBodyWithModelNormalization(ctx, req, schemas.Perplexity) {
		req.SetBody(jsonData)
	}

	// Send the request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, latency, nil, koutakuErr
	}

	// Extract provider response headers before status check so error responses also forward them
	providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		provider.logger.Debug(fmt.Sprintf("error from %s provider: %s", provider.GetProviderKey(), string(resp.Body())))
		return nil, latency, providerResponseHeaders, openai.ParseOpenAIError(resp)
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, latency, providerResponseHeaders, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
	}

	// Read the response body and copy it before releasing the response
	// to avoid use-after-free since resp.Body() references fasthttp's internal buffer
	bodyCopy := append([]byte(nil), body...)

	return bodyCopy, latency, providerResponseHeaders, nil
}

// ListModels performs a list models request to Perplexity's API.
func (provider *PerplexityProvider) ListModels(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuListModelsRequest) (*schemas.KoutakuListModelsResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ListModelsRequest, provider.GetProviderKey())
}

// TextCompletion is not supported by the Perplexity provider.
func (provider *PerplexityProvider) TextCompletion(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuTextCompletionRequest) (*schemas.KoutakuTextCompletionResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TextCompletionRequest, provider.GetProviderKey())
}

// TextCompletionStream performs a streaming text completion request to Perplexity's API.
// It formats the request, sends it to Perplexity, and processes the response.
// Returns a channel of KoutakuStreamChunk objects or an error if the request fails.
func (provider *PerplexityProvider) TextCompletionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuTextCompletionRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TextCompletionStreamRequest, provider.GetProviderKey())
}

// ChatCompletion performs a chat completion request to the Perplexity API.
func (provider *PerplexityProvider) ChatCompletion(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuChatRequest) (*schemas.KoutakuChatResponse, *schemas.KoutakuError) {
	// Convert to Perplexity chat completion request
	jsonBody, err := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToPerplexityChatCompletionRequest(request), nil
		})
	if err != nil {
		return nil, err
	}

	responseBody, latency, providerResponseHeaders, err := provider.completeRequest(ctx, jsonBody, provider.networkConfig.BaseURL+providerUtils.GetPathFromContext(ctx, "/chat/completions"), key.Value.GetValue(), request.Model)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonBody, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	var response PerplexityChatResponse
	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(responseBody, &response, jsonBody, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonBody, responseBody, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	koutakuResponse := response.ToKoutakuChatResponse(request.Model)

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

// ChatCompletionStream performs a streaming chat completion request to the Perplexity API.
// It supports real-time streaming of responses using Server-Sent Events (SSE).
// Uses Perplexity's OpenAI-compatible streaming format.
// Returns a channel containing KoutakuStreamChunk objects representing the stream or an error if the request fails.
func (provider *PerplexityProvider) ChatCompletionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuChatRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	var authHeader map[string]string
	if key.Value.GetValue() != "" {
		authHeader = map[string]string{"Authorization": "Bearer " + key.Value.GetValue()}
	}
	customRequestConverter := func(request *schemas.KoutakuChatRequest) (providerUtils.RequestBodyWithExtraParams, error) {
		reqBody := ToPerplexityChatCompletionRequest(request)
		reqBody.Stream = schemas.Ptr(true)
		return reqBody, nil
	}
	// Use shared OpenAI-compatible streaming logic
	return openai.HandleOpenAIChatCompletionStreaming(
		ctx,
		provider.streamingClient,
		provider.networkConfig.BaseURL+"/chat/completions",
		request,
		authHeader,
		provider.networkConfig.ExtraHeaders,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		schemas.Perplexity,
		postHookRunner,
		customRequestConverter,
		nil,
		nil,
		nil,
		nil,
		provider.logger,
		postHookSpanFinalizer,
	)
}

// Responses performs a responses request to the Perplexity API.
func (provider *PerplexityProvider) Responses(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuResponsesRequest) (*schemas.KoutakuResponsesResponse, *schemas.KoutakuError) {
	chatResponse, err := provider.ChatCompletion(ctx, key, request.ToChatRequest())
	if err != nil {
		return nil, err
	}

	response := chatResponse.ToKoutakuResponsesResponse()

	return response, nil
}

// ResponsesStream performs a streaming responses request to the Perplexity API.
func (provider *PerplexityProvider) ResponsesStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuResponsesRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	ctx.SetValue(schemas.KoutakuContextKeyIsResponsesToChatCompletionFallback, true)
	return provider.ChatCompletionStream(
		ctx,
		postHookRunner,
		postHookSpanFinalizer,
		key,
		request.ToChatRequest(),
	)
}

// Embedding is not supported by the Perplexity provider.
func (provider *PerplexityProvider) Embedding(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuEmbeddingRequest) (*schemas.KoutakuEmbeddingResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.EmbeddingRequest, provider.GetProviderKey())
}

// Speech is not supported by the Perplexity provider.
func (provider *PerplexityProvider) Speech(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuSpeechRequest) (*schemas.KoutakuSpeechResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.SpeechRequest, provider.GetProviderKey())
}

// Rerank is not supported by the Perplexity provider.
func (provider *PerplexityProvider) Rerank(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuRerankRequest) (*schemas.KoutakuRerankResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.RerankRequest, provider.GetProviderKey())
}

// OCR is not supported by the Perplexity provider.
func (provider *PerplexityProvider) OCR(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuOCRRequest) (*schemas.KoutakuOCRResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.OCRRequest, provider.GetProviderKey())
}

// SpeechStream is not supported by the Perplexity provider.
func (provider *PerplexityProvider) SpeechStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuSpeechRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.SpeechStreamRequest, provider.GetProviderKey())
}

// Transcription is not supported by the Perplexity provider.
func (provider *PerplexityProvider) Transcription(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuTranscriptionRequest) (*schemas.KoutakuTranscriptionResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TranscriptionRequest, provider.GetProviderKey())
}

// TranscriptionStream is not supported by the Perplexity provider.
func (provider *PerplexityProvider) TranscriptionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuTranscriptionRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TranscriptionStreamRequest, provider.GetProviderKey())
}

// ImageGeneration is not supported by the Perplexity provider.
func (provider *PerplexityProvider) ImageGeneration(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageGenerationRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageGenerationRequest, provider.GetProviderKey())
}

// ImageGenerationStream is not supported by the Perplexity provider.
func (provider *PerplexityProvider) ImageGenerationStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuImageGenerationRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageGenerationStreamRequest, provider.GetProviderKey())
}

// ImageEdit is not supported by the Perplexity provider.
func (provider *PerplexityProvider) ImageEdit(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageEditRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageEditRequest, provider.GetProviderKey())
}

// ImageEditStream is not supported by the Perplexity provider.
func (provider *PerplexityProvider) ImageEditStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuImageEditRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageEditStreamRequest, provider.GetProviderKey())
}

// ImageVariation is not supported by the Perplexity provider.
func (provider *PerplexityProvider) ImageVariation(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageVariationRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageVariationRequest, provider.GetProviderKey())
}

// VideoGeneration is not supported by the Perplexity provider.
func (provider *PerplexityProvider) VideoGeneration(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoGenerationRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoGenerationRequest, provider.GetProviderKey())
}

// VideoRetrieve is not supported by the Perplexity provider.
func (provider *PerplexityProvider) VideoRetrieve(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoRetrieveRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoRetrieveRequest, provider.GetProviderKey())
}

// VideoDownload is not supported by the Perplexity provider.
func (provider *PerplexityProvider) VideoDownload(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoDownloadRequest) (*schemas.KoutakuVideoDownloadResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoDownloadRequest, provider.GetProviderKey())
}

// VideoDelete is not supported by Perplexity provider.
func (provider *PerplexityProvider) VideoDelete(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoDeleteRequest) (*schemas.KoutakuVideoDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoDeleteRequest, provider.GetProviderKey())
}

// VideoList is not supported by Perplexity provider.
func (provider *PerplexityProvider) VideoList(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoListRequest) (*schemas.KoutakuVideoListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoListRequest, provider.GetProviderKey())
}

// VideoRemix is not supported by Perplexity provider.
func (provider *PerplexityProvider) VideoRemix(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoRemixRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoRemixRequest, provider.GetProviderKey())
}

// BatchCreate is not supported by Perplexity provider.
func (provider *PerplexityProvider) BatchCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuBatchCreateRequest) (*schemas.KoutakuBatchCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchCreateRequest, provider.GetProviderKey())
}

// BatchList is not supported by Perplexity provider.
func (provider *PerplexityProvider) BatchList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchListRequest) (*schemas.KoutakuBatchListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchListRequest, provider.GetProviderKey())
}

// BatchRetrieve is not supported by Perplexity provider.
func (provider *PerplexityProvider) BatchRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchRetrieveRequest) (*schemas.KoutakuBatchRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchRetrieveRequest, provider.GetProviderKey())
}

// BatchCancel is not supported by Perplexity provider.
func (provider *PerplexityProvider) BatchCancel(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchCancelRequest) (*schemas.KoutakuBatchCancelResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchCancelRequest, provider.GetProviderKey())
}

// BatchDelete is not supported by Perplexity provider.
func (provider *PerplexityProvider) BatchDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchDeleteRequest) (*schemas.KoutakuBatchDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchDeleteRequest, provider.GetProviderKey())
}

// BatchResults is not supported by Perplexity provider.
func (provider *PerplexityProvider) BatchResults(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchResultsRequest) (*schemas.KoutakuBatchResultsResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchResultsRequest, provider.GetProviderKey())
}

// FileUpload is not supported by Perplexity provider.
func (provider *PerplexityProvider) FileUpload(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuFileUploadRequest) (*schemas.KoutakuFileUploadResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileUploadRequest, provider.GetProviderKey())
}

// FileList is not supported by Perplexity provider.
func (provider *PerplexityProvider) FileList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuFileListRequest) (*schemas.KoutakuFileListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileListRequest, provider.GetProviderKey())
}

// FileRetrieve is not supported by Perplexity provider.
func (provider *PerplexityProvider) FileRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuFileRetrieveRequest) (*schemas.KoutakuFileRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileRetrieveRequest, provider.GetProviderKey())
}

// FileDelete is not supported by Perplexity provider.
func (provider *PerplexityProvider) FileDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuFileDeleteRequest) (*schemas.KoutakuFileDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileDeleteRequest, provider.GetProviderKey())
}

// FileContent is not supported by Perplexity provider.
func (provider *PerplexityProvider) FileContent(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuFileContentRequest) (*schemas.KoutakuFileContentResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileContentRequest, provider.GetProviderKey())
}

// CountTokens is not supported by the Perplexity provider.
func (provider *PerplexityProvider) CountTokens(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuResponsesRequest) (*schemas.KoutakuCountTokensResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.CountTokensRequest, provider.GetProviderKey())
}

// ContainerCreate is not supported by the Perplexity provider.
func (provider *PerplexityProvider) ContainerCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuContainerCreateRequest) (*schemas.KoutakuContainerCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerCreateRequest, provider.GetProviderKey())
}

// ContainerList is not supported by the Perplexity provider.
func (provider *PerplexityProvider) ContainerList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerListRequest) (*schemas.KoutakuContainerListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerListRequest, provider.GetProviderKey())
}

// ContainerRetrieve is not supported by the Perplexity provider.
func (provider *PerplexityProvider) ContainerRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerRetrieveRequest) (*schemas.KoutakuContainerRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerRetrieveRequest, provider.GetProviderKey())
}

// ContainerDelete is not supported by the Perplexity provider.
func (provider *PerplexityProvider) ContainerDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerDeleteRequest) (*schemas.KoutakuContainerDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerDeleteRequest, provider.GetProviderKey())
}

// ContainerFileCreate is not supported by the Perplexity provider.
func (provider *PerplexityProvider) ContainerFileCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuContainerFileCreateRequest) (*schemas.KoutakuContainerFileCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileCreateRequest, provider.GetProviderKey())
}

// ContainerFileList is not supported by the Perplexity provider.
func (provider *PerplexityProvider) ContainerFileList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileListRequest) (*schemas.KoutakuContainerFileListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileListRequest, provider.GetProviderKey())
}

// ContainerFileRetrieve is not supported by the Perplexity provider.
func (provider *PerplexityProvider) ContainerFileRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileRetrieveRequest) (*schemas.KoutakuContainerFileRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileRetrieveRequest, provider.GetProviderKey())
}

// ContainerFileContent is not supported by the Perplexity provider.
func (provider *PerplexityProvider) ContainerFileContent(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileContentRequest) (*schemas.KoutakuContainerFileContentResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileContentRequest, provider.GetProviderKey())
}

// ContainerFileDelete is not supported by the Perplexity provider.
func (provider *PerplexityProvider) ContainerFileDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileDeleteRequest) (*schemas.KoutakuContainerFileDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileDeleteRequest, provider.GetProviderKey())
}

// Passthrough is not supported by the Perplexity provider.
func (provider *PerplexityProvider) Passthrough(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuPassthroughRequest) (*schemas.KoutakuPassthroughResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.PassthroughRequest, provider.GetProviderKey())
}

func (provider *PerplexityProvider) PassthroughStream(_ *schemas.KoutakuContext, _ schemas.PostHookRunner, _ func(context.Context), _ schemas.Key, _ *schemas.KoutakuPassthroughRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.PassthroughStreamRequest, provider.GetProviderKey())
}
