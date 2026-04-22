package handlers

import (
	"fmt"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/fasthttp/router"
	koutaku "github.com/koutaku/koutaku/core"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/koutaku/koutaku/transports/koutaku-http/lib"
	"github.com/valyala/fasthttp"
)

type MCPInferenceHandler struct {
	client *koutaku.Koutaku
	config *lib.Config
}

// NewMCPInferenceHandler creates a new MCP inference handler instance
func NewMCPInferenceHandler(client *koutaku.Koutaku, config *lib.Config) *MCPInferenceHandler {
	return &MCPInferenceHandler{
		client: client,
		config: config,
	}
}

// RegisterRoutes registers the MCP inference routes
func (h *MCPInferenceHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.KoutakuHTTPMiddleware) {
	r.POST("/v1/mcp/tool/execute", lib.ChainMiddlewares(h.executeTool, middlewares...))
}

// executeTool handles POST /v1/mcp/tool/execute - Execute MCP tool
func (h *MCPInferenceHandler) executeTool(ctx *fasthttp.RequestCtx) {
	// Check format query parameter
	format := strings.ToLower(string(ctx.QueryArgs().Peek("format")))
	switch format {
	case "chat", "":
		h.executeChatMCPTool(ctx)
	case "responses":
		h.executeResponsesMCPTool(ctx)
	default:
		SendError(ctx, fasthttp.StatusBadRequest, "Invalid format value, must be 'chat' or 'responses'")
		return
	}
}

// executeChatMCPTool handles POST /v1/mcp/tool/execute?format=chat - Execute MCP tool
func (h *MCPInferenceHandler) executeChatMCPTool(ctx *fasthttp.RequestCtx) {
	var req schemas.ChatAssistantMessageToolCall
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid request format: %v", err))
		return
	}

	// Validate required fields
	if req.Function.Name == nil || *req.Function.Name == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "Tool function name is required")
		return
	}

	// Convert context
	koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, false, h.config.GetHeaderMatcher(), h.config.GetMCPHeaderCombinedAllowlist())
	defer cancel() // Ensure cleanup on function exit
	if koutakuCtx == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
		return
	}

	// Execute MCP tool
	toolMessage, koutakuErr := h.client.ExecuteChatMCPTool(koutakuCtx, &req)
	if koutakuErr != nil {
		SendKoutakuError(ctx, koutakuErr)
		return
	}

	// Send successful response
	SendJSON(ctx, toolMessage)
}

// executeResponsesMCPTool handles POST /v1/mcp/tool/execute?format=responses - Execute MCP tool
func (h *MCPInferenceHandler) executeResponsesMCPTool(ctx *fasthttp.RequestCtx) {
	var req schemas.ResponsesToolMessage
	if err := sonic.Unmarshal(ctx.PostBody(), &req); err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Invalid request format: %v", err))
		return
	}

	// Validate required fields
	if req.Name == nil || *req.Name == "" {
		SendError(ctx, fasthttp.StatusBadRequest, "Tool function name is required")
		return
	}

	// Convert context
	koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, false, h.config.GetHeaderMatcher(), h.config.GetMCPHeaderCombinedAllowlist())
	defer cancel() // Ensure cleanup on function exit
	if koutakuCtx == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
		return
	}

	// Execute MCP tool
	toolMessage, koutakuErr := h.client.ExecuteResponsesMCPTool(koutakuCtx, &req)
	if koutakuErr != nil {
		SendKoutakuError(ctx, koutakuErr)
		return
	}

	// Send successful response
	SendJSON(ctx, toolMessage)
}
