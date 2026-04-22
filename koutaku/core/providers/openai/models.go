package openai

import (
	"strings"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// ToKoutakuListModelsResponse converts an OpenAI list models response to a Koutaku list models response
func (response *OpenAIListModelsResponse) ToKoutakuListModelsResponse(providerKey schemas.ModelProvider, allowedModels schemas.WhiteList, blacklistedModels schemas.BlackList, aliases map[string]string, unfiltered bool) *schemas.KoutakuListModelsResponse {
	if response == nil {
		return nil
	}

	koutakuResponse := &schemas.KoutakuListModelsResponse{
		Data: make([]schemas.Model, 0, len(response.Data)),
	}

	pipeline := &providerUtils.ListModelsPipeline{
		AllowedModels:     allowedModels,
		BlacklistedModels: blacklistedModels,
		Aliases:           aliases,
		Unfiltered:        unfiltered,
		ProviderKey:       providerKey,
		MatchFns:          providerUtils.DefaultMatchFns(),
	}
	if pipeline.ShouldEarlyExit() {
		return koutakuResponse
	}

	included := make(map[string]bool)

	for _, model := range response.Data {
		for _, result := range pipeline.FilterModel(model.ID) {
			entry := schemas.Model{
				ID:            string(providerKey) + "/" + result.ResolvedID,
				Created:       model.Created,
				OwnedBy:       schemas.Ptr(model.OwnedBy),
				ContextLength: model.ContextWindow,
			}
			if result.AliasValue != "" {
				entry.Alias = schemas.Ptr(result.AliasValue)
			}
			koutakuResponse.Data = append(koutakuResponse.Data, entry)
			included[strings.ToLower(result.ResolvedID)] = true
		}
	}

	koutakuResponse.Data = append(koutakuResponse.Data,
		pipeline.BackfillModels(included)...)

	return koutakuResponse
}

// ToOpenAIListModelsResponse converts a Koutaku list models response to an OpenAI list models response
func ToOpenAIListModelsResponse(response *schemas.KoutakuListModelsResponse) *OpenAIListModelsResponse {
	if response == nil {
		return nil
	}
	openaiResponse := &OpenAIListModelsResponse{
		Data: make([]OpenAIModel, 0, len(response.Data)),
	}
	for _, model := range response.Data {
		openaiModel := OpenAIModel{
			ID:     model.ID,
			Object: "model",
		}
		if model.Created != nil {
			openaiModel.Created = model.Created
		}
		if model.OwnedBy != nil {
			openaiModel.OwnedBy = *model.OwnedBy
		}

		openaiResponse.Data = append(openaiResponse.Data, openaiModel)

	}
	return openaiResponse
}
