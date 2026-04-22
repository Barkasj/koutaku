package openai

import (
	"maps"

	"github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// ToOpenAITextCompletionRequest converts a Koutaku text completion request to OpenAI format
func ToOpenAITextCompletionRequest(koutakuReq *schemas.KoutakuTextCompletionRequest) *OpenAITextCompletionRequest {
	if koutakuReq == nil {
		return nil
	}
	params := koutakuReq.Params
	openaiReq := &OpenAITextCompletionRequest{
		Model:  koutakuReq.Model,
		Prompt: koutakuReq.Input,
	}
	if params != nil {
		openaiReq.TextCompletionParameters = *params
		// Drop user field if it exceeds OpenAI's 64 character limit
		openaiReq.TextCompletionParameters.User = SanitizeUserField(openaiReq.TextCompletionParameters.User)
		if koutakuReq.Params.ExtraParams != nil {
			openaiReq.ExtraParams = maps.Clone(koutakuReq.Params.ExtraParams)
			openaiReq.TextCompletionParameters.ExtraParams = openaiReq.ExtraParams
		}
	}
	if koutakuReq.Provider == schemas.Fireworks {
		openaiReq.applyFireworksTextCompletionCompatibility()
	}
	return openaiReq
}

// applyFireworksTextCompletionCompatibility maps Fireworks-specific text fields.
func (req *OpenAITextCompletionRequest) applyFireworksTextCompletionCompatibility() {
	if req == nil || req.ExtraParams == nil {
		return
	}

	// Fireworks uses prompt_cache_isolation_key for text-completion cache isolation.
	if req.PromptCacheIsolationKey == nil {
		if value, ok := req.ExtraParams["prompt_cache_key"]; ok {
			switch typed := value.(type) {
			case string:
				if typed != "" {
					req.PromptCacheIsolationKey = &typed
				}
			case *string:
				if typed != nil && *typed != "" {
					req.PromptCacheIsolationKey = typed
				}
			}
		}
	}
	delete(req.ExtraParams, "prompt_cache_key")
	req.TextCompletionParameters.ExtraParams = req.ExtraParams
}

// ToKoutakuTextCompletionRequest converts an OpenAI text completion request to Koutaku format
func (req *OpenAITextCompletionRequest) ToKoutakuTextCompletionRequest(ctx *schemas.KoutakuContext) *schemas.KoutakuTextCompletionRequest {
	if req == nil {
		return nil
	}

	provider, model := schemas.ParseModelString(req.Model, utils.CheckAndSetDefaultProvider(ctx, schemas.OpenAI))

	return &schemas.KoutakuTextCompletionRequest{
		Provider:  provider,
		Model:     model,
		Input:     req.Prompt,
		Params:    &req.TextCompletionParameters,
		Fallbacks: schemas.ParseFallbacks(req.Fallbacks),
	}
}
