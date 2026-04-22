// Package vllm implements the vLLM LLM provider (OpenAI-compatible).
package vllm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/koutaku/koutaku/core/providers/openai"
	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	schemas "github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

// VLLMProvider implements the Provider interface for vLLM's OpenAI-compatible API.
type VLLMProvider struct {
	logger              schemas.Logger        // Logger for provider operations
	client              *fasthttp.Client      // HTTP client for unary API requests (ReadTimeout bounds overall response)
	streamingClient     *fasthttp.Client      // HTTP client for streaming API requests (no ReadTimeout; idle governed by NewIdleTimeoutReader)
	networkConfig       schemas.NetworkConfig // Network configuration including extra headers
	sendBackRawRequest  bool                  // Whether to include raw request in KoutakuResponse
	sendBackRawResponse bool                  // Whether to include raw response in KoutakuResponse
}

// NewVLLMProvider creates a new vLLM provider instance.
func NewVLLMProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*VLLMProvider, error) {
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

	client = providerUtils.ConfigureProxy(client, config.ProxyConfig, logger)
	client = providerUtils.ConfigureDialer(client)
	client = providerUtils.ConfigureTLS(client, config.NetworkConfig, logger)
	streamingClient := providerUtils.BuildStreamingClient(client)
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")

	// BaseURL is optional when keys have vllm_key_config with per-key URLs
	return &VLLMProvider{
		logger:              logger,
		client:              client,
		streamingClient:     streamingClient,
		networkConfig:       config.NetworkConfig,
		sendBackRawRequest:  config.SendBackRawRequest,
		sendBackRawResponse: config.SendBackRawResponse,
	}, nil
}

// GetProviderKey returns the provider identifier for vLLM.
func (provider *VLLMProvider) GetProviderKey() schemas.ModelProvider {
	return schemas.VLLM
}

// getBaseURL resolves the base URL for a request from the per-key vllm_key_config.
// Each vLLM key must have its own URL configured — there is no provider-level fallback.
func (provider *VLLMProvider) getBaseURL(key schemas.Key) string {
	if key.VLLMKeyConfig != nil && key.VLLMKeyConfig.URL.GetValue() != "" {
		return strings.TrimRight(key.VLLMKeyConfig.URL.GetValue(), "/")
	}
	return ""
}

// baseURLOrError returns the resolved base URL or a KoutakuError when none is configured.
func (provider *VLLMProvider) baseURLOrError(key schemas.Key) (string, *schemas.KoutakuError) {
	u := provider.getBaseURL(key)
	if u == "" {
		return "", providerUtils.NewKoutakuOperationError(
			"no base URL configured: set vllm_key_config.url on the key",
			nil)
	}
	return u, nil
}

// listModelsByKey performs a list models request for a single vLLM key,
// resolving the per-key URL so each backend is queried individually.
func (provider *VLLMProvider) listModelsByKey(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuListModelsRequest) (*schemas.KoutakuListModelsResponse, *schemas.KoutakuError) {
	baseURL, koutakuErr := provider.baseURLOrError(key)
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	url := baseURL + providerUtils.GetPathFromContext(ctx, "/v1/models")
	return openai.ListModelsByKey(
		ctx,
		provider.client,
		url,
		key,
		request.Unfiltered,
		provider.networkConfig.ExtraHeaders,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
	)
}

// ListModels performs a list models request to vLLM's API.
// Requests are made concurrently per key so that each backend is queried
// with its own URL (from vllm_key_config).
func (provider *VLLMProvider) ListModels(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuListModelsRequest) (*schemas.KoutakuListModelsResponse, *schemas.KoutakuError) {
	return providerUtils.HandleMultipleListModelsRequests(
		ctx,
		keys,
		request,
		provider.listModelsByKey,
	)
}

// TextCompletion performs a text completion request to vLLM's API.
func (provider *VLLMProvider) TextCompletion(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuTextCompletionRequest) (*schemas.KoutakuTextCompletionResponse, *schemas.KoutakuError) {
	ctx.SetValue(schemas.KoutakuContextKeyPassthroughExtraParams, true)
	baseURL, koutakuErr := provider.baseURLOrError(key)
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	return openai.HandleOpenAITextCompletionRequest(
		ctx,
		provider.client,
		baseURL+providerUtils.GetPathFromContext(ctx, "/v1/completions"),
		request,
		key,
		provider.networkConfig.ExtraHeaders,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		HandleVLLMResponse,
		nil,
		provider.logger,
	)
}

// TextCompletionStream performs a streaming text completion request to vLLM's API.
func (provider *VLLMProvider) TextCompletionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuTextCompletionRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	ctx.SetValue(schemas.KoutakuContextKeyPassthroughExtraParams, true)
	baseURL, koutakuErr := provider.baseURLOrError(key)
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	var authHeader map[string]string
	if key.Value.GetValue() != "" {
		authHeader = map[string]string{"Authorization": "Bearer " + key.Value.GetValue()}
	}
	return openai.HandleOpenAITextCompletionStreaming(
		ctx,
		provider.streamingClient,
		baseURL+providerUtils.GetPathFromContext(ctx, "/v1/completions"),
		request,
		authHeader,
		provider.networkConfig.ExtraHeaders,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.GetProviderKey(),
		nil,
		postHookRunner,
		HandleVLLMResponse,
		nil,
		provider.logger,
		postHookSpanFinalizer,
	)
}

// ChatCompletion performs a chat completion request to vLLM's API.
func (provider *VLLMProvider) ChatCompletion(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuChatRequest) (*schemas.KoutakuChatResponse, *schemas.KoutakuError) {
	ctx.SetValue(schemas.KoutakuContextKeyPassthroughExtraParams, true)
	baseURL, koutakuErr := provider.baseURLOrError(key)
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	return openai.HandleOpenAIChatCompletionRequest(
		ctx,
		provider.client,
		baseURL+providerUtils.GetPathFromContext(ctx, "/v1/chat/completions"),
		request,
		key,
		provider.networkConfig.ExtraHeaders,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.GetProviderKey(),
		HandleVLLMResponse,
		nil,
		provider.logger,
	)
}

// ChatCompletionStream performs a streaming chat completion request to vLLM's API.
func (provider *VLLMProvider) ChatCompletionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuChatRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	ctx.SetValue(schemas.KoutakuContextKeyPassthroughExtraParams, true)
	baseURL, koutakuErr := provider.baseURLOrError(key)
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	var authHeader map[string]string
	if key.Value.GetValue() != "" {
		authHeader = map[string]string{"Authorization": "Bearer " + key.Value.GetValue()}
	}
	return openai.HandleOpenAIChatCompletionStreaming(
		ctx,
		provider.streamingClient,
		baseURL+providerUtils.GetPathFromContext(ctx, "/v1/chat/completions"),
		request,
		authHeader,
		provider.networkConfig.ExtraHeaders,
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		provider.GetProviderKey(),
		postHookRunner,
		nil,
		HandleVLLMResponse,
		nil,
		nil,
		nil,
		provider.logger,
		postHookSpanFinalizer,
	)
}

// Embedding performs an embedding request to vLLM's API.
func (provider *VLLMProvider) Embedding(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuEmbeddingRequest) (*schemas.KoutakuEmbeddingResponse, *schemas.KoutakuError) {
	baseURL, koutakuErr := provider.baseURLOrError(key)
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	return openai.HandleOpenAIEmbeddingRequest(
		ctx,
		provider.client,
		baseURL+providerUtils.GetPathFromContext(ctx, "/v1/embeddings"),
		request,
		key,
		provider.networkConfig.ExtraHeaders,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		HandleVLLMResponse,
		provider.logger,
	)
}

// Responses performs a responses request to vLLM's API (via chat completion).
func (provider *VLLMProvider) Responses(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuResponsesRequest) (*schemas.KoutakuResponsesResponse, *schemas.KoutakuError) {
	chatResponse, err := provider.ChatCompletion(ctx, key, request.ToChatRequest())
	if err != nil {
		return nil, err
	}
	response := chatResponse.ToKoutakuResponsesResponse()
	return response, nil
}

// ResponsesStream performs a streaming responses request to vLLM's API (via chat completion stream).
func (provider *VLLMProvider) ResponsesStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuResponsesRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	ctx.SetValue(schemas.KoutakuContextKeyIsResponsesToChatCompletionFallback, true)
	return provider.ChatCompletionStream(
		ctx,
		postHookRunner,
		postHookSpanFinalizer,
		key,
		request.ToChatRequest(),
	)
}

// Speech is not supported by the vLLM provider.
func (provider *VLLMProvider) Speech(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuSpeechRequest) (*schemas.KoutakuSpeechResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.SpeechRequest, provider.GetProviderKey())
}

func isRerankFallbackStatus(statusCode int) bool {
	// vLLM deployments may return 501 for unimplemented routes.
	// We fallback on 501 in addition to 404/405 for compatibility.
	return statusCode == fasthttp.StatusNotFound ||
		statusCode == fasthttp.StatusMethodNotAllowed ||
		statusCode == fasthttp.StatusNotImplemented
}

func (provider *VLLMProvider) callVLLMRerankEndpoint(
	ctx *schemas.KoutakuContext,
	key schemas.Key,
	request *schemas.KoutakuRerankRequest,
	endpointPath string,
	jsonData []byte,
) (map[string]interface{}, interface{}, interface{}, []byte, int, time.Duration, *schemas.KoutakuError) {
	baseURL, koutakuErr := provider.baseURLOrError(key)
	if koutakuErr != nil {
		return nil, nil, nil, nil, 0, 0, koutakuErr
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)

	req.SetRequestURI(baseURL + endpointPath)
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")

	if key.Value.GetValue() != "" {
		req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
	}
	if !providerUtils.ApplyLargePayloadRequestBodyWithModelNormalization(ctx, req, schemas.VLLM) {
		req.SetBody(jsonData)
	}

	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, nil, nil, nil, 0, latency, koutakuErr
	}

	statusCode := resp.StatusCode()
	if statusCode != fasthttp.StatusOK {
		rawErrBody := append([]byte(nil), resp.Body()...)
		return nil, nil, nil, rawErrBody, statusCode, latency, openai.ParseOpenAIError(resp)
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		rawErrBody := append([]byte(nil), resp.Body()...)
		return nil, nil, nil, rawErrBody, statusCode, latency, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	responsePayload := make(map[string]interface{})
	rawRequest, rawResponse, koutakuErr := HandleVLLMResponse(body, &responsePayload, jsonData, sendBackRawRequest, sendBackRawResponse)
	if koutakuErr != nil {
		return nil, nil, nil, body, statusCode, latency, koutakuErr
	}

	return responsePayload, rawRequest, rawResponse, body, statusCode, latency, nil
}

// Rerank performs a rerank request to vLLM's API.
func (provider *VLLMProvider) Rerank(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuRerankRequest) (*schemas.KoutakuRerankResponse, *schemas.KoutakuError) {
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToVLLMRerankRequest(request), nil
		})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	resolvedPath := providerUtils.GetPathFromContext(ctx, "")
	hasPathOverride := resolvedPath != ""
	if !hasPathOverride {
		resolvedPath = "/v1/rerank"
	} else if !strings.HasPrefix(resolvedPath, "/") {
		resolvedPath = "/" + resolvedPath
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	responsePayload, rawRequest, rawResponse, responseBody, statusCode, latency, koutakuErr := provider.callVLLMRerankEndpoint(ctx, key, request, resolvedPath, jsonData)
	if koutakuErr != nil && !hasPathOverride && isRerankFallbackStatus(statusCode) {
		var fallbackLatency time.Duration
		responsePayload, rawRequest, rawResponse, responseBody, statusCode, fallbackLatency, koutakuErr = provider.callVLLMRerankEndpoint(ctx, key, request, "/rerank", jsonData)
		latency += fallbackLatency
	}
	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, responseBody, sendBackRawRequest, sendBackRawResponse)
	}

	returnDocuments := request.Params != nil && request.Params.ReturnDocuments != nil && *request.Params.ReturnDocuments
	koutakuResponse, err := ToKoutakuRerankResponse(responsePayload, request.Documents, returnDocuments)
	if err != nil {
		return nil, providerUtils.EnrichError(
			ctx,
			providerUtils.NewKoutakuOperationError("error converting rerank response", err),
			jsonData,
			responseBody,
			sendBackRawRequest,
			sendBackRawResponse,
		)
	}

	// Keep requested model as the canonical model in Koutaku response.
	koutakuResponse.Model = request.Model
	koutakuResponse.ExtraFields.Latency = latency.Milliseconds()

	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		koutakuResponse.ExtraFields.RawRequest = rawRequest
	}
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		koutakuResponse.ExtraFields.RawResponse = rawResponse
	}

	return koutakuResponse, nil
}

// OCR is not supported by the Vllm provider.
func (provider *VLLMProvider) OCR(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuOCRRequest) (*schemas.KoutakuOCRResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.OCRRequest, provider.GetProviderKey())
}

// SpeechStream is not supported by the vLLM provider.
func (provider *VLLMProvider) SpeechStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuSpeechRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.SpeechStreamRequest, provider.GetProviderKey())
}

// Transcription performs a transcription request to vLLM's API.
func (provider *VLLMProvider) Transcription(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuTranscriptionRequest) (*schemas.KoutakuTranscriptionResponse, *schemas.KoutakuError) {
	baseURL, koutakuErr := provider.baseURLOrError(key)
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	return openai.HandleOpenAITranscriptionRequest(
		ctx,
		provider.client,
		baseURL+providerUtils.GetPathFromContext(ctx, "/v1/audio/transcriptions"),
		request,
		key,
		provider.networkConfig.ExtraHeaders,
		provider.GetProviderKey(),
		providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		HandleVLLMResponse,
		provider.logger,
	)
}

// TranscriptionStream performs a streaming transcription request to vLLM's API.
func (provider *VLLMProvider) TranscriptionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuTranscriptionRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	baseURL, koutakuErr := provider.baseURLOrError(key)
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	{
		logger := provider.logger
		providerName := provider.GetProviderKey()
		// Use centralized converter
		reqBody := openai.ToOpenAITranscriptionRequest(request)
		if reqBody == nil {
			return nil, providerUtils.NewKoutakuOperationError("transcription input is not provided", nil)
		}
		reqBody.Stream = schemas.Ptr(true)

		// Create multipart form
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)

		if koutakuErr := openai.ParseTranscriptionFormDataBodyFromRequest(writer, reqBody, providerName); koutakuErr != nil {
			return nil, koutakuErr
		}

		// Prepare OpenAI headers
		headers := map[string]string{
			"Content-Type":  writer.FormDataContentType(),
			"Accept":        "text/event-stream",
			"Cache-Control": "no-cache",
		}
		if key.Value.GetValue() != "" {
			headers["Authorization"] = "Bearer " + key.Value.GetValue()
		}

		// Create HTTP request for streaming
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		resp.StreamBody = true
		defer fasthttp.ReleaseRequest(req)

		// Set any extra headers from network config
		providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)

		req.Header.SetMethod(http.MethodPost)
		req.SetRequestURI(baseURL + providerUtils.GetPathFromContext(ctx, "/v1/audio/transcriptions"))

		// Set headers
		for key, value := range headers {
			req.Header.Set(key, value)
		}

		req.SetBody(body.Bytes())

		// Make the request
		err := provider.streamingClient.Do(req, resp)
		if err != nil {
			defer providerUtils.ReleaseStreamingResponse(resp)
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

		// Store provider response headers in context before status check so error responses also forward them
		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

		// Check for HTTP errors
		if resp.StatusCode() != fasthttp.StatusOK {
			defer providerUtils.ReleaseStreamingResponse(resp)
			return nil, openai.ParseOpenAIError(resp)
		}

		// Large payload streaming passthrough — pipe raw upstream SSE to client
		if providerUtils.SetupStreamingPassthrough(ctx, resp) {
			responseChan := make(chan *schemas.KoutakuStreamChunk)
			close(responseChan)
			return responseChan, nil
		}

		// Create response channel
		responseChan := make(chan *schemas.KoutakuStreamChunk, schemas.DefaultStreamBufferSize)

		providerUtils.SetStreamIdleTimeoutIfEmpty(ctx, provider.networkConfig.StreamIdleTimeoutInSeconds)

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

			sseReader := providerUtils.GetSSEDataReader(ctx, reader)
			chunkIndex := -1

			startTime := time.Now()
			lastChunkTime := startTime
			var fullTranscriptionText strings.Builder

			for {
				// If context was cancelled/timed out, let defer handle it
				if ctx.Err() != nil {
					return
				}

				dataBytes, readErr := sseReader.ReadDataLine()
				if readErr != nil {
					if readErr != io.EOF {
						// If context was cancelled/timed out, let defer handle it
						if ctx.Err() != nil {
							return
						}
						ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
						logger.Warn("Error reading stream: %v", readErr)
						providerUtils.ProcessAndSendError(ctx, postHookRunner, readErr, responseChan, logger, postHookSpanFinalizer)
					}
					break
				}

				jsonData := string(dataBytes)

				// Skip empty data
				if strings.TrimSpace(jsonData) == "" {
					continue
				}

				var response schemas.KoutakuTranscriptionStreamResponse
				var koutakuErr *schemas.KoutakuError

				_, _, koutakuErr = HandleVLLMResponse(dataBytes, &response, nil, false, false)
				if koutakuErr != nil {
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, providerUtils.EnrichError(ctx, koutakuErr, body.Bytes(), dataBytes, false, providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)), responseChan, logger, postHookSpanFinalizer)
					return
				}

				customChunk, ok := parseVLLMTranscriptionStreamChunk(dataBytes)
				if !ok || customChunk == nil {
					logger.Warn("customChunkParser returned no chunk")
					continue
				}
				response = *customChunk

				chunkIndex++
				if response.Delta != nil {
					fullTranscriptionText.WriteString(*response.Delta)
				}

				response.ExtraFields = schemas.KoutakuResponseExtraFields{
					ChunkIndex: chunkIndex,
					Latency:    time.Since(lastChunkTime).Milliseconds(),
				}
				lastChunkTime = time.Now()

				if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
					response.ExtraFields.RawResponse = jsonData
				}
				if response.Usage != nil || response.Type == schemas.TranscriptionStreamResponseTypeDone {
					response.ExtraFields.Latency = time.Since(startTime).Milliseconds()
					response.Text = fullTranscriptionText.String()
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, nil, nil, &response, nil), responseChan, postHookSpanFinalizer)
					return
				}

				providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, nil, nil, &response, nil), responseChan, postHookSpanFinalizer)
			}
		}()

		return responseChan, nil
	}
}

// ImageGeneration is not supported by the vLLM provider.
func (provider *VLLMProvider) ImageGeneration(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageGenerationRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageGenerationRequest, provider.GetProviderKey())
}

// ImageGenerationStream is not supported by the vLLM provider.
func (provider *VLLMProvider) ImageGenerationStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuImageGenerationRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageGenerationStreamRequest, provider.GetProviderKey())
}

// ImageEdit is not supported by the vLLM provider.
func (provider *VLLMProvider) ImageEdit(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageEditRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageEditRequest, provider.GetProviderKey())
}

// ImageEditStream is not supported by the vLLM provider.
func (provider *VLLMProvider) ImageEditStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuImageEditRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageEditStreamRequest, provider.GetProviderKey())
}

// ImageVariation is not supported by the vLLM provider.
func (provider *VLLMProvider) ImageVariation(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageVariationRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageVariationRequest, provider.GetProviderKey())
}

// VideoGeneration is not supported by the vLLM provider.
func (provider *VLLMProvider) VideoGeneration(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoGenerationRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoGenerationRequest, provider.GetProviderKey())
}

// VideoRetrieve is not supported by the vLLM provider.
func (provider *VLLMProvider) VideoRetrieve(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoRetrieveRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoRetrieveRequest, provider.GetProviderKey())
}

// VideoDownload is not supported by the vLLM provider.
func (provider *VLLMProvider) VideoDownload(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoDownloadRequest) (*schemas.KoutakuVideoDownloadResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoDownloadRequest, provider.GetProviderKey())
}

// VideoDelete is not supported by the vLLM provider.
func (provider *VLLMProvider) VideoDelete(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoDeleteRequest) (*schemas.KoutakuVideoDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoDeleteRequest, provider.GetProviderKey())
}

// VideoList is not supported by the vLLM provider.
func (provider *VLLMProvider) VideoList(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoListRequest) (*schemas.KoutakuVideoListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoListRequest, provider.GetProviderKey())
}

// VideoRemix is not supported by the vLLM provider.
func (provider *VLLMProvider) VideoRemix(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoRemixRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoRemixRequest, provider.GetProviderKey())
}

// FileUpload is not supported by the vLLM provider.
func (provider *VLLMProvider) FileUpload(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuFileUploadRequest) (*schemas.KoutakuFileUploadResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileUploadRequest, provider.GetProviderKey())
}

// FileList is not supported by the vLLM provider.
func (provider *VLLMProvider) FileList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuFileListRequest) (*schemas.KoutakuFileListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileListRequest, provider.GetProviderKey())
}

// FileRetrieve is not supported by the vLLM provider.
func (provider *VLLMProvider) FileRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuFileRetrieveRequest) (*schemas.KoutakuFileRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileRetrieveRequest, provider.GetProviderKey())
}

// FileDelete is not supported by the vLLM provider.
func (provider *VLLMProvider) FileDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuFileDeleteRequest) (*schemas.KoutakuFileDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileDeleteRequest, provider.GetProviderKey())
}

// FileContent is not supported by the vLLM provider.
func (provider *VLLMProvider) FileContent(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuFileContentRequest) (*schemas.KoutakuFileContentResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileContentRequest, provider.GetProviderKey())
}

// BatchCreate is not supported by the vLLM provider.
func (provider *VLLMProvider) BatchCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuBatchCreateRequest) (*schemas.KoutakuBatchCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchCreateRequest, provider.GetProviderKey())
}

// BatchList is not supported by the vLLM provider.
func (provider *VLLMProvider) BatchList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchListRequest) (*schemas.KoutakuBatchListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchListRequest, provider.GetProviderKey())
}

// BatchRetrieve is not supported by the vLLM provider.
func (provider *VLLMProvider) BatchRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchRetrieveRequest) (*schemas.KoutakuBatchRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchRetrieveRequest, provider.GetProviderKey())
}

// BatchCancel is not supported by the vLLM provider.
func (provider *VLLMProvider) BatchCancel(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchCancelRequest) (*schemas.KoutakuBatchCancelResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchCancelRequest, provider.GetProviderKey())
}

// BatchDelete is not supported by the vLLM provider.
func (provider *VLLMProvider) BatchDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchDeleteRequest) (*schemas.KoutakuBatchDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchDeleteRequest, provider.GetProviderKey())
}

// BatchResults is not supported by the vLLM provider.
func (provider *VLLMProvider) BatchResults(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchResultsRequest) (*schemas.KoutakuBatchResultsResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchResultsRequest, provider.GetProviderKey())
}

// CountTokens is not supported by the vLLM provider.
func (provider *VLLMProvider) CountTokens(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuResponsesRequest) (*schemas.KoutakuCountTokensResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.CountTokensRequest, provider.GetProviderKey())
}

// ContainerCreate is not supported by the vLLM provider.
func (provider *VLLMProvider) ContainerCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuContainerCreateRequest) (*schemas.KoutakuContainerCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerCreateRequest, provider.GetProviderKey())
}

// ContainerList is not supported by the vLLM provider.
func (provider *VLLMProvider) ContainerList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerListRequest) (*schemas.KoutakuContainerListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerListRequest, provider.GetProviderKey())
}

// ContainerRetrieve is not supported by the vLLM provider.
func (provider *VLLMProvider) ContainerRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerRetrieveRequest) (*schemas.KoutakuContainerRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerRetrieveRequest, provider.GetProviderKey())
}

// ContainerDelete is not supported by the vLLM provider.
func (provider *VLLMProvider) ContainerDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerDeleteRequest) (*schemas.KoutakuContainerDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerDeleteRequest, provider.GetProviderKey())
}

// ContainerFileCreate is not supported by the vLLM provider.
func (provider *VLLMProvider) ContainerFileCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuContainerFileCreateRequest) (*schemas.KoutakuContainerFileCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileCreateRequest, provider.GetProviderKey())
}

// ContainerFileList is not supported by the vLLM provider.
func (provider *VLLMProvider) ContainerFileList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileListRequest) (*schemas.KoutakuContainerFileListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileListRequest, provider.GetProviderKey())
}

// ContainerFileRetrieve is not supported by the vLLM provider.
func (provider *VLLMProvider) ContainerFileRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileRetrieveRequest) (*schemas.KoutakuContainerFileRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileRetrieveRequest, provider.GetProviderKey())
}

// ContainerFileContent is not supported by the vLLM provider.
func (provider *VLLMProvider) ContainerFileContent(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileContentRequest) (*schemas.KoutakuContainerFileContentResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileContentRequest, provider.GetProviderKey())
}

// ContainerFileDelete is not supported by the vLLM provider.
func (provider *VLLMProvider) ContainerFileDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileDeleteRequest) (*schemas.KoutakuContainerFileDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileDeleteRequest, provider.GetProviderKey())
}

// Passthrough is not supported by the vLLM provider.
func (provider *VLLMProvider) Passthrough(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuPassthroughRequest) (*schemas.KoutakuPassthroughResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.PassthroughRequest, provider.GetProviderKey())
}

func (provider *VLLMProvider) PassthroughStream(_ *schemas.KoutakuContext, _ schemas.PostHookRunner, _ func(context.Context), _ schemas.Key, _ *schemas.KoutakuPassthroughRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.PassthroughStreamRequest, provider.GetProviderKey())
}
