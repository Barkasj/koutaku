package cohere

import (
	"fmt"
	"time"

	"github.com/koutaku/koutaku/core/providers/anthropic"
	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// ToCohereChatCompletionRequest converts a Koutaku request to Cohere v2 format
func ToCohereChatCompletionRequest(koutakuReq *schemas.KoutakuChatRequest) (*CohereChatRequest, error) {
	if koutakuReq == nil || koutakuReq.Input == nil {
		return nil, fmt.Errorf("koutaku request is nil")
	}

	messages := koutakuReq.Input
	cohereReq := &CohereChatRequest{
		Model: koutakuReq.Model,
	}

	// Convert messages to Cohere v2 format
	var cohereMessages []CohereMessage
	for _, msg := range messages {
		cohereMsg := CohereMessage{
			Role: string(msg.Role),
		}

		// Convert content
		if msg.Content != nil && msg.Content.ContentStr != nil {
			cohereMsg.Content = NewStringContent(*msg.Content.ContentStr)
		} else if msg.Content != nil && msg.Content.ContentBlocks != nil {
			var contentBlocks []CohereContentBlock
			for _, block := range msg.Content.ContentBlocks {
				if block.Text != nil {
					contentBlocks = append(contentBlocks, CohereContentBlock{
						Type: CohereContentBlockTypeText,
						Text: block.Text,
					})
				} else if block.ImageURLStruct != nil {
					contentBlocks = append(contentBlocks, CohereContentBlock{
						Type: CohereContentBlockTypeImage,
						ImageURL: &CohereImageURL{
							URL: block.ImageURLStruct.URL,
						},
					})
				}
			}
			if len(contentBlocks) > 0 {
				cohereMsg.Content = NewBlocksContent(contentBlocks)
			}
		}

		// Convert tool calls for assistant messages
		if msg.ChatAssistantMessage != nil && msg.ChatAssistantMessage.ToolCalls != nil {
			var toolCalls []CohereToolCall
			for _, toolCall := range msg.ChatAssistantMessage.ToolCalls {
				// Safely extract function name and arguments
				var functionName *string
				var functionArguments string

				if toolCall.Function.Name != nil {
					functionName = toolCall.Function.Name
				} else {
					// Use empty string if Name is nil
					functionName = schemas.Ptr("")
				}

				// Arguments is a string, not a pointer, so it's safe to access directly
				// Default to "{}" if empty to ensure the field is always present.
				if toolCall.Function.Arguments == "" {
					functionArguments = "{}"
				} else {
					functionArguments = toolCall.Function.Arguments
				}

				cohereToolCall := CohereToolCall{
					ID:   toolCall.ID,
					Type: "function",
					Function: &CohereFunction{
						Name:      functionName,
						Arguments: functionArguments,
					},
				}
				toolCalls = append(toolCalls, cohereToolCall)
			}
			cohereMsg.ToolCalls = toolCalls
		}

		// Convert tool messages
		if msg.ChatToolMessage != nil && msg.ChatToolMessage.ToolCallID != nil {
			cohereMsg.ToolCallID = msg.ChatToolMessage.ToolCallID
		}

		cohereMessages = append(cohereMessages, cohereMsg)
	}

	cohereReq.Messages = cohereMessages

	// Convert parameters
	if koutakuReq.Params != nil {
		cohereReq.MaxTokens = koutakuReq.Params.MaxCompletionTokens
		cohereReq.Temperature = koutakuReq.Params.Temperature
		cohereReq.P = koutakuReq.Params.TopP
		cohereReq.StopSequences = koutakuReq.Params.Stop
		cohereReq.FrequencyPenalty = koutakuReq.Params.FrequencyPenalty
		cohereReq.PresencePenalty = koutakuReq.Params.PresencePenalty

		// Convert reasoning
		if koutakuReq.Params.Reasoning != nil {
			if koutakuReq.Params.Reasoning.MaxTokens != nil {
				thinking := &CohereThinking{
					Type: ThinkingTypeEnabled,
				}
				if *koutakuReq.Params.Reasoning.MaxTokens == -1 {
					// cohere does not support dynamic reasoning budget like gemini
					// setting it to minimum reasoning budget
					thinking.TokenBudget = schemas.Ptr(anthropic.MinimumReasoningMaxTokens)
				} else {
					thinking.TokenBudget = koutakuReq.Params.Reasoning.MaxTokens
				}
				cohereReq.Thinking = thinking
			} else if koutakuReq.Params.Reasoning.Effort != nil {
				if *koutakuReq.Params.Reasoning.Effort != "none" {
					maxCompletionTokens := providerUtils.GetMaxOutputTokensOrDefault(koutakuReq.Model, DefaultCompletionMaxTokens)
					if koutakuReq.Params.MaxCompletionTokens != nil {
						maxCompletionTokens = *koutakuReq.Params.MaxCompletionTokens
					}
					budgetTokens, err := providerUtils.GetBudgetTokensFromReasoningEffort(*koutakuReq.Params.Reasoning.Effort, MinimumReasoningMaxTokens, maxCompletionTokens)
					if err != nil {
						return nil, err
					}
					cohereReq.Thinking = &CohereThinking{
						Type:        ThinkingTypeEnabled,
						TokenBudget: schemas.Ptr(budgetTokens), // Max tokens for reasoning
					}
				} else {
					cohereReq.Thinking = &CohereThinking{
						Type: ThinkingTypeDisabled,
					}
				}
			}
		}

		// Convert response format
		if koutakuReq.Params.ResponseFormat != nil {
			cohereReq.ResponseFormat = convertResponseFormatToCohere(koutakuReq.Params.ResponseFormat)
		}

		// Convert extra params
		if koutakuReq.Params.ExtraParams != nil {
			// Handle thinking parameter
			cohereReq.ExtraParams = koutakuReq.Params.ExtraParams
			if thinkingParam, ok := schemas.SafeExtractFromMap(koutakuReq.Params.ExtraParams, "thinking"); ok {
				if thinkingMap, ok := thinkingParam.(map[string]interface{}); ok {
					thinking := &CohereThinking{}
					if typeStr, ok := schemas.SafeExtractString(thinkingMap["type"]); ok {
						delete(thinkingMap, "type")
						thinking.Type = CohereThinkingType(typeStr)
					}
					if tokenBudget, ok := schemas.SafeExtractIntPointer(thinkingMap["token_budget"]); ok {
						delete(thinkingMap, "token_budget")
						thinking.TokenBudget = tokenBudget
					}
					cohereReq.Thinking = thinking
					cohereReq.ExtraParams["thinking"] = thinkingMap
				}
			}

			// Handle other Cohere-specific extra params
			if safetyMode, ok := schemas.SafeExtractStringPointer(koutakuReq.Params.ExtraParams["safety_mode"]); ok {
				delete(cohereReq.ExtraParams, "safety_mode")
				cohereReq.SafetyMode = safetyMode
			}

			if logProbs, ok := schemas.SafeExtractBoolPointer(koutakuReq.Params.ExtraParams["log_probs"]); ok {
				delete(cohereReq.ExtraParams, "log_probs")
				cohereReq.LogProbs = logProbs
			}

			if strictToolChoice, ok := schemas.SafeExtractBoolPointer(koutakuReq.Params.ExtraParams["strict_tool_choice"]); ok {
				delete(cohereReq.ExtraParams, "strict_tool_choice")
				cohereReq.StrictToolChoice = strictToolChoice
			}
		}

		// Convert tools to Cohere-specific format (without "strict" field)
		if koutakuReq.Params.Tools != nil {
			cohereTools := make([]CohereChatRequestTool, len(koutakuReq.Params.Tools))
			for i, tool := range koutakuReq.Params.Tools {
				cohereTools[i] = CohereChatRequestTool{
					Type: string(tool.Type),
				}
				if tool.Function != nil {
					cohereTools[i].Function = CohereChatRequestFunction{
						Name:        tool.Function.Name,
						Description: tool.Function.Description,
						Parameters:  tool.Function.Parameters, // Convert to map
						// Note: No "strict" field - Cohere doesn't support it
					}
				}
			}
			cohereReq.Tools = cohereTools
		}

		// Convert tool choice
		if koutakuReq.Params.ToolChoice != nil {
			toolChoice := koutakuReq.Params.ToolChoice

			if toolChoice.ChatToolChoiceStr != nil {
				switch schemas.ChatToolChoiceType(*toolChoice.ChatToolChoiceStr) {
				case schemas.ChatToolChoiceTypeNone:
					toolChoice := ToolChoiceNone
					cohereReq.ToolChoice = &toolChoice
				default:
					toolChoice := ToolChoiceRequired
					cohereReq.ToolChoice = &toolChoice
				}
			} else if toolChoice.ChatToolChoiceStruct != nil {
				switch toolChoice.ChatToolChoiceStruct.Type {
				case schemas.ChatToolChoiceTypeFunction:
					toolChoice := ToolChoiceRequired
					cohereReq.ToolChoice = &toolChoice
				default:
					toolChoice := ToolChoiceAuto
					cohereReq.ToolChoice = &toolChoice
				}
			}
		}
	}

	return cohereReq, nil
}

// ToKoutakuChatRequest converts a Cohere v2 chat request to Koutaku format
func (req *CohereChatRequest) ToKoutakuChatRequest(ctx *schemas.KoutakuContext) *schemas.KoutakuChatRequest {
	if req == nil {
		return nil
	}

	provider, model := schemas.ParseModelString(req.Model, providerUtils.CheckAndSetDefaultProvider(ctx, schemas.Cohere))

	koutakuReq := &schemas.KoutakuChatRequest{
		Provider: provider,
		Model:    model,
		Params:   &schemas.ChatParameters{},
	}
	// Convert messages
	if req.Messages != nil {
		koutakuMessages := make([]schemas.ChatMessage, len(req.Messages))
		for i, message := range req.Messages {
			koutakuMessages[i] = *message.ToKoutakuChatMessage()
		}
		koutakuReq.Input = koutakuMessages
	}
	// Convert parameters
	if req.MaxTokens != nil {
		koutakuReq.Params.MaxCompletionTokens = req.MaxTokens
	}
	if req.Temperature != nil {
		koutakuReq.Params.Temperature = req.Temperature
	}
	if req.P != nil {
		koutakuReq.Params.TopP = req.P
	}
	if req.StopSequences != nil {
		koutakuReq.Params.Stop = req.StopSequences
	}
	if req.FrequencyPenalty != nil {
		koutakuReq.Params.FrequencyPenalty = req.FrequencyPenalty
	}
	if req.PresencePenalty != nil {
		koutakuReq.Params.PresencePenalty = req.PresencePenalty
	}

	// Convert reasoning
	if req.Thinking != nil {
		if req.Thinking.Type == ThinkingTypeDisabled {
			koutakuReq.Params.Reasoning = &schemas.ChatReasoning{
				Effort: schemas.Ptr("none"),
			}
		} else {
			koutakuReq.Params.Reasoning = &schemas.ChatReasoning{
				Effort: schemas.Ptr("auto"),
			}
			if req.Thinking.TokenBudget != nil {
				koutakuReq.Params.Reasoning.MaxTokens = req.Thinking.TokenBudget
			}
		}
	}
	if req.ResponseFormat != nil {
		koutakuReq.Params.ResponseFormat = convertCohereResponseFormatToKoutaku(req.ResponseFormat)
	}

	// Convert tools
	if req.Tools != nil {
		koutakuTools := make([]schemas.ChatTool, len(req.Tools))
		for i, tool := range req.Tools {
			koutakuTools[i] = schemas.ChatTool{
				Type: schemas.ChatToolTypeFunction,
				Function: &schemas.ChatToolFunction{
					Name:        tool.Function.Name,
					Description: tool.Function.Description,
					Parameters:  convertInterfaceToToolFunctionParameters(tool.Function.Parameters),
				},
			}
		}
		koutakuReq.Params.Tools = koutakuTools
	}

	// Convert tool choice
	if req.ToolChoice != nil {
		switch *req.ToolChoice {
		case ToolChoiceNone:
			koutakuReq.Params.ToolChoice = &schemas.ChatToolChoice{
				ChatToolChoiceStr: schemas.Ptr(string(schemas.ChatToolChoiceTypeNone)),
			}
		case ToolChoiceRequired:
			koutakuReq.Params.ToolChoice = &schemas.ChatToolChoice{
				ChatToolChoiceStr: schemas.Ptr(string(schemas.ChatToolChoiceTypeRequired)),
			}
		case ToolChoiceAuto:
			koutakuReq.Params.ToolChoice = &schemas.ChatToolChoice{
				ChatToolChoiceStr: schemas.Ptr(string(schemas.ChatToolChoiceTypeAny)),
			}
		}
	}

	// Convert extra params
	extraParams := make(map[string]interface{})
	if req.SafetyMode != nil {
		extraParams["safety_mode"] = *req.SafetyMode
	}
	if req.LogProbs != nil {
		extraParams["log_probs"] = *req.LogProbs
	}
	if req.StrictToolChoice != nil {
		extraParams["strict_tool_choice"] = *req.StrictToolChoice
	}
	if req.Thinking != nil {
		thinkingMap := map[string]interface{}{
			"type": string(req.Thinking.Type),
		}
		if req.Thinking.TokenBudget != nil {
			thinkingMap["token_budget"] = *req.Thinking.TokenBudget
		}
		extraParams["thinking"] = thinkingMap
	}
	if len(extraParams) > 0 {
		koutakuReq.Params.ExtraParams = extraParams
	}

	return koutakuReq
}

// ToKoutakuChatResponse converts a Cohere v2 response to Koutaku format
func (response *CohereChatResponse) ToKoutakuChatResponse(model string) *schemas.KoutakuChatResponse {
	if response == nil {
		return nil
	}

	koutakuResponse := &schemas.KoutakuChatResponse{
		ID:     response.ID,
		Model:  model,
		Object: "chat.completion",
		Choices: []schemas.KoutakuResponseChoice{
			{
				Index:                       0,
				ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{},
			},
		},
		Created: int(time.Now().Unix()),
		ExtraFields: schemas.KoutakuResponseExtraFields{
		},
	}

	// Convert messages
	if response.Message != nil {
		koutakuMessage := response.Message.ToKoutakuChatMessage()
		koutakuResponse.Choices[0].ChatNonStreamResponseChoice.Message = koutakuMessage
	}

	// Convert finish reason
	if response.FinishReason != nil {
		finishReason := ConvertCohereFinishReasonToKoutaku(*response.FinishReason)
		koutakuResponse.Choices[0].FinishReason = schemas.Ptr(finishReason)
	}

	// Convert usage information
	if response.Usage != nil {
		usage := &schemas.KoutakuLLMUsage{}

		if response.Usage.Tokens != nil {
			if response.Usage.Tokens.InputTokens != nil {
				usage.PromptTokens = *response.Usage.Tokens.InputTokens
			}
			if response.Usage.Tokens.OutputTokens != nil {
				usage.CompletionTokens = *response.Usage.Tokens.OutputTokens
			}
			if response.Usage.CachedTokens != nil {
				usage.PromptTokensDetails = &schemas.ChatPromptTokensDetails{
					CachedReadTokens: *response.Usage.CachedTokens,
				}
			}
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}

		koutakuResponse.Usage = usage
	}

	return koutakuResponse
}

func (chunk *CohereStreamEvent) ToKoutakuChatCompletionStream() (*schemas.KoutakuChatResponse, *schemas.KoutakuError, bool) {
	switch chunk.Type {
	case StreamEventMessageStart:
		if chunk.Delta != nil && chunk.Delta.Message != nil && chunk.Delta.Message.Role != nil {
			// Create streaming response for this delta
			streamResponse := &schemas.KoutakuChatResponse{
				Object: "chat.completion.chunk",
				Choices: []schemas.KoutakuResponseChoice{
					{
						Index: 0,
						ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
							Delta: &schemas.ChatStreamResponseChoiceDelta{
								Role: chunk.Delta.Message.Role,
							},
						},
					},
				},
			}

			return streamResponse, nil, false
		}

	case StreamEventContentDelta:
		if chunk.Delta != nil &&
			chunk.Delta.Message != nil &&
			chunk.Delta.Message.Content != nil &&
			chunk.Delta.Message.Content.CohereStreamContentObject != nil {
			if chunk.Delta.Message.Content.CohereStreamContentObject.Text != nil {
				// Try to cast content to CohereStreamContent
				streamResponse := &schemas.KoutakuChatResponse{
					Object: "chat.completion.chunk",
					Choices: []schemas.KoutakuResponseChoice{
						{
							Index: 0,
							ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
								Delta: &schemas.ChatStreamResponseChoiceDelta{
									Content: chunk.Delta.Message.Content.CohereStreamContentObject.Text,
								},
							},
						},
					},
				}

				return streamResponse, nil, false
			} else if chunk.Delta.Message.Content.CohereStreamContentObject.Thinking != nil {
				thinkingText := *chunk.Delta.Message.Content.CohereStreamContentObject.Thinking
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
		}

	case StreamEventToolPlanDelta:
		if chunk.Delta != nil && chunk.Delta.Message != nil && chunk.Delta.Message.ToolPlan != nil {
			streamResponse := &schemas.KoutakuChatResponse{
				Object: "chat.completion.chunk",
				Choices: []schemas.KoutakuResponseChoice{
					{
						Index: 0,
						ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
							Delta: &schemas.ChatStreamResponseChoiceDelta{
								Reasoning: chunk.Delta.Message.ToolPlan,
							},
						},
					},
				},
			}

			return streamResponse, nil, false
		}

	case StreamEventContentStart:
		// Content start event - just continue, actual content comes in content-delta
		return nil, nil, false

	case StreamEventToolCallStart, StreamEventToolCallDelta:
		if chunk.Delta != nil && chunk.Delta.Message != nil && chunk.Delta.Message.ToolCalls != nil && chunk.Delta.Message.ToolCalls.CohereToolCallObject != nil {
			// Handle single tool call object (tool-call-start/delta events)
			cohereToolCall := chunk.Delta.Message.ToolCalls.CohereToolCallObject
			toolCall := schemas.ChatAssistantMessageToolCall{}

			if chunk.Index != nil {
				toolCall.Index = uint16(*chunk.Index)
			}

			if cohereToolCall.ID != nil {
				toolCall.ID = cohereToolCall.ID
			}

			if cohereToolCall.Function != nil {
				if cohereToolCall.Function.Name != nil {
					toolCall.Function.Name = cohereToolCall.Function.Name
				}
				toolCall.Function.Arguments = cohereToolCall.Function.Arguments
			}

			streamResponse := &schemas.KoutakuChatResponse{
				Object: "chat.completion.chunk",
				Choices: []schemas.KoutakuResponseChoice{
					{
						Index: 0,
						ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
							Delta: &schemas.ChatStreamResponseChoiceDelta{
								ToolCalls: []schemas.ChatAssistantMessageToolCall{toolCall},
							},
						},
					},
				},
			}

			return streamResponse, nil, false
		}

	case StreamEventToolCallEnd:
		return nil, nil, false

	case StreamEventContentEnd:
		return nil, nil, false

	case StreamEventMessageEnd:
		if chunk.Delta != nil {
			var finishReason string
			usage := &schemas.KoutakuLLMUsage{}
			// Set finish reason
			if chunk.Delta.FinishReason != nil {
				finishReason = ConvertCohereFinishReasonToKoutaku(*chunk.Delta.FinishReason)
			}

			// Set usage information
			if chunk.Delta.Usage != nil {
				if chunk.Delta.Usage.Tokens != nil {
					if chunk.Delta.Usage.Tokens.InputTokens != nil {
						usage.PromptTokens = *chunk.Delta.Usage.Tokens.InputTokens
					}
					if chunk.Delta.Usage.Tokens.OutputTokens != nil {
						usage.CompletionTokens = *chunk.Delta.Usage.Tokens.OutputTokens
					}
					usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
				}
			}

			streamResponse := &schemas.KoutakuChatResponse{
				Object: "chat.completion.chunk",
				Choices: []schemas.KoutakuResponseChoice{
					{
						Index:        0,
						FinishReason: &finishReason,
						ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
							Delta: &schemas.ChatStreamResponseChoiceDelta{},
						},
					},
				},
				Usage: usage,
			}

			return streamResponse, nil, true
		}
		return nil, nil, false
	}

	return nil, nil, false
}

func (cm *CohereMessage) ToKoutakuChatMessage() *schemas.ChatMessage {
	if cm == nil {
		return nil
	}

	var content *string
	var contentBlocks []schemas.ChatContentBlock
	var toolCalls []schemas.ChatAssistantMessageToolCall
	var reasoningDetails []schemas.ChatReasoningDetails
	var reasoningText string

	// Convert message content
	if cm.Content != nil {
		if cm.Content.IsString() ||
			(cm.Content.IsBlocks() &&
				len(cm.Content.GetBlocks()) == 1 &&
				cm.Content.GetBlocks()[0].Type == CohereContentBlockTypeText) {
			if cm.Content.IsString() {
				content = cm.Content.GetString()
			} else {
				content = cm.Content.GetBlocks()[0].Text
			}
		} else if cm.Content.IsBlocks() {
			for _, block := range cm.Content.GetBlocks() {
				if block.Type == CohereContentBlockTypeText && block.Text != nil {
					contentBlocks = append(contentBlocks, schemas.ChatContentBlock{
						Type: schemas.ChatContentBlockTypeText,
						Text: block.Text,
					})
				} else if block.Type == CohereContentBlockTypeImage && block.ImageURL != nil {
					contentBlocks = append(contentBlocks, schemas.ChatContentBlock{
						Type: schemas.ChatContentBlockTypeImage,
						ImageURLStruct: &schemas.ChatInputImage{
							URL: block.ImageURL.URL,
						},
					})
				} else if block.Type == CohereContentBlockTypeThinking && block.Thinking != nil {
					reasoningDetails = append(reasoningDetails, schemas.ChatReasoningDetails{
						Index: len(reasoningDetails),
						Type:  schemas.KoutakuReasoningDetailsTypeText,
						Text:  block.Thinking,
					})
					if len(reasoningText) > 0 {
						reasoningText += "\n"
					}
					reasoningText += *block.Thinking
				}
			}
		}
	}

	if len(contentBlocks) == 1 && contentBlocks[0].Type == schemas.ChatContentBlockTypeText {
		content = contentBlocks[0].Text
		contentBlocks = nil
	}

	// Create the message content
	messageContent := &schemas.ChatMessageContent{
		ContentStr:    content,
		ContentBlocks: contentBlocks,
	}

	// Convert tool calls
	if cm.ToolCalls != nil {
		for _, toolCall := range cm.ToolCalls {
			// Check if Function is nil to avoid nil pointer dereference
			if toolCall.Function == nil {
				// Skip this tool call if Function is nil
				continue
			}

			// Safely extract function name and arguments
			var functionName *string
			var functionArguments string

			if toolCall.Function.Name != nil {
				functionName = toolCall.Function.Name
			} else {
				// Use empty string if Name is nil
				functionName = schemas.Ptr("")
			}

			// Arguments is a string, not a pointer, so it's safe to access directly
			functionArguments = toolCall.Function.Arguments

			koutakuToolCall := schemas.ChatAssistantMessageToolCall{
				Index: uint16(len(toolCalls)),
				ID:    toolCall.ID,
				Function: schemas.ChatAssistantMessageToolCallFunction{
					Name:      functionName,
					Arguments: functionArguments,
				},
			}
			toolCalls = append(toolCalls, koutakuToolCall)
		}
	}

	// Create assistant message if we have tool calls
	var assistantMessage *schemas.ChatAssistantMessage
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
		assistantMessage.Reasoning = schemas.Ptr(reasoningText)
	}

	koutakuMessage := &schemas.ChatMessage{
		Role:                 schemas.ChatMessageRole(cm.Role),
		Content:              messageContent,
		ChatAssistantMessage: assistantMessage,
	}

	if cm.Role == "tool" {
		koutakuMessage.ChatToolMessage = &schemas.ChatToolMessage{
			ToolCallID: cm.ToolCallID,
		}
	}
	return koutakuMessage
}
