package integrations

import (
	"bytes"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
	koutaku "github.com/koutaku/koutaku/core"
	"github.com/koutaku/koutaku/core/providers/gemini"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/koutaku/koutaku/framework/kvstore"
	"github.com/koutaku/koutaku/transports/koutaku-http/lib"
	"github.com/valyala/fasthttp"
)

var koutakuContextKeyProvider = schemas.KoutakuContextKey("provider")

var availableIntegrations = []string{
	"openai",
	"anthropic",
	"genai",
	"litellm",
	"langchain",
	"bedrock",
	"pydantic",
	"cohere",
}

// newKoutakuErrorWithCode is like newKoutakuError but sets an explicit HTTP status code.
func newKoutakuErrorWithCode(err error, message string, statusCode int) *schemas.KoutakuError {
	e := newKoutakuError(err, message)
	e.StatusCode = &statusCode
	return e
}

// newKoutakuError wraps a standard error into a KoutakuError with IsKoutakuError set to false.
// This helper function reduces code duplication when handling non-Koutaku errors.
func newKoutakuError(err error, message string) *schemas.KoutakuError {
	if err == nil {
		return &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: message,
			},
		}
	}

	return &schemas.KoutakuError{
		IsKoutakuError: false,
		Error: &schemas.ErrorField{
			Message: message,
			Error:   err,
		},
	}
}

// safeGetRequestType safely obtains the request type from a KoutakuStreamChunk chunk.
// It checks multiple sources in order of preference:
// 1. Response ExtraFields if any response is available
// 2. KoutakuError ExtraFields if error is available and not nil
// 3. Falls back to "unknown" if no source is available
func safeGetRequestType(chunk *schemas.KoutakuStreamChunk) string {
	if chunk == nil {
		return "unknown"
	}

	// Try to get RequestType from response ExtraFields (preferred source)
	switch {
	case chunk.KoutakuTextCompletionResponse != nil:
		return string(chunk.KoutakuTextCompletionResponse.ExtraFields.RequestType)
	case chunk.KoutakuChatResponse != nil:
		return string(chunk.KoutakuChatResponse.ExtraFields.RequestType)
	case chunk.KoutakuResponsesStreamResponse != nil:
		return string(chunk.KoutakuResponsesStreamResponse.ExtraFields.RequestType)
	case chunk.KoutakuSpeechStreamResponse != nil:
		return string(chunk.KoutakuSpeechStreamResponse.ExtraFields.RequestType)
	case chunk.KoutakuTranscriptionStreamResponse != nil:
		return string(chunk.KoutakuTranscriptionStreamResponse.ExtraFields.RequestType)
	}

	// Try to get RequestType from error ExtraFields (fallback)
	if chunk.KoutakuError != nil && chunk.KoutakuError.ExtraFields.RequestType != "" {
		return string(chunk.KoutakuError.ExtraFields.RequestType)
	}

	// Final fallback
	return "unknown"
}

// extractHeadersFromRequest extracts headers from the request and returns them as a map.
// It uses the fasthttp.RequestCtx.Header.All() method to iterate over all headers.
func extractHeadersFromRequest(ctx *fasthttp.RequestCtx) map[string][]string {
	headers := make(map[string][]string)

	for key, value := range ctx.Request.Header.All() {
		keyStr := string(key)
		headers[keyStr] = append(headers[keyStr], string(value))
	}

	return headers
}

// extractExactPath returns the request path *after* the integration prefix,
// preserving the original query string exactly as sent by the client.
//
// Example:
//
//	/openai/v1/chat/completions?model=gpt-4o  ->  v1/chat/completions?model=gpt-4o
func extractExactPath(ctx *fasthttp.RequestCtx) string {
	// ctx.Path() returns only the path (no query) as a []byte backed by fasthttp’s internal buffers.
	// Treat it as read-only; don’t append to it directly.
	path := ctx.Path() // e.g. "/openai/v1/chat/completions"

	// Strip the integration prefix only if it’s at the start.
	for _, integration := range availableIntegrations {
		if bytes.HasPrefix(path, []byte("/"+integration+"/")) {
			path = path[len("/"+integration+"/"):]
			break
		}
	}

	// Raw query string as sent by client (unparsed, preserves ordering/duplicates/encoding).
	q := ctx.URI().QueryString() // e.g. "model=gpt-4o&stream=true"

	if len(q) == 0 {
		// No query → just return the (possibly trimmed) path.
		return string(path)
	}

	// --- Build "<path>?<query>" efficiently and safely ---
	//
	// Why not do: return string(path) + "?" + string(q) ?
	//   - That allocates multiple temporary strings and may copy data more than necessary.
	//
	// Why not append into 'path' directly?
	//   - 'path' may alias fasthttp’s internal buffers; mutating/expanding it could corrupt request state.
	//
	// We instead allocate a new buffer with exact capacity and copy into it,
	// staying in []byte until the final string conversion (1 allocation for the new slice).
	out := make([]byte, 0, len(path)+1+len(q)) // pre-size: path + "?" + query
	out = append(out, path...)                 // copy path bytes
	out = append(out, '?')                     // separator
	out = append(out, q...)                    // copy raw query bytes

	return string(out)
}

// sendStreamError sends an error response for a streaming request that failed before streaming started.
// It propagates the provider's HTTP status code and returns a JSON error body (not SSE format),
// since no streaming has begun and clients should receive a standard error response.
func (g *GenericRouter) sendStreamError(ctx *fasthttp.RequestCtx, koutakuCtx *schemas.KoutakuContext, config RouteConfig, koutakuErr *schemas.KoutakuError) {
	// Forward provider response headers from context so streaming error responses include them
	if koutakuCtx != nil {
		if headers, ok := koutakuCtx.Value(schemas.KoutakuContextKeyProviderResponseHeaders).(map[string]string); ok {
			for key, value := range headers {
				ctx.Response.Header.Set(key, value)
			}
		}
	}

	// Set the HTTP status code from the provider error
	if koutakuErr.StatusCode != nil {
		ctx.SetStatusCode(*koutakuErr.StatusCode)
	} else {
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
	}
	ctx.SetContentType("application/json")

	// Always use the route-level ErrorConverter (not StreamConfig.ErrorConverter) because
	// sendStreamError returns JSON, not SSE. StreamConfig.ErrorConverter is designed for
	// in-stream SSE errors (e.g., Anthropic's returns a raw SSE string that would be
	// double-escaped by JSON marshaling).
	errorResponse := config.ErrorConverter(koutakuCtx, koutakuErr)

	errorJSON, err := sonic.Marshal(errorResponse)
	if err != nil {
		g.logger.Error("failed to marshal error response", "err", err, "path", extractExactPath(ctx))
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		ctx.SetContentType("text/plain; charset=utf-8")
		ctx.SetBodyString(fmt.Sprintf("failed to encode error response: %v", err))
		return
	}

	ctx.SetBody(errorJSON)
}

// sendError sends an error response with the appropriate status code and JSON body.
// It handles different error types (string, error interface, or arbitrary objects).
func (g *GenericRouter) sendError(ctx *fasthttp.RequestCtx, koutakuCtx *schemas.KoutakuContext, errorConverter ErrorConverter, koutakuErr *schemas.KoutakuError) {
	// Forward provider response headers from context so error responses include them
	if koutakuCtx != nil {
		if headers, ok := koutakuCtx.Value(schemas.KoutakuContextKeyProviderResponseHeaders).(map[string]string); ok {
			for key, value := range headers {
				ctx.Response.Header.Set(key, value)
			}
		}
	}

	if koutakuErr.StatusCode != nil {
		ctx.SetStatusCode(*koutakuErr.StatusCode)
	} else {
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
	}
	ctx.SetContentType("application/json")

	// Marshal the error for response and log the error for diagnostics
	responseObj := errorConverter(koutakuCtx, koutakuErr)
	errorBody, err := sonic.Marshal(responseObj)
	if err != nil {
		// Log the marshal failure and return a plain text error
		g.logger.Error("failed to marshal error response", "err", err, "path", extractExactPath(ctx))
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		ctx.SetContentType("text/plain; charset=utf-8")
		ctx.SetBodyString(fmt.Sprintf("failed to encode error response: %v", err))
		return
	}

	ctx.SetBody(errorBody)
}

// sendSuccess sends a successful response with HTTP 200 status and JSON body.
func (g *GenericRouter) sendSuccess(ctx *fasthttp.RequestCtx, koutakuCtx *schemas.KoutakuContext, errorConverter ErrorConverter, response interface{}, extraHeaders map[string]string) {
	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.SetContentType("application/json")

	if extraHeaders != nil {
		for key, value := range extraHeaders {
			ctx.Response.Header.Set(key, value)
		}
	}

	responseBody, err := sonic.Marshal(response)
	if err != nil {
		g.sendError(ctx, koutakuCtx, errorConverter, newKoutakuError(err, "failed to encode response"))
		return
	}

	ctx.SetBody(responseBody)
}

// tryStreamLargeResponse checks if large response mode was activated by the provider,
// sets the transport marker, and streams the response directly to the client.
// Returns true if the response was handled (caller should return).
func (g *GenericRouter) tryStreamLargeResponse(ctx *fasthttp.RequestCtx, koutakuCtx *schemas.KoutakuContext) bool {
	isLargeResponse, ok := koutakuCtx.Value(schemas.KoutakuContextKeyLargeResponseMode).(bool)
	if !ok || !isLargeResponse {
		return false
	}
	// Forward provider response headers before streaming — providers store them in
	// context via KoutakuContextKeyProviderResponseHeaders, but some early-return
	// branches in the router skip the common footer that normally forwards them.
	if headers, ok := koutakuCtx.Value(schemas.KoutakuContextKeyProviderResponseHeaders).(map[string]string); ok {
		for key, value := range headers {
			ctx.Response.Header.Set(key, value)
		}
	}
	if g.streamLargeResponse(ctx, koutakuCtx) {
		ctx.SetUserValue(lib.FastHTTPUserValueLargeResponseMode, true)
	}
	return true
}

// streamLargeResponse streams the large response body directly from the upstream provider to the client.
// This bypasses the normal serialize → set body path, piping the response bytes unchanged.
func (g *GenericRouter) streamLargeResponse(ctx *fasthttp.RequestCtx, koutakuCtx *schemas.KoutakuContext) bool {
	// Enterprise hook: wrap the reader with Phase B scanning (e.g., usage extraction
	// from the full response stream) before streaming to client.
	if g.largeResponseHook != nil {
		g.largeResponseHook(ctx, koutakuCtx)
	}

	if !lib.StreamLargeResponseBody(ctx, koutakuCtx) {
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		ctx.SetBodyString("large response reader not available")
		return false
	}
	return true
}

// extractAndParseFallbacks extracts fallbacks from the integration request and adds them to the KoutakuRequest
func (g *GenericRouter) extractAndParseFallbacks(req interface{}, koutakuReq *schemas.KoutakuRequest) error {
	// Check if the request has a fallbacks field ([]string)
	fallbacks, err := g.extractFallbacksFromRequest(req)
	if err != nil {
		return fmt.Errorf("failed to extract fallbacks: %w", err)
	}

	if len(fallbacks) == 0 {
		return nil // No fallbacks to process
	}

	provider, _, _ := koutakuReq.GetRequestFields()

	// Parse fallbacks from strings to Fallback structs
	parsedFallbacks := make([]schemas.Fallback, 0, len(fallbacks))
	for _, fallbackStr := range fallbacks {
		if fallbackStr == "" {
			continue // Skip empty strings
		}

		// Use ParseModelString to extract provider and model
		provider, model := schemas.ParseModelString(fallbackStr, provider)

		parsedFallback := schemas.Fallback{
			Provider: provider,
			Model:    model,
		}
		parsedFallbacks = append(parsedFallbacks, parsedFallback)
	}

	if len(parsedFallbacks) == 0 {
		return nil // No valid fallbacks found
	}

	// Add fallbacks to the main KoutakuRequest
	koutakuReq.SetFallbacks(parsedFallbacks)

	// Also add fallbacks to the specific request type if it exists
	switch koutakuReq.RequestType {
	case schemas.TextCompletionRequest, schemas.TextCompletionStreamRequest:
		if koutakuReq.TextCompletionRequest != nil {
			koutakuReq.TextCompletionRequest.Fallbacks = parsedFallbacks
		}
	case schemas.ChatCompletionRequest, schemas.ChatCompletionStreamRequest:
		if koutakuReq.ChatRequest != nil {
			koutakuReq.ChatRequest.Fallbacks = parsedFallbacks
		}
	case schemas.ResponsesRequest, schemas.ResponsesStreamRequest:
		if koutakuReq.ResponsesRequest != nil {
			koutakuReq.ResponsesRequest.Fallbacks = parsedFallbacks
		}
	case schemas.EmbeddingRequest:
		if koutakuReq.EmbeddingRequest != nil {
			koutakuReq.EmbeddingRequest.Fallbacks = parsedFallbacks
		}
	case schemas.RerankRequest:
		if koutakuReq.RerankRequest != nil {
			koutakuReq.RerankRequest.Fallbacks = parsedFallbacks
		}
	case schemas.SpeechRequest, schemas.SpeechStreamRequest:
		if koutakuReq.SpeechRequest != nil {
			koutakuReq.SpeechRequest.Fallbacks = parsedFallbacks
		}
	case schemas.TranscriptionRequest, schemas.TranscriptionStreamRequest:
		if koutakuReq.TranscriptionRequest != nil {
			koutakuReq.TranscriptionRequest.Fallbacks = parsedFallbacks
		}
	case schemas.ImageGenerationRequest, schemas.ImageGenerationStreamRequest:
		if koutakuReq.ImageGenerationRequest != nil {
			koutakuReq.ImageGenerationRequest.Fallbacks = parsedFallbacks
		}
	}

	return nil
}

// extractFallbacksFromRequest uses reflection to extract fallbacks field from any request type
func (g *GenericRouter) extractFallbacksFromRequest(req interface{}) ([]string, error) {
	if req == nil {
		return nil, nil
	}

	// Try to use reflection to find a "fallbacks" field
	reqValue := reflect.ValueOf(req)
	if reqValue.Kind() == reflect.Ptr {
		reqValue = reqValue.Elem()
	}

	if reqValue.Kind() != reflect.Struct {
		return nil, nil // Not a struct, no fallbacks
	}

	// Look for the "fallbacks" field
	fallbacksField := reqValue.FieldByName("fallbacks")
	if !fallbacksField.IsValid() {
		return nil, nil // No fallbacks field found
	}

	// Handle different types of fallbacks field
	switch fallbacksField.Kind() {
	case reflect.Slice:
		if fallbacksField.Type().Elem().Kind() == reflect.String {
			// []string case
			fallbacks := make([]string, fallbacksField.Len())
			for i := 0; i < fallbacksField.Len(); i++ {
				fallbacks[i] = fallbacksField.Index(i).String()
			}
			return fallbacks, nil
		}
	case reflect.String:
		// Single string case - treat as one fallback
		return []string{fallbacksField.String()}, nil
	}

	return nil, nil
}

// getVirtualKeyFromKoutakuContext extracts the virtual key value from koutaku context.
// Returns nil if no VK is present (e.g., direct key mode or no governance).
func getVirtualKeyFromKoutakuContext(ctx *schemas.KoutakuContext) *string {
	vkValue := koutaku.GetStringFromContext(ctx, schemas.KoutakuContextKeyVirtualKey)
	if vkValue == "" {
		return nil
	}
	return &vkValue
}

// getResultTTLFromHeaderWithDefault extracts the result TTL from the x-bf-async-job-result-ttl header.
// Returns the default TTL if the header is not present or invalid.
func getResultTTLFromHeaderWithDefault(ctx *fasthttp.RequestCtx, defaultTTL int) int {
	resultTTL := string(ctx.Request.Header.Peek(schemas.AsyncHeaderResultTTL))
	if resultTTL == "" {
		return defaultTTL
	}
	resultTTLInt, err := strconv.Atoi(resultTTL)
	if err != nil || resultTTLInt < 0 {
		return defaultTTL
	}
	return resultTTLInt
}

// isAnthropicAPIKeyAuth checks if the request uses standard API key authentication.
// Returns true for API key auth (x-api-key header), false for OAuth (Bearer sk-ant-oat*).
// This is required for Claude Code specifically, which may use OAuth authentication.
// Default behavior is to assume API mode when neither x-api-key nor OAuth token is present.
func isAnthropicAPIKeyAuth(ctx *fasthttp.RequestCtx) bool {
	// If x-api-key header is present - this is definitely API mode
	if apiKey := string(ctx.Request.Header.Peek("x-api-key")); apiKey != "" {
		return true
	}
	// Check for OAuth token in Authorization header
	if authHeader := string(ctx.Request.Header.Peek("Authorization")); authHeader != "" {
		if strings.HasPrefix(strings.ToLower(authHeader), "bearer sk-ant-oat") {
			return false // OAuth mode, NOT API
		}
	}
	// Default to API mode
	return true
}

// resolveLargePayloadMetadata returns metadata from the sync context key,
// falling back to a non-blocking read from the deferred channel.
// If deferred metadata is resolved, it is cached in the sync key for later readers.
func resolveLargePayloadMetadata(koutakuCtx *schemas.KoutakuContext) *schemas.LargePayloadMetadata {
	if koutakuCtx == nil {
		return nil
	}
	if metadata, ok := koutakuCtx.Value(schemas.KoutakuContextKeyLargePayloadMetadata).(*schemas.LargePayloadMetadata); ok && metadata != nil {
		return metadata
	}
	ch, ok := koutakuCtx.Value(schemas.KoutakuContextKeyDeferredLargePayloadMetadata).(<-chan *schemas.LargePayloadMetadata)
	if !ok || ch == nil {
		return nil
	}
	select {
	case metadata := <-ch:
		if metadata != nil {
			koutakuCtx.SetValue(schemas.KoutakuContextKeyLargePayloadMetadata, metadata)
		}
		return metadata
	default:
		return nil
	}
}

// ParseProviderScopedVideoID parses a provider-scoped video ID in the form "id:provider".
// The ID portion is automatically URL-decoded to restore the original ID.
func ParseProviderScopedVideoID(videoID string) (schemas.ModelProvider, string, error) {
	parts := strings.SplitN(videoID, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("video_id must be in id:provider format")
	}
	provider := schemas.ModelProvider(parts[1])
	rawID := parts[0]

	// URL decode the ID to restore original characters (e.g., %2F -> /)
	// This handles IDs from all providers that may contain special characters
	if decoded, err := url.PathUnescape(rawID); err == nil {
		rawID = decoded
	}

	return provider, rawID, nil
}

func getProviderFromHeader(ctx *fasthttp.RequestCtx, defaultProvider schemas.ModelProvider) schemas.ModelProvider {
	providerHeader := string(ctx.Request.Header.Peek("x-model-provider"))
	if providerHeader == "" {
		return defaultProvider
	}
	return schemas.ModelProvider(providerHeader)
}

func RegisterKVDecoders(store *kvstore.Store) {
	store.RegisterDecoder("genai_upload_session:", func(data []byte) (any, error) {
		var v gemini.GeminiResumableUploadSession
		return &v, sonic.Unmarshal(data, &v)
	})
}
