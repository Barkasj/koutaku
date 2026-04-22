package handlers

import (
	"context"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/fasthttp/router"
	ws "github.com/fasthttp/websocket"
	koutaku "github.com/koutaku/koutaku/core"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/koutaku/koutaku/transports/koutaku-http/integrations"
	"github.com/koutaku/koutaku/transports/koutaku-http/lib"
	bfws "github.com/koutaku/koutaku/transports/koutaku-http/websocket"
	"github.com/valyala/fasthttp"
)

// wsWriter abstracts a WebSocket write target. Both *ws.Conn (pre-session)
// and *bfws.Session (post-session, mutex-protected) satisfy this interface.
type wsWriter interface {
	WriteMessage(messageType int, data []byte) error
}

// WSResponsesHandler handles WebSocket connections for the Responses API WebSocket Mode.
// Clients connect via `GET /v1/responses` with a WS upgrade and send `response.create` events.
// Each event is routed through the standard Koutaku inference pipeline (PreLLMHook, key selection,
// provider call, PostLLMHook) via the HTTP bridge, with native WS upstream as an optimization.
type WSResponsesHandler struct {
	client       *koutaku.Koutaku
	config       *lib.Config
	handlerStore lib.HandlerStore
	pool         *bfws.Pool
	sessions     *bfws.SessionManager
	upgrader     ws.FastHTTPUpgrader
}

// NewWSResponsesHandler creates a new WebSocket Responses handler.
func NewWSResponsesHandler(client *koutaku.Koutaku, config *lib.Config, pool *bfws.Pool) *WSResponsesHandler {
	maxConns := config.WebSocketConfig.MaxConnections

	return &WSResponsesHandler{
		client:       client,
		config:       config,
		handlerStore: config,
		pool:         pool,
		sessions:     bfws.NewSessionManager(maxConns),
		upgrader: ws.FastHTTPUpgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin: func(ctx *fasthttp.RequestCtx) bool {
				origin := string(ctx.Request.Header.Peek("Origin"))
				if origin == "" {
					return true
				}
				return IsOriginAllowed(origin, config.ClientConfig.AllowedOrigins)
			},
		},
	}
}

// Close gracefully shuts down all active WebSocket responses sessions.
func (h *WSResponsesHandler) Close() {
	if h == nil || h.sessions == nil {
		return
	}
	h.sessions.CloseAll()
}

// RegisterRoutes registers the WebSocket Responses endpoint at the base path
// and all OpenAI integration paths.
func (h *WSResponsesHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.KoutakuHTTPMiddleware) {
	handler := lib.ChainMiddlewares(h.handleUpgrade, middlewares...)
	// Base path (outside integration prefix)
	r.GET("/v1/responses", handler)
	// OpenAI integration paths (/openai/v1/responses, /openai/responses, /openai/openai/responses)
	for _, path := range integrations.OpenAIWSResponsesPaths("/openai") {
		r.GET(path, handler)
	}
}

// handleUpgrade upgrades the HTTP connection to WebSocket and starts the event loop.
func (h *WSResponsesHandler) handleUpgrade(ctx *fasthttp.RequestCtx) {
	err := h.upgrader.Upgrade(ctx, func(conn *ws.Conn) {
		defer conn.Close()

		session, sessionErr := h.sessions.Create(conn)
		if sessionErr != nil {
			writeWSError(conn, 429, "websocket_connection_limit_reached", sessionErr.Error())
			return
		}
		defer h.sessions.Remove(conn)

		// Capture auth headers from the upgrade request for per-event context creation
		authHeaders := captureAuthHeaders(ctx)

		h.eventLoop(conn, session, authHeaders)
	})
	if err != nil {
		logger.Warn("websocket upgrade failed for /v1/responses: %v", err)
	}
}

// authHeaders holds auth-related headers captured during the WS upgrade.
type authHeaders struct {
	authorization string
	virtualKey    string
	apiKey        string
	googAPIKey    string
	baggage       string
	extraHeaders  map[string]string
}

// captureAuthHeaders captures the auth headers from the request.
func captureAuthHeaders(ctx *fasthttp.RequestCtx) *authHeaders {
	ah := &authHeaders{
		authorization: string(ctx.Request.Header.Peek("Authorization")),
		virtualKey:    string(ctx.Request.Header.Peek("x-bf-vk")),
		apiKey:        string(ctx.Request.Header.Peek("x-api-key")),
		googAPIKey:    string(ctx.Request.Header.Peek("x-goog-api-key")),
		baggage:       string(ctx.Request.Header.Peek("baggage")),
		extraHeaders:  make(map[string]string),
	}

	for key, value := range ctx.Request.Header.All() {
		k := string(key)
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-bf-") {
			ah.extraHeaders[k] = string(value)
		}
	}
	return ah
}

// eventLoop reads events from the client WebSocket and processes them.
func (h *WSResponsesHandler) eventLoop(conn *ws.Conn, session *bfws.Session, auth *authHeaders) {
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if ws.IsUnexpectedCloseError(err, ws.CloseGoingAway, ws.CloseNormalClosure) {
				logger.Warn("websocket read error: %v", err)
			}
			return
		}

		// Parse the event type
		var envelope struct {
			Type string `json:"type"`
		}
		if err := sonic.Unmarshal(message, &envelope); err != nil {
			writeWSError(session, 400, "invalid_request_error", "failed to parse event JSON")
			continue
		}

		switch schemas.WebSocketEventType(envelope.Type) {
		case schemas.WSEventResponseCreate:
			h.handleResponseCreate(session, auth, message)
		default:
			writeWSError(session, 400, "invalid_request_error", "unsupported event type: "+envelope.Type)
		}
	}
}

// handleResponseCreate processes a response.create event.
// Strategy: try native WS upstream for providers that support it, otherwise use HTTP bridge.
// If native WS upstream fails mid-stream, falls back to HTTP bridge.
func (h *WSResponsesHandler) handleResponseCreate(session *bfws.Session, auth *authHeaders, message []byte) {
	var event schemas.WebSocketResponsesEvent

	if err := sonic.Unmarshal(message, &event); err != nil {
		writeWSError(session, 400, "invalid_request_error", "failed to parse response.create event")
		return
	}

	// Store override: default to store=true (Codex sends false by default but expects true).
	// If DisableStore is set in provider config, force store=false.
	// If client explicitly sets store, respect that value unless DisableStore overrides it.
	provider, modelName := schemas.ParseModelString(event.Model, "")
	if provider == "" || modelName == "" {
		writeWSError(session, 400, "invalid_request_error", "failed to parse model string")
		return
	}

	if providerCfg, cfgErr := h.config.GetProviderConfigRaw(provider); cfgErr == nil &&
		providerCfg.OpenAIConfig != nil && providerCfg.OpenAIConfig.DisableStore {
		event.Store = schemas.Ptr(false)
	} else {
		event.Store = schemas.Ptr(true)
	}

	koutakuReq, err := h.convertEventToRequest(&event)
	if err != nil {
		writeWSError(session, 400, "invalid_request_error", err.Error())
		return
	}

	// Extract extra params (unknown fields) and forward them, matching the HTTP path behavior
	extraParams, extractErr := extractExtraParams(message, wsResponsesKnownFields)
	if extractErr == nil && len(extraParams) > 0 {
		if koutakuReq.Params == nil {
			koutakuReq.Params = &schemas.ResponsesParameters{}
		}
		koutakuReq.Params.ExtraParams = extraParams
	}

	koutakuCtx, cancel := createKoutakuContextFromAuth(h.handlerStore, auth)
	if koutakuCtx == nil {
		writeWSError(session, 500, "server_error", "failed to create request context")
		return
	}

	// Try native WS upstream first
	if h.tryNativeWSUpstream(session, koutakuCtx, koutakuReq, message) {
		cancel()
		return
	}

	// Fall back to HTTP bridge
	h.executeHTTPBridge(session, koutakuCtx, cancel, koutakuReq)
}

// tryNativeWSUpstream attempts to forward the event to a native WS upstream connection.
// Returns true if the event was handled (successfully or with error sent to client).
// Returns false if the provider doesn't support WS and we should fall back to HTTP bridge.
func (h *WSResponsesHandler) tryNativeWSUpstream(
	session *bfws.Session,
	ctx *schemas.KoutakuContext,
	req *schemas.KoutakuResponsesRequest,
	rawEvent []byte,
) bool {
	provider := h.client.GetProviderByKey(req.Provider)
	if provider == nil {
		return false
	}

	wsProvider, ok := provider.(schemas.WebSocketCapableProvider)
	if !ok || !wsProvider.SupportsWebSocketMode() {
		return false
	}

	key, err := h.client.SelectKeyForProviderRequestType(ctx, schemas.WebSocketResponsesRequest, req.Provider, req.Model)
	if err != nil {
		writeWSError(session, 400, "invalid_request_error", err.Error())
		return true
	}

	wsURL := wsProvider.WebSocketResponsesURL(key)
	upstream := session.Upstream()

	// Validate the pinned upstream matches the current request's provider/key
	if upstream != nil && !upstream.IsClosed() &&
		(upstream.Provider() != req.Provider || upstream.KeyID() != key.ID) {
		h.pool.Discard(upstream)
		session.SetUpstream(nil)
		upstream = nil
	}

	// If no upstream connection pinned, get one from the pool or dial
	if upstream == nil || upstream.IsClosed() {
		headers := wsProvider.WebSocketHeaders(key)
		poolKey := bfws.PoolKey{
			Provider: req.Provider,
			KeyID:    key.ID,
			Endpoint: wsURL,
		}

		upstream, err = h.pool.Get(poolKey, headers)
		if err != nil {
			logger.Warn("failed to get upstream WS connection for %s: %v, falling back to HTTP bridge", req.Provider, err)
			return false
		}
		session.SetUpstream(upstream)
	}

	// Run plugin pre-hooks before forwarding to upstream
	koutakuReq := &schemas.KoutakuRequest{
		RequestType:      schemas.WebSocketResponsesRequest,
		ResponsesRequest: req,
	}

	hooks, preErr := h.client.RunStreamPreHooks(ctx, koutakuReq)
	if preErr != nil {
		writeWSKoutakuError(session, preErr)
		return true
	}
	defer hooks.Cleanup()

	// If a plugin short-circuited with a cached response, write it and skip upstream
	if hooks.ShortCircuitResponse != nil {
		writeWSShortCircuitResponse(session, hooks.ShortCircuitResponse)
		return true
	}

	// Forward the raw event to upstream
	if err := upstream.WriteMessage(ws.TextMessage, rawEvent); err != nil {
		logger.Warn("upstream WS write failed for %s: %v, falling back to HTTP bridge", req.Provider, err)
		h.pool.Discard(upstream)
		session.SetUpstream(nil)
		return false
	}

	// Retrieve tracer and traceID for chunk accumulation
	tracer, _ := ctx.Value(schemas.KoutakuContextKeyTracer).(schemas.Tracer)
	traceID, _ := ctx.Value(schemas.KoutakuContextKeyTraceID).(string)

	// Read response events from upstream and relay to client, running post-hooks per chunk
	forwardedAny := false
	for {
		msgType, data, readErr := upstream.ReadMessage()
		if readErr != nil {
			logger.Warn("upstream WS read failed for %s: %v, falling back to HTTP bridge", req.Provider, readErr)
			h.pool.Discard(upstream)
			session.SetUpstream(nil)
			if !forwardedAny {
				return false
			}
			writeWSError(session, 502, "upstream_connection_error", "upstream websocket stream interrupted")
			return true
		}

		streamResp := parseUpstreamWSEvent(data, req.Provider, req.Model)
		isTerminal := streamResp != nil && isTerminalStreamType(streamResp.Type)

		if isTerminal {
			ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
		}

		if streamResp != nil {
			resp := &schemas.KoutakuResponse{ResponsesStreamResponse: streamResp}

			if tracer != nil && traceID != "" {
				tracer.AddStreamingChunk(traceID, resp)
			}

			_, postErr := hooks.PostHookRunner(ctx, resp, nil)
			if postErr != nil {
				h.pool.Discard(upstream)
				session.SetUpstream(nil)
				writeWSKoutakuError(session, postErr)
				return true
			}
		}

		if writeErr := session.WriteMessage(msgType, data); writeErr != nil {
			h.pool.Discard(upstream)
			session.SetUpstream(nil)
			return true
		}
		forwardedAny = true

		if isTerminal {
			h.trackResponseID(session, data)
			return true
		}
	}
}

// writeWSShortCircuitResponse writes a short-circuited plugin response as WS events.
func writeWSShortCircuitResponse(session *bfws.Session, resp *schemas.KoutakuResponse) {
	if resp.ResponsesResponse != nil {
		data, err := sonic.Marshal(resp.ResponsesResponse)
		if err != nil {
			return
		}
		if err := session.WriteMessage(ws.TextMessage, data); err != nil {
			return
		}
		if resp.ResponsesResponse.ID != nil && *resp.ResponsesResponse.ID != "" {
			session.SetLastResponseID(*resp.ResponsesResponse.ID)
		}
	} else if resp.ResponsesStreamResponse != nil {
		data, err := sonic.Marshal(resp.ResponsesStreamResponse)
		if err != nil {
			return
		}
		session.WriteMessage(ws.TextMessage, data)
	}
}

// parseUpstreamWSEvent attempts to parse a raw upstream WS event into a KoutakuResponsesStreamResponse.
// It populates ExtraFields so downstream plugins (logging, tracing) can identify the request type.
// Returns nil if the data cannot be parsed (non-fatal, the raw bytes are still relayed).
func parseUpstreamWSEvent(data []byte, provider schemas.ModelProvider, model string) *schemas.KoutakuResponsesStreamResponse {
	var streamResp schemas.KoutakuResponsesStreamResponse
	if err := sonic.Unmarshal(data, &streamResp); err != nil {
		return nil
	}
	if streamResp.Type == "" {
		return nil
	}
	streamResp.ExtraFields.RequestType = schemas.ResponsesStreamRequest
	streamResp.ExtraFields.Provider = provider
	streamResp.ExtraFields.OriginalModelRequested = model
	return &streamResp
}

// isTerminalStreamType returns true if the event type signals the end of a response stream.
func isTerminalStreamType(t schemas.ResponsesStreamResponseType) bool {
	switch t {
	case schemas.ResponsesStreamResponseTypeCompleted,
		schemas.ResponsesStreamResponseTypeFailed,
		schemas.ResponsesStreamResponseTypeIncomplete,
		schemas.ResponsesStreamResponseTypeError:
		return true
	}
	return false
}

// trackResponseID extracts and stores the response ID from terminal events.
func (h *WSResponsesHandler) trackResponseID(session *bfws.Session, data []byte) {
	var envelope struct {
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if err := sonic.Unmarshal(data, &envelope); err == nil && envelope.Response.ID != "" {
		session.SetLastResponseID(envelope.Response.ID)
	}
}

// convertEventToRequest converts a WebSocket response.create event to a KoutakuResponsesRequest.
func (h *WSResponsesHandler) convertEventToRequest(event *schemas.WebSocketResponsesEvent) (*schemas.KoutakuResponsesRequest, error) {
	provider, modelName := schemas.ParseModelString(event.Model, "")
	if provider == "" || modelName == "" {
		return nil, errModelFormat
	}

	var input []schemas.ResponsesMessage
	if event.Input != nil {
		// Try parsing as array first
		if err := sonic.Unmarshal(event.Input, &input); err != nil {
			// Try as string
			var inputStr string
			if strErr := sonic.Unmarshal(event.Input, &inputStr); strErr != nil {
				return nil, errInputRequired
			}
			input = []schemas.ResponsesMessage{
				{
					Role:    schemas.Ptr(schemas.ResponsesInputMessageRoleUser),
					Content: &schemas.ResponsesMessageContent{ContentStr: &inputStr},
				},
			}
		}
	}
	if len(input) == 0 {
		return nil, errInputRequired
	}

	params := &schemas.ResponsesParameters{}
	if event.Temperature != nil {
		params.Temperature = event.Temperature
	}
	if event.TopP != nil {
		params.TopP = event.TopP
	}
	if event.MaxOutputTokens != nil {
		params.MaxOutputTokens = event.MaxOutputTokens
	}
	if event.Instructions != "" {
		params.Instructions = &event.Instructions
	}
	if event.PreviousResponseID != "" {
		params.PreviousResponseID = &event.PreviousResponseID
	}
	if event.Store != nil {
		params.Store = event.Store
	}
	if event.Tools != nil {
		var tools []schemas.ResponsesTool
		if err := sonic.Unmarshal(event.Tools, &tools); err == nil {
			params.Tools = tools
		}
	}
	if event.ToolChoice != nil {
		var tc schemas.ResponsesToolChoice
		if err := sonic.Unmarshal(event.ToolChoice, &tc); err == nil {
			params.ToolChoice = &tc
		}
	}
	if event.Reasoning != nil {
		var reasoning schemas.ResponsesParametersReasoning
		if err := sonic.Unmarshal(event.Reasoning, &reasoning); err == nil {
			params.Reasoning = &reasoning
		}
	}
	if event.Text != nil {
		var text schemas.ResponsesTextConfig
		if err := sonic.Unmarshal(event.Text, &text); err == nil {
			params.Text = &text
		}
	}
	if event.Metadata != nil {
		var metadata map[string]any
		if err := sonic.Unmarshal(event.Metadata, &metadata); err == nil {
			params.Metadata = &metadata
		}
	}
	if event.Truncation != "" {
		params.Truncation = &event.Truncation
	}

	return &schemas.KoutakuResponsesRequest{
		Provider: schemas.ModelProvider(provider),
		Model:    modelName,
		Input:    input,
		Params:   params,
	}, nil
}

// createKoutakuContextFromAuth builds a KoutakuContext from the auth headers captured during upgrade.
func createKoutakuContextFromAuth(handlerStore lib.HandlerStore, auth *authHeaders) (*schemas.KoutakuContext, context.CancelFunc) {
	ctx, cancel := schemas.NewKoutakuContextWithCancel(context.Background())

	if sessionID := lib.ParseSessionIDFromBaggage(auth.baggage); sessionID != "" {
		ctx.SetValue(schemas.KoutakuContextKeyParentRequestID, sessionID)
	}

	if auth.virtualKey != "" {
		ctx.SetValue(schemas.KoutakuContextKeyVirtualKey, auth.virtualKey)
	}

	// Handle Bearer token with sk-bf- prefix (virtual key via Authorization header)
	if auth.authorization != "" {
		if strings.HasPrefix(auth.authorization, "Bearer ") {
			token := strings.TrimPrefix(auth.authorization, "Bearer ")
			if strings.HasPrefix(token, "sk-bf-") {
				ctx.SetValue(schemas.KoutakuContextKeyVirtualKey, strings.TrimPrefix(token, "sk-bf-"))
			} else if handlerStore.ShouldAllowDirectKeys() {
				key := schemas.Key{
					ID:     "header-provided",
					Value:  *schemas.NewEnvVar(token),
					Models: schemas.WhiteList{"*"},
					Weight: 1.0,
				}
				ctx.SetValue(schemas.KoutakuContextKeyDirectKey, key)
			}
		}
	}
	if auth.apiKey != "" {
		if strings.HasPrefix(auth.apiKey, "sk-bf-") {
			ctx.SetValue(schemas.KoutakuContextKeyVirtualKey, strings.TrimPrefix(auth.apiKey, "sk-bf-"))
		} else if handlerStore.ShouldAllowDirectKeys() {
			key := schemas.Key{
				ID:     "header-provided",
				Value:  *schemas.NewEnvVar(auth.apiKey),
				Models: schemas.WhiteList{"*"},
				Weight: 1.0,
			}
			ctx.SetValue(schemas.KoutakuContextKeyDirectKey, key)
		}
	}
	if auth.googAPIKey != "" {
		if strings.HasPrefix(auth.googAPIKey, "sk-bf-") {
			ctx.SetValue(schemas.KoutakuContextKeyVirtualKey, strings.TrimPrefix(auth.googAPIKey, "sk-bf-"))
		} else if handlerStore.ShouldAllowDirectKeys() {
			key := schemas.Key{
				ID:     "header-provided",
				Value:  *schemas.NewEnvVar(auth.googAPIKey),
				Models: schemas.WhiteList{"*"},
				Weight: 1.0,
			}
			ctx.SetValue(schemas.KoutakuContextKeyDirectKey, key)
		}
	}

	// Forward x-bf-* headers
	for k, v := range auth.extraHeaders {
		lk := strings.ToLower(k)
		switch {
		case lk == "x-bf-vk":
			// Already handled above
		case lk == "x-bf-api-key":
			ctx.SetValue(schemas.KoutakuContextKeyAPIKeyName, v)
		case strings.HasPrefix(lk, "x-bf-eh-"):
			suffix := strings.TrimPrefix(lk, "x-bf-eh-")
			existing, _ := ctx.Value(schemas.KoutakuContextKeyExtraHeaders).(map[string]string)
			if existing == nil {
				existing = make(map[string]string)
			}
			existing[suffix] = v
			ctx.SetValue(schemas.KoutakuContextKeyExtraHeaders, existing)
		}
	}

	return ctx, cancel
}

// executeHTTPBridge runs the response through the existing streaming inference pipeline.
func (h *WSResponsesHandler) executeHTTPBridge(
	session *bfws.Session,
	ctx *schemas.KoutakuContext,
	cancel context.CancelFunc,
	req *schemas.KoutakuResponsesRequest,
) {
	defer cancel()

	stream, koutakuErr := h.client.ResponsesStreamRequest(ctx, req)
	if koutakuErr != nil {
		writeWSKoutakuError(session, koutakuErr)
		return
	}

	// Relay streaming chunks as WS messages
	for chunk := range stream {
		if chunk == nil {
			continue
		}

		chunkJSON, err := sonic.Marshal(chunk)
		if err != nil {
			logger.Warn("failed to marshal stream chunk: %v", err)
			continue
		}

		if writeErr := session.WriteMessage(ws.TextMessage, chunkJSON); writeErr != nil {
			return
		}

		// Track last response ID for session chaining
		if chunk.KoutakuResponsesStreamResponse != nil &&
			chunk.KoutakuResponsesStreamResponse.Response != nil &&
			chunk.KoutakuResponsesStreamResponse.Response.ID != nil &&
			*chunk.KoutakuResponsesStreamResponse.Response.ID != "" {
			session.SetLastResponseID(*chunk.KoutakuResponsesStreamResponse.Response.ID)
		}
	}
}

// writeWSError sends a JSON error event to a WebSocket write target.
// Accepts either a raw *ws.Conn (pre-session) or a *bfws.Session (mutex-protected).
func writeWSError(w wsWriter, status int, code, message string) {
	event := schemas.WebSocketErrorEvent{
		Type:   schemas.WSEventError,
		Status: status,
		Error: &schemas.WebSocketErrorBody{
			Code:    code,
			Message: message,
		},
	}
	data, err := sonic.Marshal(event)
	if err != nil {
		return
	}
	w.WriteMessage(ws.TextMessage, data)
}

// writeWSKoutakuError converts a KoutakuError to a WS error event.
func writeWSKoutakuError(w wsWriter, koutakuErr *schemas.KoutakuError) {
	status := 500
	if koutakuErr.StatusCode != nil && *koutakuErr.StatusCode > 0 {
		status = *koutakuErr.StatusCode
	}
	code := "server_error"
	msg := "internal server error"
	if koutakuErr.Error != nil {
		if koutakuErr.Error.Code != nil && *koutakuErr.Error.Code != "" {
			code = *koutakuErr.Error.Code
		} else if koutakuErr.Error.Type != nil && *koutakuErr.Error.Type != "" {
			code = *koutakuErr.Error.Type
		}
		if koutakuErr.Error.Message != "" {
			msg = koutakuErr.Error.Message
		}
	}
	writeWSError(w, status, code, msg)
}

// wsResponsesKnownFields lists the fields explicitly handled by WebSocketResponsesEvent.
// Anything not in this set is treated as an extra param and forwarded as-is to the provider.
var wsResponsesKnownFields = map[string]bool{
	"type":                 true,
	"model":                true,
	"store":                true,
	"input":                true,
	"instructions":         true,
	"previous_response_id": true,
	"tools":                true,
	"tool_choice":          true,
	"temperature":          true,
	"top_p":                true,
	"max_output_tokens":    true,
	"reasoning":            true,
	"metadata":             true,
	"text":                 true,
	"truncation":           true,
}

var (
	errModelFormat   = errorf("model should be in provider/model format")
	errInputRequired = errorf("input is required for responses")
)

func errorf(msg string) error {
	return &simpleError{msg: msg}
}

type simpleError struct {
	msg string
}

func (e *simpleError) Error() string {
	return e.msg
}
