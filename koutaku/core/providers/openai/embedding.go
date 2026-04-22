package openai

import (
	"github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// ToKoutakuEmbeddingRequest converts an OpenAI embedding request to Koutaku format
func (request *OpenAIEmbeddingRequest) ToKoutakuEmbeddingRequest(ctx *schemas.KoutakuContext) *schemas.KoutakuEmbeddingRequest {
	provider, model := schemas.ParseModelString(request.Model, utils.CheckAndSetDefaultProvider(ctx, schemas.OpenAI))

	return &schemas.KoutakuEmbeddingRequest{
		Provider:  provider,
		Model:     model,
		Input:     request.Input,
		Params:    &request.EmbeddingParameters,
		Fallbacks: schemas.ParseFallbacks(request.Fallbacks),
	}
}

// ToOpenAIEmbeddingRequest converts a Koutaku embedding request to OpenAI format
func ToOpenAIEmbeddingRequest(koutakuReq *schemas.KoutakuEmbeddingRequest) *OpenAIEmbeddingRequest {
	if koutakuReq == nil {
		return nil
	}

	params := koutakuReq.Params

	openaiReq := &OpenAIEmbeddingRequest{
		Model: koutakuReq.Model,
		Input: koutakuReq.Input,
	}

	// Map parameters
	if params != nil {
		openaiReq.EmbeddingParameters = *params
		openaiReq.ExtraParams = params.ExtraParams
	}
	return openaiReq
}
