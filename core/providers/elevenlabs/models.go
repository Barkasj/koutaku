package elevenlabs

import (
	"strings"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

func (response *ElevenlabsListModelsResponse) ToKoutakuListModelsResponse(providerKey schemas.ModelProvider, allowedModels schemas.WhiteList, blacklistedModels schemas.BlackList, aliases map[string]string, unfiltered bool) *schemas.KoutakuListModelsResponse {
	if response == nil {
		return nil
	}

	koutakuResponse := &schemas.KoutakuListModelsResponse{
		Data: make([]schemas.Model, 0, len(*response)),
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

	for _, model := range *response {
		for _, result := range pipeline.FilterModel(model.ModelID) {
			entry := schemas.Model{
				ID:   string(providerKey) + "/" + result.ResolvedID,
				Name: schemas.Ptr(model.Name),
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
