package cohere

import (
	"github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// ToCohereEmbeddingRequest converts a Koutaku embedding request to Cohere format
func ToCohereEmbeddingRequest(koutakuReq *schemas.KoutakuEmbeddingRequest) *CohereEmbeddingRequest {
	if koutakuReq == nil || koutakuReq.Input == nil || (koutakuReq.Input.Text == nil && koutakuReq.Input.Texts == nil) {
		return nil
	}

	embeddingInput := koutakuReq.Input
	cohereReq := &CohereEmbeddingRequest{
		Model: koutakuReq.Model,
	}

	texts := []string{}
	if embeddingInput.Text != nil {
		texts = append(texts, *embeddingInput.Text)
	} else {
		texts = embeddingInput.Texts
	}

	// Convert texts from Koutaku format
	if len(texts) > 0 {
		cohereReq.Texts = texts
	}

	// Set default input type if not specified in extra params
	cohereReq.InputType = "search_document" // Default value

	if koutakuReq.Params != nil {
		cohereReq.OutputDimension = koutakuReq.Params.Dimensions
		cohereReq.ExtraParams = koutakuReq.Params.ExtraParams
		if koutakuReq.Params.ExtraParams != nil {
			if maxTokens, ok := schemas.SafeExtractIntPointer(koutakuReq.Params.ExtraParams["max_tokens"]); ok {
				delete(cohereReq.ExtraParams, "max_tokens")
				cohereReq.MaxTokens = maxTokens
			}
		}
	}

	// Handle extra params
	if koutakuReq.Params != nil && koutakuReq.Params.ExtraParams != nil {
		// Input type
		if inputType, ok := schemas.SafeExtractString(koutakuReq.Params.ExtraParams["input_type"]); ok {
			delete(cohereReq.ExtraParams, "input_type")
			cohereReq.InputType = inputType
		}

		// Embedding types
		if embeddingTypes, ok := schemas.SafeExtractStringSlice(koutakuReq.Params.ExtraParams["embedding_types"]); ok {
			if len(embeddingTypes) > 0 {
				delete(cohereReq.ExtraParams, "embedding_types")
				cohereReq.EmbeddingTypes = embeddingTypes
			}
		}

		// Truncate
		if truncate, ok := schemas.SafeExtractStringPointer(koutakuReq.Params.ExtraParams["truncate"]); ok {
			delete(cohereReq.ExtraParams, "truncate")
			cohereReq.Truncate = truncate
		}
	}

	return cohereReq
}

// ToKoutakuEmbeddingRequest converts a Cohere embedding request to Koutaku format
func (req *CohereEmbeddingRequest) ToKoutakuEmbeddingRequest(ctx *schemas.KoutakuContext) *schemas.KoutakuEmbeddingRequest {
	if req == nil {
		return nil
	}

	provider, model := schemas.ParseModelString(req.Model, utils.CheckAndSetDefaultProvider(ctx, schemas.Cohere))

	koutakuReq := &schemas.KoutakuEmbeddingRequest{
		Provider: provider,
		Model:    model,
		Input:    &schemas.EmbeddingInput{},
		Params:   &schemas.EmbeddingParameters{},
	}

	// Convert texts
	if len(req.Texts) > 0 {
		if len(req.Texts) == 1 {
			koutakuReq.Input.Text = &req.Texts[0]
		} else {
			koutakuReq.Input.Texts = req.Texts
		}
	}

	// Convert parameters
	if req.OutputDimension != nil {
		koutakuReq.Params.Dimensions = req.OutputDimension
	}

	// Convert extra params
	extraParams := make(map[string]interface{})
	if req.InputType != "" {
		extraParams["input_type"] = req.InputType
	}
	if req.EmbeddingTypes != nil {
		extraParams["embedding_types"] = req.EmbeddingTypes
	}
	if req.Truncate != nil {
		extraParams["truncate"] = *req.Truncate
	}
	if req.MaxTokens != nil {
		extraParams["max_tokens"] = *req.MaxTokens
	}
	if len(extraParams) > 0 {
		koutakuReq.Params.ExtraParams = extraParams
	}

	return koutakuReq
}

// ToKoutakuEmbeddingResponse converts a Cohere embedding response to Koutaku format
func (response *CohereEmbeddingResponse) ToKoutakuEmbeddingResponse() *schemas.KoutakuEmbeddingResponse {
	if response == nil {
		return nil
	}

	koutakuResponse := &schemas.KoutakuEmbeddingResponse{
		Object: "list",
	}

	// Convert embeddings data
	if response.Embeddings != nil {
		var koutakuEmbeddings []schemas.EmbeddingData

		// Handle different embedding types - prioritize float embeddings
		if response.Embeddings.Float != nil {
			for i, embedding := range response.Embeddings.Float {
				koutakuEmbedding := schemas.EmbeddingData{
					Object: "embedding",
					Index:  i,
					Embedding: schemas.EmbeddingStruct{
						EmbeddingArray: embedding,
					},
				}
				koutakuEmbeddings = append(koutakuEmbeddings, koutakuEmbedding)
			}
		} else if response.Embeddings.Base64 != nil {
			// Handle base64 embeddings as strings
			for i, embedding := range response.Embeddings.Base64 {
				koutakuEmbedding := schemas.EmbeddingData{
					Object: "embedding",
					Index:  i,
					Embedding: schemas.EmbeddingStruct{
						EmbeddingStr: &embedding,
					},
				}
				koutakuEmbeddings = append(koutakuEmbeddings, koutakuEmbedding)
			}
		}
		// Note: Int8, Uint8, Binary, Ubinary types would need special handling
		// depending on how Koutaku wants to represent them

		koutakuResponse.Data = koutakuEmbeddings
	}

	// Convert usage information
	if response.Meta != nil {
		if response.Meta.Tokens != nil {
			koutakuResponse.Usage = &schemas.KoutakuLLMUsage{}
			if response.Meta.Tokens.InputTokens != nil {
				koutakuResponse.Usage.PromptTokens = int(*response.Meta.Tokens.InputTokens)
			}
			if response.Meta.Tokens.OutputTokens != nil {
				koutakuResponse.Usage.CompletionTokens = int(*response.Meta.Tokens.OutputTokens)
			}
			koutakuResponse.Usage.TotalTokens = koutakuResponse.Usage.PromptTokens + koutakuResponse.Usage.CompletionTokens
		} else if response.Meta.BilledUnits != nil {
			koutakuResponse.Usage = &schemas.KoutakuLLMUsage{}
			if response.Meta.BilledUnits.InputTokens != nil {
				koutakuResponse.Usage.PromptTokens = int(*response.Meta.BilledUnits.InputTokens)
			}
			if response.Meta.BilledUnits.OutputTokens != nil {
				koutakuResponse.Usage.CompletionTokens = int(*response.Meta.BilledUnits.OutputTokens)
			}
			koutakuResponse.Usage.TotalTokens = koutakuResponse.Usage.PromptTokens + koutakuResponse.Usage.CompletionTokens
		}
	}

	return koutakuResponse
}
