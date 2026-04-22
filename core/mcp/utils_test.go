package mcp

import (
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConvertMCPToolToKoutakuSchema_EmptyParameters tests that tools with no parameters
// get an empty properties map instead of nil, which is required by some providers like OpenAI
func TestConvertMCPToolToKoutakuSchema_EmptyParameters(t *testing.T) {
	// Create a tool with no parameters (like return_special_chars or return_null)
	mcpTool := &mcp.Tool{
		Name:        "test_tool_no_params",
		Description: "A test tool with no parameters",
		InputSchema: mcp.ToolInputSchema{
			Type:       "object",
			Properties: map[string]interface{}{}, // Empty properties
			Required:   []string{},
		},
	}

	// Convert the tool
	koutakuTool := convertMCPToolToKoutakuSchema(mcpTool, defaultLogger)

	// Verify the function was created
	if koutakuTool.Function == nil {
		t.Fatal("Function should not be nil")
	}

	// Verify parameters were created
	if koutakuTool.Function.Parameters == nil {
		t.Fatal("Parameters should not be nil")
	}

	// Verify properties is not nil (this is the key fix)
	if koutakuTool.Function.Parameters.Properties == nil {
		t.Error("Properties should not be nil for object type, even if empty")
	}

	// Verify it's an empty map
	if koutakuTool.Function.Parameters.Properties != nil && koutakuTool.Function.Parameters.Properties.Len() != 0 {
		t.Errorf("Expected empty properties map, got %d properties", koutakuTool.Function.Parameters.Properties.Len())
	}

	// Verify the type is preserved
	if koutakuTool.Function.Parameters.Type != "object" {
		t.Errorf("Expected type 'object', got '%s'", koutakuTool.Function.Parameters.Type)
	}
}

// TestConvertMCPToolToKoutakuSchema_WithAnnotations tests that MCP tool annotations
// are preserved on ChatTool.Annotations (not ChatToolFunction) and are absent from JSON.
func TestConvertMCPToolToKoutakuSchema_WithAnnotations(t *testing.T) {
	readOnly := true
	destructive := false

	mcpTool := &mcp.Tool{
		Name:        "read_resource",
		Description: "Reads a resource",
		InputSchema: mcp.ToolInputSchema{
			Type:       "object",
			Properties: map[string]interface{}{},
		},
		Annotations: mcp.ToolAnnotation{
			Title:           "Resource Reader",
			ReadOnlyHint:    &readOnly,
			DestructiveHint: &destructive,
			IdempotentHint:  schemas.Ptr(true),
		},
	}

	koutakuTool := convertMCPToolToKoutakuSchema(mcpTool, defaultLogger)

	// Annotations must be on ChatTool, not buried in Function
	require.NotNil(t, koutakuTool.Annotations, "Annotations should be set on ChatTool")
	assert.Equal(t, "Resource Reader", koutakuTool.Annotations.Title)
	require.NotNil(t, koutakuTool.Annotations.ReadOnlyHint)
	assert.True(t, *koutakuTool.Annotations.ReadOnlyHint)
	require.NotNil(t, koutakuTool.Annotations.DestructiveHint)
	assert.False(t, *koutakuTool.Annotations.DestructiveHint)
	require.NotNil(t, koutakuTool.Annotations.IdempotentHint)
	assert.True(t, *koutakuTool.Annotations.IdempotentHint)
	assert.Nil(t, koutakuTool.Annotations.OpenWorldHint)

	// The JSON sent to providers must not contain annotations
	toolJSON, err := json.Marshal(koutakuTool)
	require.NoError(t, err)
	s := string(toolJSON)
	assert.NotContains(t, s, "annotations", "annotations must be absent from provider JSON")
	assert.NotContains(t, s, "readOnlyHint", "readOnlyHint must be absent from provider JSON")
	assert.NotContains(t, s, "Resource Reader", "annotation title must be absent from provider JSON")
}

// TestConvertMCPToolToKoutakuSchema_NilAnnotationsWhenAllZero verifies the nil guard:
// when all annotation fields are zero-valued, ChatTool.Annotations must remain nil.
func TestConvertMCPToolToKoutakuSchema_NilAnnotationsWhenAllZero(t *testing.T) {
	mcpTool := &mcp.Tool{
		Name:        "no_hints_tool",
		Description: "A tool with no annotation hints",
		InputSchema: mcp.ToolInputSchema{
			Type:       "object",
			Properties: map[string]interface{}{},
		},
		Annotations: mcp.ToolAnnotation{}, // All zero values — Title empty, all hints nil
	}

	koutakuTool := convertMCPToolToKoutakuSchema(mcpTool, defaultLogger)

	assert.Nil(t, koutakuTool.Annotations,
		"Annotations should be nil when all MCP annotation fields are zero")
}

// TestConvertMCPToolToKoutakuSchema_WithParameters tests the normal case with parameters
func TestConvertMCPToolToKoutakuSchema_WithParameters(t *testing.T) {
	// Create a tool with parameters
	mcpTool := &mcp.Tool{
		Name:        "test_tool_with_params",
		Description: "A test tool with parameters",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]interface{}{
				"param1": map[string]interface{}{
					"type":        "string",
					"description": "A string parameter",
				},
				"param2": map[string]interface{}{
					"type":        "number",
					"description": "A number parameter",
				},
			},
			Required: []string{"param1"},
		},
	}

	// Convert the tool
	koutakuTool := convertMCPToolToKoutakuSchema(mcpTool, defaultLogger)

	// Verify the function was created
	if koutakuTool.Function == nil {
		t.Fatal("Function should not be nil")
	}

	// Verify parameters were created
	if koutakuTool.Function.Parameters == nil {
		t.Fatal("Parameters should not be nil")
	}

	// Verify properties is not nil
	if koutakuTool.Function.Parameters.Properties == nil {
		t.Fatal("Properties should not be nil")
	}

	// Verify the correct number of properties
	if koutakuTool.Function.Parameters.Properties.Len() != 2 {
		t.Errorf("Expected 2 properties, got %d", koutakuTool.Function.Parameters.Properties.Len())
	}

	// Verify required fields
	if len(koutakuTool.Function.Parameters.Required) != 1 {
		t.Errorf("Expected 1 required field, got %d", len(koutakuTool.Function.Parameters.Required))
	}

	if koutakuTool.Function.Parameters.Required[0] != "param1" {
		t.Errorf("Expected required field 'param1', got '%s'", koutakuTool.Function.Parameters.Required[0])
	}
}
