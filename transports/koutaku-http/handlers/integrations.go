// Package handlers provides HTTP request handlers for the Koutaku HTTP transport.
// This file contains integration management handlers for AI provider integrations.
package handlers

import (
	"github.com/fasthttp/router"
	koutaku "github.com/koutaku/koutaku/core"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/koutaku/koutaku/transports/koutaku-http/integrations"
	"github.com/koutaku/koutaku/transports/koutaku-http/lib"
)

// IntegrationHandler manages HTTP requests for AI provider integrations
type IntegrationHandler struct {
	extensions            []integrations.ExtensionRouter
	wsResponses           *WSResponsesHandler
	wsRealtime            *WSRealtimeHandler
	webrtcRealtime        *WebRTCRealtimeHandler
	realtimeClientSecrets *RealtimeClientSecretsHandler
}

// NewIntegrationHandler creates a new integration handler instance.
// WebSocket handlers may be nil if WebSocket support is not configured.
func NewIntegrationHandler(client *koutaku.Koutaku, handlerStore lib.HandlerStore, wsResponses *WSResponsesHandler, wsRealtime *WSRealtimeHandler, webrtcRealtime *WebRTCRealtimeHandler, realtimeClientSecrets *RealtimeClientSecretsHandler) *IntegrationHandler {
	// Initialize all available integration routers
	extensions := []integrations.ExtensionRouter{
		integrations.NewOpenAIRouter(client, handlerStore, logger),
		integrations.NewAnthropicRouter(client, handlerStore, logger),
		integrations.NewGenAIRouter(client, handlerStore, logger),
		integrations.NewLiteLLMRouter(client, handlerStore, logger),
		integrations.NewCohereRouter(client, handlerStore, logger),
		integrations.NewLangChainRouter(client, handlerStore, logger),
		integrations.NewPydanticAIRouter(client, handlerStore, logger),
		integrations.NewBedrockRouter(client, handlerStore, logger),
		// passthrough routers
		integrations.NewGenAIPassthroughRouter(client, handlerStore, logger),
		integrations.NewOpenAIPassthroughRouter(client, handlerStore, logger),
		integrations.NewAnthropicPassthroughRouter(client, handlerStore, logger),
		integrations.NewAzurePassthroughRouter(client, handlerStore, logger),
		integrations.NewCursorRouter(client, handlerStore, logger),
	}

	return &IntegrationHandler{
		extensions:            extensions,
		wsResponses:           wsResponses,
		wsRealtime:            wsRealtime,
		webrtcRealtime:        webrtcRealtime,
		realtimeClientSecrets: realtimeClientSecrets,
	}
}

// RegisterRoutes registers all integration routes for AI provider compatibility endpoints
func (h *IntegrationHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.KoutakuHTTPMiddleware) {
	// Register routes for each integration extension
	for _, extension := range h.extensions {
		extension.RegisterRoutes(r, middlewares...)
	}
	// Register WebSocket routes (base path + integration paths)
	if h.wsResponses != nil {
		h.wsResponses.RegisterRoutes(r, middlewares...)
	}
	if h.wsRealtime != nil {
		h.wsRealtime.RegisterRoutes(r, middlewares...)
	}
	if h.webrtcRealtime != nil {
		h.webrtcRealtime.RegisterRoutes(r, middlewares...)
	}
	if h.realtimeClientSecrets != nil {
		h.realtimeClientSecrets.RegisterRoutes(r, middlewares...)
	}
}

func (h *IntegrationHandler) Close() {
	if h == nil {
		return
	}
	if h.wsResponses != nil {
		h.wsResponses.Close()
	}
	if h.wsRealtime != nil {
		h.wsRealtime.Close()
	}
	if h.webrtcRealtime != nil {
		h.webrtcRealtime.Close()
	}
}

// SetLargePayloadHook sets the large payload detection hook on all integration routers
// that support it. This is used by enterprise to inject large payload optimization.
func (h *IntegrationHandler) SetLargePayloadHook(hook integrations.LargePayloadHook) {
	for _, extension := range h.extensions {
		if setter, ok := extension.(interface {
			SetLargePayloadHook(integrations.LargePayloadHook)
		}); ok {
			setter.SetLargePayloadHook(hook)
		}
	}
}

// SetLargeResponseHook sets the large response scanning hook on all integration routers
// that support it. Enterprise uses this to inject Phase B usage extraction into the
// response stream without embedding scanning logic in the OSS router.
func (h *IntegrationHandler) SetLargeResponseHook(hook integrations.LargeResponseHook) {
	for _, extension := range h.extensions {
		if setter, ok := extension.(interface {
			SetLargeResponseHook(integrations.LargeResponseHook)
		}); ok {
			setter.SetLargeResponseHook(hook)
		}
	}
}
