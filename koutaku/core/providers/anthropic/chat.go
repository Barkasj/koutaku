package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// convertFunctionToolToAnthropic turns an OpenAI-style function tool
// (schemas.ChatTool with non-nil Function) into an AnthropicTool.
// Factored out from ToAnthropicChatRequest's tool loop so the loop can branch
// cleanly between function and server-tool shapes.
func convertFunctionToolToAnthropic(tool schemas.ChatTool) AnthropicTool {
	anthropicTool := AnthropicTool{
		Name: tool.Function.Name,
	}
	if tool.Function.Description != nil {
		anthropicTool.Description = tool.Function.Description
	}

	// Convert function parameters to input_schema
	if tool.Function.Parameters != nil && (tool.Function.Parameters.Type != "" || tool.Function.Parameters.Properties != nil) {
		anthropicTool.InputSchema = &schemas.ToolFunctionParameters{
			Type:                 tool.Function.Parameters.Type,
			Description:          tool.Function.Parameters.Description,
			Properties:           tool.Function.Parameters.Properties,
			Required:             tool.Function.Parameters.Required,
			Enum:                 tool.Function.Parameters.Enum,
			AdditionalProperties: tool.Function.Parameters.AdditionalProperties,
			Defs:                 tool.Function.Parameters.Defs,
			Definitions:          tool.Function.Parameters.Definitions,
			Ref:                  tool.Function.Parameters.Ref,
			Items:                tool.Function.Parameters.Items,
			MinItems:             tool.Function.Parameters.MinItems,
			MaxItems:             tool.Function.Parameters.MaxItems,
			AnyOf:                tool.Function.Parameters.AnyOf,
			OneOf:                tool.Function.Parameters.OneOf,
			AllOf:                tool.Function.Parameters.AllOf,
			Format:               tool.Function.Parameters.Format,
			Pattern:              tool.Function.Parameters.Pattern,
			MinLength:            tool.Function.Parameters.MinLength,
			MaxLength:            tool.Function.Parameters.MaxLength,
			Minimum:              tool.Function.Parameters.Minimum,
			Maximum:              tool.Function.Parameters.Maximum,
			Title:                tool.Function.Parameters.Title,
			Default:              tool.Function.Parameters.Default,
			Nullable:             tool.Function.Parameters.Nullable,
		}
	}

	if anthropicTool.InputSchema != nil {
		anthropicTool.InputSchema = anthropicTool.InputSchema.Normalized()
	}

	if tool.CacheControl != nil {
		anthropicTool.CacheControl = tool.CacheControl
	}
	if tool.DeferLoading != nil {
		anthropicTool.DeferLoading = tool.DeferLoading
	}
	if len(tool.AllowedCallers) > 0 {
		anthropicTool.AllowedCallers = tool.AllowedCallers
	}
	if len(tool.InputExamples) > 0 {
		anthropicTool.InputExamples = make([]AnthropicToolInputExample, len(tool.InputExamples))
		for i, ex := range tool.InputExamples {
			anthropicTool.InputExamples[i] = AnthropicToolInputExample{
				Input:       ex.Input,
				Description: ex.Description,
			}
		}
	}
	if tool.EagerInputStreaming != nil {
		anthropicTool.EagerInputStreaming = tool.EagerInputStreaming
	}
	// ChatToolFunction.Strict is the canonical neutral slot for Anthropic's strict.
	if tool.Function.Strict != nil {
		anthropicTool.Strict = tool.Function.Strict
	}
	return anthropicTool
}

// convertServerToolToAnthropic reconstructs an AnthropicTool from the
// server-tool shape of a schemas.ChatTool (Function=nil, Name+Type+variant
// fields populated). Returns (tool, true) when Type looks like a known
// server-tool; (zero, false) when it doesn't, so the caller can drop it
// cleanly rather than forward a malformed tool.
//
// Supported type prefixes:
//   - web_search_*    → AnthropicToolWebSearch
//   - web_fetch_*     → AnthropicToolWebFetch
//   - computer_*      → AnthropicToolComputerUse
//   - text_editor_*   → AnthropicToolTextEditor
//   - mcp_toolset     → AnthropicMCPToolsetTool (via MCPToolset pointer)
//
// bash_*, memory_*, code_execution_*, and tool_search_* carry no variant
// config — their Type + Name alone are enough, handled in the default branch.
func convertServerToolToAnthropic(tool schemas.ChatTool) (AnthropicTool, bool) {
	typeStr := string(tool.Type)
	if typeStr == "" {
		return AnthropicTool{}, false
	}

	// mcp_toolset is serialized via a dedicated embedded type (AnthropicMCPToolsetTool)
	// and carries its identity in MCPServerName, not Name — handle before the
	// generic Name guard below.
	if typeStr == "mcp_toolset" {
		if tool.MCPServerName == "" {
			return AnthropicTool{}, false
		}
		toolset := &AnthropicMCPToolsetTool{
			Type:          "mcp_toolset",
			MCPServerName: tool.MCPServerName,
			DefaultConfig: convertMCPToolsetConfig(tool.DefaultConfig),
			Configs:       convertMCPToolsetConfigMap(tool.Configs),
			CacheControl:  tool.CacheControl,
		}
		return AnthropicTool{MCPToolset: toolset}, true
	}

	// Remaining server tools (web_search, web_fetch, computer, text_editor, etc.)
	// identify themselves via Name.
	if tool.Name == "" {
		return AnthropicTool{}, false
	}

	atype := AnthropicToolType(typeStr)
	anthropicTool := AnthropicTool{
		Name:                tool.Name,
		Type:                &atype,
		CacheControl:        tool.CacheControl,
		DeferLoading:        tool.DeferLoading,
		AllowedCallers:      tool.AllowedCallers,
		EagerInputStreaming: tool.EagerInputStreaming,
	}
	if len(tool.InputExamples) > 0 {
		anthropicTool.InputExamples = make([]AnthropicToolInputExample, len(tool.InputExamples))
		for i, ex := range tool.InputExamples {
			anthropicTool.InputExamples[i] = AnthropicToolInputExample{
				Input:       ex.Input,
				Description: ex.Description,
			}
		}
	}

	switch {
	case strings.HasPrefix(typeStr, "web_search_"):
		anthropicTool.AnthropicToolWebSearch = &AnthropicToolWebSearch{
			MaxUses:        tool.MaxUses,
			AllowedDomains: tool.AllowedDomains,
			BlockedDomains: tool.BlockedDomains,
			UserLocation:   convertUserLocation(tool.UserLocation),
		}
	case strings.HasPrefix(typeStr, "web_fetch_"):
		anthropicTool.AnthropicToolWebFetch = &AnthropicToolWebFetch{
			MaxUses:          tool.MaxUses,
			AllowedDomains:   tool.AllowedDomains,
			BlockedDomains:   tool.BlockedDomains,
			MaxContentTokens: tool.MaxContentTokens,
			Citations:        convertCitationsConfig(tool.Citations),
			UseCache:         tool.UseCache,
		}
	case strings.HasPrefix(typeStr, "computer_"):
		anthropicTool.AnthropicToolComputerUse = &AnthropicToolComputerUse{
			DisplayWidthPx:  tool.DisplayWidthPx,
			DisplayHeightPx: tool.DisplayHeightPx,
			DisplayNumber:   tool.DisplayNumber,
			EnableZoom:      tool.EnableZoom,
		}
	case strings.HasPrefix(typeStr, "text_editor_"):
		anthropicTool.AnthropicToolTextEditor = &AnthropicToolTextEditor{
			MaxCharacters: tool.MaxCharacters,
		}
	case strings.HasPrefix(typeStr, "bash_"),
		strings.HasPrefix(typeStr, "memory_"),
		strings.HasPrefix(typeStr, "code_execution_"),
		strings.HasPrefix(typeStr, "tool_search_tool_"):
		// No variant-specific config — Type + Name alone.
	default:
		// Unknown type — pass through Type + Name and let Anthropic reject
		// if it's truly invalid. This keeps forward-compat for new tool
		// versions that aren't yet known to Koutaku.
	}
	return anthropicTool, true
}

// convertUserLocation mirrors schemas.ChatToolUserLocation onto
// AnthropicToolWebSearchUserLocation.
func convertUserLocation(loc *schemas.ChatToolUserLocation) *AnthropicToolWebSearchUserLocation {
	if loc == nil {
		return nil
	}
	return &AnthropicToolWebSearchUserLocation{
		Type:     loc.Type,
		City:     loc.City,
		Region:   loc.Region,
		Country:  loc.Country,
		Timezone: loc.Timezone,
	}
}

// convertCitationsConfig mirrors the request-side citations config
// ({"enabled": true/false}) onto AnthropicCitations' request form.
func convertCitationsConfig(c *schemas.ChatToolCitationsConfig) *AnthropicCitations {
	if c == nil {
		return nil
	}
	return &AnthropicCitations{Config: &schemas.Citations{Enabled: c.Enabled}}
}

// convertMCPToolsetConfig mirrors a single mcp_toolset config.
func convertMCPToolsetConfig(c *schemas.ChatMCPToolsetConfig) *AnthropicMCPToolsetConfig {
	if c == nil {
		return nil
	}
	return &AnthropicMCPToolsetConfig{
		Enabled:      c.Enabled,
		DeferLoading: c.DeferLoading,
	}
}

// convertMCPToolsetConfigMap mirrors the per-tool mcp_toolset configs map.
func convertMCPToolsetConfigMap(m map[string]*schemas.ChatMCPToolsetConfig) map[string]*AnthropicMCPToolsetConfig {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]*AnthropicMCPToolsetConfig, len(m))
	for k, v := range m {
		out[k] = convertMCPToolsetConfig(v)
	}
	return out
}

// ToAnthropicChatRequest converts a Koutaku request to Anthropic format
// This is the reverse of ConvertChatRequestToKoutaku for provider-side usage
func ToAnthropicChatRequest(ctx *schemas.KoutakuContext, koutakuReq *schemas.KoutakuChatRequest) (*AnthropicMessageRequest, error) {
	if koutakuReq == nil || koutakuReq.Input == nil {
		return nil, fmt.Errorf("koutaku request is nil or input is nil")
	}

	messages := koutakuReq.Input
	anthropicReq := &AnthropicMessageRequest{
		Model:     koutakuReq.Model,
		MaxTokens: providerUtils.GetMaxOutputTokensOrDefault(koutakuReq.Model, AnthropicDefaultMaxTokens),
	}

	// Convert parameters
	if koutakuReq.Params != nil {
		anthropicReq.ExtraParams = koutakuReq.Params.ExtraParams
		if koutakuReq.Params.MaxCompletionTokens != nil {
			anthropicReq.MaxTokens = *koutakuReq.Params.MaxCompletionTokens
		}

		// Opus 4.7+ rejects temperature, top_p, and top_k with a 400 error.
		if !IsOpus47(koutakuReq.Model) {
			// Anthropic doesn't allow both temperature and top_p to be specified.
			// If both are present, prefer temperature (more commonly used).
			if koutakuReq.Params.Temperature != nil {
				anthropicReq.Temperature = koutakuReq.Params.Temperature
			} else if koutakuReq.Params.TopP != nil {
				anthropicReq.TopP = koutakuReq.Params.TopP
			}
		}
		anthropicReq.StopSequences = koutakuReq.Params.Stop

		// TopK — prefer the promoted neutral field; fall back to ExtraParams.
		// Opus 4.7+ rejects top_k with a 400 error.
		if koutakuReq.Params.TopK != nil {
			if !IsOpus47(koutakuReq.Model) {
				anthropicReq.TopK = koutakuReq.Params.TopK
			}
		} else if topK, ok := schemas.SafeExtractIntPointer(koutakuReq.Params.ExtraParams["top_k"]); ok {
			delete(anthropicReq.ExtraParams, "top_k")
			if !IsOpus47(koutakuReq.Model) {
				anthropicReq.TopK = topK
			}
		}

		// Speed — prefer neutral field, then ExtraParams.
		if koutakuReq.Params.Speed != nil {
			anthropicReq.Speed = koutakuReq.Params.Speed
		} else if speed, ok := schemas.SafeExtractStringPointer(koutakuReq.Params.ExtraParams["speed"]); ok {
			delete(anthropicReq.ExtraParams, "speed")
			anthropicReq.Speed = speed
		}

		// InferenceGeo — prefer neutral field, then ExtraParams.
		if koutakuReq.Params.InferenceGeo != nil {
			anthropicReq.InferenceGeo = koutakuReq.Params.InferenceGeo
		} else if inferenceGeo, ok := schemas.SafeExtractStringPointer(koutakuReq.Params.ExtraParams["inference_geo"]); ok {
			delete(anthropicReq.ExtraParams, "inference_geo")
			anthropicReq.InferenceGeo = inferenceGeo
		}

		// ContextManagement — the neutral type is json.RawMessage; decode to
		// the Anthropic-shape ContextManagement. Fall back to ExtraParams
		// (legacy map-valued or typed-pointer paths) if the raw is empty.
		// Surface decode errors on the typed path so callers get immediate
		// feedback on malformed config instead of a silent drop.
		if len(koutakuReq.Params.ContextManagement) > 0 {
			var cm ContextManagement
			if err := sonic.Unmarshal(koutakuReq.Params.ContextManagement, &cm); err != nil {
				return nil, fmt.Errorf("context_management: failed to parse: %w", err)
			}
			anthropicReq.ContextManagement = &cm
		} else if cmVal := koutakuReq.Params.ExtraParams["context_management"]; cmVal != nil {
			if cm, ok := cmVal.(*ContextManagement); ok && cm != nil {
				delete(anthropicReq.ExtraParams, "context_management")
				anthropicReq.ContextManagement = cm
			} else if data, err := providerUtils.MarshalSorted(cmVal); err == nil {
				var cm ContextManagement
				if sonic.Unmarshal(data, &cm) == nil {
					delete(anthropicReq.ExtraParams, "context_management")
					anthropicReq.ContextManagement = &cm
				}
			}
		}

		// Container — map the neutral ChatContainer union onto the Anthropic
		// AnthropicContainer union. Both follow the string-or-object pattern.
		if koutakuReq.Params.Container != nil {
			c := &AnthropicContainer{}
			if koutakuReq.Params.Container.ContainerStr != nil {
				c.ContainerStr = koutakuReq.Params.Container.ContainerStr
			} else if koutakuReq.Params.Container.ContainerObject != nil {
				obj := &AnthropicContainerObject{
					ID: koutakuReq.Params.Container.ContainerObject.ID,
				}
				if len(koutakuReq.Params.Container.ContainerObject.Skills) > 0 {
					obj.Skills = make([]AnthropicContainerSkill, len(koutakuReq.Params.Container.ContainerObject.Skills))
					for i, sk := range koutakuReq.Params.Container.ContainerObject.Skills {
						obj.Skills[i] = AnthropicContainerSkill{
							SkillID: sk.SkillID,
							Type:    sk.Type,
							Version: sk.Version,
						}
					}
				}
				c.ContainerObject = obj
			}
			anthropicReq.Container = c
		}

		// Top-level CacheControl on the request.
		if koutakuReq.Params.CacheControl != nil {
			anthropicReq.CacheControl = koutakuReq.Params.CacheControl
		}

		// TaskBudget — maps onto output_config.task_budget. If an OutputConfig
		// already exists (e.g. from structured outputs), attach the budget to
		// it; otherwise create one.
		if koutakuReq.Params.TaskBudget != nil {
			tb := &AnthropicTaskBudget{
				Type:      koutakuReq.Params.TaskBudget.Type,
				Total:     koutakuReq.Params.TaskBudget.Total,
				Remaining: koutakuReq.Params.TaskBudget.Remaining,
			}
			if anthropicReq.OutputConfig == nil {
				anthropicReq.OutputConfig = &AnthropicOutputConfig{}
			}
			anthropicReq.OutputConfig.TaskBudget = tb
		}

		// MCPServers — mirror the neutral ChatMCPServer[] to AnthropicMCPServerV2[].
		if len(koutakuReq.Params.MCPServers) > 0 {
			servers := make([]AnthropicMCPServerV2, len(koutakuReq.Params.MCPServers))
			for i, s := range koutakuReq.Params.MCPServers {
				servers[i] = AnthropicMCPServerV2{
					Type:               s.Type,
					URL:                s.URL,
					Name:               s.Name,
					AuthorizationToken: s.AuthorizationToken,
				}
			}
			anthropicReq.MCPServers = servers
		}
		if koutakuReq.Params.ResponseFormat != nil {
			// Vertex doesn't support native structured outputs, so convert to tool
			if koutakuReq.Provider == schemas.Vertex {
				responseFormatTool := convertChatResponseFormatToTool(ctx, koutakuReq.Params)
				if responseFormatTool != nil {
					anthropicReq.Tools = append(anthropicReq.Tools, *responseFormatTool)
					// Force the model to use this specific tool
					anthropicReq.ToolChoice = &AnthropicToolChoice{
						Type: "tool",
						Name: responseFormatTool.Name,
					}
				}
			} else {
				// Use GA structured outputs (output_config.format) instead of beta (output_format)
				outputFormat := convertChatResponseFormatToAnthropicOutputFormat(koutakuReq.Params.ResponseFormat)
				if outputFormat != nil {
					anthropicReq.OutputConfig = &AnthropicOutputConfig{
						Format: outputFormat,
					}
				}
			}
		}

		// Convert tools. Three neutral ChatTool shapes are supported:
		//   (1) Function tool (tool.Function != nil) — existing path.
		//   (2) Anthropic server tool (tool.Function == nil, Type is a
		//       server-tool version string, Name populated at top level) —
		//       new path handled by convertServerToolToAnthropic.
		//   (3) Custom tool (tool.Custom != nil) — not currently forwarded
		//       to Anthropic; skipped.
		if koutakuReq.Params.Tools != nil {
			// Strip server tools the target provider doesn't support per
			// ProviderFeatures (e.g. web_search on Vertex's non-supporting
			// model variants, or MCP on Bedrock when this converter is used
			// by non-Bedrock providers). Function/custom tools are always
			// kept. The dropped set is discarded — "silent strip + continue"
			// policy per user direction. See Bedrock's convertToolConfig for
			// the direct-Bedrock-path equivalent.
			filtered, _ := ValidateChatToolsForProvider(koutakuReq.Params.Tools, koutakuReq.Provider)
			tools := make([]AnthropicTool, 0, len(filtered))
			for _, tool := range filtered {
				if tool.Function != nil {
					tools = append(tools, convertFunctionToolToAnthropic(tool))
					continue
				}
				// Non-function tool: attempt server-tool reconstruction.
				if converted, ok := convertServerToolToAnthropic(tool); ok {
					tools = append(tools, converted)
				}
			}
			if anthropicReq.Tools == nil {
				anthropicReq.Tools = tools
			} else {
				anthropicReq.Tools = append(anthropicReq.Tools, tools...)
			}
		}

		// Convert tool choice
		if koutakuReq.Params.ToolChoice != nil {
			toolChoice := &AnthropicToolChoice{}
			if koutakuReq.Params.ToolChoice.ChatToolChoiceStr != nil {
				switch schemas.ChatToolChoiceType(*koutakuReq.Params.ToolChoice.ChatToolChoiceStr) {
				case schemas.ChatToolChoiceTypeAny:
					toolChoice.Type = "any"
				case schemas.ChatToolChoiceTypeRequired:
					toolChoice.Type = "any"
				case schemas.ChatToolChoiceTypeNone:
					toolChoice.Type = "none"
				default:
					toolChoice.Type = "auto"
				}
			} else if koutakuReq.Params.ToolChoice.ChatToolChoiceStruct != nil {
				switch koutakuReq.Params.ToolChoice.ChatToolChoiceStruct.Type {
				case schemas.ChatToolChoiceTypeFunction:
					toolChoice.Type = "tool"
					if koutakuReq.Params.ToolChoice.ChatToolChoiceStruct.Function != nil {
						toolChoice.Name = koutakuReq.Params.ToolChoice.ChatToolChoiceStruct.Function.Name
					}
				case schemas.ChatToolChoiceTypeAllowedTools:
					toolChoice.Type = "any"
				case schemas.ChatToolChoiceTypeCustom:
					toolChoice.Type = "auto"
				default:
					toolChoice.Type = "auto"
				}
			}
			anthropicReq.ToolChoice = toolChoice
		}

		// Convert reasoning
		if koutakuReq.Params.Reasoning != nil {
			if koutakuReq.Params.Reasoning.MaxTokens != nil {
				if IsOpus47(koutakuReq.Model) {
					// Opus 4.7+: budget_tokens removed; adaptive thinking is the only thinking-on mode.
					anthropicReq.Thinking = &AnthropicThinking{Type: "adaptive"}
				} else {
					budgetTokens := *koutakuReq.Params.Reasoning.MaxTokens
					if *koutakuReq.Params.Reasoning.MaxTokens == -1 {
						// anthropic does not support dynamic reasoning budget like gemini
						// setting it to default max tokens
						budgetTokens = MinimumReasoningMaxTokens
					}
					if budgetTokens < MinimumReasoningMaxTokens {
						return nil, fmt.Errorf("reasoning.max_tokens must be >= %d for anthropic", MinimumReasoningMaxTokens)
					}
					anthropicReq.Thinking = &AnthropicThinking{
						Type:         "enabled",
						BudgetTokens: schemas.Ptr(budgetTokens),
					}
				}
			} else if koutakuReq.Params.Reasoning.Effort != nil && *koutakuReq.Params.Reasoning.Effort != "none" {
				effort := MapKoutakuEffortToAnthropic(*koutakuReq.Params.Reasoning.Effort)
				if SupportsAdaptiveThinking(koutakuReq.Model) || IsOpus47(koutakuReq.Model) {
					// Opus 4.6+ and Opus 4.7+: adaptive thinking + native effort
					anthropicReq.Thinking = &AnthropicThinking{Type: "adaptive"}
					setEffortOnOutputConfig(anthropicReq, effort)
				} else if SupportsNativeEffort(koutakuReq.Model) {
					// Opus 4.5: native effort + budget_tokens thinking
					setEffortOnOutputConfig(anthropicReq, effort)
					budgetTokens, err := providerUtils.GetBudgetTokensFromReasoningEffort(effort, MinimumReasoningMaxTokens, anthropicReq.MaxTokens)
					if err != nil {
						return nil, err
					}
					anthropicReq.Thinking = &AnthropicThinking{
						Type:         "enabled",
						BudgetTokens: schemas.Ptr(budgetTokens),
					}
				} else {
					// Older models: budget_tokens only
					budgetTokens, err := providerUtils.GetBudgetTokensFromReasoningEffort(*koutakuReq.Params.Reasoning.Effort, MinimumReasoningMaxTokens, anthropicReq.MaxTokens)
					if err != nil {
						return nil, err
					}
					anthropicReq.Thinking = &AnthropicThinking{
						Type:         "enabled",
						BudgetTokens: schemas.Ptr(budgetTokens),
					}
				}
			} else {
				anthropicReq.Thinking = &AnthropicThinking{
					Type: "disabled",
				}
			}

			// thinking.display — map the neutral ChatReasoning.Display onto
			// AnthropicThinking.Display. Valid for "enabled" and "adaptive"
			// modes only; Anthropic rejects display on "disabled" ("there is
			// nothing to display", per the extended-thinking doc). We attach
			// on non-disabled modes and let the upstream provider enforce
			// model-level support.
			if koutakuReq.Params.Reasoning.Display != nil &&
				anthropicReq.Thinking != nil &&
				anthropicReq.Thinking.Type != "disabled" {
				anthropicReq.Thinking.Display = koutakuReq.Params.Reasoning.Display
			}
		}

		// Convert service tier
		anthropicReq.ServiceTier = koutakuReq.Params.ServiceTier
	}

	// Convert messages - group consecutive tool messages into single user messages
	var anthropicMessages []AnthropicMessage
	var systemContent *AnthropicContent

	i := 0
	for i < len(messages) {
		msg := messages[i]

		switch msg.Role {
		case schemas.ChatMessageRoleSystem:
			// Handle system message separately
			if msg.Content != nil {
				if msg.Content.ContentStr != nil && *msg.Content.ContentStr != "" {
					systemContent = &AnthropicContent{ContentStr: msg.Content.ContentStr}
				} else if msg.Content.ContentBlocks != nil {
					blocks := make([]AnthropicContentBlock, 0, len(msg.Content.ContentBlocks))
					for _, block := range msg.Content.ContentBlocks {
						if block.Text != nil && *block.Text != "" {
							blocks = append(blocks, AnthropicContentBlock{
								Type:         AnthropicContentBlockTypeText,
								Text:         block.Text,
								CacheControl: block.CacheControl,
							})
						}
					}
					if len(blocks) > 0 {
						systemContent = &AnthropicContent{ContentBlocks: blocks}
					}
				}
			}
			i++

		case schemas.ChatMessageRoleTool:
			// Group consecutive tool messages into a single user message
			var toolResults []AnthropicContentBlock

			// Collect all consecutive tool messages
			for i < len(messages) && messages[i].Role == schemas.ChatMessageRoleTool {
				toolMsg := messages[i]
				if toolMsg.ChatToolMessage != nil && toolMsg.ChatToolMessage.ToolCallID != nil {
					toolResult := AnthropicContentBlock{
						Type:      AnthropicContentBlockTypeToolResult,
						ToolUseID: toolMsg.ChatToolMessage.ToolCallID,
					}

					// Convert tool result content
					if toolMsg.Content != nil {
						if toolMsg.Content.ContentStr != nil && *toolMsg.Content.ContentStr != "" {
							toolResult.Content = &AnthropicContent{ContentStr: toolMsg.Content.ContentStr}
						} else if toolMsg.Content.ContentBlocks != nil {
							blocks := make([]AnthropicContentBlock, 0, len(toolMsg.Content.ContentBlocks))
							for _, block := range toolMsg.Content.ContentBlocks {
								if block.Text != nil && *block.Text != "" {
									blocks = append(blocks, AnthropicContentBlock{
										Type:         AnthropicContentBlockTypeText,
										Text:         block.Text,
										CacheControl: block.CacheControl,
									})
								} else if block.ImageURLStruct != nil {
									blocks = append(blocks, ConvertToAnthropicImageBlock(block))
								}
							}
							if len(blocks) > 0 {
								toolResult.Content = &AnthropicContent{ContentBlocks: blocks}
							}
						}
					}

					toolResults = append(toolResults, toolResult)
				}
				i++
			}

			// Create a single user message with all tool results
			if len(toolResults) > 0 {
				anthropicMessages = append(anthropicMessages, AnthropicMessage{
					Role:    "user", // Tool results are sent as user messages in Anthropic
					Content: AnthropicContent{ContentBlocks: toolResults},
				})
			}

		default:
			// Handle user and assistant messages
			anthropicMsg := AnthropicMessage{
				Role: AnthropicMessageRole(msg.Role),
			}

			var content []AnthropicContentBlock

			// First add reasoning details
			if msg.ChatAssistantMessage != nil && msg.ChatAssistantMessage.ReasoningDetails != nil {
				for _, reasoningDetail := range msg.ChatAssistantMessage.ReasoningDetails {
					content = append(content, AnthropicContentBlock{
						Type:      AnthropicContentBlockTypeThinking,
						Signature: reasoningDetail.Signature,
						Thinking:  reasoningDetail.Text,
					})
				}
			}

			if msg.Content != nil {
				// Convert text content
				if msg.Content.ContentStr != nil && *msg.Content.ContentStr != "" {
					content = append(content, AnthropicContentBlock{
						Type: AnthropicContentBlockTypeText,
						Text: msg.Content.ContentStr,
					})
				} else if msg.Content.ContentBlocks != nil {
					for _, block := range msg.Content.ContentBlocks {
						if block.Text != nil && *block.Text != "" {
							content = append(content, AnthropicContentBlock{
								Type:         AnthropicContentBlockTypeText,
								Text:         block.Text,
								CacheControl: block.CacheControl,
							})
						} else if block.ImageURLStruct != nil {
							content = append(content, ConvertToAnthropicImageBlock(block))
						} else if block.File != nil {
							content = append(content, ConvertToAnthropicDocumentBlock(block))
						}
					}
				}
			}

			// Convert tool calls
			if msg.ChatAssistantMessage != nil && msg.ChatAssistantMessage.ToolCalls != nil {
				for _, toolCall := range msg.ChatAssistantMessage.ToolCalls {
					toolUse := AnthropicContentBlock{
						Type: AnthropicContentBlockTypeToolUse,
						ID:   toolCall.ID,
						Name: toolCall.Function.Name,
					}

					// Preserve original key ordering of tool arguments for prompt caching.
					// Using json.RawMessage avoids the map[string]interface{} round-trip
					// that would destroy key order.
					if toolCall.Function.Arguments == "" {
						toolUse.Input = json.RawMessage("{}")
					} else if compacted := compactJSONBytes([]byte(toolCall.Function.Arguments)); compacted != nil {
						toolUse.Input = json.RawMessage(compacted)
					} else {
						// Preserve original payload instead of silently dropping args.
						toolUse.Input = json.RawMessage([]byte(toolCall.Function.Arguments))
					}

					content = append(content, toolUse)
				}
			}

			// Set content
			if len(content) == 1 && content[0].Type == AnthropicContentBlockTypeText {
				// Always use ContentBlocks for consistent array serialization
				anthropicMsg.Content = AnthropicContent{ContentBlocks: content}
			} else if len(content) > 0 {
				// Multiple content blocks
				anthropicMsg.Content = AnthropicContent{ContentBlocks: content}
			}

			anthropicMessages = append(anthropicMessages, anthropicMsg)
			i++
		}
	}

	anthropicReq.Messages = anthropicMessages
	anthropicReq.System = systemContent

	// Strip request- and tool-level fields the target Anthropic-family
	// provider does not support. Fail-closed tool validation stays in
	// ValidateToolsForProvider; this is strip-silently for additive fields.
	stripUnsupportedAnthropicFields(anthropicReq, koutakuReq.Provider, koutakuReq.Model)

	return anthropicReq, nil
}

// ToKoutakuChatResponse converts an Anthropic message response to Koutaku format
func (response *AnthropicMessageResponse) ToKoutakuChatResponse(ctx *schemas.KoutakuContext) *schemas.KoutakuChatResponse {
	if response == nil {
		return nil
	}

	// Initialize Koutaku response
	koutakuResponse := &schemas.KoutakuChatResponse{
		ID:      response.ID,
		Model:   response.Model,
		Created: int(time.Now().Unix()),
	}

	// Check if we have a structured output tool
	var structuredOutputToolName string
	if ctx != nil {
		if toolName, ok := ctx.Value(schemas.KoutakuContextKeyStructuredOutputToolName).(string); ok {
			structuredOutputToolName = toolName
		}
	}

	// Collect all content and tool calls into a single message
	var toolCalls []schemas.ChatAssistantMessageToolCall
	var contentBlocks []schemas.ChatContentBlock
	var reasoningDetails []schemas.ChatReasoningDetails
	var reasoningText string
	var contentStr *string

	// Process content and tool calls
	if response.Content != nil {
		for _, c := range response.Content {
			switch c.Type {
			case AnthropicContentBlockTypeText:
				if c.Text != nil {
					contentBlocks = append(contentBlocks, schemas.ChatContentBlock{
						Type: schemas.ChatContentBlockTypeText,
						Text: c.Text,
					})
				}
			case AnthropicContentBlockTypeToolUse:
				if c.ID != nil && c.Name != nil {
					// Check if this is the structured output tool - if so, convert to text content
					if structuredOutputToolName != "" && *c.Name == structuredOutputToolName {
						// This is a structured output tool - convert to text content
						var jsonStr string
						if c.Input != nil {
							if argBytes, err := providerUtils.MarshalSorted(c.Input); err == nil {
								jsonStr = string(argBytes)
							} else {
								jsonStr = fmt.Sprintf("%v", c.Input)
							}
						} else {
							jsonStr = "{}"
						}
						contentStr = &jsonStr
						continue // Skip adding to toolCalls
					}

					function := schemas.ChatAssistantMessageToolCallFunction{
						Name: c.Name,
					}

					// Marshal the input to JSON string
					if c.Input != nil {
						args, err := providerUtils.MarshalSorted(c.Input)
						if err != nil {
							function.Arguments = fmt.Sprintf("%v", c.Input)
						} else {
							function.Arguments = string(args)
						}
					} else {
						function.Arguments = "{}"
					}

					toolCalls = append(toolCalls, schemas.ChatAssistantMessageToolCall{
						Index:    uint16(len(toolCalls)),
						Type:     schemas.Ptr(string(schemas.ChatToolTypeFunction)),
						ID:       c.ID,
						Function: function,
					})
				}
			case AnthropicContentBlockTypeThinking:
				reasoningDetails = append(reasoningDetails, schemas.ChatReasoningDetails{
					Index:     len(reasoningDetails),
					Type:      schemas.KoutakuReasoningDetailsTypeText,
					Text:      c.Thinking,
					Signature: c.Signature,
				})
				if c.Thinking != nil {
					reasoningText += *c.Thinking + "\n"
				}
			}
		}
	}

	if len(contentBlocks) == 1 && contentBlocks[0].Type == schemas.ChatContentBlockTypeText {
		contentStr = contentBlocks[0].Text
		contentBlocks = nil
	}

	// Create a single choice with the collected content
	// Create message content
	messageContent := schemas.ChatMessageContent{
		ContentStr:    contentStr,
		ContentBlocks: contentBlocks,
	}

	// Create the assistant message
	var assistantMessage *schemas.ChatAssistantMessage

	// Create AssistantMessage if we have tool calls or thinking
	if len(toolCalls) > 0 {
		assistantMessage = &schemas.ChatAssistantMessage{
			ToolCalls: toolCalls,
		}
	}

	if len(reasoningDetails) > 0 {
		if assistantMessage == nil {
			assistantMessage = &schemas.ChatAssistantMessage{}
		}
		assistantMessage.ReasoningDetails = reasoningDetails
		if reasoningText != "" {
			assistantMessage.Reasoning = &reasoningText
		}
	}

	// Create message
	message := schemas.ChatMessage{
		Role:                 schemas.ChatMessageRoleAssistant,
		Content:              &messageContent,
		ChatAssistantMessage: assistantMessage,
	}

	// Create choice
	choice := schemas.KoutakuResponseChoice{
		Index: 0,
		ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
			Message:    &message,
			StopString: response.StopSequence,
		},
		FinishReason: func() *string {
			if response.StopReason != "" {
				mapped := ConvertAnthropicFinishReasonToKoutaku(response.StopReason)
				return &mapped
			}
			return nil
		}(),
	}

	koutakuResponse.Choices = []schemas.KoutakuResponseChoice{choice}

	// Convert usage information
	if response.Usage != nil {
		koutakuResponse.Usage = &schemas.KoutakuLLMUsage{
			PromptTokens: response.Usage.InputTokens + response.Usage.CacheReadInputTokens + response.Usage.CacheCreationInputTokens,
			PromptTokensDetails: &schemas.ChatPromptTokensDetails{
				CachedReadTokens:  response.Usage.CacheReadInputTokens,
				CachedWriteTokens: response.Usage.CacheCreationInputTokens,
			},
			CompletionTokens: response.Usage.OutputTokens,
		}
		koutakuResponse.Usage.TotalTokens = koutakuResponse.Usage.PromptTokens + koutakuResponse.Usage.CompletionTokens
		// Forward service tier from usage to response
		if response.Usage.ServiceTier != nil {
			koutakuResponse.ServiceTier = response.Usage.ServiceTier
		}
	}

	return koutakuResponse
}

// ToAnthropicChatResponse converts a Koutaku response to Anthropic format
func ToAnthropicChatResponse(koutakuResp *schemas.KoutakuChatResponse) *AnthropicMessageResponse {
	if koutakuResp == nil {
		return nil
	}

	anthropicResp := &AnthropicMessageResponse{
		ID:    koutakuResp.ID,
		Type:  "message",
		Role:  string(schemas.ChatMessageRoleAssistant),
		Model: koutakuResp.Model,
	}

	// Convert usage information
	if koutakuResp.Usage != nil {
		anthropicResp.Usage = &AnthropicUsage{
			InputTokens:  koutakuResp.Usage.PromptTokens,
			OutputTokens: koutakuResp.Usage.CompletionTokens,
		}

		// Cache read/write are now segregated via PromptTokensDetails. We map CachedReadTokens ->
		// CacheReadInputTokens and CachedWriteTokens -> CacheCreationInputTokens, subtracting each
		// from InputTokens so the non-cached input count is correct.
		if koutakuResp.Usage.PromptTokensDetails != nil && koutakuResp.Usage.PromptTokensDetails.CachedReadTokens > 0 {
			anthropicResp.Usage.CacheReadInputTokens = koutakuResp.Usage.PromptTokensDetails.CachedReadTokens
			anthropicResp.Usage.InputTokens = anthropicResp.Usage.InputTokens - koutakuResp.Usage.PromptTokensDetails.CachedReadTokens
		}
		if koutakuResp.Usage.PromptTokensDetails != nil && koutakuResp.Usage.PromptTokensDetails.CachedWriteTokens > 0 {
			anthropicResp.Usage.CacheCreationInputTokens = koutakuResp.Usage.PromptTokensDetails.CachedWriteTokens
			anthropicResp.Usage.InputTokens = anthropicResp.Usage.InputTokens - koutakuResp.Usage.PromptTokensDetails.CachedWriteTokens
		}
		// Forward service tier
		if koutakuResp.ServiceTier != nil {
			anthropicResp.Usage.ServiceTier = koutakuResp.ServiceTier
		}
	}

	// Convert choices to content
	var content []AnthropicContentBlock
	if len(koutakuResp.Choices) > 0 {
		choice := koutakuResp.Choices[0] // Anthropic typically returns one choice

		if choice.FinishReason != nil {
			anthropicResp.StopReason = ConvertKoutakuFinishReasonToAnthropic(*choice.FinishReason)
		}
		if choice.ChatNonStreamResponseChoice != nil && choice.StopString != nil {
			anthropicResp.StopSequence = choice.StopString
		}

		// Add reasoning content
		if choice.ChatNonStreamResponseChoice != nil && choice.Message != nil && choice.Message.ChatAssistantMessage != nil && choice.Message.ChatAssistantMessage.ReasoningDetails != nil {
			for _, reasoningDetail := range choice.Message.ChatAssistantMessage.ReasoningDetails {
				if reasoningDetail.Type == schemas.KoutakuReasoningDetailsTypeText && reasoningDetail.Text != nil &&
					((reasoningDetail.Text != nil && *reasoningDetail.Text != "") ||
						(reasoningDetail.Signature != nil && *reasoningDetail.Signature != "")) {
					content = append(content, AnthropicContentBlock{
						Type:      AnthropicContentBlockTypeThinking,
						Thinking:  reasoningDetail.Text,
						Signature: reasoningDetail.Signature,
					})
				}
			}
		}

		// Add text content
		if choice.ChatNonStreamResponseChoice != nil && choice.Message != nil && choice.Message.Content != nil && choice.Message.Content.ContentStr != nil && *choice.Message.Content.ContentStr != "" {
			content = append(content, AnthropicContentBlock{
				Type: AnthropicContentBlockTypeText,
				Text: choice.Message.Content.ContentStr,
			})
		} else if choice.ChatNonStreamResponseChoice != nil && choice.Message != nil && choice.Message.Content != nil && choice.Message.Content.ContentBlocks != nil {
			for _, block := range choice.Message.Content.ContentBlocks {
				if block.Text != nil {
					content = append(content, AnthropicContentBlock{
						Type: AnthropicContentBlockTypeText,
						Text: block.Text,
					})
				}
			}
		}

		// Add tool calls as tool_use content
		if choice.ChatNonStreamResponseChoice != nil && choice.Message != nil && choice.Message.ChatAssistantMessage != nil && choice.Message.ChatAssistantMessage.ToolCalls != nil {
			for _, toolCall := range choice.Message.ChatAssistantMessage.ToolCalls {
				// Parse arguments JSON string to raw message
				var inputRaw json.RawMessage
				if toolCall.Function.Arguments != "" {
					// Validate it's valid JSON, otherwise use empty object
					if json.Valid([]byte(toolCall.Function.Arguments)) {
						inputRaw = json.RawMessage(toolCall.Function.Arguments)
					} else {
						inputRaw = json.RawMessage("{}")
					}
				} else {
					inputRaw = json.RawMessage("{}")
				}

				content = append(content, AnthropicContentBlock{
					Type:  AnthropicContentBlockTypeToolUse,
					ID:    toolCall.ID,
					Name:  toolCall.Function.Name,
					Input: inputRaw,
				})
			}
		}
	}

	if content == nil {
		content = []AnthropicContentBlock{}
	}

	anthropicResp.Content = content
	return anthropicResp
}

// AnthropicStreamState tracks per-stream tool call index state.
type AnthropicStreamState struct {
	nextToolCallIndex         int
	contentBlockToToolCallIdx map[int]int
}

// NewAnthropicStreamState returns an initialised stream state for one streaming response.
func NewAnthropicStreamState() *AnthropicStreamState {
	return &AnthropicStreamState{
		contentBlockToToolCallIdx: make(map[int]int),
	}
}

// ToKoutakuChatCompletionStream converts an Anthropic stream event to a Koutaku Chat Completion Stream response
func (chunk *AnthropicStreamEvent) ToKoutakuChatCompletionStream(ctx *schemas.KoutakuContext, structuredOutputToolName string, state *AnthropicStreamState) (*schemas.KoutakuChatResponse, *schemas.KoutakuError, bool) {
	if state == nil {
		state = NewAnthropicStreamState()
	} else if state.contentBlockToToolCallIdx == nil {
		state.contentBlockToToolCallIdx = make(map[int]int)
	}

	switch chunk.Type {
	case AnthropicStreamEventTypeMessageStart:
		return nil, nil, false

	case AnthropicStreamEventTypeMessageStop:
		return nil, nil, true

	case AnthropicStreamEventTypeContentBlockStart:
		// Emit tool-call metadata when starting a tool_use content block
		if chunk.Index != nil && chunk.ContentBlock != nil && chunk.ContentBlock.Type == AnthropicContentBlockTypeToolUse {
			// Check if this is the structured output tool - if so, skip emitting tool call metadata
			if structuredOutputToolName != "" && chunk.ContentBlock.Name != nil && *chunk.ContentBlock.Name == structuredOutputToolName {
				// Skip emitting tool call for structured output - it will be emitted as content later
				return nil, nil, false
			}

			// Assign the next sequential tool-call index
			toolCallIdx := state.nextToolCallIndex
			state.contentBlockToToolCallIdx[*chunk.Index] = toolCallIdx
			state.nextToolCallIndex++

			// Create streaming response with tool call metadata
			streamResponse := &schemas.KoutakuChatResponse{
				Object: "chat.completion.chunk",
				Choices: []schemas.KoutakuResponseChoice{
					{
						Index: 0,
						ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
							Delta: &schemas.ChatStreamResponseChoiceDelta{
								ToolCalls: []schemas.ChatAssistantMessageToolCall{
									{
										Index: uint16(toolCallIdx),
										Type:  schemas.Ptr(string(schemas.ChatToolTypeFunction)),
										ID:    chunk.ContentBlock.ID,
										Function: schemas.ChatAssistantMessageToolCallFunction{
											Name:      chunk.ContentBlock.Name,
											Arguments: "", // Empty arguments initially, will be filled by subsequent deltas
										},
									},
								},
							},
						},
					},
				},
			}

			return streamResponse, nil, false
		}

		return nil, nil, false

	case AnthropicStreamEventTypeContentBlockDelta:
		if chunk.Index != nil && chunk.Delta != nil {
			// Handle different delta types
			switch chunk.Delta.Type {
			case AnthropicStreamDeltaTypeText:
				if chunk.Delta.Text != nil && *chunk.Delta.Text != "" {
					// Create streaming response for this delta
					streamResponse := &schemas.KoutakuChatResponse{
						Object: "chat.completion.chunk",
						Choices: []schemas.KoutakuResponseChoice{
							{
								Index: 0,
								ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
									Delta: &schemas.ChatStreamResponseChoiceDelta{
										Content: chunk.Delta.Text,
									},
								},
							},
						},
					}

					return streamResponse, nil, false
				}

			case AnthropicStreamDeltaTypeInputJSON:
				// Handle tool use streaming - accumulate partial JSON
				if chunk.Delta.PartialJSON != nil {
					if structuredOutputToolName != "" {
						// Structured output: stream JSON as content
						streamResponse := &schemas.KoutakuChatResponse{
							Object: "chat.completion.chunk",
							Choices: []schemas.KoutakuResponseChoice{
								{
									Index: 0,
									ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
										Delta: &schemas.ChatStreamResponseChoiceDelta{
											Content: chunk.Delta.PartialJSON,
										},
									},
								},
							},
						}
						return streamResponse, nil, false
					}

					// Resolve which tool-call this delta belongs to via the content-block index.
					toolCallIdx := state.contentBlockToToolCallIdx[*chunk.Index]

					// Create streaming response for tool input delta
					streamResponse := &schemas.KoutakuChatResponse{
						Object: "chat.completion.chunk",
						Choices: []schemas.KoutakuResponseChoice{
							{
								Index: 0,
								ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
									Delta: &schemas.ChatStreamResponseChoiceDelta{
										ToolCalls: []schemas.ChatAssistantMessageToolCall{
											{
												Index: uint16(toolCallIdx),
												Type:  schemas.Ptr(string(schemas.ChatToolTypeFunction)),
												Function: schemas.ChatAssistantMessageToolCallFunction{
													Arguments: *chunk.Delta.PartialJSON,
												},
											},
										},
									},
								},
							},
						},
					}

					return streamResponse, nil, false
				}

			case AnthropicStreamDeltaTypeThinking:
				// Handle thinking content streaming
				if chunk.Delta.Thinking != nil && *chunk.Delta.Thinking != "" {
					thinkingText := *chunk.Delta.Thinking
					// Create streaming response for thinking delta
					streamResponse := &schemas.KoutakuChatResponse{
						Object: "chat.completion.chunk",
						Choices: []schemas.KoutakuResponseChoice{
							{
								Index: 0,
								ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
									Delta: &schemas.ChatStreamResponseChoiceDelta{
										Reasoning: schemas.Ptr(thinkingText),
										ReasoningDetails: []schemas.ChatReasoningDetails{
											{
												Index: 0,
												Type:  schemas.KoutakuReasoningDetailsTypeText,
												Text:  schemas.Ptr(thinkingText),
											},
										},
									},
								},
							},
						},
					}

					return streamResponse, nil, false
				}

			case AnthropicStreamDeltaTypeSignature:
				if chunk.Delta.Signature != nil && *chunk.Delta.Signature != "" {
					// Create streaming response for signature delta
					streamResponse := &schemas.KoutakuChatResponse{
						Object: "chat.completion.chunk",
						Choices: []schemas.KoutakuResponseChoice{
							{
								Index: 0,
								ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
									Delta: &schemas.ChatStreamResponseChoiceDelta{
										ReasoningDetails: []schemas.ChatReasoningDetails{
											{
												Index:     0,
												Type:      schemas.KoutakuReasoningDetailsTypeText,
												Signature: chunk.Delta.Signature,
											},
										},
									},
								},
							},
						},
					}
					return streamResponse, nil, false
				}
			}
		}

	case AnthropicStreamEventTypeContentBlockStop:
		// Content block is complete, no specific action needed for streaming
		return nil, nil, false

	case AnthropicStreamEventTypeMessageDelta:
		return nil, nil, false

	case AnthropicStreamEventTypePing:
		// Ping events are just keepalive, no action needed
		return nil, nil, false

	case AnthropicStreamEventTypeError:
		if chunk.Error != nil {
			// Send error through channel before closing
			koutakuErr := &schemas.KoutakuError{
				IsKoutakuError: false,
				Error: &schemas.ErrorField{
					Type:    &chunk.Error.Type,
					Message: chunk.Error.Message,
				},
			}

			return nil, koutakuErr, true
		}
	}

	return nil, nil, false
}

// ToAnthropicChatStreamResponse converts a Koutaku streaming response to Anthropic SSE string format
func ToAnthropicChatStreamResponse(koutakuResp *schemas.KoutakuChatResponse) string {
	if koutakuResp == nil {
		return ""
	}

	streamResp := &AnthropicStreamEvent{}

	// Handle different streaming event types based on the response content
	if len(koutakuResp.Choices) > 0 {
		choice := koutakuResp.Choices[0] // Anthropic typically returns one choice

		// Handle streaming responses
		if choice.ChatStreamResponseChoice != nil && choice.ChatStreamResponseChoice.Delta != nil {
			delta := choice.ChatStreamResponseChoice.Delta

			// Handle text content deltas
			if delta.Content != nil {
				streamResp.Type = "content_block_delta"
				streamResp.Index = &choice.Index
				streamResp.Delta = &AnthropicStreamDelta{
					Type: AnthropicStreamDeltaTypeText,
					Text: delta.Content,
				}
			} else if delta.Reasoning != nil {
				// Handle thinking content deltas
				streamResp.Type = "content_block_delta"
				streamResp.Index = &choice.Index
				streamResp.Delta = &AnthropicStreamDelta{
					Type:     AnthropicStreamDeltaTypeThinking,
					Thinking: delta.Reasoning,
				}
			} else if len(delta.ReasoningDetails) > 0 && delta.ReasoningDetails[0].Signature != nil && *delta.ReasoningDetails[0].Signature != "" {
				// Handle signature deltas
				streamResp.Type = "content_block_delta"
				streamResp.Index = &choice.Index
				streamResp.Delta = &AnthropicStreamDelta{
					Type:      AnthropicStreamDeltaTypeSignature,
					Signature: delta.ReasoningDetails[0].Signature,
				}
			} else if len(delta.ToolCalls) > 0 {
				// Handle tool call deltas
				toolCall := delta.ToolCalls[0] // Take first tool call

				if toolCall.Function.Name != nil && *toolCall.Function.Name != "" {
					// Tool use start event
					streamResp.Type = "content_block_start"
					streamResp.Index = &choice.Index
					streamResp.ContentBlock = &AnthropicContentBlock{
						Type: AnthropicContentBlockTypeToolUse,
						ID:   toolCall.ID,
						Name: toolCall.Function.Name,
					}
				} else if toolCall.Function.Arguments != "" {
					// Tool input delta
					streamResp.Type = "content_block_delta"
					streamResp.Index = &choice.Index
					streamResp.Delta = &AnthropicStreamDelta{
						Type:        AnthropicStreamDeltaTypeInputJSON,
						PartialJSON: &toolCall.Function.Arguments,
					}
				}
			} else if choice.FinishReason != nil && *choice.FinishReason != "" {
				// Handle finish reason - map back to Anthropic format
				stopReason := ConvertKoutakuFinishReasonToAnthropic(*choice.FinishReason)
				streamResp.Type = "message_delta"
				streamResp.Delta = &AnthropicStreamDelta{
					Type:       "message_delta",
					StopReason: &stopReason,
				}
			}

		} else if choice.ChatNonStreamResponseChoice != nil {
			// Handle non-streaming response converted to streaming format
			streamResp.Type = "message_start"

			// Create message start event
			streamMessage := &AnthropicMessageResponse{
				ID:    koutakuResp.ID,
				Type:  "message",
				Role:  string(choice.ChatNonStreamResponseChoice.Message.Role),
				Model: koutakuResp.Model,
			}

			// Convert content
			var content []AnthropicContentBlock
			if choice.ChatNonStreamResponseChoice.Message.Content.ContentStr != nil {
				content = append(content, AnthropicContentBlock{
					Type: AnthropicContentBlockTypeText,
					Text: choice.ChatNonStreamResponseChoice.Message.Content.ContentStr,
				})
			}

			streamMessage.Content = content
			streamResp.Message = streamMessage
		}
	}

	// Handle usage information
	if koutakuResp.Usage != nil {
		if streamResp.Type == "" {
			streamResp.Type = "message_delta"
		}
		streamResp.Usage = &AnthropicUsage{
			InputTokens:  koutakuResp.Usage.PromptTokens,
			OutputTokens: koutakuResp.Usage.CompletionTokens,
		}
	}

	// Set common fields
	if koutakuResp.ID != "" {
		streamResp.ID = &koutakuResp.ID
	}
	if koutakuResp.Model != "" {
		if streamResp.Message == nil {
			streamResp.Message = &AnthropicMessageResponse{}
		}
		streamResp.Message.Model = koutakuResp.Model
	}

	// Default to empty content_block_delta if no specific type was set
	if streamResp.Type == "" {
		streamResp.Type = "content_block_delta"
		streamResp.Index = schemas.Ptr(0)
		streamResp.Delta = &AnthropicStreamDelta{
			Type: AnthropicStreamDeltaTypeText,
			Text: schemas.Ptr(""),
		}
	}

	// Marshal to JSON and format as SSE
	jsonData, err := providerUtils.MarshalSorted(streamResp)
	if err != nil {
		return ""
	}

	// Format as Anthropic SSE
	return fmt.Sprintf("event: %s\ndata: %s\n\n", streamResp.Type, jsonData)
}

// ToAnthropicChatStreamError converts a KoutakuError to Anthropic streaming error in SSE format
func ToAnthropicChatStreamError(koutakuErr *schemas.KoutakuError) string {
	errorResp := ToAnthropicChatCompletionError(koutakuErr)
	if errorResp == nil {
		return ""
	}
	// Marshal to JSON
	jsonData, err := providerUtils.MarshalSorted(errorResp)
	if err != nil {
		return ""
	}
	// Format as Anthropic SSE error event
	return fmt.Sprintf("event: error\ndata: %s\n\n", jsonData)
}
