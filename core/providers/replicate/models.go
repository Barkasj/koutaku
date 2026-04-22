package replicate

import (
	"strings"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// ToKoutakuListModelsResponse converts Replicate deployments to a Koutaku list models response.
// Replicate model IDs are composite: "{owner}/{name}" (e.g. "stability-ai/stable-diffusion").
func ToKoutakuListModelsResponse(
	deploymentsResponse *ReplicateDeploymentListResponse,
	providerKey schemas.ModelProvider,
	allowedModels schemas.WhiteList,
	blacklistedModels schemas.BlackList,
	aliases map[string]string,
	unfiltered bool,
) *schemas.KoutakuListModelsResponse {
	koutakuResponse := &schemas.KoutakuListModelsResponse{
		Data: make([]schemas.Model, 0),
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

	if deploymentsResponse != nil {
		for _, deployment := range deploymentsResponse.Results {
			// Replicate model IDs are composite owner/name
			deploymentID := deployment.Owner + "/" + deployment.Name

			var created *int64
			if deployment.CurrentRelease != nil && deployment.CurrentRelease.CreatedAt != "" {
				createdTimestamp := ParseReplicateTimestamp(deployment.CurrentRelease.CreatedAt)
				if createdTimestamp > 0 {
					created = schemas.Ptr(createdTimestamp)
				}
			}

			for _, result := range pipeline.FilterModel(deploymentID) {
				koutakuModel := schemas.Model{
					ID:      string(providerKey) + "/" + result.ResolvedID,
					Name:    schemas.Ptr(deployment.Name),
					OwnedBy: schemas.Ptr(deployment.Owner),
					Created: created,
				}
				if result.AliasValue != "" {
					koutakuModel.Alias = schemas.Ptr(result.AliasValue)
				}
				koutakuResponse.Data = append(koutakuResponse.Data, koutakuModel)
				included[strings.ToLower(result.ResolvedID)] = true
			}
		}

		if deploymentsResponse.Next != nil {
			koutakuResponse.NextPageToken = *deploymentsResponse.Next
		}
	}

	koutakuResponse.Data = append(koutakuResponse.Data,
		pipeline.BackfillModels(included)...)

	return koutakuResponse
}
