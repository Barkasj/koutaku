package openai

import (
	"github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// ToKoutakuSpeechRequest converts an OpenAI speech request to Koutaku format
func (request *OpenAISpeechRequest) ToKoutakuSpeechRequest(ctx *schemas.KoutakuContext) *schemas.KoutakuSpeechRequest {
	provider, model := schemas.ParseModelString(request.Model, utils.CheckAndSetDefaultProvider(ctx, schemas.OpenAI))

	return &schemas.KoutakuSpeechRequest{
		Provider:  provider,
		Model:     model,
		Input:     &schemas.SpeechInput{Input: request.Input},
		Params:    &request.SpeechParameters,
		Fallbacks: schemas.ParseFallbacks(request.Fallbacks),
	}
}

// ToOpenAISpeechRequest converts a Koutaku speech request to OpenAI format
func ToOpenAISpeechRequest(koutakuReq *schemas.KoutakuSpeechRequest) *OpenAISpeechRequest {
	if koutakuReq == nil || koutakuReq.Input.Input == "" {
		return nil
	}

	speechInput := koutakuReq.Input
	params := koutakuReq.Params

	openaiReq := &OpenAISpeechRequest{
		Model: koutakuReq.Model,
		Input: speechInput.Input,
	}

	if params != nil {
		openaiReq.SpeechParameters = *params
	}

	if koutakuReq.Params != nil {
		openaiReq.ExtraParams = koutakuReq.Params.ExtraParams
	}
	return openaiReq
}
