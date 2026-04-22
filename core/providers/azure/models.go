package azure

import (
	"strings"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

func (response *AzureListModelsResponse) ToKoutakuListModelsResponse(allowedModels schemas.WhiteList, blacklistedModels schemas.BlackList, aliases map[string]string, unfiltered bool) *schemas.KoutakuListModelsResponse {
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
		ProviderKey:       schemas.Azure,
		MatchFns:          providerUtils.DefaultMatchFns(),
	}
	if pipeline.ShouldEarlyExit() {
		return koutakuResponse
	}

	included := make(map[string]bool)

	for _, model := range response.Data {
		for _, result := range pipeline.FilterModel(model.ID) {
			entry := schemas.Model{
				ID:      string(schemas.Azure) + "/" + result.ResolvedID,
				Created: schemas.Ptr(model.CreatedAt),
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
