package bedrock

import (
	"fmt"
	"sort"
	"strings"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// ToBedrockRerankRequest converts a Koutaku rerank request into Bedrock Agent Runtime format.
func ToBedrockRerankRequest(koutakuReq *schemas.KoutakuRerankRequest, modelARN string) (*BedrockRerankRequest, error) {
	if koutakuReq == nil {
		return nil, fmt.Errorf("koutaku rerank request is nil")
	}
	if strings.TrimSpace(modelARN) == "" {
		return nil, fmt.Errorf("bedrock rerank model ARN is empty")
	}
	if len(koutakuReq.Documents) == 0 {
		return nil, fmt.Errorf("documents are required for rerank request")
	}

	bedrockReq := &BedrockRerankRequest{
		Queries: []BedrockRerankQuery{
			{
				Type: bedrockRerankQueryTypeText,
				TextQuery: BedrockRerankTextRef{
					Text: koutakuReq.Query,
				},
			},
		},
		Sources: make([]BedrockRerankSource, len(koutakuReq.Documents)),
		RerankingConfiguration: BedrockRerankingConfiguration{
			Type: bedrockRerankConfigurationTypeBedrock,
			BedrockRerankingConfiguration: BedrockRerankingModelConfiguration{
				ModelConfiguration: BedrockRerankModelConfiguration{
					ModelARN: modelARN,
				},
			},
		},
	}

	for i, doc := range koutakuReq.Documents {
		bedrockReq.Sources[i] = BedrockRerankSource{
			Type: bedrockRerankSourceTypeInline,
			InlineDocumentSource: BedrockRerankInlineSource{
				Type: bedrockRerankInlineDocumentTypeText,
				TextDocument: BedrockRerankTextValue{
					Text: doc.Text,
				},
			},
		}
	}

	if koutakuReq.Params == nil {
		return bedrockReq, nil
	}

	if koutakuReq.Params.TopN != nil {
		topN := *koutakuReq.Params.TopN
		if topN < 1 {
			return nil, fmt.Errorf("top_n must be at least 1")
		}
		if topN > len(koutakuReq.Documents) {
			topN = len(koutakuReq.Documents)
		}
		bedrockReq.RerankingConfiguration.BedrockRerankingConfiguration.NumberOfResults = schemas.Ptr(topN)
	}

	additionalFields := make(map[string]interface{})
	if koutakuReq.Params.MaxTokensPerDoc != nil {
		additionalFields["max_tokens_per_doc"] = *koutakuReq.Params.MaxTokensPerDoc
	}
	if koutakuReq.Params.Priority != nil {
		additionalFields["priority"] = *koutakuReq.Params.Priority
	}
	for k, v := range koutakuReq.Params.ExtraParams {
		additionalFields[k] = v
	}
	if len(additionalFields) > 0 {
		bedrockReq.RerankingConfiguration.BedrockRerankingConfiguration.ModelConfiguration.AdditionalModelRequestFields = additionalFields
	}

	return bedrockReq, nil
}

// ToKoutakuRerankResponse converts a Bedrock rerank response into Koutaku format.
func (response *BedrockRerankResponse) ToKoutakuRerankResponse(documents []schemas.RerankDocument, returnDocuments bool) *schemas.KoutakuRerankResponse {
	if response == nil {
		return nil
	}

	koutakuResponse := &schemas.KoutakuRerankResponse{
		Results: make([]schemas.RerankResult, 0, len(response.Results)),
	}

	for _, result := range response.Results {
		rerankResult := schemas.RerankResult{
			Index:          result.Index,
			RelevanceScore: result.RelevanceScore,
		}
		if result.Document != nil && result.Document.TextDocument != nil {
			rerankResult.Document = &schemas.RerankDocument{
				Text: result.Document.TextDocument.Text,
			}
		}
		koutakuResponse.Results = append(koutakuResponse.Results, rerankResult)
	}

	sort.SliceStable(koutakuResponse.Results, func(i, j int) bool {
		if koutakuResponse.Results[i].RelevanceScore == koutakuResponse.Results[j].RelevanceScore {
			return koutakuResponse.Results[i].Index < koutakuResponse.Results[j].Index
		}
		return koutakuResponse.Results[i].RelevanceScore > koutakuResponse.Results[j].RelevanceScore
	})

	if returnDocuments {
		for i := range koutakuResponse.Results {
			resultIndex := koutakuResponse.Results[i].Index
			if resultIndex >= 0 && resultIndex < len(documents) {
				koutakuResponse.Results[i].Document = schemas.Ptr(documents[resultIndex])
			}
		}
	}

	return koutakuResponse
}

// ToKoutakuRerankRequest converts a Bedrock Agent Runtime rerank request to Koutaku format.
func (req *BedrockRerankRequest) ToKoutakuRerankRequest(ctx *schemas.KoutakuContext) *schemas.KoutakuRerankRequest {
	if req == nil {
		return nil
	}

	modelARN := req.RerankingConfiguration.BedrockRerankingConfiguration.ModelConfiguration.ModelARN
	provider, model := schemas.ParseModelString(modelARN, providerUtils.CheckAndSetDefaultProvider(ctx, schemas.Bedrock))

	koutakuReq := &schemas.KoutakuRerankRequest{
		Provider: provider,
		Model:    model,
		Params:   &schemas.RerankParameters{},
	}

	// Extract query from the first query entry
	if len(req.Queries) > 0 {
		koutakuReq.Query = req.Queries[0].TextQuery.Text
	}

	// Convert sources to documents
	for _, source := range req.Sources {
		koutakuReq.Documents = append(koutakuReq.Documents, schemas.RerankDocument{
			Text: source.InlineDocumentSource.TextDocument.Text,
		})
	}

	// Extract TopN from NumberOfResults
	if req.RerankingConfiguration.BedrockRerankingConfiguration.NumberOfResults != nil {
		koutakuReq.Params.TopN = req.RerankingConfiguration.BedrockRerankingConfiguration.NumberOfResults
	}

	// Pass AdditionalModelRequestFields as ExtraParams
	if fields := req.RerankingConfiguration.BedrockRerankingConfiguration.ModelConfiguration.AdditionalModelRequestFields; len(fields) > 0 {
		koutakuReq.Params.ExtraParams = fields
	}

	return koutakuReq
}
