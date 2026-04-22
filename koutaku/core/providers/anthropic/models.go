package anthropic

import (
	"strings"
	"time"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

func (response *AnthropicListModelsResponse) ToKoutakuListModelsResponse(providerKey schemas.ModelProvider, allowedModels schemas.WhiteList, blacklistedModels schemas.BlackList, aliases map[string]string, unfiltered bool) *schemas.KoutakuListModelsResponse {
	if response == nil {
		return nil
	}

	koutakuResponse := &schemas.KoutakuListModelsResponse{
		Data:    make([]schemas.Model, 0, len(response.Data)),
		FirstID: response.FirstID,
		LastID:  response.LastID,
		HasMore: schemas.Ptr(response.HasMore),
	}

	// Map Anthropic's cursor-based pagination to Koutaku's token-based pagination.
	// If there are more results, set next_page_token to last_id for the next request.
	if response.HasMore && response.LastID != nil {
		koutakuResponse.NextPageToken = *response.LastID
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
			resolvedKey := strings.ToLower(result.ResolvedID)
			if included[resolvedKey] {
				continue
			}
			entry := schemas.Model{
				ID:              string(providerKey) + "/" + result.ResolvedID,
				Name:            schemas.Ptr(model.DisplayName),
				Created:         schemas.Ptr(model.CreatedAt.Unix()),
				MaxInputTokens:  model.MaxInputTokens,
				MaxOutputTokens: model.MaxTokens,
				ProviderExtra:   model.Capabilities,
			}
			if result.AliasValue != "" {
				entry.Alias = schemas.Ptr(result.AliasValue)
			}
			koutakuResponse.Data = append(koutakuResponse.Data, entry)
			included[resolvedKey] = true
		}
	}

	koutakuResponse.Data = append(koutakuResponse.Data,
		pipeline.BackfillModels(included)...)

	return koutakuResponse
}

func ToAnthropicListModelsResponse(response *schemas.KoutakuListModelsResponse) *AnthropicListModelsResponse {
	if response == nil {
		return nil
	}

	anthropicResponse := &AnthropicListModelsResponse{
		Data: make([]AnthropicModel, 0, len(response.Data)),
	}
	if response.FirstID != nil {
		anthropicResponse.FirstID = response.FirstID
	}
	if response.LastID != nil {
		anthropicResponse.LastID = response.LastID
	}
	if response.HasMore != nil {
		anthropicResponse.HasMore = *response.HasMore
	}

	for _, model := range response.Data {
		_, modelID := schemas.ParseModelString(model.ID, schemas.Anthropic)
		anthropicModel := AnthropicModel{
			ID:             modelID,
			Type:           "model",
			MaxInputTokens: model.MaxInputTokens,
			MaxTokens:      model.MaxOutputTokens,
			Capabilities:   model.ProviderExtra,
		}
		if model.Name != nil {
			anthropicModel.DisplayName = *model.Name
		}
		if model.Created != nil {
			anthropicModel.CreatedAt = time.Unix(*model.Created, 0)
		}
		anthropicResponse.Data = append(anthropicResponse.Data, anthropicModel)
	}

	return anthropicResponse
}
