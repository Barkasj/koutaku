package cohere

import (
	"sort"

	"github.com/bytedance/sonic"
	"github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
	"gopkg.in/yaml.v3"
)

// ToCohereRerankRequest converts a Koutaku rerank request to Cohere format
func ToCohereRerankRequest(koutakuReq *schemas.KoutakuRerankRequest) *CohereRerankRequest {
	if koutakuReq == nil {
		return nil
	}

	cohereReq := &CohereRerankRequest{
		Model: koutakuReq.Model,
		Query: koutakuReq.Query,
	}

	// Cohere v2 expects documents as a list of strings.
	documents := make([]string, len(koutakuReq.Documents))
	for i, doc := range koutakuReq.Documents {
		documents[i] = formatCohereRerankDocument(doc)
	}
	cohereReq.Documents = documents

	if koutakuReq.Params != nil {
		cohereReq.TopN = koutakuReq.Params.TopN
		cohereReq.MaxTokensPerDoc = koutakuReq.Params.MaxTokensPerDoc
		cohereReq.Priority = koutakuReq.Params.Priority
		cohereReq.ExtraParams = koutakuReq.Params.ExtraParams
	}

	return cohereReq
}

// ToKoutakuRerankRequest converts a Cohere rerank request to Koutaku format
func (req *CohereRerankRequest) ToKoutakuRerankRequest(ctx *schemas.KoutakuContext) *schemas.KoutakuRerankRequest {
	if req == nil {
		return nil
	}

	provider, model := schemas.ParseModelString(req.Model, utils.CheckAndSetDefaultProvider(ctx, schemas.Cohere))

	koutakuReq := &schemas.KoutakuRerankRequest{
		Provider: provider,
		Model:    model,
		Query:    req.Query,
		Params:   &schemas.RerankParameters{},
	}

	// Convert documents
	for _, doc := range req.Documents {
		koutakuReq.Documents = append(koutakuReq.Documents, schemas.RerankDocument{
			Text: doc,
		})
	}

	if req.TopN != nil {
		koutakuReq.Params.TopN = req.TopN
	}
	if req.MaxTokensPerDoc != nil {
		koutakuReq.Params.MaxTokensPerDoc = req.MaxTokensPerDoc
	}
	if req.Priority != nil {
		koutakuReq.Params.Priority = req.Priority
	}
	if req.ExtraParams != nil {
		koutakuReq.Params.ExtraParams = req.ExtraParams
	}

	return koutakuReq
}

// ToKoutakuRerankResponse converts a Cohere rerank response to Koutaku format.
func (response *CohereRerankResponse) ToKoutakuRerankResponse(documents []schemas.RerankDocument, returnDocuments bool) *schemas.KoutakuRerankResponse {
	if response == nil {
		return nil
	}

	koutakuResponse := &schemas.KoutakuRerankResponse{
		ID: response.ID,
	}

	// Convert results
	for _, result := range response.Results {
		rerankResult := schemas.RerankResult{
			Index:          result.Index,
			RelevanceScore: result.RelevanceScore,
		}

		// Convert document if present
		if len(result.Document) > 0 {
			var docMap map[string]interface{}
			if err := sonic.Unmarshal(result.Document, &docMap); err == nil {
				doc := &schemas.RerankDocument{}
				populated := false
				if text, ok := docMap["text"].(string); ok {
					doc.Text = text
					populated = true
				}
				if id, ok := docMap["id"].(string); ok {
					doc.ID = &id
					populated = true
				}
				// Collect metadata: unwrap "metadata"/"meta" keys to avoid nesting
				meta := make(map[string]interface{})
				if rawMeta, ok := docMap["metadata"].(map[string]interface{}); ok {
					for k, v := range rawMeta {
						meta[k] = v
					}
				} else if rawMeta, ok := docMap["meta"].(map[string]interface{}); ok {
					for k, v := range rawMeta {
						meta[k] = v
					}
				}
				for k, v := range docMap {
					if k != "text" && k != "id" && k != "metadata" && k != "meta" {
						meta[k] = v
					}
				}
				if len(meta) > 0 {
					doc.Meta = meta
					populated = true
				}
				if populated {
					rerankResult.Document = doc
				}
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

	// Convert usage information
	if response.Meta != nil {
		promptTokens := 0
		completionTokens := 0
		hasTokenUsage := false
		if response.Meta.Tokens != nil {
			if response.Meta.Tokens.InputTokens != nil {
				promptTokens = int(*response.Meta.Tokens.InputTokens)
				hasTokenUsage = true
			}
			if response.Meta.Tokens.OutputTokens != nil {
				completionTokens = int(*response.Meta.Tokens.OutputTokens)
				hasTokenUsage = true
			}
		} else if response.Meta.BilledUnits != nil {
			if response.Meta.BilledUnits.InputTokens != nil {
				promptTokens = int(*response.Meta.BilledUnits.InputTokens)
				hasTokenUsage = true
			}
			if response.Meta.BilledUnits.OutputTokens != nil {
				completionTokens = int(*response.Meta.BilledUnits.OutputTokens)
				hasTokenUsage = true
			}
		}
		if hasTokenUsage {
			koutakuResponse.Usage = &schemas.KoutakuLLMUsage{
				PromptTokens:     promptTokens,
				CompletionTokens: completionTokens,
				TotalTokens:      promptTokens + completionTokens,
			}
		}
	}

	return koutakuResponse
}

func formatCohereRerankDocument(doc schemas.RerankDocument) string {
	if doc.ID == nil && len(doc.Meta) == 0 {
		return doc.Text
	}

	// Keep metadata/id available by encoding a structured string document.
	documentPayload := map[string]interface{}{
		"text": doc.Text,
	}
	if doc.ID != nil {
		documentPayload["id"] = *doc.ID
	}
	if len(doc.Meta) > 0 {
		documentPayload["metadata"] = doc.Meta
	}

	encoded, err := yaml.Marshal(documentPayload)
	if err != nil {
		return doc.Text
	}
	return string(encoded)
}
