// Package providers implements various LLM providers and their utility functions.
// This file contains the replicate provider implementation.
package replicate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	schemas "github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

// ReplicateProvider implements the Provider interface for Replicate's API.
type ReplicateProvider struct {
	logger               schemas.Logger        // Logger for provider operations
	client               *fasthttp.Client      // HTTP client for unary API requests (ReadTimeout bounds overall response)
	streamingClient      *fasthttp.Client      // HTTP client for streaming API requests (no ReadTimeout; idle governed by NewIdleTimeoutReader)
	networkConfig        schemas.NetworkConfig // Network configuration including extra headers
	sendBackRawRequest   bool                  // Whether to include raw request in KoutakuResponse
	sendBackRawResponse  bool                  // Whether to include raw response in KoutakuResponse
	customProviderConfig *schemas.CustomProviderConfig
}

// NewReplicateProvider creates a new Replicate provider instance.
// It initializes the HTTP client with the provided configuration and sets up response pools.
// The client is configured with timeouts, concurrency limits, and optional proxy settings.
func NewReplicateProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*ReplicateProvider, error) {
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
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")

	if config.NetworkConfig.BaseURL == "" {
		config.NetworkConfig.BaseURL = replicateAPIBaseURL
	}

	return &ReplicateProvider{
		logger:               logger,
		client:               client,
		streamingClient:      streamingClient,
		networkConfig:        config.NetworkConfig,
		sendBackRawRequest:   config.SendBackRawRequest,
		sendBackRawResponse:  config.SendBackRawResponse,
		customProviderConfig: config.CustomProviderConfig,
	}, nil
}

// GetProviderKey returns the provider identifier for Replicate.
func (provider *ReplicateProvider) GetProviderKey() schemas.ModelProvider {
	return schemas.Replicate
}

// buildRequestURL builds the request URL with custom provider config support
func (provider *ReplicateProvider) buildRequestURL(ctx *schemas.KoutakuContext, defaultPath string, requestType schemas.RequestType) string {
	path, isCompleteURL := providerUtils.GetRequestPath(ctx, defaultPath, provider.customProviderConfig, requestType)
	if isCompleteURL {
		return path
	}
	return provider.networkConfig.BaseURL + path
}

const (
	replicateAPIBaseURL = "https://api.replicate.com"
	pollingInterval     = 2 * time.Second
)

// useDeploymentsEndpoint returns whether the key uses the deployments endpoint.
// Nil ReplicateKeyConfig is treated as false (default models/predictions behavior).
func useDeploymentsEndpoint(key schemas.Key) bool {
	return key.ReplicateKeyConfig != nil && key.ReplicateKeyConfig.UseDeploymentsEndpoint
}

// createPrediction creates a new prediction on Replicate API
// Supports both sync (with Prefer: wait header) and async modes
// stripPrefer should be true for streaming requests to exclude the Prefer header
func createPrediction(
	ctx *schemas.KoutakuContext,
	client *fasthttp.Client,
	jsonBody []byte,
	key schemas.Key,
	url string,
	extraHeaders map[string]string,
	stripPrefer bool,
	logger schemas.Logger,
	sendBackRawRequest bool,
	sendBackRawResponse bool,
) (*ReplicatePredictionResponse, interface{}, time.Duration, map[string]string, *schemas.KoutakuError) {
	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Set URL
	req.SetRequestURI(url)
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType("application/json")

	// Set authorization header
	if value := key.Value.GetValue(); value != "" {
		req.Header.Set("Authorization", "Bearer "+value)
	}

	// Set any extra headers from network config
	// Strip Prefer header for streaming requests to ensure async mode
	headersToUse := extraHeaders
	if stripPrefer {
		headersToUse = stripPreferHeader(extraHeaders)
	}
	providerUtils.SetExtraHeaders(ctx, req, headersToUse, nil)

	if !providerUtils.ApplyLargePayloadRequestBodyWithModelNormalization(ctx, req, schemas.Replicate) {
		req.SetBody(jsonBody)
	}

	// Make request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, nil, latency, nil, koutakuErr
	}

	// Extract provider response headers before releasing the response
	providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK && resp.StatusCode() != fasthttp.StatusCreated {
		logger.Debug(fmt.Sprintf("error from replicate provider: %s", string(resp.Body())))
		return nil, nil, latency, providerResponseHeaders, parseReplicateError(resp.Body(), resp.StatusCode())
	}

	// Parse response
	body, decodeErr := providerUtils.CheckAndDecodeBody(resp)
	if decodeErr != nil {
		return nil, nil, latency, providerResponseHeaders, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, decodeErr)
	}

	var prediction ReplicatePredictionResponse
	_, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &prediction, jsonBody, providerUtils.ShouldSendBackRawRequest(ctx, sendBackRawRequest), providerUtils.ShouldSendBackRawResponse(ctx, sendBackRawResponse))
	if koutakuErr != nil {
		return nil, nil, latency, providerResponseHeaders, koutakuErr
	}

	return &prediction, rawResponse, latency, providerResponseHeaders, nil
}

// getPrediction retrieves the current state of a prediction
func getPrediction(
	ctx *schemas.KoutakuContext,
	client *fasthttp.Client,
	predictionURL string,
	key schemas.Key,
	logger schemas.Logger,
	sendBackRawResponse bool,
) (*ReplicatePredictionResponse, interface{}, map[string]string, *schemas.KoutakuError) {
	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Set URL
	req.SetRequestURI(predictionURL)
	req.Header.SetMethod(http.MethodGet)

	// Set authorization header
	if value := key.Value.GetValue(); value != "" {
		req.Header.Set("Authorization", "Bearer "+value)
	}

	// Make request
	_, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, nil, nil, koutakuErr
	}

	// Extract provider response headers before releasing the response
	providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		logger.Debug(fmt.Sprintf("error from replicate provider: %s", string(resp.Body())))
		return nil, nil, providerResponseHeaders, parseReplicateError(resp.Body(), resp.StatusCode())
	}

	// Parse response
	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, nil, providerResponseHeaders, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
	}

	prediction := &ReplicatePredictionResponse{}
	_, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, prediction, nil, false, sendBackRawResponse)
	if koutakuErr != nil {
		return nil, nil, providerResponseHeaders, koutakuErr
	}

	return prediction, rawResponse, providerResponseHeaders, nil
}

// pollPrediction polls a prediction URL until it reaches a terminal state or timeout
func pollPrediction(
	ctx *schemas.KoutakuContext,
	client *fasthttp.Client,
	predictionURL string,
	key schemas.Key,
	timeoutSeconds int,
	logger schemas.Logger,
	sendBackRawResponse bool,
) (*ReplicatePredictionResponse, interface{}, map[string]string, *schemas.KoutakuError) {
	// Create context with timeout
	pollCtx, cancel := schemas.NewKoutakuContextWithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	ticker := time.NewTicker(pollingInterval)
	defer ticker.Stop()

	// Poll immediately first time
	prediction, rawResponse, providerResponseHeaders, err := getPrediction(pollCtx, client, predictionURL, key, logger, sendBackRawResponse)
	if err != nil {
		return nil, nil, providerResponseHeaders, err
	}

	// If already in terminal state, return immediately
	if isTerminalStatus(prediction.Status) {
		return prediction, rawResponse, providerResponseHeaders, checkForErrorStatus(prediction)
	}

	logger.Debug(fmt.Sprintf("polling replicate prediction %s, status: %s", prediction.ID, prediction.Status))

	// Continue polling until terminal state or timeout
	for {
		select {
		case <-pollCtx.Done():
			return nil, nil, providerResponseHeaders, providerUtils.NewKoutakuOperationError(
				schemas.ErrProviderRequestTimedOut,
				fmt.Errorf("prediction polling timed out after %d seconds", timeoutSeconds))
		case <-ticker.C:
			prediction, rawResponse, providerResponseHeaders, err = getPrediction(pollCtx, client, predictionURL, key, logger, sendBackRawResponse)
			if err != nil {
				return nil, nil, providerResponseHeaders, err
			}

			logger.Debug(fmt.Sprintf("prediction %s status: %s", prediction.ID, prediction.Status))

			if isTerminalStatus(prediction.Status) {
				return prediction, rawResponse, providerResponseHeaders, checkForErrorStatus(prediction)
			}
		}
	}
}

// listDeploymentsByKey performs a list deployments request for a single key.
// Deployments are account-specific, so this needs to be called per key.
func (provider *ReplicateProvider) listDeploymentsByKey(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuListModelsRequest) (*schemas.KoutakuListModelsResponse, *schemas.KoutakuError) {
	providerName := provider.GetProviderKey()
	client := provider.client
	extraHeaders := provider.networkConfig.ExtraHeaders

	if !useDeploymentsEndpoint(key) {
		return ToKoutakuListModelsResponse(
			&ReplicateDeploymentListResponse{},
			providerName,
			key.Models,
			key.BlacklistedModels,
			key.Aliases,
			request.Unfiltered,
		), nil
	}

	// Build deployments URL
	deploymentsURL := provider.buildRequestURL(ctx, "/v1/deployments", schemas.ListModelsRequest)

	// Initialize pagination variables
	currentURL := deploymentsURL
	allDeployments := []ReplicateDeployment{}

	// Follow pagination until there are no more pages
	for currentURL != "" {
		// Create request
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		// Set up request
		req.SetRequestURI(currentURL)
		req.Header.SetMethod(http.MethodGet)
		req.Header.SetContentType("application/json")

		// Set authorization header if key is provided
		if key.Value.GetValue() != "" {
			req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
		}

		// Set extra headers from network config
		providerUtils.SetExtraHeaders(ctx, req, extraHeaders, nil)

		// Make request
		_, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, client, req, resp)

		// Release resources
		wait()
		fasthttp.ReleaseRequest(req)

		if koutakuErr != nil {
			fasthttp.ReleaseResponse(resp)
			return nil, koutakuErr
		}

		// Handle error response
		if resp.StatusCode() != fasthttp.StatusOK {
			errorResponse := parseReplicateError(resp.Body(), resp.StatusCode())
			fasthttp.ReleaseResponse(resp)
			return nil, errorResponse
		}

		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

		// Make a copy of the response body before releasing
		bodyCopy := make([]byte, len(resp.Body()))
		copy(bodyCopy, resp.Body())

		fasthttp.ReleaseResponse(resp)

		// Parse response from the copy
		var pageResponse ReplicateDeploymentListResponse
		if err := sonic.Unmarshal(bodyCopy, &pageResponse); err != nil {
			return nil, providerUtils.NewKoutakuOperationError(
				"failed to parse deployments response",
				err)
		}

		// Append results from this page
		allDeployments = append(allDeployments, pageResponse.Results...)

		// Check if there's a next page
		if pageResponse.Next != nil && *pageResponse.Next != "" {
			currentURL = *pageResponse.Next
		} else {
			currentURL = ""
		}
	}

	// Wrap deployments in response structure
	deploymentsResponse := &ReplicateDeploymentListResponse{
		Results: allDeployments,
	}

	// Convert deployments to Koutaku response (no public models here)
	response := ToKoutakuListModelsResponse(
		deploymentsResponse,
		providerName,
		key.Models,
		key.BlacklistedModels,
		key.Aliases,
		request.Unfiltered,
	)

	return response, nil
}

// ListModels performs a list models request to Replicate's API.
func (provider *ReplicateProvider) ListModels(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuListModelsRequest) (*schemas.KoutakuListModelsResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Replicate, provider.customProviderConfig, schemas.ListModelsRequest); err != nil {
		return nil, err
	}

	if provider.networkConfig.BaseURL == "" {
		return nil, providerUtils.NewConfigurationError("base_url is not set")
	}

	startTime := time.Now()

	response, err := providerUtils.HandleMultipleListModelsRequests(
		ctx,
		keys,
		request,
		provider.listDeploymentsByKey,
	)
	if err != nil {
		return nil, err
	}

	// Update metadata with total latency
	latency := time.Since(startTime)
	response.ExtraFields.Latency = latency.Milliseconds()

	return response, nil
}

// TextCompletion performs a text completion request to the replicate API.
func (provider *ReplicateProvider) TextCompletion(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuTextCompletionRequest) (*schemas.KoutakuTextCompletionResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Replicate, provider.customProviderConfig, schemas.TextCompletionRequest); err != nil {
		return nil, err
	}

	// build replicate request
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) { return ToReplicateTextRequest(request) })
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Check for Prefer: wait header from context for sync mode
	isSync := parsePreferHeader(provider.networkConfig.ExtraHeaders)

	// Build prediction URL based on model type (version ID or model name)
	predictionURL := buildPredictionURL(
		ctx,
		provider.networkConfig.BaseURL,
		request.Model,
		provider.customProviderConfig,
		schemas.TextCompletionRequest,
		useDeploymentsEndpoint(key),
	)

	// create prediction
	prediction, rawResponse, latency, providerResponseHeaders, err := createPrediction(
		ctx,
		provider.client,
		jsonData,
		key,
		providerUtils.GetPathFromContext(ctx, predictionURL),
		provider.networkConfig.ExtraHeaders,
		false,
		provider.logger,
		provider.sendBackRawRequest,
		provider.sendBackRawResponse,
	)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// if not sync, poll until done
	if !isSync && !isTerminalStatus(prediction.Status) {
		prediction, rawResponse, providerResponseHeaders, err = pollPrediction(
			ctx,
			provider.client,
			prediction.URLs.Get,
			key,
			provider.networkConfig.DefaultRequestTimeoutInSeconds,
			provider.logger,
			providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		)
		if err != nil {
			return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
		}
	}

	// Check for terminal error status (failed/canceled) after sync mode or polling
	if err := checkForErrorStatus(prediction); err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	if providerResponseHeaders != nil {
		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)
	}

	// Convert to Koutaku response
	koutakuResponse := prediction.ToKoutakuTextCompletionResponse()

	// Set extra fields
	koutakuResponse.ExtraFields.Latency = latency.Milliseconds()
	koutakuResponse.ExtraFields.ProviderResponseHeaders = providerResponseHeaders
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		providerUtils.ParseAndSetRawRequest(&koutakuResponse.ExtraFields, jsonData)
	}
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		koutakuResponse.ExtraFields.RawResponse = rawResponse
	}

	return koutakuResponse, nil
}

// TextCompletionStream performs a streaming text completion request to replicate's API.
// It formats the request, sends it to replicate, and processes the response.
// Returns a channel of KoutakuStream objects or an error if the request fails.
func (provider *ReplicateProvider) TextCompletionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuTextCompletionRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Replicate, provider.customProviderConfig, schemas.TextCompletionStreamRequest); err != nil {
		return nil, err
	}

	// Convert Koutaku request to Replicate format with streaming enabled
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			replicateReq, err := ToReplicateTextRequest(request)
			if err != nil {
				return nil, err
			}
			replicateReq.Stream = schemas.Ptr(true)
			return replicateReq, nil
		})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Build prediction URL based on model type (version ID or model name)
	predictionURL := buildPredictionURL(
		ctx,
		provider.networkConfig.BaseURL,
		request.Model,
		provider.customProviderConfig,
		schemas.TextCompletionStreamRequest,
		useDeploymentsEndpoint(key),
	)

	// Create prediction
	prediction, _, _, _, err := createPrediction(
		ctx,
		provider.client,
		jsonData,
		key,
		providerUtils.GetPathFromContext(ctx, predictionURL),
		provider.networkConfig.ExtraHeaders,
		true, // Streaming request, strip Prefer header for async mode
		provider.logger,
		provider.sendBackRawRequest,
		provider.sendBackRawResponse,
	)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Verify stream URL is available
	if prediction.URLs == nil || prediction.URLs.Stream == nil || *prediction.URLs.Stream == "" {
		koutakuErr := providerUtils.NewKoutakuOperationError(
			"stream URL not available in prediction response",
			fmt.Errorf("prediction response missing stream URL"))
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	streamURL := *prediction.URLs.Stream

	// Connect to stream URL
	_, resp, koutakuErr := listenToReplicateStreamURL(ctx, provider.streamingClient, streamURL, key)
	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Store provider response headers in context for transport layer
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

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

		startTime := time.Now()
		lastChunkTime := startTime
		chunkIndex := 0

		// Setup SSE event reader for event+data format
		sseReader := providerUtils.GetSSEEventReader(ctx, reader)

		messageID := prediction.ID

		for {
			// Check for context cancellation
			select {
			case <-ctx.Done():
				return
			default:
			}

			eventType, eventDataBytes, readErr := sseReader.ReadEvent()
			if readErr != nil {
				if readErr != io.EOF {
					// If context was cancelled/timed out, let defer handle it
					if ctx.Err() != nil {
						return
					}
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					provider.logger.Warn("Error reading stream: %v", readErr)
					enrichedErr := providerUtils.EnrichError(ctx, providerUtils.NewKoutakuOperationError(schemas.ErrProviderDoRequest, readErr), jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
					providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, enrichedErr, responseChan, provider.logger, postHookSpanFinalizer)
				}
				break
			}

			eventData := string(eventDataBytes)
			if eventType == "" && eventData == "" {
				continue
			}

			// Process the complete event
			switch eventType {
			case "output":
				// Accumulate content from data field
				if eventData != "" {
					// Create a streaming chunk with text completion response
					text := eventData
					response := &schemas.KoutakuTextCompletionResponse{
						ID:     messageID,
						Model:  request.Model,
						Object: "text_completion",
						Choices: []schemas.KoutakuResponseChoice{
							{
								Index: 0,
								TextCompletionResponseChoice: &schemas.TextCompletionResponseChoice{
									Text: &text,
								},
							},
						},
						ExtraFields: schemas.KoutakuResponseExtraFields{
							ChunkIndex: chunkIndex,
							Latency:    time.Since(lastChunkTime).Milliseconds(),
						},
					}

					// Set raw response if enabled (per-chunk event as JSON string)
					if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
						rawEvent := ReplicateSSEEvent{Event: eventType, Data: eventData}
						if eventJSON, err := providerUtils.MarshalSorted(rawEvent); err == nil {
							response.ExtraFields.RawResponse = string(eventJSON)
						}
					}

					lastChunkTime = time.Now()
					chunkIndex++

					providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
						providerUtils.GetKoutakuResponseForStreamResponse(response, nil, nil, nil, nil, nil),
						responseChan, postHookSpanFinalizer)
				}

			case "done":
				// Parse done event data
				var doneData ReplicateDoneEvent
				if eventData != "" && eventData != "{}" {
					if err := sonic.Unmarshal(eventDataBytes, &doneData); err != nil {
						provider.logger.Warn(fmt.Sprintf("Failed to parse done event data: %v", err))
					}
				}

				// Check for cancellation or error
				switch doneData.Reason {
				case "canceled":
					koutakuErr := providerUtils.NewKoutakuOperationError(
						"prediction was canceled",
						fmt.Errorf("stream ended: prediction canceled"))
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					enrichedErr := providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
					providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, enrichedErr, responseChan, provider.logger, postHookSpanFinalizer)
					// Explicitly close the body stream to terminate connection to Replicate
					resp.CloseBodyStream()
					return

				case "error":
					errorMsg := "prediction failed"
					if doneData.Output != nil {
						errorMsg = fmt.Sprintf("prediction failed: %v", doneData.Output)
					}
					koutakuErr := providerUtils.NewKoutakuOperationError(
						errorMsg,
						fmt.Errorf("stream ended with error"))
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					enrichedErr := providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
					providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, enrichedErr, responseChan, provider.logger, postHookSpanFinalizer)
					// Explicitly close the body stream to terminate connection to Replicate
					resp.CloseBodyStream()
					return
				}

				// Send final chunk with finish reason
				finishReason := schemas.Ptr("stop")
				finalResponse := providerUtils.CreateKoutakuTextCompletionChunkResponse(
					messageID,
					nil, // usage - not available in done event
					finishReason,
					chunkIndex,
					schemas.TextCompletionStreamRequest)

				// Set raw request if enabled
				if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
					providerUtils.ParseAndSetRawRequest(&finalResponse.ExtraFields, jsonData)
				}

				finalResponse.ExtraFields.Latency = time.Since(startTime).Milliseconds()

				ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
				providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
					providerUtils.GetKoutakuResponseForStreamResponse(finalResponse, nil, nil, nil, nil, nil),
					responseChan, postHookSpanFinalizer)
				resp.CloseBodyStream()
				return
			}
		}
	}()

	return responseChan, nil
}

// ChatCompletion performs a chat completion request to the replicate API.
func (provider *ReplicateProvider) ChatCompletion(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuChatRequest) (*schemas.KoutakuChatResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Replicate, provider.customProviderConfig, schemas.ChatCompletionRequest); err != nil {
		return nil, err
	}

	// build replicate request
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) { return ToReplicateChatRequest(request) })
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Check for Prefer: wait header from context for sync mode
	isSync := parsePreferHeader(provider.networkConfig.ExtraHeaders)

	// Build prediction URL based on model type (version ID or model name)
	predictionURL := buildPredictionURL(
		ctx,
		provider.networkConfig.BaseURL,
		request.Model,
		provider.customProviderConfig,
		schemas.ChatCompletionRequest,
		useDeploymentsEndpoint(key),
	)

	// create prediction
	prediction, rawResponse, latency, providerResponseHeaders, err := createPrediction(
		ctx,
		provider.client,
		jsonData,
		key,
		providerUtils.GetPathFromContext(ctx, predictionURL),
		provider.networkConfig.ExtraHeaders,
		false,
		provider.logger,
		provider.sendBackRawRequest,
		provider.sendBackRawResponse,
	)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// if not sync, poll until done
	if !isSync && !isTerminalStatus(prediction.Status) {
		prediction, rawResponse, providerResponseHeaders, err = pollPrediction(
			ctx,
			provider.client,
			prediction.URLs.Get,
			key,
			provider.networkConfig.DefaultRequestTimeoutInSeconds,
			provider.logger,
			providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		)
		if err != nil {
			return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
		}
	}

	// Check for terminal error status (failed/canceled) after sync mode or polling
	if err := checkForErrorStatus(prediction); err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	if providerResponseHeaders != nil {
		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)
	}

	// Convert to Koutaku response
	koutakuResponse := prediction.ToKoutakuChatResponse()

	// Set extra fields
	koutakuResponse.ExtraFields.Latency = latency.Milliseconds()
	koutakuResponse.ExtraFields.ProviderResponseHeaders = providerResponseHeaders
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		providerUtils.ParseAndSetRawRequest(&koutakuResponse.ExtraFields, jsonData)
	}
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		koutakuResponse.ExtraFields.RawResponse = rawResponse
	}

	return koutakuResponse, nil
}

// ChatCompletionStream performs a streaming chat completion request to the replicate API.
// It supports real-time streaming of responses using Server-Sent Events (SSE).
// Returns a channel containing KoutakuResponse objects representing the stream or an error if the request fails.
func (provider *ReplicateProvider) ChatCompletionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuChatRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Replicate, provider.customProviderConfig, schemas.ChatCompletionStreamRequest); err != nil {
		return nil, err
	}

	// Convert Koutaku request to Replicate format with streaming enabled
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			replicateReq, err := ToReplicateChatRequest(request)
			if err != nil {
				return nil, err
			}
			replicateReq.Stream = schemas.Ptr(true)
			return replicateReq, nil
		})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Build prediction URL based on model type (version ID or model name)
	predictionURL := buildPredictionURL(
		ctx,
		provider.networkConfig.BaseURL,
		request.Model,
		provider.customProviderConfig,
		schemas.ChatCompletionStreamRequest,
		useDeploymentsEndpoint(key),
	)

	// Create prediction
	prediction, _, _, _, err := createPrediction(
		ctx,
		provider.client,
		jsonData,
		key,
		providerUtils.GetPathFromContext(ctx, predictionURL),
		provider.networkConfig.ExtraHeaders,
		true,
		provider.logger,
		provider.sendBackRawRequest,
		provider.sendBackRawResponse,
	)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Verify stream URL is available
	if prediction.URLs == nil || prediction.URLs.Stream == nil || *prediction.URLs.Stream == "" {
		koutakuErr := providerUtils.NewKoutakuOperationError(
			"stream URL not available in prediction response",
			fmt.Errorf("prediction response missing stream URL"))
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	streamURL := *prediction.URLs.Stream

	// Connect to stream URL
	_, resp, koutakuErr := listenToReplicateStreamURL(ctx, provider.streamingClient, streamURL, key)
	if koutakuErr != nil {
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Store provider response headers in context for transport layer
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

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

		startTime := time.Now()
		lastChunkTime := startTime
		chunkIndex := 0

		// Setup SSE event reader for event+data format
		sseReader := providerUtils.GetSSEEventReader(ctx, reader)

		messageID := prediction.ID

		for {
			// Check for context cancellation
			select {
			case <-ctx.Done():
				return
			default:
			}

			eventType, eventDataBytes, readErr := sseReader.ReadEvent()
			if readErr != nil {
				if readErr != io.EOF {
					// If context was cancelled/timed out, let defer handle it
					if ctx.Err() != nil {
						return
					}
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					provider.logger.Warn("Error reading stream: %v", readErr)
					enrichedErr := providerUtils.EnrichError(ctx, providerUtils.NewKoutakuOperationError(schemas.ErrProviderDoRequest, readErr), jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
					providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, enrichedErr, responseChan, provider.logger, postHookSpanFinalizer)
				}
				break
			}

			eventData := string(eventDataBytes)
			if eventType == "" && eventData == "" {
				continue
			}

			// Process the complete event
			switch eventType {
			case "output":
				// Accumulate content from data field
				if eventData != "" {
					// Create a streaming chunk
					content := eventData
					role := string(schemas.ChatMessageRoleAssistant)
					delta := &schemas.ChatStreamResponseChoiceDelta{
						Content: &content,
						Role:    &role,
					}

					response := &schemas.KoutakuChatResponse{
						ID:      messageID,
						Model:   request.Model,
						Object:  "chat.completion.chunk",
						Created: int(time.Now().Unix()),
						Choices: []schemas.KoutakuResponseChoice{
							{
								Index: 0,
								ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
									Delta: delta,
								},
							},
						},
						ExtraFields: schemas.KoutakuResponseExtraFields{
							ChunkIndex: chunkIndex,
							Latency:    time.Since(lastChunkTime).Milliseconds(),
						},
					}

					// Set raw response if enabled (per-chunk event as JSON string)
					if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
						rawEvent := ReplicateSSEEvent{Event: eventType, Data: eventData}
						if eventJSON, err := providerUtils.MarshalSorted(rawEvent); err == nil {
							response.ExtraFields.RawResponse = string(eventJSON)
						}
					}

					lastChunkTime = time.Now()
					chunkIndex++

					providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
						providerUtils.GetKoutakuResponseForStreamResponse(nil, response, nil, nil, nil, nil),
						responseChan, postHookSpanFinalizer)
				}

			case "done":
				// Parse done event data
				var doneData ReplicateDoneEvent
				if eventData != "" && eventData != "{}" {
					if err := sonic.Unmarshal(eventDataBytes, &doneData); err != nil {
						provider.logger.Warn(fmt.Sprintf("Failed to parse done event data: %v", err))
					}
				}

				// Check for cancellation or error
				switch doneData.Reason {
				case "canceled":
					koutakuErr := providerUtils.NewKoutakuOperationError(
						"prediction was canceled",
						fmt.Errorf("stream ended: prediction canceled"))
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					enrichedErr := providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
					providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, enrichedErr, responseChan, provider.logger, postHookSpanFinalizer)
					// Explicitly close the body stream to terminate connection to Replicate
					resp.CloseBodyStream()
					return

				case "error":
					errorMsg := "prediction failed"
					if doneData.Output != nil {
						errorMsg = fmt.Sprintf("prediction failed: %v", doneData.Output)
					}
					koutakuErr := providerUtils.NewKoutakuOperationError(
						errorMsg,
						fmt.Errorf("stream ended with error"))
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					enrichedErr := providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
					providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, enrichedErr, responseChan, provider.logger, postHookSpanFinalizer)
					// Explicitly close the body stream to terminate connection to Replicate
					resp.CloseBodyStream()
					return
				}

				// Send final chunk with finish reason
				finishReason := "stop"
				finalResponse := &schemas.KoutakuChatResponse{
					ID:      messageID,
					Model:   request.Model,
					Object:  "chat.completion.chunk",
					Created: int(time.Now().Unix()),
					Choices: []schemas.KoutakuResponseChoice{
						{
							Index:        0,
							FinishReason: &finishReason,
							ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
								Delta: &schemas.ChatStreamResponseChoiceDelta{},
							},
						},
					},
					ExtraFields: schemas.KoutakuResponseExtraFields{
						ChunkIndex: chunkIndex,
						Latency:    time.Since(startTime).Milliseconds(),
					},
				}

				// Set raw request if enabled
				if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
					providerUtils.ParseAndSetRawRequest(&finalResponse.ExtraFields, jsonData)
				}

				ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
				providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
					providerUtils.GetKoutakuResponseForStreamResponse(nil, finalResponse, nil, nil, nil, nil),
					responseChan, postHookSpanFinalizer)
				resp.CloseBodyStream()
				return
			}
		}
	}()

	return responseChan, nil
}

// Responses performs a responses request to the replicate API.
func (provider *ReplicateProvider) Responses(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuResponsesRequest) (*schemas.KoutakuResponsesResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Replicate, provider.customProviderConfig, schemas.ResponsesRequest); err != nil {
		return nil, err
	}

	// build replicate request
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) { return ToReplicateResponsesRequest(request) })
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Check for Prefer: wait header from context for sync mode
	isSync := parsePreferHeader(provider.networkConfig.ExtraHeaders)

	// Build prediction URL based on model type (version ID or model name)
	predictionURL := buildPredictionURL(
		ctx,
		provider.networkConfig.BaseURL,
		request.Model,
		provider.customProviderConfig,
		schemas.ResponsesRequest,
		useDeploymentsEndpoint(key),
	)

	// create prediction
	prediction, rawResponse, latency, providerResponseHeaders, err := createPrediction(
		ctx,
		provider.client,
		jsonData,
		key,
		providerUtils.GetPathFromContext(ctx, predictionURL),
		provider.networkConfig.ExtraHeaders,
		false,
		provider.logger,
		provider.sendBackRawRequest,
		provider.sendBackRawResponse,
	)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// if not sync, poll until done
	if !isSync && !isTerminalStatus(prediction.Status) {
		prediction, rawResponse, providerResponseHeaders, err = pollPrediction(
			ctx,
			provider.client,
			prediction.URLs.Get,
			key,
			provider.networkConfig.DefaultRequestTimeoutInSeconds,
			provider.logger,
			providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		)
		if err != nil {
			return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
		}
	}

	// Check for terminal error status (failed/canceled) after sync mode or polling
	if err := checkForErrorStatus(prediction); err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	if providerResponseHeaders != nil {
		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)
	}

	// Convert to Koutaku response
	response := prediction.ToKoutakuResponsesResponse()
	response.ExtraFields.Latency = latency.Milliseconds()
	response.ExtraFields.ProviderResponseHeaders = providerResponseHeaders
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		providerUtils.ParseAndSetRawRequest(&response.ExtraFields, jsonData)
	}
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		response.ExtraFields.RawResponse = rawResponse
	}
	return response, nil
}

// ResponsesStream performs a streaming responses request to the replicate API.
func (provider *ReplicateProvider) ResponsesStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuResponsesRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Replicate, provider.customProviderConfig, schemas.ResponsesStreamRequest); err != nil {
		return nil, err
	}

	// Build replicate request
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) { return ToReplicateResponsesRequest(request) })
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Enable streaming (using sjson to set field directly, preserving key order)
	if updatedData, err := providerUtils.SetJSONField(jsonData, "stream", true); err != nil {
		return nil, providerUtils.NewKoutakuOperationError("failed to set stream field", err)
	} else {
		jsonData = updatedData
	}

	// Build prediction URL
	predictionURL := buildPredictionURL(
		ctx,
		provider.networkConfig.BaseURL,
		request.Model,
		provider.customProviderConfig,
		schemas.ResponsesStreamRequest,
		useDeploymentsEndpoint(key),
	)

	// Create prediction
	prediction, _, _, _, err := createPrediction(
		ctx,
		provider.client,
		jsonData,
		key,
		providerUtils.GetPathFromContext(ctx, predictionURL),
		provider.networkConfig.ExtraHeaders,
		true, // Streaming request, strip Prefer header for async mode
		provider.logger,
		provider.sendBackRawRequest,
		provider.sendBackRawResponse,
	)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Verify stream URL is available
	if prediction.URLs == nil || prediction.URLs.Stream == nil || *prediction.URLs.Stream == "" {
		koutakuErr := providerUtils.NewKoutakuOperationError(
			"stream URL not available in prediction response",
			fmt.Errorf("prediction response missing stream URL"))
		return nil, providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	streamURL := *prediction.URLs.Stream

	// Setup request for streaming
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	resp.StreamBody = true
	defer fasthttp.ReleaseRequest(req)

	req.Header.SetMethod(http.MethodGet)
	req.SetRequestURI(streamURL)
	req.Header.Set("Accept", "text/event-stream")

	// Set authorization
	if key.Value.GetValue() != "" {
		req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
	}

	// Set extra headers
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)

	// Make the streaming request
	streamErr := provider.streamingClient.Do(req, resp)
	if streamErr != nil {
		defer providerUtils.ReleaseStreamingResponse(resp)
		if errors.Is(streamErr, context.Canceled) {
			return nil, providerUtils.EnrichError(ctx, &schemas.KoutakuError{
				IsKoutakuError: false,
				Error: &schemas.ErrorField{
					Type:    schemas.Ptr(schemas.RequestCancelled),
					Message: schemas.ErrRequestCancelled,
					Error:   streamErr,
				},
			}, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
		}
		if errors.Is(streamErr, fasthttp.ErrTimeout) || errors.Is(streamErr, context.DeadlineExceeded) {
			return nil, providerUtils.EnrichError(ctx, providerUtils.NewKoutakuTimeoutError(schemas.ErrProviderRequestTimedOut, streamErr), jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
		}
		return nil, providerUtils.EnrichError(ctx, providerUtils.NewKoutakuOperationError(schemas.ErrProviderDoRequest, streamErr), jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Extract provider response headers before status check so error responses also forward them
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

	// Check for HTTP errors
	if resp.StatusCode() != fasthttp.StatusOK {
		defer providerUtils.ReleaseStreamingResponse(resp)
		body := resp.Body()
		return nil, providerUtils.EnrichError(ctx, parseReplicateError(body, resp.StatusCode()), jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
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
		// Registered first so the post-hook span finalizer runs on every exit
		// path — including the empty-reader early return below, which would
		// otherwise skip any finalizer declared later in this goroutine.
		defer providerUtils.EnsureStreamFinalizerCalled(ctx, postHookSpanFinalizer)
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

		if reader == nil {
			koutakuErr := providerUtils.NewKoutakuOperationError(
				"provider returned an empty response",
				fmt.Errorf("provider returned an empty response"))
			ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
			providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse), responseChan, provider.logger, postHookSpanFinalizer)
			return
		}

		// Wrap reader with idle timeout to detect stalled streams.
		reader, stopIdleTimeout := providerUtils.NewIdleTimeoutReader(reader, resp.BodyStream(), providerUtils.GetStreamIdleTimeout(ctx))
		defer stopIdleTimeout()

		// Setup cancellation handler to close the raw network stream on ctx cancellation,
		// which immediately unblocks any in-progress read (including reads blocked inside a gzip decompression layer).
		stopCancellation := providerUtils.SetupStreamCancellation(ctx, resp.BodyStream(), provider.logger)
		defer stopCancellation()

		sseReader := providerUtils.GetSSEEventReader(ctx, reader)
		startTime := time.Now()
		sequenceNumber := 0
		messageID := prediction.ID
		// Generate a unique item ID for the message (needed for accumulator to track deltas)
		itemID := "msg_" + messageID

		// Track lifecycle state
		var hasEmittedCreated, hasEmittedInProgress bool
		var hasEmittedOutputItemAdded, hasEmittedContentPartAdded bool
		var hasReceivedContent bool
		outputIndex := 0
		contentIndex := 0

		// Accumulate raw responses for debugging
		var rawResponseChunks []interface{}
		sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
		sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

		for {
			if ctx.Err() != nil {
				return
			}

			eventType, eventDataBytes, readErr := sseReader.ReadEvent()
			if readErr != nil {
				if readErr != io.EOF {
					if ctx.Err() != nil {
						return
					}
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					provider.logger.Warn("Error reading stream: %v", readErr)
					koutakuErr := providerUtils.NewKoutakuOperationError(schemas.ErrProviderDoRequest, readErr)

					// Include accumulated raw responses in error
					if sendBackRawResponse && len(rawResponseChunks) > 0 {
						koutakuErr.ExtraFields.RawResponse = rawResponseChunks
					}

					enrichedErr := providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, sendBackRawRequest, sendBackRawResponse)
					providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, enrichedErr, responseChan, provider.logger, postHookSpanFinalizer)
					return
				}
				break
			}

			currentEvent := ReplicateSSEEvent{
				Event: eventType,
				Data:  string(eventDataBytes),
			}

			if currentEvent.Event != "" {
				// Process the event
				switch currentEvent.Event {
				case "output":
					// Text chunk received
					if currentEvent.Data != "" {
						// Accumulate raw response if enabled
						if sendBackRawResponse {
							rawResponseChunks = append(rawResponseChunks, currentEvent)
						}

						// Emit lifecycle events on first content
						if !hasEmittedCreated {
							// response.created
							createdResp := &schemas.KoutakuResponsesStreamResponse{
								Type:           schemas.ResponsesStreamResponseTypeCreated,
								SequenceNumber: sequenceNumber,
								Response: &schemas.KoutakuResponsesResponse{
									ID:        schemas.Ptr(messageID),
									Model:     request.Model,
									CreatedAt: int(startTime.Unix()),
								},
								ExtraFields: schemas.KoutakuResponseExtraFields{
									Latency:    time.Since(startTime).Milliseconds(),
									ChunkIndex: sequenceNumber,
								},
							}
							if sendBackRawRequest {
								providerUtils.ParseAndSetRawRequest(&createdResp.ExtraFields, jsonData)
							}
							providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
								providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, createdResp, nil, nil, nil),
								responseChan, postHookSpanFinalizer)
							sequenceNumber++
							hasEmittedCreated = true
						}

						if !hasEmittedInProgress {
							// response.in_progress
							inProgressResp := &schemas.KoutakuResponsesStreamResponse{
								Type:           schemas.ResponsesStreamResponseTypeInProgress,
								SequenceNumber: sequenceNumber,
								Response: &schemas.KoutakuResponsesResponse{
									ID:        schemas.Ptr(messageID),
									CreatedAt: int(startTime.Unix()),
								},
								ExtraFields: schemas.KoutakuResponseExtraFields{
									ChunkIndex: sequenceNumber,
								},
							}
							providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
								providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, inProgressResp, nil, nil, nil),
								responseChan, postHookSpanFinalizer)
							sequenceNumber++
							hasEmittedInProgress = true
						}

						if !hasEmittedOutputItemAdded {
							// response.output_item.added
							messageType := schemas.ResponsesMessageTypeMessage
							role := schemas.ResponsesInputMessageRoleAssistant
							status := "in_progress"
							itemAddedResp := &schemas.KoutakuResponsesStreamResponse{
								Type:           schemas.ResponsesStreamResponseTypeOutputItemAdded,
								SequenceNumber: sequenceNumber,
								OutputIndex:    schemas.Ptr(outputIndex),
								Item: &schemas.ResponsesMessage{
									ID:     schemas.Ptr(itemID),
									Type:   &messageType,
									Role:   &role,
									Status: &status,
									Content: &schemas.ResponsesMessageContent{
										ContentBlocks: []schemas.ResponsesMessageContentBlock{},
									},
								},
								ExtraFields: schemas.KoutakuResponseExtraFields{
									ChunkIndex: sequenceNumber,
								},
							}
							providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
								providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, itemAddedResp, nil, nil, nil),
								responseChan, postHookSpanFinalizer)
							sequenceNumber++
							hasEmittedOutputItemAdded = true
						}

						if !hasEmittedContentPartAdded {
							// response.content_part.added
							emptyText := ""
							partAddedResp := &schemas.KoutakuResponsesStreamResponse{
								Type:           schemas.ResponsesStreamResponseTypeContentPartAdded,
								SequenceNumber: sequenceNumber,
								OutputIndex:    schemas.Ptr(outputIndex),
								ContentIndex:   schemas.Ptr(contentIndex),
								ItemID:         schemas.Ptr(itemID),
								Part: &schemas.ResponsesMessageContentBlock{
									Type: schemas.ResponsesOutputMessageContentTypeText,
									Text: &emptyText,
									ResponsesOutputMessageContentText: &schemas.ResponsesOutputMessageContentText{
										Annotations: []schemas.ResponsesOutputMessageContentTextAnnotation{},
										LogProbs:    []schemas.ResponsesOutputMessageContentTextLogProb{},
									},
								},
								ExtraFields: schemas.KoutakuResponseExtraFields{
									ChunkIndex: sequenceNumber,
								},
							}
							providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
								providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, partAddedResp, nil, nil, nil),
								responseChan, postHookSpanFinalizer)
							sequenceNumber++
							hasEmittedContentPartAdded = true
						}

						// response.output_text.delta
						deltaResp := &schemas.KoutakuResponsesStreamResponse{
							Type:           schemas.ResponsesStreamResponseTypeOutputTextDelta,
							SequenceNumber: sequenceNumber,
							OutputIndex:    schemas.Ptr(outputIndex),
							ContentIndex:   schemas.Ptr(contentIndex),
							ItemID:         schemas.Ptr(itemID),
							Delta:          schemas.Ptr(currentEvent.Data),
							LogProbs:       []schemas.ResponsesOutputMessageContentTextLogProb{},
							ExtraFields: schemas.KoutakuResponseExtraFields{
								ChunkIndex: sequenceNumber,
							},
						}
						providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
							providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, deltaResp, nil, nil, nil),
							responseChan, postHookSpanFinalizer)
						sequenceNumber++
						hasReceivedContent = true
					}
				case "done":
					// Accumulate done event in raw responses if enabled
					if sendBackRawResponse {
						rawResponseChunks = append(rawResponseChunks, currentEvent)
					}

					// Stream completed
					if hasReceivedContent {
						// response.output_text.done
						textDoneResp := &schemas.KoutakuResponsesStreamResponse{
							Type:           schemas.ResponsesStreamResponseTypeOutputTextDone,
							SequenceNumber: sequenceNumber,
							OutputIndex:    schemas.Ptr(outputIndex),
							ContentIndex:   schemas.Ptr(contentIndex),
							ItemID:         schemas.Ptr(itemID),
							LogProbs:       []schemas.ResponsesOutputMessageContentTextLogProb{},
							ExtraFields: schemas.KoutakuResponseExtraFields{
								ChunkIndex: sequenceNumber,
							},
						}
						providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
							providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, textDoneResp, nil, nil, nil),
							responseChan, postHookSpanFinalizer)
						sequenceNumber++

						// response.content_part.done
						partDoneResp := &schemas.KoutakuResponsesStreamResponse{
							Type:           schemas.ResponsesStreamResponseTypeContentPartDone,
							SequenceNumber: sequenceNumber,
							OutputIndex:    schemas.Ptr(outputIndex),
							ContentIndex:   schemas.Ptr(contentIndex),
							ItemID:         schemas.Ptr(itemID),
							Part: &schemas.ResponsesMessageContentBlock{
								Type: schemas.ResponsesOutputMessageContentTypeText,
								ResponsesOutputMessageContentText: &schemas.ResponsesOutputMessageContentText{
									Annotations: []schemas.ResponsesOutputMessageContentTextAnnotation{},
									LogProbs:    []schemas.ResponsesOutputMessageContentTextLogProb{},
								},
							},
							ExtraFields: schemas.KoutakuResponseExtraFields{
								ChunkIndex: sequenceNumber,
							},
						}
						providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
							providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, partDoneResp, nil, nil, nil),
							responseChan, postHookSpanFinalizer)
						sequenceNumber++

						// response.output_item.done
						messageType := schemas.ResponsesMessageTypeMessage
						role := schemas.ResponsesInputMessageRoleAssistant
						status := "completed"
						itemDoneResp := &schemas.KoutakuResponsesStreamResponse{
							Type:           schemas.ResponsesStreamResponseTypeOutputItemDone,
							SequenceNumber: sequenceNumber,
							OutputIndex:    schemas.Ptr(outputIndex),
							Item: &schemas.ResponsesMessage{
								ID:     schemas.Ptr(itemID),
								Type:   &messageType,
								Role:   &role,
								Status: &status,
								Content: &schemas.ResponsesMessageContent{
									ContentBlocks: []schemas.ResponsesMessageContentBlock{
										{
											Type: schemas.ResponsesOutputMessageContentTypeText,
											ResponsesOutputMessageContentText: &schemas.ResponsesOutputMessageContentText{
												Annotations: []schemas.ResponsesOutputMessageContentTextAnnotation{},
												LogProbs:    []schemas.ResponsesOutputMessageContentTextLogProb{},
											},
										},
									},
								},
							},
							ExtraFields: schemas.KoutakuResponseExtraFields{
								ChunkIndex: sequenceNumber,
							},
						}
						providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
							providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, itemDoneResp, nil, nil, nil),
							responseChan, postHookSpanFinalizer)
						sequenceNumber++
					}

					// response.completed
					completedResp := &schemas.KoutakuResponsesStreamResponse{
						Type:           schemas.ResponsesStreamResponseTypeCompleted,
						SequenceNumber: sequenceNumber,
						Response: &schemas.KoutakuResponsesResponse{
							ID:          schemas.Ptr(messageID),
							Model:       request.Model,
							CreatedAt:   int(startTime.Unix()),
							CompletedAt: schemas.Ptr(int(time.Now().Unix())),
						},
						ExtraFields: schemas.KoutakuResponseExtraFields{
							Latency:    time.Since(startTime).Milliseconds(),
							ChunkIndex: sequenceNumber,
						},
					}

					// Set raw request if enabled (on final chunk only)
					if sendBackRawRequest {
						providerUtils.ParseAndSetRawRequest(&completedResp.ExtraFields, jsonData)
					}

					// Set raw response if enabled
					if sendBackRawResponse && len(rawResponseChunks) > 0 {
						completedResp.ExtraFields.RawResponse = rawResponseChunks
					}

					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
						providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, completedResp, nil, nil, nil),
						responseChan, postHookSpanFinalizer)
					resp.CloseBodyStream()
					return
				case "error":
					// Accumulate error event in raw responses if enabled
					if sendBackRawResponse {
						rawResponseChunks = append(rawResponseChunks, currentEvent)
					}

					// Handle error
					errorMsg := "stream error"
					if currentEvent.Data != "" {
						errorMsg = currentEvent.Data
					}
					koutakuErr := providerUtils.NewKoutakuOperationError(
						errorMsg,
						fmt.Errorf("stream error: %s", errorMsg))

					// Include accumulated raw responses in error
					if sendBackRawResponse && len(rawResponseChunks) > 0 {
						koutakuErr.ExtraFields.RawResponse = rawResponseChunks
					}

					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					enrichedErr := providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, sendBackRawRequest, sendBackRawResponse)
					providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, enrichedErr, responseChan, provider.logger, postHookSpanFinalizer)
					resp.CloseBodyStream()
					return
				}
			}
		}
	}()

	return responseChan, nil
}

// Embedding is not supported by the replicate provider.
func (provider *ReplicateProvider) Embedding(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuEmbeddingRequest) (*schemas.KoutakuEmbeddingResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.EmbeddingRequest, provider.GetProviderKey())
}

// Speech is not supported by the replicate provider.
func (provider *ReplicateProvider) Speech(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuSpeechRequest) (*schemas.KoutakuSpeechResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.SpeechRequest, provider.GetProviderKey())
}

// Rerank is not supported by the Replicate provider.
func (provider *ReplicateProvider) Rerank(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuRerankRequest) (*schemas.KoutakuRerankResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.RerankRequest, provider.GetProviderKey())
}

// OCR is not supported by the Replicate provider.
func (provider *ReplicateProvider) OCR(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuOCRRequest) (*schemas.KoutakuOCRResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.OCRRequest, provider.GetProviderKey())
}

// SpeechStream is not supported by the replicate provider.
func (provider *ReplicateProvider) SpeechStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuSpeechRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.SpeechStreamRequest, provider.GetProviderKey())
}

// Transcription is not supported by the replicate provider.
func (provider *ReplicateProvider) Transcription(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuTranscriptionRequest) (*schemas.KoutakuTranscriptionResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TranscriptionRequest, provider.GetProviderKey())
}

// TranscriptionStream is not supported by the replicate provider.
func (provider *ReplicateProvider) TranscriptionStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuTranscriptionRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.TranscriptionStreamRequest, provider.GetProviderKey())
}

// ImageGeneration performs an image generation request to the replicate API using predictions.
func (provider *ReplicateProvider) ImageGeneration(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageGenerationRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Replicate, provider.customProviderConfig, schemas.ImageGenerationRequest); err != nil {
		return nil, err
	}

	// Convert Koutaku request to Replicate format
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToReplicateImageGenerationInput(request), nil
		})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Check for Prefer: wait header from context for sync mode
	isSync := parsePreferHeader(provider.networkConfig.ExtraHeaders)

	// Build prediction URL based on model type (version ID or model name)
	predictionURL := buildPredictionURL(
		ctx,
		provider.networkConfig.BaseURL,
		request.Model,
		provider.customProviderConfig,
		schemas.ImageGenerationRequest,
		useDeploymentsEndpoint(key),
	)

	// Create prediction with appropriate mode
	prediction, rawResponse, latency, providerResponseHeaders, err := createPrediction(
		ctx,
		provider.client,
		jsonData,
		key,
		providerUtils.GetPathFromContext(ctx, predictionURL),
		provider.networkConfig.ExtraHeaders,
		false,
		provider.logger,
		provider.sendBackRawRequest,
		provider.sendBackRawResponse,
	)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// If async mode and not complete, poll until done
	if !isSync && !isTerminalStatus(prediction.Status) {
		prediction, rawResponse, providerResponseHeaders, err = pollPrediction(
			ctx,
			provider.client,
			prediction.URLs.Get,
			key,
			provider.networkConfig.DefaultRequestTimeoutInSeconds,
			provider.logger,
			providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		)
		if err != nil {
			return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
		}
	}

	// Check for terminal error status (failed/canceled) after sync mode or polling
	if err := checkForErrorStatus(prediction); err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	if providerResponseHeaders != nil {
		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)
	}

	// Convert to Koutaku response
	koutakuResponse, err := ToKoutakuImageGenerationResponse(prediction)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Set extra fields
	koutakuResponse.ExtraFields.Latency = latency.Milliseconds()
	koutakuResponse.ExtraFields.ProviderResponseHeaders = providerResponseHeaders
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		providerUtils.ParseAndSetRawRequest(&koutakuResponse.ExtraFields, jsonData)
	}
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		koutakuResponse.ExtraFields.RawResponse = rawResponse
	}

	return koutakuResponse, nil
}

// ImageGenerationStream performs a streaming image generation request to the replicate API.
// It creates a prediction with streaming enabled and listens to the stream URL for progressive updates.
func (provider *ReplicateProvider) ImageGenerationStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuImageGenerationRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Replicate, provider.customProviderConfig, schemas.ImageGenerationStreamRequest); err != nil {
		return nil, err
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	// Convert Koutaku request to Replicate format with streaming enabled
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			replicateReq := ToReplicateImageGenerationInput(request)
			replicateReq.Stream = schemas.Ptr(true)
			return replicateReq, nil
		})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Build prediction URL based on model type (version ID or model name)
	predictionURL := buildPredictionURL(
		ctx,
		provider.networkConfig.BaseURL,
		request.Model,
		provider.customProviderConfig,
		schemas.ImageGenerationStreamRequest,
		useDeploymentsEndpoint(key),
	)
	// Create prediction
	prediction, _, _, _, err := createPrediction(
		ctx,
		provider.client,
		jsonData,
		key,
		providerUtils.GetPathFromContext(ctx, predictionURL),
		provider.networkConfig.ExtraHeaders,
		true, // Streaming request, strip Prefer header for async mode
		provider.logger,
		provider.sendBackRawRequest,
		provider.sendBackRawResponse,
	)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Verify stream URL is available
	if prediction.URLs == nil || prediction.URLs.Stream == nil || *prediction.URLs.Stream == "" {
		return nil, providerUtils.EnrichError(
			ctx,
			providerUtils.NewKoutakuOperationError(
				"stream URL not available in prediction response",
				fmt.Errorf("prediction response missing stream URL"),
			),
			jsonData,
			nil,
			sendBackRawRequest,
			sendBackRawResponse,
		)
	}

	streamURL := *prediction.URLs.Stream

	// Connect to stream URL
	_, resp, koutakuErr := listenToReplicateStreamURL(ctx, provider.streamingClient, streamURL, key)
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Store provider response headers in context for transport layer
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

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

		startTime := time.Now()
		lastChunkTime := startTime
		chunkIndex := 0

		// Setup SSE event reader for event+data format
		sseReader := providerUtils.GetSSEEventReader(ctx, reader)

		// Track last image data for final chunk
		var lastB64Data string
		var lastOutputFormat string
		// Accumulate all raw response chunks for complete stream history
		var rawResponseChunks []interface{}

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			eventType, eventDataBytes, readErr := sseReader.ReadEvent()
			if readErr != nil {
				if readErr != io.EOF {
					if ctx.Err() != nil {
						return
					}
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					provider.logger.Warn(fmt.Sprintf("Error reading SSE stream: %v", readErr))
					enrichedErr := providerUtils.EnrichError(ctx, providerUtils.NewKoutakuOperationError(schemas.ErrProviderDoRequest, readErr), jsonData, nil, sendBackRawRequest, sendBackRawResponse)
					providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, enrichedErr, responseChan, provider.logger, postHookSpanFinalizer)
				}
				break
			}

			eventData := string(eventDataBytes)
			if eventType == "" && eventData == "" {
				continue
			}

			// Process the complete event
			switch eventType {
			case "output":
				// Check if data is a data URI (image) or plain text
				var b64Data, outputFormat string
				if strings.HasPrefix(eventData, "data:") {
					// Parse image data from data URI
					var mimeType string
					b64Data, mimeType = parseDataURIImage(eventData)

					// Extract output format from MIME type
					if mimeType != "" {
						// Convert "image/webp" to "webp"
						parts := strings.Split(mimeType, "/")
						if len(parts) == 2 {
							outputFormat = parts[1]
						}
					}
				} else {
					// For non-data-URI output (e.g., text), store as-is
					// This shouldn't happen for image generation but handle it gracefully
					provider.logger.Debug(fmt.Sprintf("Received non-data-URI output: %s", eventData[:min(100, len(eventData))]))
					// Skip non-image output for image generation
					continue
				}

				// Create chunk
				chunk := &schemas.KoutakuImageGenerationStreamResponse{
					Type:         schemas.ImageGenerationEventTypePartial,
					Index:        0, // Single image for now
					ChunkIndex:   chunkIndex,
					B64JSON:      b64Data,
					CreatedAt:    time.Now().Unix(),
					OutputFormat: outputFormat,
					ExtraFields: schemas.KoutakuResponseExtraFields{
						ChunkIndex: chunkIndex,
						Latency:    time.Since(lastChunkTime).Milliseconds(),
					},
				}

				// Accumulate raw response chunks if enabled
				if sendBackRawResponse {
					rawResponseChunks = append(rawResponseChunks, ReplicateSSEEvent{Event: eventType, Data: eventData})
				}

				// Track last image data for final chunk
				lastB64Data = b64Data
				lastOutputFormat = outputFormat

				lastChunkTime = time.Now()
				chunkIndex++

				providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
					providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, nil, nil, nil, chunk),
					responseChan, postHookSpanFinalizer)

			case "done":
				// Parse done event data
				var doneData ReplicateDoneEvent
				if eventData != "" && eventData != "{}" {
					if err := sonic.Unmarshal(eventDataBytes, &doneData); err != nil {
						provider.logger.Warn(fmt.Sprintf("Failed to parse done event data: %v", err))
					}
				}

				// Check for cancellation or error
				switch doneData.Reason {
				case "canceled":
					koutakuErr := providerUtils.NewKoutakuOperationError(
						"prediction was canceled",
						fmt.Errorf("stream ended: prediction canceled"))
					// Include accumulated raw responses in error
					if sendBackRawResponse && len(rawResponseChunks) > 0 {
						koutakuErr.ExtraFields.RawResponse = rawResponseChunks
					}
					koutakuErr = providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, sendBackRawRequest, sendBackRawResponse)
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, koutakuErr, responseChan, provider.logger, postHookSpanFinalizer)
					return
				case "error":
					koutakuErr := providerUtils.NewKoutakuOperationError(
						"prediction failed",
						fmt.Errorf("stream ended with error"))
					// Include accumulated raw responses in error
					if sendBackRawResponse && len(rawResponseChunks) > 0 {
						koutakuErr.ExtraFields.RawResponse = rawResponseChunks
					}
					koutakuErr = providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, sendBackRawRequest, sendBackRawResponse)
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, koutakuErr, responseChan, provider.logger, postHookSpanFinalizer)
					return
				}

				// Send completion chunk (success case when reason is empty or not present)
				finalChunk := &schemas.KoutakuImageGenerationStreamResponse{
					Type:         schemas.ImageGenerationEventTypeCompleted,
					Index:        0,
					ChunkIndex:   chunkIndex,
					B64JSON:      lastB64Data,      // Include last image data
					OutputFormat: lastOutputFormat, // Include output format
					CreatedAt:    time.Now().Unix(),
					ExtraFields: schemas.KoutakuResponseExtraFields{
						ChunkIndex: chunkIndex,
						Latency:    time.Since(startTime).Milliseconds(),
					},
				}

				// Set raw request only on final chunk if enabled
				if sendBackRawRequest {
					providerUtils.ParseAndSetRawRequest(&finalChunk.ExtraFields, jsonData)
				}

				// Set accumulated raw responses on final chunk if enabled
				if sendBackRawResponse {
					// Append the final done event to the accumulated chunks
					rawResponseChunks = append(rawResponseChunks, ReplicateSSEEvent{Event: eventType, Data: eventData})
					finalChunk.ExtraFields.RawResponse = rawResponseChunks
				}

				ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
				providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
					providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, nil, nil, nil, finalChunk),
					responseChan, postHookSpanFinalizer)
				return

			case "error":
				// Parse error event data
				var errorData ReplicateErrorEvent
				errorMsg := "stream error"

				if eventData != "" {
					if err := sonic.Unmarshal(eventDataBytes, &errorData); err != nil {
						provider.logger.Warn(fmt.Sprintf("Failed to parse error event data: %v", err))
						// Fallback to raw data
						errorMsg = eventData
					} else if errorData.Detail != "" {
						errorMsg = errorData.Detail
					}
				}

				koutakuErr := &schemas.KoutakuError{
					IsKoutakuError: false,
					Error: &schemas.ErrorField{
						Message: errorMsg,
					},
				}
				// Include accumulated raw responses in error
				if sendBackRawResponse {
					rawResponseChunks = append(rawResponseChunks, ReplicateSSEEvent{Event: eventType, Data: eventData})
					koutakuErr.ExtraFields.RawResponse = rawResponseChunks
				}
				koutakuErr = providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, sendBackRawRequest, sendBackRawResponse)
				ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
				providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, koutakuErr, responseChan, provider.logger, postHookSpanFinalizer)
				return
			}
		}
	}()

	return responseChan, nil
}

// ImageEdit is not supported by the Replicate provider.
func (provider *ReplicateProvider) ImageEdit(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageEditRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Replicate, provider.customProviderConfig, schemas.ImageEditRequest); err != nil {
		return nil, err
	}

	// Convert Koutaku request to Replicate format
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToReplicateImageEditInput(request), nil
		})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Check for Prefer: wait header from context for sync mode
	isSync := parsePreferHeader(provider.networkConfig.ExtraHeaders)

	// Build prediction URL based on model type (version ID or model name)
	predictionURL := buildPredictionURL(
		ctx,
		provider.networkConfig.BaseURL,
		request.Model,
		provider.customProviderConfig,
		schemas.ImageEditRequest,
		useDeploymentsEndpoint(key),
	)

	// Create prediction with appropriate mode
	prediction, rawResponse, latency, providerResponseHeaders, err := createPrediction(
		ctx,
		provider.client,
		jsonData,
		key,
		providerUtils.GetPathFromContext(ctx, predictionURL),
		provider.networkConfig.ExtraHeaders,
		false,
		provider.logger,
		provider.sendBackRawRequest,
		provider.sendBackRawResponse,
	)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// If async mode and not complete, poll until done
	if !isSync && !isTerminalStatus(prediction.Status) {
		prediction, rawResponse, providerResponseHeaders, err = pollPrediction(
			ctx,
			provider.client,
			prediction.URLs.Get,
			key,
			provider.networkConfig.DefaultRequestTimeoutInSeconds,
			provider.logger,
			providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse),
		)
		if err != nil {
			return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
		}
	}

	// Check for terminal error status (failed/canceled) after sync mode or polling
	if err := checkForErrorStatus(prediction); err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	if providerResponseHeaders != nil {
		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)
	}

	// Convert to Koutaku response (reuse image generation response format)
	koutakuResponse, err := ToKoutakuImageGenerationResponse(prediction)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Set extra fields
	koutakuResponse.ExtraFields.Latency = latency.Milliseconds()
	koutakuResponse.ExtraFields.ProviderResponseHeaders = providerResponseHeaders
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		providerUtils.ParseAndSetRawRequest(&koutakuResponse.ExtraFields, jsonData)
	}
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		koutakuResponse.ExtraFields.RawResponse = rawResponse
	}

	return koutakuResponse, nil
}

// ImageEditStream performs a streaming image edit request to the replicate API.
// It creates a prediction with streaming enabled and listens to the stream URL for progressive updates.
func (provider *ReplicateProvider) ImageEditStream(ctx *schemas.KoutakuContext, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context), key schemas.Key, request *schemas.KoutakuImageEditRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Replicate, provider.customProviderConfig, schemas.ImageEditStreamRequest); err != nil {
		return nil, err
	}

	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	// Convert Koutaku request to Replicate format with streaming enabled
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			replicateReq := ToReplicateImageEditInput(request)
			replicateReq.Stream = schemas.Ptr(true)
			return replicateReq, nil
		})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Build prediction URL based on model type (version ID or model name)
	predictionURL := buildPredictionURL(
		ctx,
		provider.networkConfig.BaseURL,
		request.Model,
		provider.customProviderConfig,
		schemas.ImageEditStreamRequest,
		useDeploymentsEndpoint(key),
	)

	// Create prediction
	prediction, _, _, _, err := createPrediction(
		ctx,
		provider.client,
		jsonData,
		key,
		providerUtils.GetPathFromContext(ctx, predictionURL),
		provider.networkConfig.ExtraHeaders,
		true, // Streaming request, strip Prefer header for async mode
		provider.logger,
		provider.sendBackRawRequest,
		provider.sendBackRawResponse,
	)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	// Verify stream URL is available
	if prediction.URLs == nil || prediction.URLs.Stream == nil || *prediction.URLs.Stream == "" {
		return nil, providerUtils.EnrichError(
			ctx,
			providerUtils.NewKoutakuOperationError(
				"stream URL not available in prediction response",
				fmt.Errorf("prediction response missing stream URL"),
			),
			jsonData,
			nil,
			sendBackRawRequest,
			sendBackRawResponse,
		)
	}

	streamURL := *prediction.URLs.Stream

	// Connect to stream URL
	_, resp, koutakuErr := listenToReplicateStreamURL(ctx, provider.streamingClient, streamURL, key)
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Store provider response headers in context for transport layer
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

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

		startTime := time.Now()
		lastChunkTime := startTime
		chunkIndex := 0

		// Setup SSE event reader for event+data format
		sseReader := providerUtils.GetSSEEventReader(ctx, reader)

		// Track last image data for final chunk
		var lastB64Data string
		var lastOutputFormat string
		// Accumulate all raw response chunks for complete stream history
		var rawResponseChunks []interface{}

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			eventType, eventDataBytes, readErr := sseReader.ReadEvent()
			if readErr != nil {
				if readErr != io.EOF {
					if errors.Is(readErr, context.Canceled) {
						return
					}
					enrichedErr := providerUtils.EnrichError(ctx, providerUtils.NewKoutakuOperationError("stream read error", readErr), jsonData, nil, sendBackRawRequest, sendBackRawResponse)
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, enrichedErr, responseChan, provider.logger, postHookSpanFinalizer)
				}
				break
			}

			eventData := string(eventDataBytes)
			if eventType == "" && eventData == "" {
				continue
			}

			// Process the complete event
			switch eventType {
			case "output":
				// Check if data is a data URI (image) or plain text
				var b64Data, outputFormat string
				if strings.HasPrefix(eventData, "data:") {
					// Parse image data from data URI
					var mimeType string
					b64Data, mimeType = parseDataURIImage(eventData)

					// Extract output format from MIME type
					if mimeType != "" {
						// Convert "image/webp" to "webp"
						parts := strings.Split(mimeType, "/")
						if len(parts) == 2 {
							outputFormat = parts[1]
						}
					}
				} else {
					// For non-data-URI output, skip for image edit
					provider.logger.Debug(fmt.Sprintf("Received non-data-URI output: %s", eventData[:min(100, len(eventData))]))
					continue
				}

				// Create chunk (use ImageEditEventTypePartial)
				chunk := &schemas.KoutakuImageGenerationStreamResponse{
					Type:         schemas.ImageEditEventTypePartial,
					Index:        0,
					ChunkIndex:   chunkIndex,
					B64JSON:      b64Data,
					CreatedAt:    time.Now().Unix(),
					OutputFormat: outputFormat,
					ExtraFields: schemas.KoutakuResponseExtraFields{
						ChunkIndex: chunkIndex,
						Latency:    time.Since(lastChunkTime).Milliseconds(),
					},
				}

				// Accumulate raw response chunks if enabled
				if sendBackRawResponse {
					rawResponseChunks = append(rawResponseChunks, ReplicateSSEEvent{Event: eventType, Data: eventData})
				}

				// Track last image data for final chunk
				lastB64Data = b64Data
				lastOutputFormat = outputFormat

				lastChunkTime = time.Now()
				chunkIndex++

				providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
					providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, nil, nil, nil, chunk),
					responseChan, postHookSpanFinalizer)

			case "done":
				// Parse done event data
				var doneData ReplicateDoneEvent
				if eventData != "" && eventData != "{}" {
					if err := sonic.Unmarshal(eventDataBytes, &doneData); err != nil {
						provider.logger.Warn(fmt.Sprintf("Failed to parse done event data: %v", err))
					}
				}

				// Check for cancellation or error
				switch doneData.Reason {
				case "canceled":
					koutakuErr := providerUtils.NewKoutakuOperationError(
						"prediction was canceled",
						fmt.Errorf("stream ended: prediction canceled"))
					if sendBackRawResponse && len(rawResponseChunks) > 0 {
						koutakuErr.ExtraFields.RawResponse = rawResponseChunks
					}
					koutakuErr = providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, sendBackRawRequest, sendBackRawResponse)
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, koutakuErr, responseChan, provider.logger, postHookSpanFinalizer)
					return
				case "error":
					koutakuErr := providerUtils.NewKoutakuOperationError(
						"prediction failed",
						fmt.Errorf("stream ended with error"))
					if sendBackRawResponse && len(rawResponseChunks) > 0 {
						koutakuErr.ExtraFields.RawResponse = rawResponseChunks
					}
					koutakuErr = providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, sendBackRawRequest, sendBackRawResponse)
					ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
					providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, koutakuErr, responseChan, provider.logger, postHookSpanFinalizer)
					return
				}

				// Send completion chunk (success case)
				finalChunk := &schemas.KoutakuImageGenerationStreamResponse{
					Type:         schemas.ImageEditEventTypeCompleted,
					Index:        0,
					ChunkIndex:   chunkIndex,
					B64JSON:      lastB64Data,
					CreatedAt:    time.Now().Unix(),
					OutputFormat: lastOutputFormat,
					ExtraFields: schemas.KoutakuResponseExtraFields{
						ChunkIndex: chunkIndex,
						Latency:    time.Since(startTime).Milliseconds(),
					},
				}

				if sendBackRawRequest {
					providerUtils.ParseAndSetRawRequest(&finalChunk.ExtraFields, jsonData)
				}
				if sendBackRawResponse {
					rawResponseChunks = append(rawResponseChunks, ReplicateSSEEvent{Event: eventType, Data: eventData})
					finalChunk.ExtraFields.RawResponse = rawResponseChunks
				}

				ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
				providerUtils.ProcessAndSendResponse(ctx, postHookRunner,
					providerUtils.GetKoutakuResponseForStreamResponse(nil, nil, nil, nil, nil, finalChunk),
					responseChan, postHookSpanFinalizer)
				return

			case "error":
				// Parse error event
				var errorData ReplicateErrorEvent
				if err := sonic.Unmarshal(eventDataBytes, &errorData); err != nil {
					provider.logger.Warn(fmt.Sprintf("Failed to parse error event: %v", err))
					errorData.Detail = eventData
				}

				koutakuErr := providerUtils.NewKoutakuOperationError(
					"stream error",
					fmt.Errorf("%s", errorData.Detail))
				if sendBackRawResponse {
					rawResponseChunks = append(rawResponseChunks, ReplicateSSEEvent{Event: eventType, Data: eventData})
					koutakuErr.ExtraFields.RawResponse = rawResponseChunks
				}
				koutakuErr = providerUtils.EnrichError(ctx, koutakuErr, jsonData, nil, sendBackRawRequest, sendBackRawResponse)
				ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
				providerUtils.ProcessAndSendKoutakuError(ctx, postHookRunner, koutakuErr, responseChan, provider.logger, postHookSpanFinalizer)
				return
			}
		}
	}()

	return responseChan, nil
}

// ImageVariation is not supported by the Replicate provider.
func (provider *ReplicateProvider) ImageVariation(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuImageVariationRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ImageVariationRequest, provider.GetProviderKey())
}

// VideoGeneration performs a video generation request to Replicate's API.
func (provider *ReplicateProvider) VideoGeneration(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuVideoGenerationRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Replicate, provider.customProviderConfig, schemas.VideoGenerationRequest); err != nil {
		return nil, err
	}

	// Convert Koutaku request to Replicate format
	jsonData, koutakuErr := providerUtils.CheckContextAndGetRequestBody(
		ctx,
		request,
		func() (providerUtils.RequestBodyWithExtraParams, error) {
			return ToReplicateVideoGenerationInput(request)
		})
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Create prediction asynchronously and return job ID without polling.
	predictionURL := buildPredictionURL(
		ctx,
		provider.networkConfig.BaseURL,
		request.Model,
		provider.customProviderConfig,
		schemas.VideoGenerationRequest,
		useDeploymentsEndpoint(key),
	)

	// Create prediction with appropriate mode
	prediction, rawResponse, latency, providerResponseHeaders, err := createPrediction(
		ctx,
		provider.client,
		jsonData,
		key,
		providerUtils.GetPathFromContext(ctx, predictionURL),
		provider.networkConfig.ExtraHeaders,
		false,
		provider.logger,
		provider.sendBackRawRequest,
		provider.sendBackRawResponse,
	)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}

	if providerResponseHeaders != nil {
		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)
	}

	// Convert to Koutaku response
	koutakuResponse, err := ToKoutakuVideoGenerationResponse(prediction)
	if err != nil {
		return nil, providerUtils.EnrichError(ctx, err, jsonData, nil, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}
	koutakuResponse.ID = providerUtils.AddVideoIDProviderSuffix(koutakuResponse.ID, schemas.Replicate)

	// Set extra fields
	koutakuResponse.ExtraFields.Latency = latency.Milliseconds()
	koutakuResponse.ExtraFields.ProviderResponseHeaders = providerResponseHeaders
	if providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest) {
		providerUtils.ParseAndSetRawRequest(&koutakuResponse.ExtraFields, jsonData)
	}
	if providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse) {
		koutakuResponse.ExtraFields.RawResponse = rawResponse
	}

	return koutakuResponse, nil
}

// VideoRetrieve fetches the status/output of a Replicate video generation job.
func (provider *ReplicateProvider) VideoRetrieve(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuVideoRetrieveRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Replicate, provider.customProviderConfig, schemas.VideoRetrieveRequest); err != nil {
		return nil, err
	}

	providerName := provider.GetProviderKey()
	if request.ID == "" {
		return nil, providerUtils.NewKoutakuOperationError("video_id is required", nil)
	}

	videoID := providerUtils.StripVideoIDProviderSuffix(request.ID, providerName)
	// Build URL to fetch the prediction by ID.
	predictionURL := provider.buildRequestURL(ctx, "/v1/predictions/"+videoID, schemas.VideoRetrieveRequest)

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(predictionURL)
	req.Header.SetMethod(http.MethodGet)
	req.Header.SetContentType("application/json")
	if key.Value.GetValue() != "" {
		req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
	}

	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, providerUtils.EnrichError(
			ctx,
			parseReplicateError(resp.Body(), resp.StatusCode()),
			nil,
			nil,
			provider.sendBackRawRequest,
			provider.sendBackRawResponse,
		)
	}

	providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
	}

	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	var prediction ReplicatePredictionResponse
	_, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &prediction, nil, false, sendBackRawResponse)
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	koutakuResponse, convertErr := ToKoutakuVideoGenerationResponse(&prediction)
	if convertErr != nil {
		return nil, providerUtils.EnrichError(ctx, convertErr, nil, body, provider.sendBackRawRequest, provider.sendBackRawResponse)
	}
	koutakuResponse.ID = providerUtils.AddVideoIDProviderSuffix(koutakuResponse.ID, providerName)

	koutakuResponse.ExtraFields.Latency = latency.Milliseconds()
	koutakuResponse.ExtraFields.ProviderResponseHeaders = providerResponseHeaders
	if sendBackRawResponse {
		koutakuResponse.ExtraFields.RawResponse = rawResponse
	}

	return koutakuResponse, nil
}

// VideoDownload is not supported by the Replicate provider.
func (provider *ReplicateProvider) VideoDownload(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuVideoDownloadRequest) (*schemas.KoutakuVideoDownloadResponse, *schemas.KoutakuError) {
	if err := providerUtils.CheckOperationAllowed(schemas.Replicate, provider.customProviderConfig, schemas.VideoDownloadRequest); err != nil {
		return nil, err
	}
	if request.ID == "" {
		return nil, providerUtils.NewKoutakuOperationError("video_id is required", nil)
	}
	// Retrieve latest status/output first.
	koutakuVideoRetrieveRequest := &schemas.KoutakuVideoRetrieveRequest{
		Provider: request.Provider,
		ID:       request.ID,
	}
	videoResp, koutakuErr := provider.VideoRetrieve(ctx, key, koutakuVideoRetrieveRequest)
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	if videoResp.Status != schemas.VideoStatusCompleted {
		return nil, providerUtils.NewKoutakuOperationError(
			fmt.Sprintf("video not ready, current status: %s", videoResp.Status),
			nil)
	}
	if len(videoResp.Videos) == 0 {
		return nil, providerUtils.NewKoutakuOperationError("video URL not available", nil)
	}
	var videoUrl string
	if videoResp.Videos[0].URL != nil {
		videoUrl = *videoResp.Videos[0].URL
	}
	if videoUrl == "" {
		return nil, providerUtils.NewKoutakuOperationError("invalid video output type", nil)
	}
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI(videoUrl)
	req.Header.SetMethod(http.MethodGet)
	// Some output URLs are signed and don't need auth, but keep auth if present.
	if key.Value.GetValue() != "" {
		req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
	}
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, providerUtils.NewKoutakuOperationError(
			fmt.Sprintf("failed to download video: HTTP %d", resp.StatusCode()),
			nil)
	}

	providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
	}
	contentType := string(resp.Header.ContentType())
	if contentType == "" {
		contentType = "video/mp4"
	}
	content := append([]byte(nil), body...)
	koutakuResp := &schemas.KoutakuVideoDownloadResponse{
		VideoID:     request.ID,
		Content:     content,
		ContentType: contentType,
	}

	koutakuResp.ExtraFields.Latency = latency.Milliseconds()
	koutakuResp.ExtraFields.ProviderResponseHeaders = providerResponseHeaders

	return koutakuResp, nil
}

// VideoDelete is not supported by replicate provider.
func (provider *ReplicateProvider) VideoDelete(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoDeleteRequest) (*schemas.KoutakuVideoDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoDeleteRequest, provider.GetProviderKey())
}

// VideoList is not supported by replicate provider.
func (provider *ReplicateProvider) VideoList(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoListRequest) (*schemas.KoutakuVideoListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoListRequest, provider.GetProviderKey())
}

// VideoRemix is not supported by replicate provider.
func (provider *ReplicateProvider) VideoRemix(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuVideoRemixRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.VideoRemixRequest, provider.GetProviderKey())
}

// BatchCreate is not supported by replicate provider.
func (provider *ReplicateProvider) BatchCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuBatchCreateRequest) (*schemas.KoutakuBatchCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchCreateRequest, provider.GetProviderKey())
}

// BatchList is not supported by replicate provider.
func (provider *ReplicateProvider) BatchList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchListRequest) (*schemas.KoutakuBatchListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchListRequest, provider.GetProviderKey())
}

// BatchRetrieve is not supported by replicate provider.
func (provider *ReplicateProvider) BatchRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchRetrieveRequest) (*schemas.KoutakuBatchRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchRetrieveRequest, provider.GetProviderKey())
}

// BatchCancel is not supported by replicate provider.
func (provider *ReplicateProvider) BatchCancel(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchCancelRequest) (*schemas.KoutakuBatchCancelResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchCancelRequest, provider.GetProviderKey())
}

// BatchDelete is not supported by replicate provider.
func (provider *ReplicateProvider) BatchDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchDeleteRequest) (*schemas.KoutakuBatchDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchDeleteRequest, provider.GetProviderKey())
}

// BatchResults is not supported by replicate provider.
func (provider *ReplicateProvider) BatchResults(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuBatchResultsRequest) (*schemas.KoutakuBatchResultsResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.BatchResultsRequest, provider.GetProviderKey())
}

// FileUpload uploads a file to Replicate's Files API.
func (provider *ReplicateProvider) FileUpload(ctx *schemas.KoutakuContext, key schemas.Key, request *schemas.KoutakuFileUploadRequest) (*schemas.KoutakuFileUploadResponse, *schemas.KoutakuError) {
	providerName := provider.GetProviderKey()

	if len(request.File) == 0 {
		return nil, providerUtils.NewKoutakuOperationError("file content is required", nil)
	}

	// Create multipart form data
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Add file field (content)
	filename := request.Filename
	if filename == "" {
		filename = "file"
	}

	// Determine content type - use from request or infer from filename
	contentType := "application/octet-stream"
	if request.ContentType != nil && *request.ContentType != "" {
		contentType = *request.ContentType
	} else {
		// Try to infer from filename extension
		if strings.HasSuffix(filename, ".json") {
			contentType = "application/json"
		} else if strings.HasSuffix(filename, ".jsonl") {
			contentType = "application/x-ndjson"
		} else if strings.HasSuffix(filename, ".txt") {
			contentType = "text/plain"
		} else if strings.HasSuffix(filename, ".zip") {
			contentType = "application/zip"
		}
	}

	// Add filename field if provided
	if filename != "" {
		if err := writer.WriteField("filename", filename); err != nil {
			return nil, providerUtils.NewKoutakuOperationError("failed to write filename field", err)
		}
	}

	// Add type field (content type)
	if err := writer.WriteField("type", contentType); err != nil {
		return nil, providerUtils.NewKoutakuOperationError("failed to write type field", err)
	}

	// Add metadata field if provided
	if request.ExtraParams != nil {
		if metadata, ok := request.ExtraParams["metadata"].(map[string]interface{}); ok {
			if len(metadata) > 0 {
				metadataJSON, err := providerUtils.MarshalSorted(metadata)
				if err != nil {
					return nil, providerUtils.NewKoutakuOperationError("failed to marshal metadata", err)
				}
				h := make(textproto.MIMEHeader)
				h.Set("Content-Disposition", `form-data; name="metadata"`)
				h.Set("Content-Type", "application/json")
				metadataPart, err := writer.CreatePart(h)
				if err != nil {
					return nil, providerUtils.NewKoutakuOperationError("failed to create metadata part", err)
				}
				if _, err := metadataPart.Write(metadataJSON); err != nil {
					return nil, providerUtils.NewKoutakuOperationError("failed to write metadata", err)
				}
			}
		}
	}

	// Create form file with proper headers
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="content"; filename="%s"`, filename))
	h.Set("Content-Type", contentType)

	part, err := writer.CreatePart(h)
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
	req.SetRequestURI(provider.networkConfig.BaseURL + "/v1/files")
	req.Header.SetMethod(http.MethodPost)
	req.Header.SetContentType(writer.FormDataContentType())

	if key.Value.GetValue() != "" {
		req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
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
		provider.logger.Debug("error from %s provider: %s", providerName, string(resp.Body()))
		return nil, parseReplicateError(resp.Body(), resp.StatusCode())
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
	}

	var replicateResp ReplicateFileResponse
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &replicateResp, nil, sendBackRawRequest, sendBackRawResponse)
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	fileResponse := replicateResp.ToKoutakuFileUploadResponse(providerName, latency, sendBackRawRequest, sendBackRawResponse, rawRequest, rawResponse)
	providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)
	fileResponse.ExtraFields.ProviderResponseHeaders = providerResponseHeaders
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)
	return fileResponse, nil
}

// FileList lists files using serial pagination across keys.
// Exhausts all pages from one key before moving to the next.
func (provider *ReplicateProvider) FileList(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuFileListRequest) (*schemas.KoutakuFileListResponse, *schemas.KoutakuError) {
	providerName := provider.GetProviderKey()
	sendBackRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)
	sendBackRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)

	// Initialize serial pagination helper (Replicate uses cursor-based pagination)
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
	requestURL := provider.networkConfig.BaseURL + "/v1/files"
	values := url.Values{}
	if request.Limit > 0 {
		values.Set("limit", fmt.Sprintf("%d", request.Limit))
	}
	// Use native cursor from serial helper (Replicate pagination URL)
	if nativeCursor != "" {
		// For Replicate, the cursor is actually the full next URL
		requestURL = nativeCursor
	} else if encodedValues := values.Encode(); encodedValues != "" {
		requestURL += "?" + encodedValues
	}

	// Set headers
	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(requestURL)
	req.Header.SetMethod(http.MethodGet)
	req.Header.SetContentType("application/json")

	if key.Value.GetValue() != "" {
		req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
	}

	// Make request
	latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		provider.logger.Debug("error from %s provider: %s", providerName, string(resp.Body()))
		return nil, parseReplicateError(resp.Body(), resp.StatusCode())
	}

	body, decodeErr := providerUtils.CheckAndDecodeBody(resp)
	if decodeErr != nil {
		return nil, providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, decodeErr)
	}

	var replicateResp ReplicateFileListResponse
	_, _, koutakuErr = providerUtils.HandleProviderResponse(body, &replicateResp, nil, sendBackRawRequest, sendBackRawResponse)
	if koutakuErr != nil {
		return nil, koutakuErr
	}

	// Convert files to Koutaku format
	files := make([]schemas.FileObject, 0, len(replicateResp.Results))
	for _, file := range replicateResp.Results {
		files = append(files, schemas.FileObject{
			ID:        file.ID,
			Object:    "file",
			Bytes:     file.Size,
			CreatedAt: ParseReplicateTimestamp(file.CreatedAt),
			Filename:  file.Name,
			Purpose:   schemas.FilePurposeBatch,
			Status:    ToKoutakuFileStatus(&file),
		})
	}

	// Build cursor for next request
	// Replicate uses full URL for pagination
	var nextCursor string
	hasMore := false
	if replicateResp.Next != nil && *replicateResp.Next != "" {
		nextCursor = *replicateResp.Next
		hasMore = true
	}

	// Use helper to build proper cursor with key index
	finalCursor, finalHasMore := helper.BuildNextCursor(hasMore, nextCursor)

	// Convert to Koutaku response
	providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)
	ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)

	koutakuResp := &schemas.KoutakuFileListResponse{
		Object:  "list",
		Data:    files,
		HasMore: finalHasMore,
		ExtraFields: schemas.KoutakuResponseExtraFields{
			Latency:                 latency.Milliseconds(),
			ProviderResponseHeaders: providerResponseHeaders,
		},
	}
	if finalCursor != "" {
		koutakuResp.After = &finalCursor
	}

	return koutakuResp, nil
}

// FileRetrieve retrieves file metadata from Replicate's Files API by trying each key until found.
func (provider *ReplicateProvider) FileRetrieve(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuFileRetrieveRequest) (*schemas.KoutakuFileRetrieveResponse, *schemas.KoutakuError) {
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
		req.SetRequestURI(provider.networkConfig.BaseURL + "/v1/files/" + url.PathEscape(request.FileID))
		req.Header.SetMethod(http.MethodGet)
		req.Header.SetContentType("application/json")

		if key.Value.GetValue() != "" {
			req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
		}

		// Make request
		latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
		wait()
		if koutakuErr != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = koutakuErr
			continue
		}

		// Handle error response
		if resp.StatusCode() != fasthttp.StatusOK {
			provider.logger.Debug("error from %s provider: %s", providerName, string(resp.Body()))
			lastErr = parseReplicateError(resp.Body(), resp.StatusCode())
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			continue
		}

		body, err := providerUtils.CheckAndDecodeBody(resp)
		if err != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
			continue
		}

		var replicateResp ReplicateFileResponse
		rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &replicateResp, nil, sendBackRawRequest, sendBackRawResponse)
		if koutakuErr != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = koutakuErr
			continue
		}

		providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)

		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)

		fileRetrieveResponse := replicateResp.ToKoutakuFileRetrieveResponse(providerName, latency, sendBackRawRequest, sendBackRawResponse, rawRequest, rawResponse)
		fileRetrieveResponse.ExtraFields.ProviderResponseHeaders = providerResponseHeaders
		return fileRetrieveResponse, nil
	}

	return nil, lastErr
}

// FileDelete deletes a file from Replicate's Files API by trying each key until successful.
func (provider *ReplicateProvider) FileDelete(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuFileDeleteRequest) (*schemas.KoutakuFileDeleteResponse, *schemas.KoutakuError) {
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
		req.SetRequestURI(provider.networkConfig.BaseURL + "/v1/files/" + url.PathEscape(request.FileID))
		req.Header.SetMethod(http.MethodDelete)
		req.Header.SetContentType("application/json")

		if key.Value.GetValue() != "" {
			req.Header.Set("Authorization", "Bearer "+key.Value.GetValue())
		}

		// Make request
		latency, koutakuErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
		wait()
		if koutakuErr != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = koutakuErr
			continue
		}

		// Handle success response (204 No Content is expected for DELETE)
		if resp.StatusCode() == fasthttp.StatusNoContent {
			providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)
			return &schemas.KoutakuFileDeleteResponse{
				ID:      request.FileID,
				Object:  "file",
				Deleted: true,
				ExtraFields: schemas.KoutakuResponseExtraFields{
					Latency:                 latency.Milliseconds(),
					ProviderResponseHeaders: providerResponseHeaders,
				},
			}, nil
		}

		// Handle error response
		if resp.StatusCode() != fasthttp.StatusOK {
			provider.logger.Debug("error from %s provider: %s", providerName, string(resp.Body()))
			lastErr = parseReplicateError(resp.Body(), resp.StatusCode())
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			continue
		}

		// Some APIs return 200 with body, parse it
		body, err := providerUtils.CheckAndDecodeBody(resp)
		if err != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = providerUtils.NewKoutakuOperationError(schemas.ErrProviderResponseDecode, err)
			continue
		}

		// Try to parse response body if present
		var deleteResp map[string]interface{}
		rawRequest, rawResponse, koutakuErr := providerUtils.HandleProviderResponse(body, &deleteResp, nil, sendBackRawRequest, sendBackRawResponse)
		if koutakuErr != nil {
			fasthttp.ReleaseRequest(req)
			fasthttp.ReleaseResponse(resp)
			lastErr = koutakuErr
			continue
		}

		providerResponseHeaders := providerUtils.ExtractProviderResponseHeaders(resp)
		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)

		ctx.SetValue(schemas.KoutakuContextKeyProviderResponseHeaders, providerResponseHeaders)

		result := &schemas.KoutakuFileDeleteResponse{
			ID:      request.FileID,
			Object:  "file",
			Deleted: true,
			ExtraFields: schemas.KoutakuResponseExtraFields{
				Latency:                 latency.Milliseconds(),
				ProviderResponseHeaders: providerResponseHeaders,
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

// FileContent is not supported by replicate provider.
func (provider *ReplicateProvider) FileContent(ctx *schemas.KoutakuContext, keys []schemas.Key, request *schemas.KoutakuFileContentRequest) (*schemas.KoutakuFileContentResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.FileContentRequest, provider.GetProviderKey())
}

func (provider *ReplicateProvider) CountTokens(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuResponsesRequest) (*schemas.KoutakuCountTokensResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.CountTokensRequest, provider.GetProviderKey())
}

// ContainerCreate is not supported by replicate provider.
func (provider *ReplicateProvider) ContainerCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuContainerCreateRequest) (*schemas.KoutakuContainerCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerCreateRequest, provider.GetProviderKey())
}

// ContainerList is not supported by replicate provider.
func (provider *ReplicateProvider) ContainerList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerListRequest) (*schemas.KoutakuContainerListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerListRequest, provider.GetProviderKey())
}

// ContainerRetrieve is not supported by replicate provider.
func (provider *ReplicateProvider) ContainerRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerRetrieveRequest) (*schemas.KoutakuContainerRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerRetrieveRequest, provider.GetProviderKey())
}

// ContainerDelete is not supported by replicate provider.
func (provider *ReplicateProvider) ContainerDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerDeleteRequest) (*schemas.KoutakuContainerDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerDeleteRequest, provider.GetProviderKey())
}

// ContainerFileCreate is not supported by replicate provider.
func (provider *ReplicateProvider) ContainerFileCreate(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuContainerFileCreateRequest) (*schemas.KoutakuContainerFileCreateResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileCreateRequest, provider.GetProviderKey())
}

// ContainerFileList is not supported by replicate provider.
func (provider *ReplicateProvider) ContainerFileList(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileListRequest) (*schemas.KoutakuContainerFileListResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileListRequest, provider.GetProviderKey())
}

// ContainerFileRetrieve is not supported by replicate provider.
func (provider *ReplicateProvider) ContainerFileRetrieve(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileRetrieveRequest) (*schemas.KoutakuContainerFileRetrieveResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileRetrieveRequest, provider.GetProviderKey())
}

// ContainerFileContent is not supported by replicate provider.
func (provider *ReplicateProvider) ContainerFileContent(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileContentRequest) (*schemas.KoutakuContainerFileContentResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileContentRequest, provider.GetProviderKey())
}

// ContainerFileDelete is not supported by replicate provider.
func (provider *ReplicateProvider) ContainerFileDelete(_ *schemas.KoutakuContext, _ []schemas.Key, _ *schemas.KoutakuContainerFileDeleteRequest) (*schemas.KoutakuContainerFileDeleteResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.ContainerFileDeleteRequest, provider.GetProviderKey())
}

// Passthrough is not supported by the Replicate provider.
func (provider *ReplicateProvider) Passthrough(_ *schemas.KoutakuContext, _ schemas.Key, _ *schemas.KoutakuPassthroughRequest) (*schemas.KoutakuPassthroughResponse, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.PassthroughRequest, provider.GetProviderKey())
}

func (provider *ReplicateProvider) PassthroughStream(_ *schemas.KoutakuContext, _ schemas.PostHookRunner, _ func(context.Context), _ schemas.Key, _ *schemas.KoutakuPassthroughRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	return nil, providerUtils.NewUnsupportedOperationError(schemas.PassthroughStreamRequest, provider.GetProviderKey())
}
