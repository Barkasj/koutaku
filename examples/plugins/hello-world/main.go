package main

import (
	"fmt"

	"github.com/koutaku/koutaku/core/schemas"
)

const (
	transportPreHookKey  schemas.KoutakuContextKey = "hello-world-plugin-transport-pre-hook"
	transportPostHookKey schemas.KoutakuContextKey = "hello-world-plugin-transport-post-hook"
	preHookKey           schemas.KoutakuContextKey = "hello-world-plugin-pre-hook"
)

func Init(config any) error {
	fmt.Println("Init called")
	return nil
}

// GetName returns the name of the plugin (required)
// This is the system identifier - not editable by users
// Users can set a custom display_name in the config for the UI
func GetName() string {
	return "hello-world"
}

func HTTPTransportPreHook(ctx *schemas.KoutakuContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	fmt.Println("HTTPTransportPreHook called")
	// Modify request in-place
	req.Headers["x-hello-world-plugin"] = "transport-pre-hook-value"
	// Store value in context for PreLLMHook/PostLLMHook
	ctx.SetValue(transportPreHookKey, "transport-pre-hook-value")
	// Return nil to continue processing, or return &schemas.HTTPResponse{} to short-circuit
	ctx.Log(schemas.LogLevelInfo, "HTTPTransportPreHook called")
	return nil, nil
}

func HTTPTransportPostHook(ctx *schemas.KoutakuContext, req *schemas.HTTPRequest, resp *schemas.HTTPResponse) error {
	fmt.Println("HTTPTransportPostHook called")
	// Modify response in-place
	resp.Headers["x-hello-world-plugin"] = "transport-post-hook-value"
	// Store value in context
	ctx.Log(schemas.LogLevelInfo, "HTTPTransportPostHook called")
	ctx.SetValue(transportPostHookKey, "transport-post-hook-value")
	// Return nil to continue processing
	return nil
}

func HTTPTransportStreamChunkHook(ctx *schemas.KoutakuContext, req *schemas.HTTPRequest, chunk *schemas.KoutakuStreamChunk) (*schemas.KoutakuStreamChunk, error) {
	fmt.Println("HTTPTransportStreamChunkHook called")
	// Modify chunk in-place
	ctx.Log(schemas.LogLevelInfo, "HTTPTransportStreamChunkHook called")
	if chunk.KoutakuChatResponse != nil && chunk.KoutakuChatResponse.Choices != nil && len(chunk.KoutakuChatResponse.Choices) > 0 && chunk.KoutakuChatResponse.Choices[0].ChatStreamResponseChoice != nil && chunk.KoutakuChatResponse.Choices[0].ChatStreamResponseChoice.Delta != nil && chunk.KoutakuChatResponse.Choices[0].ChatStreamResponseChoice.Delta.Content != nil {
		*chunk.KoutakuChatResponse.Choices[0].ChatStreamResponseChoice.Delta.Content += " - modified by hello-world-plugin"
	}
	// Return the modified chunk
	return chunk, nil
}

func PreLLMHook(ctx *schemas.KoutakuContext, req *schemas.KoutakuRequest) (*schemas.KoutakuRequest, *schemas.LLMPluginShortCircuit, error) {
	value1 := ctx.Value(transportPreHookKey)
	fmt.Println("value1:", value1)
	ctx.SetValue(preHookKey, "pre-hook-value")
	ctx.Log(schemas.LogLevelInfo, "PreLLMHook called")
	fmt.Println("PreLLMHook called")
	return req, nil, nil
}

func PostLLMHook(ctx *schemas.KoutakuContext, resp *schemas.KoutakuResponse, koutakuErr *schemas.KoutakuError) (*schemas.KoutakuResponse, *schemas.KoutakuError, error) {
	fmt.Println("PostLLMHook called")
	value1 := ctx.Value(transportPreHookKey)
	fmt.Println("value1:", value1)
	value2 := ctx.Value(preHookKey)
	fmt.Println("value2:", value2)
	ctx.Log(schemas.LogLevelInfo, "PostLLMHook called")
	return resp, koutakuErr, nil
}

func Cleanup() error {
	fmt.Println("Cleanup called")
	return nil
}
