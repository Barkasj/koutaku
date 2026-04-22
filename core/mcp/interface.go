//go:build !tinygo && !wasm

package mcp

import (
	"context"

	"github.com/koutaku/koutaku/core/schemas"
)

// MCPManagerInterface defines the interface for MCP management functionality.
// This interface allows different implementations (OSS and Enterprise) to be used
// interchangeably in the Koutaku core.
type MCPManagerInterface interface {
	// Tool Operations
	// AddToolsToRequest parses available MCP tools and adds them to the request
	AddToolsToRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuRequest) *schemas.KoutakuRequest

	// GetAvailableTools returns all available MCP tools for the given context
	GetAvailableTools(ctx *schemas.KoutakuContext) []schemas.ChatTool

	// ExecuteToolCall executes a single tool call and returns the result
	ExecuteToolCall(ctx *schemas.KoutakuContext, request *schemas.KoutakuMCPRequest) (*schemas.KoutakuMCPResponse, error)

	// UpdateToolManagerConfig updates the configuration for the tool manager.
	// DisableAutoToolInject in the config controls auto injection — pass the
	// current value whenever only other fields change so it is never silently reset.
	UpdateToolManagerConfig(config *schemas.MCPToolManagerConfig)

	// Agent Mode Operations
	// CheckAndExecuteAgentForChatRequest handles agent mode for Chat Completions API
	CheckAndExecuteAgentForChatRequest(
		ctx *schemas.KoutakuContext,
		req *schemas.KoutakuChatRequest,
		response *schemas.KoutakuChatResponse,
		makeReq func(ctx *schemas.KoutakuContext, req *schemas.KoutakuChatRequest) (*schemas.KoutakuChatResponse, *schemas.KoutakuError),
		executeTool func(ctx *schemas.KoutakuContext, request *schemas.KoutakuMCPRequest) (*schemas.KoutakuMCPResponse, error),
	) (*schemas.KoutakuChatResponse, *schemas.KoutakuError)

	// CheckAndExecuteAgentForResponsesRequest handles agent mode for Responses API
	CheckAndExecuteAgentForResponsesRequest(
		ctx *schemas.KoutakuContext,
		req *schemas.KoutakuResponsesRequest,
		response *schemas.KoutakuResponsesResponse,
		makeReq func(ctx *schemas.KoutakuContext, req *schemas.KoutakuResponsesRequest) (*schemas.KoutakuResponsesResponse, *schemas.KoutakuError),
		executeTool func(ctx *schemas.KoutakuContext, request *schemas.KoutakuMCPRequest) (*schemas.KoutakuMCPResponse, error),
	) (*schemas.KoutakuResponsesResponse, *schemas.KoutakuError)

	// Client Management
	// GetClients returns all MCP clients
	GetClients() []schemas.MCPClientState

	// AddClient adds a new MCP client with the given configuration
	AddClient(config *schemas.MCPClientConfig) error

	// RemoveClient removes an MCP client by ID
	RemoveClient(id string) error

	// UpdateClient updates an existing MCP client configuration
	UpdateClient(id string, updatedConfig *schemas.MCPClientConfig) error

	// ReconnectClient reconnects an MCP client by ID
	ReconnectClient(id string) error

	// VerifyPerUserOAuthConnection creates a temporary MCP connection using a
	// test access token to verify connectivity and discover tools. The connection
	// is closed after verification.
	VerifyPerUserOAuthConnection(ctx context.Context, config *schemas.MCPClientConfig, accessToken string) (map[string]schemas.ChatTool, map[string]string, error)

	// SetClientTools updates the tool map and name mapping for an existing client.
	SetClientTools(clientID string, tools map[string]schemas.ChatTool, toolNameMapping map[string]string)

	// Tool Registration
	// RegisterTool registers a local tool with the MCP server
	RegisterTool(name, description string, toolFunction MCPToolFunction[any], toolSchema schemas.ChatTool) error

	// Lifecycle
	// Cleanup performs cleanup of all MCP resources
	Cleanup() error
}

// Ensure MCPManager implements MCPManagerInterface
var _ MCPManagerInterface = (*MCPManager)(nil)
