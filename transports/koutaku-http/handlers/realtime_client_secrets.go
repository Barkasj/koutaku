package handlers

import (
	"encoding/json"
	"fmt"
	"mime"
	"strings"
	"time"

	"github.com/fasthttp/router"
	koutaku "github.com/koutaku/koutaku/core"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/koutaku/koutaku/plugins/governance"
	"github.com/koutaku/koutaku/transports/koutaku-http/integrations"
	"github.com/koutaku/koutaku/transports/koutaku-http/lib"
	"github.com/valyala/fasthttp"
)

// RealtimeClientSecretsHandler exposes OpenAI-compatible HTTP routes for
// minting short-lived Realtime client secrets.
type RealtimeClientSecretsHandler struct {
	client       *koutaku.Koutaku
	config       *lib.Config
	handlerStore lib.HandlerStore
	routeSpecs   map[string]schemas.RealtimeSessionRoute
}

func NewRealtimeClientSecretsHandler(client *koutaku.Koutaku, config *lib.Config) *RealtimeClientSecretsHandler {
	return &RealtimeClientSecretsHandler{
		client:       client,
		config:       config,
		handlerStore: config,
		routeSpecs:   make(map[string]schemas.RealtimeSessionRoute),
	}
}

func (h *RealtimeClientSecretsHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.KoutakuHTTPMiddleware) {
	handler := lib.ChainMiddlewares(h.handleRequest, middlewares...)
	for _, route := range h.realtimeSessionRoutes() {
		h.routeSpecs[route.Path] = route
		r.POST(route.Path, handler)
	}
}

func (h *RealtimeClientSecretsHandler) findGovernancePlugin() governance.BaseGovernancePlugin {
	basePlugins := h.config.BasePlugins.Load()
	if basePlugins == nil {
		return nil
	}

	for _, plugin := range *basePlugins {
		if governancePlugin, ok := plugin.(governance.BaseGovernancePlugin); ok {
			return governancePlugin
		}
	}

	return nil
}

func (h *RealtimeClientSecretsHandler) handleRequest(ctx *fasthttp.RequestCtx) {
	if !isJSONContentType(string(ctx.Request.Header.ContentType())) {
		SendKoutakuError(ctx, newRealtimeClientSecretHandlerError(
			fasthttp.StatusBadRequest,
			"invalid_request_error",
			"Content-Type must be application/json",
			nil,
		))
		return
	}

	body := append([]byte(nil), ctx.Request.Body()...)
	route, ok := h.routeSpecs[string(ctx.Path())]
	if !ok {
		SendKoutakuError(ctx, newRealtimeClientSecretHandlerError(
			fasthttp.StatusNotFound,
			"invalid_request_error",
			"unsupported realtime client secret route",
			nil,
		))
		return
	}

	providerKey, model, normalizedBody, err := resolveRealtimeClientSecretTarget(route, body)
	if err != nil {
		SendKoutakuError(ctx, err)
		return
	}

	koutakuCtx, cancel := lib.ConvertToKoutakuContext(
		ctx,
		h.handlerStore.ShouldAllowDirectKeys(),
		h.config.GetHeaderMatcher(),
		h.config.GetMCPHeaderCombinedAllowlist(),
	)
	defer cancel()
	koutakuCtx.SetValue(schemas.KoutakuContextKeyHTTPRequestType, schemas.RealtimeRequest)
	if route.DefaultProvider == schemas.OpenAI {
		koutakuCtx.SetValue(schemas.KoutakuContextKeyIntegrationType, "openai")
	}
	if governanceUserID, ok := ctx.UserValue(schemas.KoutakuContextKeyUserID).(string); ok && governanceUserID != "" {
		koutakuCtx.SetValue(schemas.KoutakuContextKeyUserID, governanceUserID)
	}
	if userName, ok := ctx.UserValue(schemas.KoutakuContextKeyUserName).(string); ok && userName != "" {
		koutakuCtx.SetValue(schemas.KoutakuContextKeyUserName, userName)
	}
	if koutakuErr := h.evaluateMintingGovernance(koutakuCtx, providerKey, model); koutakuErr != nil {
		SendKoutakuError(ctx, koutakuErr)
		return
	}

	provider := h.client.GetProviderByKey(providerKey)
	if provider == nil {
		SendKoutakuError(ctx, newRealtimeClientSecretHandlerError(
			fasthttp.StatusBadRequest,
			"invalid_request_error",
			"provider not found: "+string(providerKey),
			nil,
		))
		return
	}

	key, keyErr := h.client.SelectKeyForProviderRequestType(koutakuCtx, schemas.RealtimeRequest, providerKey, model)
	if keyErr != nil {
		SendKoutakuError(ctx, newRealtimeClientSecretHandlerError(
			fasthttp.StatusBadRequest,
			"invalid_request_error",
			keyErr.Error(),
			keyErr,
		))
		return
	}

	// Resolve model aliases now that the key is selected so the forwarded body
	// carries the provider's canonical model, matching wsrealtime/webrtc flows.
	if resolved := key.Aliases.Resolve(model); resolved != "" && resolved != model {
		model = resolved
		reparsed, parseErr := schemas.ParseRealtimeClientSecretBody(normalizedBody)
		if parseErr != nil {
			SendKoutakuError(ctx, parseErr)
			return
		}
		rewritten, normalizeErr := normalizeRealtimeClientSecretBody(reparsed, model)
		if normalizeErr != nil {
			SendKoutakuError(ctx, normalizeErr)
			return
		}
		normalizedBody = rewritten
	}

	sessionProvider, ok := provider.(schemas.RealtimeSessionProvider)
	if !ok {
		SendKoutakuError(ctx, realtimeSessionNotSupportedError(providerKey, provider))
		return
	}

	resp, koutakuErr := sessionProvider.CreateRealtimeClientSecret(koutakuCtx, key, route.EndpointType, normalizedBody)
	if koutakuErr != nil {
		SendKoutakuError(ctx, koutakuErr)
		return
	}
	cacheRealtimeEphemeralKeyMapping(
		h.handlerStore.GetKVStore(),
		resp.Body,
		key.ID,
		koutaku.GetStringFromContext(koutakuCtx, schemas.KoutakuContextKeyVirtualKey),
	)

	writeRealtimeClientSecretResponse(ctx, resp)
}

func (h *RealtimeClientSecretsHandler) evaluateMintingGovernance(
	koutakuCtx *schemas.KoutakuContext,
	providerKey schemas.ModelProvider,
	model string,
) *schemas.KoutakuError {
	governancePlugin := h.findGovernancePlugin()
	if governancePlugin == nil {
		return nil
	}

	_, koutakuErr := governancePlugin.EvaluateGovernanceRequest(koutakuCtx, &governance.EvaluationRequest{
		VirtualKey: koutaku.GetStringFromContext(koutakuCtx, schemas.KoutakuContextKeyVirtualKey),
		Provider:   providerKey,
		Model:      model,
		UserID:     koutaku.GetStringFromContext(koutakuCtx, schemas.KoutakuContextKeyUserID),
	}, schemas.RealtimeRequest)
	return koutakuErr
}

func (h *RealtimeClientSecretsHandler) realtimeSessionRoutes() []schemas.RealtimeSessionRoute {
	routes := []schemas.RealtimeSessionRoute{
		{
			Path:         "/v1/realtime/client_secrets",
			EndpointType: schemas.RealtimeSessionEndpointClientSecrets,
		},
		{
			Path:         "/v1/realtime/sessions",
			EndpointType: schemas.RealtimeSessionEndpointSessions,
		},
	}

	for _, path := range integrations.OpenAIRealtimeClientSecretPaths("/openai") {
		endpointType := schemas.RealtimeSessionEndpointClientSecrets
		if strings.HasSuffix(path, "/realtime/sessions") {
			endpointType = schemas.RealtimeSessionEndpointSessions
		}
		routes = append(routes, schemas.RealtimeSessionRoute{
			Path:            path,
			EndpointType:    endpointType,
			DefaultProvider: schemas.OpenAI,
		})
	}
	return routes
}

func resolveRealtimeClientSecretTarget(route schemas.RealtimeSessionRoute, body []byte) (schemas.ModelProvider, string, []byte, *schemas.KoutakuError) {
	root, err := schemas.ParseRealtimeClientSecretBody(body)
	if err != nil {
		return "", "", nil, err
	}

	rawModel, err := schemas.ExtractRealtimeClientSecretModel(root)
	if err != nil {
		return "", "", nil, err
	}

	defaultProvider := route.DefaultProvider
	providerKey, model := schemas.ParseModelString(rawModel, defaultProvider)
	if defaultProvider == "" && providerKey == "" {
		return "", "", nil, newRealtimeClientSecretHandlerError(
			fasthttp.StatusBadRequest,
			"invalid_request_error",
			"session.model must use provider/model on /v1 realtime client secret routes",
			nil,
		)
	}
	if providerKey == "" || model == "" {
		return "", "", nil, newRealtimeClientSecretHandlerError(
			fasthttp.StatusBadRequest,
			"invalid_request_error",
			"session.model is required",
			nil,
		)
	}

	// Normalize the forwarded body so the upstream provider sees the bare model
	// (strip provider prefix). Mirrors resolveRealtimeSDPTarget normalization.
	normalizedBody, normalizeErr := normalizeRealtimeClientSecretBody(root, model)
	if normalizeErr != nil {
		return "", "", nil, normalizeErr
	}

	return providerKey, model, normalizedBody, nil
}

func normalizeRealtimeClientSecretBody(root map[string]json.RawMessage, bareModel string) ([]byte, *schemas.KoutakuError) {
	normalizedModel, marshalErr := json.Marshal(bareModel)
	if marshalErr != nil {
		return nil, newRealtimeClientSecretHandlerError(fasthttp.StatusInternalServerError, "server_error", "failed to encode normalized model", marshalErr)
	}

	// Normalize session.model if present
	if sessionJSON, ok := root["session"]; ok && len(sessionJSON) > 0 {
		var session map[string]json.RawMessage
		if err := json.Unmarshal(sessionJSON, &session); err == nil {
			if _, hasModel := session["model"]; hasModel {
				session["model"] = normalizedModel
				rewritten, err := json.Marshal(session)
				if err != nil {
					return nil, newRealtimeClientSecretHandlerError(fasthttp.StatusInternalServerError, "server_error", "failed to re-encode session", err)
				}
				root["session"] = rewritten
			}
		}
	}
	// Normalize top-level model if present
	if _, ok := root["model"]; ok {
		root["model"] = normalizedModel
	}

	normalized, marshalErr := json.Marshal(root)
	if marshalErr != nil {
		return nil, newRealtimeClientSecretHandlerError(fasthttp.StatusInternalServerError, "server_error", "failed to re-encode body", marshalErr)
	}
	return normalized, nil
}

const realtimeEphemeralKeyMappingPrefix = "realtime:ephemeral-key:"

type realtimeEphemeralKeyMapping struct {
	KeyID      string `json:"key_id,omitempty"`
	VirtualKey string `json:"virtual_key,omitempty"`
}

func cacheRealtimeEphemeralKeyMapping(kv schemas.KVStore, body []byte, keyID string, virtualKey string) {
	if kv == nil || len(body) == 0 || strings.TrimSpace(keyID) == "" {
		return
	}

	token, ttl, ok := parseRealtimeEphemeralKeyMapping(body)
	if !ok || strings.TrimSpace(token) == "" || ttl <= 0 {
		return
	}

	payload, err := json.Marshal(realtimeEphemeralKeyMapping{
		KeyID:      strings.TrimSpace(keyID),
		VirtualKey: strings.TrimSpace(virtualKey),
	})
	if err != nil {
		logger.Warn("failed to encode realtime ephemeral key mapping for key_id=%s: %v", keyID, err)
		return
	}

	if err := kv.SetWithTTL(buildRealtimeEphemeralKeyMappingKey(token), payload, ttl); err != nil {
		logger.Warn("failed to cache realtime ephemeral key mapping for key_id=%s: %v", keyID, err)
	}
}

func parseRealtimeEphemeralKeyMapping(body []byte) (string, time.Duration, bool) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return "", 0, false
	}

	var clientSecret struct {
		Value     string `json:"value"`
		ExpiresAt int64  `json:"expires_at"`
	}

	// OpenAI client_secrets responses expose the ephemeral token at the top level.
	// Keep accepting the nested shape too so the mapping logic stays compatible
	// with any provider/session endpoint variants that wrap the secret object.
	if err := json.Unmarshal(body, &clientSecret); err != nil || strings.TrimSpace(clientSecret.Value) == "" || clientSecret.ExpiresAt <= 0 {
		clientSecretRaw, ok := root["client_secret"]
		if !ok || len(clientSecretRaw) == 0 || string(clientSecretRaw) == "null" {
			return "", 0, false
		}
		if err := json.Unmarshal(clientSecretRaw, &clientSecret); err != nil {
			return "", 0, false
		}
	}
	if strings.TrimSpace(clientSecret.Value) == "" || clientSecret.ExpiresAt <= 0 {
		return "", 0, false
	}

	ttl := time.Until(time.Unix(clientSecret.ExpiresAt, 0))
	if ttl <= 0 {
		return "", 0, false
	}

	return clientSecret.Value, ttl, true
}

func buildRealtimeEphemeralKeyMappingKey(token string) string {
	return realtimeEphemeralKeyMappingPrefix + strings.TrimSpace(token)
}

func realtimeSessionNotSupportedError(providerKey schemas.ModelProvider, provider schemas.Provider) *schemas.KoutakuError {
	if rtProvider, ok := provider.(schemas.RealtimeProvider); ok && rtProvider.SupportsRealtimeAPI() {
		return newRealtimeClientSecretHandlerError(
			fasthttp.StatusBadRequest,
			"invalid_request_error",
			fmt.Sprintf("provider %s supports realtime websocket connections but not realtime client secret creation", providerKey),
			nil,
		)
	}

	return newRealtimeClientSecretHandlerError(
		fasthttp.StatusBadRequest,
		"invalid_request_error",
		fmt.Sprintf("provider %s does not support realtime client secret creation", providerKey),
		nil,
	)
}

func newRealtimeClientSecretHandlerError(status int, errorType, message string, err error) *schemas.KoutakuError {
	return &schemas.KoutakuError{
		IsKoutakuError: false,
		StatusCode:     schemas.Ptr(status),
		Error: &schemas.ErrorField{
			Type:    schemas.Ptr(errorType),
			Message: message,
			Error:   err,
		},
		ExtraFields: schemas.KoutakuErrorExtraFields{
			RequestType: schemas.RealtimeRequest,
		},
	}
}

func writeRealtimeClientSecretResponse(ctx *fasthttp.RequestCtx, resp *schemas.KoutakuPassthroughResponse) {
	if resp == nil {
		SendKoutakuError(ctx, newRealtimeClientSecretHandlerError(
			fasthttp.StatusInternalServerError,
			"server_error",
			"provider returned an empty realtime client secret response",
			nil,
		))
		return
	}

	for key, value := range resp.Headers {
		ctx.Response.Header.Set(key, value)
	}
	if len(ctx.Response.Header.ContentType()) == 0 {
		ctx.SetContentType("application/json")
	}
	ctx.SetStatusCode(resp.StatusCode)
	ctx.SetBody(resp.Body)
}

func isJSONContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}
