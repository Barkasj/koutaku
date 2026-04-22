package bedrock

import (
	"strings"

	"github.com/koutaku/koutaku/core/providers/anthropic"
	"github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// ToBedrockTextCompletionRequest converts a Koutaku text completion request to Bedrock format
func ToBedrockTextCompletionRequest(koutakuReq *schemas.KoutakuTextCompletionRequest) *BedrockTextCompletionRequest {
	if koutakuReq == nil || (koutakuReq.Input.PromptStr == nil && len(koutakuReq.Input.PromptArray) == 0) {
		return nil
	}

	// Extract the raw prompt from koutakuReq
	prompt := ""
	if koutakuReq.Input != nil {
		if koutakuReq.Input.PromptStr != nil {
			prompt = *koutakuReq.Input.PromptStr
		} else if len(koutakuReq.Input.PromptArray) > 0 && koutakuReq.Input.PromptArray != nil {
			prompt = strings.Join(koutakuReq.Input.PromptArray, "\n\n")
		}
	}

	bedrockReq := &BedrockTextCompletionRequest{
		Prompt: prompt,
	}

	// Apply parameters
	if koutakuReq.Params != nil {
		bedrockReq.Temperature = koutakuReq.Params.Temperature
		bedrockReq.TopP = koutakuReq.Params.TopP

		if koutakuReq.Params.ExtraParams != nil {
			bedrockReq.ExtraParams = koutakuReq.Params.ExtraParams
			if topK, ok := schemas.SafeExtractIntPointer(koutakuReq.Params.ExtraParams["top_k"]); ok {
				delete(bedrockReq.ExtraParams, "top_k")
				bedrockReq.TopK = topK
			}
		}
	}

	// Apply model-specific formatting and field naming
	if strings.Contains(koutakuReq.Model, "anthropic.") || strings.Contains(koutakuReq.Model, "claude") {
		// For Claude models, wrap the prompt in Anthropic format and use Anthropic field names
		anthropicReq := anthropic.ToAnthropicTextCompletionRequest(koutakuReq)
		bedrockReq.Prompt = anthropicReq.Prompt
		bedrockReq.MaxTokensToSample = &anthropicReq.MaxTokensToSample
		bedrockReq.StopSequences = anthropicReq.StopSequences
	} else {
		// For other models, use standard field names with raw prompt
		if koutakuReq.Params != nil {
			bedrockReq.MaxTokens = koutakuReq.Params.MaxTokens
			bedrockReq.Stop = koutakuReq.Params.Stop
		}
	}

	return bedrockReq
}

// ToKoutakuTextCompletionRequest converts a Bedrock text completion request to Koutaku format
func (request *BedrockTextCompletionRequest) ToKoutakuTextCompletionRequest(ctx *schemas.KoutakuContext) *schemas.KoutakuTextCompletionRequest {
	if request == nil {
		return nil
	}

	prompt := request.Prompt
	// Fallback for Claude 3 Messages API
	if prompt == "" && len(request.Messages) > 0 {
		var parts []string
		for _, msg := range request.Messages {
			for _, content := range msg.Content {
				if content.Text != nil {
					parts = append(parts, *content.Text)
				}
			}
		}
		prompt = strings.Join(parts, "\n\n")
	}

	provider, model := schemas.ParseModelString(request.ModelID, utils.CheckAndSetDefaultProvider(ctx, schemas.Bedrock))

	koutakuReq := &schemas.KoutakuTextCompletionRequest{
		Provider: provider,
		Model:    model,
		Input: &schemas.TextCompletionInput{
			PromptStr: &prompt,
		},
		Params: &schemas.TextCompletionParameters{
			Temperature: request.Temperature,
			TopP:        request.TopP,
		},
	}

	if request.MaxTokens != nil {
		koutakuReq.Params.MaxTokens = request.MaxTokens
	} else if request.MaxTokensToSample != nil {
		koutakuReq.Params.MaxTokens = request.MaxTokensToSample
	}

	if len(request.Stop) > 0 {
		koutakuReq.Params.Stop = request.Stop
	} else if len(request.StopSequences) > 0 {
		koutakuReq.Params.Stop = request.StopSequences
	}

	return koutakuReq
}

// ToKoutakuTextCompletionResponse converts a Bedrock Anthropic text response to Koutaku format
func (response *BedrockAnthropicTextResponse) ToKoutakuTextCompletionResponse() *schemas.KoutakuTextCompletionResponse {
	if response == nil {
		return nil
	}

	return &schemas.KoutakuTextCompletionResponse{
		Object: "text_completion",
		Choices: []schemas.KoutakuResponseChoice{
			{
				Index: 0,
				TextCompletionResponseChoice: &schemas.TextCompletionResponseChoice{
					Text: &response.Completion,
				},
				FinishReason: &response.StopReason,
			},
		},
		ExtraFields: schemas.KoutakuResponseExtraFields{
		},
	}
}

// ToKoutakuTextCompletionResponse converts a Bedrock Mistral text response to Koutaku format
func (response *BedrockMistralTextResponse) ToKoutakuTextCompletionResponse() *schemas.KoutakuTextCompletionResponse {
	if response == nil {
		return nil
	}

	var choices []schemas.KoutakuResponseChoice
	for i, output := range response.Outputs {
		choices = append(choices, schemas.KoutakuResponseChoice{
			Index: i,
			TextCompletionResponseChoice: &schemas.TextCompletionResponseChoice{
				Text: &output.Text,
			},
			FinishReason: &output.StopReason,
		})
	}

	return &schemas.KoutakuTextCompletionResponse{
		Object:  "text_completion",
		Choices: choices,
		ExtraFields: schemas.KoutakuResponseExtraFields{
		},
	}
}

// ToBedrockTextCompletionResponse converts a KoutakuTextCompletionResponse back to Bedrock text completion format
// Returns either *BedrockAnthropicTextResponse or *BedrockMistralTextResponse based on the model
func ToBedrockTextCompletionResponse(koutakuResp *schemas.KoutakuTextCompletionResponse) interface{} {
	if koutakuResp == nil {
		return nil
	}

	// Determine response format based on resolved model identity.
	// Use ResolvedModelUsed (actual provider ID) for accurate family detection,
	// falling back to koutakuResp.Model, then OriginalModelRequested as a last resort.
	model := koutakuResp.Model
	if koutakuResp.ExtraFields.ResolvedModelUsed != "" {
		model = koutakuResp.ExtraFields.ResolvedModelUsed
	} else if model == "" && koutakuResp.ExtraFields.OriginalModelRequested != "" {
		model = koutakuResp.ExtraFields.OriginalModelRequested
	}

	if strings.Contains(model, "anthropic.") || strings.Contains(model, "claude") {
		// Convert to Anthropic format
		bedrockResp := &BedrockAnthropicTextResponse{}

		// Convert choices to completion text
		if len(koutakuResp.Choices) > 0 {
			choice := koutakuResp.Choices[0] // Anthropic text API typically returns one choice
			if choice.TextCompletionResponseChoice != nil && choice.TextCompletionResponseChoice.Text != nil {
				bedrockResp.Completion = *choice.TextCompletionResponseChoice.Text
			}
			if choice.FinishReason != nil {
				bedrockResp.StopReason = *choice.FinishReason
			}
		}

		return bedrockResp
	} else if strings.Contains(model, "mistral.") {
		// Convert to Mistral format
		bedrockResp := &BedrockMistralTextResponse{}

		// Convert choices to outputs
		for _, choice := range koutakuResp.Choices {
			var output struct {
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
			}

			if choice.TextCompletionResponseChoice != nil && choice.TextCompletionResponseChoice.Text != nil {
				output.Text = *choice.TextCompletionResponseChoice.Text
			}
			if choice.FinishReason != nil {
				output.StopReason = *choice.FinishReason
			}

			bedrockResp.Outputs = append(bedrockResp.Outputs, output)
		}

		return bedrockResp
	}

	// Default to Anthropic format if model type cannot be determined
	bedrockResp := &BedrockAnthropicTextResponse{}
	if len(koutakuResp.Choices) > 0 {
		choice := koutakuResp.Choices[0]
		if choice.TextCompletionResponseChoice != nil && choice.TextCompletionResponseChoice.Text != nil {
			bedrockResp.Completion = *choice.TextCompletionResponseChoice.Text
		}
		if choice.FinishReason != nil {
			bedrockResp.StopReason = *choice.FinishReason
		}
	}

	return bedrockResp
}
