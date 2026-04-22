package elevenlabs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	schemas "github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

type ElevenlabsProvider struct {
	logger               schemas.Logger                // Logger for provider operations
	client               *fasthttp.Client              // HTTP client for unary API requests (ReadTimeout bounds overall response)
	streamingClient      *fasthttp.Client              // HTTP client for streaming API requests (no ReadTimeout; idle governed by NewIdleTimeoutReader)
	networkConfig        schemas.NetworkConfig         // Network configuration including extra headers
	sendBackRawRequest   bool                          // Whether to include raw request in KoutakuResponse
	sendBackRawResponse  bool                          // Whether to include raw response in KoutakuResponse
	customProviderConfig *schemas.CustomProviderConfig // Custom provider config
}

// NewElevenlabsProvider creates a new Elevenlabs provider instance.
// It initializes the HTTP client with the provided configuration.
// The client is configured with timeouts, concurrency limits, and optional proxy settings.
func NewElevenlabsProvider(config *schemas.ProviderConfig, logger schemas.Logger) *ElevenlabsProvider {
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
		config.NetworkConfig.BaseURL = "https://api.elevenlabs.io"
	}
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")

	return &ElevenlabsProvider{
		logger:               logger,
		client:               client,
		streamingClient:      streamingClient,
		networkConfig:        config.NetworkConfig,
		customProviderConfig: config.CustomProviderConfig,
		sendBackRawRequest:   config.SendBackRawRequest,
		sendBackRawResponse:  config.SendBackRawResponse,
	}
}

// GetProviderKey returns the provider identifier for Elevenlabs.
func (provider *ElevenlabsProvider) GetProviderKey() schemas.ModelProvider {
	return providerUtils.GetProviderName(schemas.Elevenlabs, provider.customProviderConfig)
}

// listModelsByKey performs a list models request for a single key.
// Returns the response and latency, or an error if the request fails.
func (provider *ElevenlabsProvider) listModelsByKey(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuListModelsRequest) (*schemas.KoutakuListModelsResponse, *schemas.KoutakuError) {
	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Set any extra headers from network config
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)

	// Build URL using centralized URL construction
	req.SetRequestURI(provider.networkConfig.BaseURL + providerUtils.GetPathFromContext(ctx, "/v1/models"))
	req.Header.SetMethod(http.MethodGet)
	req.Header.SetContentType("application/json")

	if key.Value.GetValue() != "" {
		req.Header.Set("xi-api-key", key.Value.GetValue())
	}

	// Make request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	// Extract and set provider response headers so they're available on error paths
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, parseElevenlabsError(resp)
	}

	var elevenlabsResponse ElevenlabsListModelsResponse
	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(resp.Body(), &elevenlabsResponse, nil, providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse))
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	response := elevenlabsResponse.ToKoutakuListModelsResponse(provider.GetProviderKey(), key.Models, key.BlacklistedModels, key.Aliases, request.Unfiltered)

	response.ExtraFields.Latency = latency.Milliseconds()
	response.ExtraFields.ProviderResponseHeaders = providerUtils.ExtractProviderResponseHeaders(resp)

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

// ListModels performs a list models request to Elevenlabs' API.
// Requests are made concurrently for improved performance.
func (provider *ElevenlabsProvider) ListModels(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuListModelsRequest) (*schemas.KoutakuListModelsResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Elevenlabs, provider.customProviderConfig, schemas.ListModelsRequest); err != nil {
		return nil, err
	}
	return providerUtils.HandleMultipleListModelsRequests(
		ctx,
		keys,
		request,
		provider.listModelsByKey,
	)
}

// TextCompletion is not supported by the Elevenlabs provider
func (provider *ElevenlabsProvider) TextCompletion(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuTextCompletionRequest) (*schemas.KoutakuTextCompletionResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TextCompletionRequest, provider.GetProviderKey())
}

// TextCompletionStream is not supported by the Elevenlabs provider
func (provider *ElevenlabsProvider) TextCompletionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuTextCompletionRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TextCompletionStreamRequest, provider.GetProviderKey())
}

// ChatCompletion is not supported by the Elevenlabs provider
func (provider *ElevenlabsProvider) ChatCompletion(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuChatRequest) (*schemas.KoutakuChatResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ChatCompletionRequest, provider.GetProviderKey())
}

// ChatCompletionStream is not supported by the Elevenlabs provider
func (provider *ElevenlabsProvider) ChatCompletionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuChatRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ChatCompletionStreamRequest, provider.GetProviderKey())
}

// Responses is not supported by the Elevenlabs provider
func (provider *ElevenlabsProvider) Responses(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuResponsesRequest) (*schemas.KoutakuResponsesResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ResponsesRequest, provider.GetProviderKey())
}

// ResponsesStream is not supported by the Elevenlabs provider
func (provider *ElevenlabsProvider) ResponsesStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuResponsesRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ResponsesStreamRequest, provider.GetProviderKey())
}

// Embedding is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) Embedding(ctx *schemas.KoutakuContext, key schemas.Key, input *schemas.KoutakuEmbeddingRequest) (*schemas.KoutakuEmbeddingResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.EmbeddingRequest, provider.GetProviderKey())
}

// Speech performs a text to speech request
func (provider *ElevenlabsProvider) Speech(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuSpeechRequest) (*schemas.KoutakuSpeechResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Elevenlabs, provider.customProviderConfig, schemas.SpeechRequest); err != nil {
		return nil, err
	}

	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Set any extra headers from network config
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)

	withTimestampsRequest := request.Params != nil && request.Params.WithTimestamps != nil && *request.Params.WithTimestamps

	var endpoint string
	if request.Params != nil && request.Params.VoiceConfig != nil && request.Params.VoiceConfig.Voice != nil {
		voice := *request.Params.VoiceConfig.Voice
		// Determine if timestamps are requested
		if withTimestampsRequest {
			endpoint = "/v1/text-to-speech/" + voice + "/with-timestamps"
		} else {
			endpoint = "/v1/text-to-speech/" + voice
		}
	} else {
		return nil, providerUtils.NewKoutakuOperationError("voice parameter is required", nil)
	}

	requestURL := provider.buildBaseSpeechRequestURL(ctx, endpoint, schemas.SpeechRequest, request)
	req.SetRequestURI(requestURL)

	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")
	if key.Value.GetValue() != "" {
		req.Header.Set("xi-api-key", key.Value.GetValue())
	}

	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToElevenlabsSpeechRequest(request), nil
		})

	if koutakuErr != nil {
		return nil, koutakuErr
	}

	if !providerUtils.ApplyLargePayloadRequestBody(ctx, req) {
		req.SetBody(jsonData)
	}

	// Make request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}
	// Extract and set provider response headers so they're available on error paths
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, providerUtils.EnrichError(ctx, parseElevenlabsError(resp), jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Get the response body
	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err), jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Create response based on whether timestamps were requested
	koutakuResponse := &schemas.KoutakuSpeechResponse{
		ExtraFields: schemas.KoutakuResponseExtraFields{
			Latency:                 latency.Milliseconds(),
			ProviderResponseHeaders: providerUtils.ExtractProviderResponseHeaders(resp),
		},
	}

	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		providerUtils.ParseAndSetRawRequest(&koutakuResponse.ExtraFields, jsonData)
	}

	if withTimestampsRequest {
		var timestampResponse ElevenlabsSpeechWithTimestampsResponse
		if err := sonic.Unmarshal(body, &timestampResponse); err != nil {
			return nil, providerUtils.NewKoutakuOperationError("failed to parse with-timestamps response", err)
		}

		koutakuResponse.AudioBase64 = &timestampResponse.AudioBase64

		if timestampResponse.Alignment != nil {
			koutakuResponse.Alignment = &schemas.SpeechAlignment{
				CharStartTimesMs: timestampResponse.Alignment.CharStartTimesMs,
				CharEndTimesMs:   timestampResponse.Alignment.CharEndTimesMs,
				Characters:       timestampResponse.Alignment.Characters,
			}
		}

		if timestampResponse.NormalizedAlignment != nil {
			koutakuResponse.NormalizedAlignment = &schemas.SpeechAlignment{
				CharStartTimesMs: timestampResponse.NormalizedAlignment.CharStartTimesMs,
				CharEndTimesMs:   timestampResponse.NormalizedAlignment.CharEndTimesMs,
				Characters:       timestampResponse.NormalizedAlignment.Characters,
			}
		}

		return koutakuResponse, nil
	}

	koutakuResponse.Audio = body
	return koutakuResponse, nil
}

// Rerank is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) Rerank(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuRerankRequest) (*schemas.KoutakuRerankResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.RerankRequest, provider.GetProviderKey())
}

// OCR is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) OCR(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuOCRRequest) (*schemas.KoutakuOCRResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.OCRRequest, provider.GetProviderKey())
}

// SpeechStream performs a text to speech stream request
func (provider *ElevenlabsProvider) SpeechStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuSpeechRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Elevenlabs, provider.customProviderConfig, schemas.SpeechStreamRequest); err != nil {
		return nil, err
	}

	jsonBody, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToElevenlabsSpeechRequest(request), nil
		})

	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Create HTTP request for streaming
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	resp.StreamBody = true
	defer fasthttp.ReleaseRequest(req)

	// Set any extra headers from network config
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)

	if request.Params == nil || request.Params.VoiceConfig == nil || request.Params.VoiceConfig.Voice == nil {
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewKoutakuOperationError("voice parameter is required", nil), jsonBody, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	req.SetRequestURI(provider.buildBaseSpeechRequestURL(ctx, "/v1/text-to-speech/"+*request.Params.VoiceConfig.Voice+"/stream", schemas.SpeechStreamRequest, request))

	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")
	if key.Value.GetValue() != "" {
		req.Header.Set("xi-api-key", key.Value.GetValue())
	}

	if !providerUtils.ApplyLargePayloadRequestBody(ctx, req) {
		req.SetBody(jsonBody)
	}

	// Make request
	startTime := time.Now()
	err := provider.streamingClient.Do(req, resp)
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
			}, jsonBody, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
		}
		if errors.Is(err, fasthttp.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
			return nil, providerUtils.EnrichError(ctx, providerUtils.NewKoutakuTimeoutError(schemas.ErrProviderRequestTimedOut, err), jsonBody, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
		}
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewKoutakuOperationError(schemas.ErrProviderDoRequest, err), jsonBody, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Extract provider response headers before status check so error responses also forward them
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

	// Check for HTTP errors
	if resp.StatusCode() != fasthttp.StatusOK {
		defer providerUtils.ReleaseStreamingResponse(resp)
		return nil, providerUtils.EnrichError(ctx, parseElevenlabsError(resp), jsonBody, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Create response channel
	responseChan := make(chan *schemas.KoutakuStreamChunk, schemas.DefaultStreamBufferSize)

	providerUtils.SetStreamIdleTimeoutIfEmpty(ctx, provider.networkConfig.StreamIdleTimeoutInSeconds)

	go func() {
		defer func() {
			if ctx.Err() == context.Canceled {
				providerUtils.HandleStreamCancellation(ctx, postHookRunner, responseChan, provider.logger, postHookSpanFinalizer)
			} else if ctx.Err() == context.DeadlineExceeded {
				providerUtils.HandleStreamTimeout(ctx, postHookRunner, responseChan, provider.logger, postHookSpanFinalizer)
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
		stopCancellation := providerUtils.SetupStreamCancellation(ctx, resp.BodyStream(), provider.logger)
		defer stopCancellation()
		defer providerUtils.EnsureStreamFinalizerCalled(ctx, postHookSpanFinalizer)

		// read binary audio chunks from the stream
		// 4KB buffer for reading chunks
		buffer := make([]byte, 4096)
		bodyStream := reader
		chunkIndex := -1
		lastChunkTime := time.Now()

		for {
			// If context was cancelled/timed out, let defer handle it
			if ctx.Err() != nil {
				return
			}
			n, err := bodyStream.Read(buffer)
			if err != nil {
				// If context was cancelled/timed out, let defer handle it
				if ctx.Err() != nil {
					return
				}
				if err == io.EOF {
					break
				}
				ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
				provider.logger.Warn("Error reading stream: %v", err)
				providerUtils.ProcessAndSendError(ctx, postHookRunner, err, responseChan, provider.logger, postHookSpanFinalizer)
				return
			}

			if n > 0 {
				chunkIndex++
				audioChunk := make([]byte, n)
				copy(audioChunk, buffer[:n])

				response := &schemas.KoutakuSpeechStreamResponse{
					Type:  schemas.SpeechStreamResponseTypeDelta,
					Audio: audioChunk,
					ExtraFields: schemas.KoutakuResponseExtraFields{
						ChunkIndex: chunkIndex,
						Latency:    time.Since(lastChunkTime).Milliseconds(),
					},
				}

				lastChunkTime = time.Now()

				if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
					response.ExtraFields.RawResponse = audioChunk
				}

				providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, nil, response, nil, nil), responseChan, postHookSpanFinalizer)
			}
		}

		// Send final response after natural loop termination (similar to Gemini pattern)
		finalResponse := &schemas.KoutakuSpeechStreamResponse{
			Type:  schemas.SpeechStreamResponseTypeDone,
			Audio: []byte{},
			ExtraFields: schemas.KoutakuResponseExtraFields{
				ChunkIndex: chunkIndex + 1,
				Latency:    time.Since(startTime).Milliseconds(),
			},
		}

		// Set raw request if enabled
		if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
			providerUtils.ParseAndSetRawRequest(&finalResponse.ExtraFields, jsonBody)
		}
		ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
		providerUtils.ProcessAndSendResponse(ctx, postHookRunner, providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, nil, finalResponse, nil, nil), responseChan, postHookSpanFinalizer)
	}()

	return responseChan, nil
}

// Transcription performs a transcription request
func (provider *ElevenlabsProvider) Transcription(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuTranscriptionRequest) (*schemas.KoutakuTranscriptionResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Elevenlabs, provider.customProviderConfig, schemas.TranscriptionRequest); err != nil {
		return nil, err
	}

	reqBody := ToElevenlabsTranscriptionRequest(request)
	if reqBody == nil {
		return nil, providerUtils.NewKoutakuOperationError("transcription request is not provided", nil)
	}

	hasFile := len(reqBody.File) > 0
	hasURL := reqBody.CloudStorageURL != nil && strings.TrimSpace(*reqBody.CloudStorageURL) != ""
	if hasFile && hasURL {
		return nil, providerUtils.NewKoutakuOperationError("provide either a file or cloud_storage_url, not both", nil)
	}
	if !hasFile && !hasURL {
		return nil, providerUtils.NewKoutakuOperationError("either a transcription file or cloud_storage_url must be provided", nil)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if koutakuErr := writeTranscriptionMultipart(writer, reqBody); koutakuErr != nil {
		return nil, koutakuErr
	}

	contentType := writer.FormDataContentType()
	if err := writer.Close(); err != nil {
		return nil, providerUtils.NewKoutakuOperationError("failed to finalize multipart transcription request", err)
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)

	requestPath, isCompleteURL := providerUtils.GetRequestPath(ctx, "/v1/speech-to-text", provider.customProviderConfig, schemas.TranscriptionRequest)
	if isCompleteURL {
		req.SetRequestURI(requestPath)
	} else {
		req.SetRequestURI(provider.networkConfig.BaseURL + requestPath)
	}
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType(contentType)
	if key.Value.GetValue() != "" {
		req.Header.Set("xi-api-key", key.Value.GetValue())
	}
	req.SetBody(body.Bytes())

	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	// Extract and set provider response headers so they're available on error paths
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, parseElevenlabsError(resp)
	}

	responseBody, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
	}

	// Check for empty response
	trimmed := strings.TrimSpace(string(responseBody))
	if len(trimmed) == 0 {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: true,
			Error: &schemas.ErrorField{
				Message: schemas.ErrProviderResponseEmpty,
			},
		}
	}

	chunks, err := parseTranscriptionResponse(responseBody)
	if err != nil {
		return nil, providerUtils.NewKoutakuOperationError(err.Error(), nil)
	}

	if len(chunks) == 0 {
		return nil, providerUtils.NewKoutakuOperationError("no chunks found in transcription response", nil)
	}

	response := ToKoutakuTranscriptionResponse(chunks)
	response.ExtraFields = schemas.KoutakuResponseExtraFields{
		Latency:                 latency.Milliseconds(),
		ProviderResponseHeaders: providerUtils.ExtractProviderResponseHeaders(resp),
	}

	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		var rawResponse interface{}
		if err := sonic.Unmarshal(responseBody, &rawResponse); err != nil {
			rawResponse = string(responseBody)
		}
		response.ExtraFields.RawResponse = rawResponse
	}

	return response, nil
}

func writeTranscriptionMultipart(writer *multipart.Writer, reqBody *ElevenlabsTranscriptionRequest) *schemas.KoutakuError {
	if err := writer.WriteField("model_id", reqBody.ModelID); err != nil {
		return providerUtils.NewKoutakuOperationError("failed to write model_id field", err)
	}

	if len(reqBody.File) > 0 {
		filename := reqBody.Filename
		if filename == "" {
			filename = providerUtils.AudioFilenameFromBytes(reqBody.File)
		}
		fileWriter, err := writer.CreateFormFile("file", filename)
		if err != nil {
			return providerUtils.NewKoutakuOperationError("failed to create file field", err)
		}
		if _, err := fileWriter.Write(reqBody.File); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write file data", err)
		}
	}

	if reqBody.CloudStorageURL != nil && strings.TrimSpace(*reqBody.CloudStorageURL) != "" {
		if err := writer.WriteField("cloud_storage_url", *reqBody.CloudStorageURL); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write cloud_storage_url field", err)
		}
	}

	if reqBody.LanguageCode != nil && strings.TrimSpace(*reqBody.LanguageCode) != "" {
		if err := writer.WriteField("language_code", *reqBody.LanguageCode); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write language_code field", err)
		}
	}

	if reqBody.TagAudioEvents != nil {
		if err := writer.WriteField("tag_audio_events", strconv.FormatBool(*reqBody.TagAudioEvents)); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write tag_audio_events field", err)
		}
	}

	if reqBody.NumSpeakers != nil && *reqBody.NumSpeakers > 0 {
		if err := writer.WriteField("num_speakers", strconv.Itoa(*reqBody.NumSpeakers)); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write num_speakers field", err)
		}
	}

	if reqBody.TimestampsGranularity != nil && *reqBody.TimestampsGranularity != "" {
		if err := writer.WriteField("timestamps_granularity", string(*reqBody.TimestampsGranularity)); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write timestamps_granularity field", err)
		}
	}

	if reqBody.Diarize != nil {
		if err := writer.WriteField("diarize", strconv.FormatBool(*reqBody.Diarize)); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write diarize field", err)
		}
	}

	if reqBody.DiarizationThreshold != nil {
		if err := writer.WriteField("diarization_threshold", strconv.FormatFloat(*reqBody.DiarizationThreshold, 'f', -1, 64)); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write diarization_threshold field", err)
		}
	}

	if len(reqBody.AdditionalFormats) > 0 {
		payload, err := providerUtils.MarshalSorted(reqBody.AdditionalFormats)
		if err != nil {
			return providerUtils.NewKoutakuOperationError("failed to marshal additional_formats", err)
		}
		if err := writer.WriteField("additional_formats", string(payload)); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write additional_formats field", err)
		}
	}

	if reqBody.FileFormat != nil && *reqBody.FileFormat != "" {
		if err := writer.WriteField("file_format", string(*reqBody.FileFormat)); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write file_format field", err)
		}
	}

	if reqBody.Webhook != nil {
		if err := writer.WriteField("webhook", strconv.FormatBool(*reqBody.Webhook)); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write webhook field", err)
		}
	}

	if reqBody.WebhookID != nil && strings.TrimSpace(*reqBody.WebhookID) != "" {
		if err := writer.WriteField("webhook_id", *reqBody.WebhookID); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write webhook_id field", err)
		}
	}

	if reqBody.Temperature != nil {
		if err := writer.WriteField("temperature", strconv.FormatFloat(*reqBody.Temperature, 'f', -1, 64)); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write temperature field", err)
		}
	}

	if reqBody.Seed != nil {
		if err := writer.WriteField("seed", strconv.Itoa(*reqBody.Seed)); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write seed field", err)
		}
	}

	if reqBody.UseMultiChannel != nil {
		if err := writer.WriteField("use_multi_channel", strconv.FormatBool(*reqBody.UseMultiChannel)); err != nil {
			return providerUtils.NewKoutakuOperationError("failed to write use_multi_channel field", err)
		}
	}

	if reqBody.WebhookMetadata != nil {
		switch v := reqBody.WebhookMetadata.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				if err := writer.WriteField("webhook_metadata", v); err != nil {
					return providerUtils.NewKoutakuOperationError("failed to write webhook_metadata field", err)
				}
			}
		default:
			payload, err := providerUtils.MarshalSorted(v)
			if err != nil {
				return providerUtils.NewKoutakuOperationError("failed to marshal webhook_metadata", err)
			}
			if err := writer.WriteField("webhook_metadata", string(payload)); err != nil {
				return providerUtils.NewKoutakuOperationError("failed to write webhook_metadata field", err)
			}
		}
	}

	return nil
}

// TranscriptionStream is not supported by the Elevenlabs provider
func (provider *ElevenlabsProvider) TranscriptionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuTranscriptionRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TranscriptionStreamRequest, provider.GetProviderKey())
}

// ImageGeneration is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) ImageGeneration(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageGenerationRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageGenerationRequest, provider.GetProviderKey())
}

// ImageGenerationStream is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) ImageGenerationStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuImageGenerationRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageGenerationStreamRequest, provider.GetProviderKey())
}

// ImageEdit is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) ImageEdit(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageEditRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageEditRequest, provider.GetProviderKey())
}

// ImageEditStream is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) ImageEditStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuImageEditRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageEditStreamRequest, provider.GetProviderKey())
}

// ImageVariation is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) ImageVariation(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageVariationRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageVariationRequest, provider.GetProviderKey())
}

// VideoGeneration is not supported by the ElevenLabs provider.
func (provider *ElevenlabsProvider) VideoGeneration(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoGenerationRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoGenerationRequest, provider.GetProviderKey())
}

// VideoRetrieve is not supported by the ElevenLabs provider.
func (provider *ElevenlabsProvider) VideoRetrieve(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoRetrieveRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoRetrieveRequest, provider.GetProviderKey())
}

// VideoDownload is not supported by the ElevenLabs provider.
func (provider *ElevenlabsProvider) VideoDownload(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoDownloadRequest) (*schemas.KoutakuVideoDownloadResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoDownloadRequest, provider.GetProviderKey())
}

// VideoDelete is not supported by Elevenlabs provider.
func (provider *ElevenlabsProvider) VideoDelete(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoDeleteRequest) (*schemas.KoutakuVideoDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoDeleteRequest, provider.GetProviderKey())
}

// VideoList is not supported by Elevenlabs provider.
func (provider *ElevenlabsProvider) VideoList(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoListRequest) (*schemas.KoutakuVideoListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoListRequest, provider.GetProviderKey())
}

// VideoRemix is not supported by Elevenlabs provider.
func (provider *ElevenlabsProvider) VideoRemix(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoRemixRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoRemixRequest, provider.GetProviderKey())
}

// buildSpeechRequestURL constructs the full request URL using the provider's configuration for speech.
func (provider *ElevenlabsProvider) buildBaseSpeechRequestURL(ctx *schemas.KoutakuContext, defaultPath string, requestType schemas.RequestType, request *schemas.KoutakuSpeechRequest) string {
	baseURL := provider.networkConfig.BaseURL
	requestPath, isCompleteURL := providerUtils.GetRequestPath(ctx, defaultPath, provider.customProviderConfig, requestType)

	var finalURL string
	if isCompleteURL {
		finalURL = requestPath
	} else {
		u, parseErr := url.Parse(baseURL)
		if parseErr != nil {
			finalURL = baseURL + requestPath
		} else {
			u.Path = path.Join(u.Path, requestPath)
			finalURL = u.String()
		}
	}

	// Parse the final URL to add query parameters
	u, parseErr := url.Parse(finalURL)
	if parseErr != nil {
		return finalURL
	}

	q := u.Query()

	if request.Params != nil {
		if request.Params.EnableLogging != nil {
			q.Set("enable_logging", strconv.FormatBool(*request.Params.EnableLogging))
		}

		convertedFormat := ConvertKoutakuSpeechFormatToElevenlabs(request.Params.ResponseFormat)
		if convertedFormat != "" {
			q.Set("output_format", convertedFormat)
		}

		if request.Params.OptimizeStreamingLatency != nil {
			q.Set("optimize_streaming_latency", strconv.FormatBool(*request.Params.OptimizeStreamingLatency))
		}
	}

	u.RawQuery = q.Encode()
	return u.String()
}

// BatchCreate is not supported by Elevenlabs provider.
func (provider *ElevenlabsProvider) BatchCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuBatchCreateRequest) (*schemas.KoutakuBatchCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchCreateRequest, provider.GetProviderKey())
}

// BatchList is not supported by Elevenlabs provider.
func (provider *ElevenlabsProvider) BatchList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchListRequest) (*schemas.KoutakuBatchListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchListRequest, provider.GetProviderKey())
}

// BatchRetrieve is not supported by Elevenlabs provider.
func (provider *ElevenlabsProvider) BatchRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchRetrieveRequest) (*schemas.KoutakuBatchRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchRetrieveRequest, provider.GetProviderKey())
}

// BatchCancel is not supported by Elevenlabs provider.
func (provider *ElevenlabsProvider) BatchCancel(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchCancelRequest) (*schemas.KoutakuBatchCancelResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchCancelRequest, provider.GetProviderKey())
}

// BatchDelete is not supported by Elevenlabs provider.
func (provider *ElevenlabsProvider) BatchDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchDeleteRequest) (*schemas.KoutakuBatchDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchDeleteRequest, provider.GetProviderKey())
}

// BatchResults is not supported by Elevenlabs provider.
func (provider *ElevenlabsProvider) BatchResults(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchResultsRequest) (*schemas.KoutakuBatchResultsResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchResultsRequest, provider.GetProviderKey())
}

// FileUpload is not supported by Elevenlabs provider.
func (provider *ElevenlabsProvider) FileUpload(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuFileUploadRequest) (*schemas.KoutakuFileUploadResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileUploadRequest, provider.GetProviderKey())
}

// FileList is not supported by Elevenlabs provider.
func (provider *ElevenlabsProvider) FileList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuFileListRequest) (*schemas.KoutakuFileListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileListRequest, provider.GetProviderKey())
}

// FileRetrieve is not supported by Elevenlabs provider.
func (provider *ElevenlabsProvider) FileRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuFileRetrieveRequest) (*schemas.KoutakuFileRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileRetrieveRequest, provider.GetProviderKey())
}

// FileDelete is not supported by Elevenlabs provider.
func (provider *ElevenlabsProvider) FileDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuFileDeleteRequest) (*schemas.KoutakuFileDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileDeleteRequest, provider.GetProviderKey())
}

// FileContent is not supported by Elevenlabs provider.
func (provider *ElevenlabsProvider) FileContent(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuFileContentRequest) (*schemas.KoutakuFileContentResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileContentRequest, provider.GetProviderKey())
}

// CountTokens is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) CountTokens(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuResponsesRequest) (*schemas.KoutakuCountTokensResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.CountTokensRequest, provider.GetProviderKey())
}

// ContainerCreate is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) ContainerCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuContainerCreateRequest) (*schemas.KoutakuContainerCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerCreateRequest, provider.GetProviderKey())
}

// ContainerList is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) ContainerList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerListRequest) (*schemas.KoutakuContainerListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerListRequest, provider.GetProviderKey())
}

// ContainerRetrieve is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) ContainerRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerRetrieveRequest) (*schemas.KoutakuContainerRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerRetrieveRequest, provider.GetProviderKey())
}

// ContainerDelete is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) ContainerDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerDeleteRequest) (*schemas.KoutakuContainerDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerDeleteRequest, provider.GetProviderKey())
}

// ContainerFileCreate is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) ContainerFileCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuContainerFileCreateRequest) (*schemas.KoutakuContainerFileCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileCreateRequest, provider.GetProviderKey())
}

// ContainerFileList is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) ContainerFileList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileListRequest) (*schemas.KoutakuContainerFileListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileListRequest, provider.GetProviderKey())
}

// ContainerFileRetrieve is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) ContainerFileRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileRetrieveRequest) (*schemas.KoutakuContainerFileRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileRetrieveRequest, provider.GetProviderKey())
}

// ContainerFileContent is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) ContainerFileContent(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileContentRequest) (*schemas.KoutakuContainerFileContentResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileContentRequest, provider.GetProviderKey())
}

// ContainerFileDelete is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) ContainerFileDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileDeleteRequest) (*schemas.KoutakuContainerFileDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileDeleteRequest, provider.GetProviderKey())
}

// Passthrough is not supported by the Elevenlabs provider.
func (provider *ElevenlabsProvider) Passthrough(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuPassthroughRequest) (*schemas.KoutakuPassthroughResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.PassthroughRequest, provider.GetProviderKey())
}

func (provider *ElevenlabsProvider) PassthroughStream(_ *schemas.KoutakuContext, _ schemas.PostHookRunner, _ func(context.Context), _ schemas.Key, _ *schemas.KoutakuPassthroughRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.PassthroughStreamRequest, provider.GetProviderKey())
}
