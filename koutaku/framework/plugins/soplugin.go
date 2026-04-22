package plugins

import (
	"context"
	"plugin"

	"github.com/koutaku/koutaku/core/schemas"
)

// DynamicPlugin is a generic dynamic plugin that can implement any combination of plugin interfaces
// It uses optional function pointers - nil pointers indicate the interface is not implemented
type DynamicPlugin struct {
	Enabled bool
	Path    string
	Config  any

	filename string
	plugin   *plugin.Plugin

	// BasePlugin (required)
	getName func() string
	cleanup func() error

	// HTTPTransportPlugin (optional)
	httpTransportPreHook         func(ctx *schemas.KoutakuContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error)
	httpTransportPostHook        func(ctx *schemas.KoutakuContext, req *schemas.HTTPRequest, resp *schemas.HTTPResponse) error
	httpTransportStreamChunkHook func(ctx *schemas.KoutakuContext, req *schemas.HTTPRequest, stream *schemas.KoutakuStreamChunk) (*schemas.KoutakuStreamChunk, error)

	// LLMPlugin (optional)
	preLLMHook  func(ctx *schemas.KoutakuContext, req *schemas.KoutakuRequest) (*schemas.KoutakuRequest, *schemas.LLMPluginShortCircuit, error)
	postLLMHook func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuResponse, koutakuErr *schemas.KoutakuError) (*schemas.KoutakuResponse, *schemas.KoutakuError, error)

	// MCPPlugin (optional)
	preMCPHook  func(ctx *schemas.KoutakuContext, req *schemas.KoutakuMCPRequest) (*schemas.KoutakuMCPRequest, *schemas.MCPPluginShortCircuit, error)
	postMCPHook func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuMCPResponse, koutakuErr *schemas.KoutakuError) (*schemas.KoutakuMCPResponse, *schemas.KoutakuError, error)

	// ObservabilityPlugin (optional)
	inject func(ctx context.Context, trace *schemas.Trace) error
}

// GetName returns the name of the plugin (BasePlugin interface)
func (dp *DynamicPlugin) GetName() string {
	return dp.getName()
}

// Cleanup is invoked by core/koutaku.go during plugin unload, reload, and shutdown (BasePlugin interface)
func (dp *DynamicPlugin) Cleanup() error {
	return dp.cleanup()
}

// HTTPTransportPreHook intercepts HTTP requests at the transport layer before entering Koutaku core (HTTPTransportPlugin interface)
func (dp *DynamicPlugin) HTTPTransportPreHook(ctx *schemas.KoutakuContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	if dp.httpTransportPreHook == nil {
		return nil, nil // No-op if not implemented
	}
	return dp.httpTransportPreHook(ctx, req)
}

// HTTPTransportPostHook intercepts HTTP responses at the transport layer after exiting Koutaku core (HTTPTransportPlugin interface)
func (dp *DynamicPlugin) HTTPTransportPostHook(ctx *schemas.KoutakuContext, req *schemas.HTTPRequest, resp *schemas.HTTPResponse) error {
	if dp.httpTransportPostHook == nil {
		return nil // No-op if not implemented
	}
	return dp.httpTransportPostHook(ctx, req, resp)
}

// HTTPTransportStreamChunkHook intercepts streaming chunks before they are written to the client
func (dp *DynamicPlugin) HTTPTransportStreamChunkHook(ctx *schemas.KoutakuContext, req *schemas.HTTPRequest, stream *schemas.KoutakuStreamChunk) (*schemas.KoutakuStreamChunk, error) {
	if dp.httpTransportStreamChunkHook == nil {
		return stream, nil // No-op if not implemented
	}
	return dp.httpTransportStreamChunkHook(ctx, req, stream)
}

// PreLLMHook is invoked before LLM provider calls (LLMPlugin interface)
func (dp *DynamicPlugin) PreLLMHook(ctx *schemas.KoutakuContext, req *schemas.KoutakuRequest) (*schemas.KoutakuRequest, *schemas.LLMPluginShortCircuit, error) {
	if dp.preLLMHook == nil {
		return req, nil, nil // No-op if not implemented
	}
	return dp.preLLMHook(ctx, req)
}

// PostLLMHook is invoked after LLM provider calls (LLMPlugin interface)
func (dp *DynamicPlugin) PostLLMHook(ctx *schemas.KoutakuContext, resp *schemas.KoutakuResponse, koutakuErr *schemas.KoutakuError) (*schemas.KoutakuResponse, *schemas.KoutakuError, error) {
	if dp.postLLMHook == nil {
		return resp, koutakuErr, nil // No-op if not implemented
	}
	return dp.postLLMHook(ctx, resp, koutakuErr)
}

// PreMCPHook is invoked before MCP calls (MCPPlugin interface)
func (dp *DynamicPlugin) PreMCPHook(ctx *schemas.KoutakuContext, req *schemas.KoutakuMCPRequest) (*schemas.KoutakuMCPRequest, *schemas.MCPPluginShortCircuit, error) {
	if dp.preMCPHook == nil {
		return req, nil, nil // No-op if not implemented
	}
	return dp.preMCPHook(ctx, req)
}

// PostMCPHook is invoked after MCP calls (MCPPlugin interface)
func (dp *DynamicPlugin) PostMCPHook(ctx *schemas.KoutakuContext, resp *schemas.KoutakuMCPResponse, koutakuErr *schemas.KoutakuError) (*schemas.KoutakuMCPResponse, *schemas.KoutakuError, error) {
	if dp.postMCPHook == nil {
		return resp, koutakuErr, nil // No-op if not implemented
	}
	return dp.postMCPHook(ctx, resp, koutakuErr)
}

// Inject receives completed traces for observability backends (ObservabilityPlugin interface)
func (dp *DynamicPlugin) Inject(ctx context.Context, trace *schemas.Trace) error {
	if dp.inject == nil {
		return nil // No-op if not implemented
	}
	return dp.inject(ctx, trace)
}
