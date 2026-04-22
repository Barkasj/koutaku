package anthropic

import (
	"fmt"
	"strings"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// ToAnthropicTextCompletionRequest converts a Koutaku text completion request to Anthropic format
func ToAnthropicTextCompletionRequest(koutakuReq *schemas.KoutakuTextCompletionRequest) *AnthropicTextRequest {
	if koutakuReq == nil {
		return nil
	}

	prompt := ""
	if koutakuReq.Input.PromptStr != nil {
		prompt = *koutakuReq.Input.PromptStr
	} else if len(koutakuReq.Input.PromptArray) > 0 {
		prompt = strings.Join(koutakuReq.Input.PromptArray, "\n\n")
	}

	anthropicReq := &AnthropicTextRequest{
		Model:             koutakuReq.Model,
		Prompt:            fmt.Sprintf("\n\nHuman: %s\n\nAssistant:", prompt),
		MaxTokensToSample: providerUtils.GetMaxOutputTokensOrDefault(koutakuReq.Model, AnthropicDefaultMaxTokens),
	}

	// Convert parameters
	if koutakuReq.Params != nil {
		if koutakuReq.Params.MaxTokens != nil {
			anthropicReq.MaxTokensToSample = *koutakuReq.Params.MaxTokens
		}
		anthropicReq.Temperature = koutakuReq.Params.Temperature
		anthropicReq.TopP = koutakuReq.Params.TopP
		anthropicReq.StopSequences = koutakuReq.Params.Stop

		if koutakuReq.Params.ExtraParams != nil {
			anthropicReq.ExtraParams = koutakuReq.Params.ExtraParams
			if topK, ok := schemas.SafeExtractIntPointer(koutakuReq.Params.ExtraParams["top_k"]); ok {
				delete(anthropicReq.ExtraParams, "top_k")
				anthropicReq.TopK = topK
			}
		}
	}

	return anthropicReq
}

// ToKoutakuTextCompletionRequest converts an Anthropic text request back to Koutaku format
func (req *AnthropicTextRequest) ToKoutakuTextCompletionRequest(ctx *schemas.KoutakuContext) *schemas.KoutakuTextCompletionRequest {
	if req == nil {
		return nil
	}

	provider, model := schemas.ParseModelString(req.Model, providerUtils.CheckAndSetDefaultProvider(ctx, schemas.Anthropic))

	koutakuReq := &schemas.KoutakuTextCompletionRequest{
		Provider: provider,
		Model:    model,
		Input: &schemas.TextCompletionInput{
			PromptStr: &req.Prompt,
		},
		Params: &schemas.TextCompletionParameters{
			MaxTokens:   &req.MaxTokensToSample,
			Temperature: req.Temperature,
			TopP:        req.TopP,
			Stop:        req.StopSequences,
		},
		Fallbacks: schemas.ParseFallbacks(req.Fallbacks),
	}

	// Add extra params if present
	if req.TopK != nil {
		koutakuReq.Params.ExtraParams = map[string]interface{}{
			"top_k": *req.TopK,
		}
	}

	return koutakuReq
}

// ToKoutakuTextCompletionResponse converts an Anthropic text response back to Koutaku format
func (response *AnthropicTextResponse) ToKoutakuTextCompletionResponse() *schemas.KoutakuTextCompletionResponse {
	if response == nil {
		return nil
	}
	return &schemas.KoutakuTextCompletionResponse{
		ID:     response.ID,
		Object: "text_completion",
		Choices: []schemas.KoutakuResponseChoice{
			{
				Index: 0,
				TextCompletionResponseChoice: &schemas.TextCompletionResponseChoice{
					Text: &response.Completion,
				},
			},
		},
		Usage: &schemas.KoutakuLLMUsage{
			PromptTokens:     response.Usage.InputTokens,
			CompletionTokens: response.Usage.OutputTokens,
			TotalTokens:      response.Usage.InputTokens + response.Usage.OutputTokens,
		},
		Model: response.Model,
	}
}

// ToAnthropicTextCompletionResponse converts a KoutakuResponse back to Anthropic text completion format
func ToAnthropicTextCompletionResponse(koutakuResp *schemas.KoutakuTextCompletionResponse) *AnthropicTextResponse {
	if koutakuResp == nil {
		return nil
	}

	anthropicResp := &AnthropicTextResponse{
		ID:    koutakuResp.ID,
		Type:  "completion",
		Model: koutakuResp.Model,
	}

	// Convert choices to completion text
	if len(koutakuResp.Choices) > 0 {
		choice := koutakuResp.Choices[0] // Anthropic text API typically returns one choice

		if choice.TextCompletionResponseChoice != nil && choice.TextCompletionResponseChoice.Text != nil {
			anthropicResp.Completion = *choice.TextCompletionResponseChoice.Text
		}
	}

	// Convert usage information
	if koutakuResp.Usage != nil {
		anthropicResp.Usage.InputTokens = koutakuResp.Usage.PromptTokens
		anthropicResp.Usage.OutputTokens = koutakuResp.Usage.CompletionTokens
	}

	return anthropicResp
}
