package gemini

import (
	"github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// ToGeminiEmbeddingRequest converts a KoutakuRequest with embedding input to Gemini's batch embedding request format
// GeminiGenerationRequest contains requests array for batch embed content endpoint
func ToGeminiEmbeddingRequest(koutakuReq *schemas.KoutakuEmbeddingRequest) *GeminiBatchEmbeddingRequest {
	if koutakuReq == nil || koutakuReq.Input == nil || (koutakuReq.Input.Text == nil && koutakuReq.Input.Texts == nil) {
		return nil
	}

	embeddingInput := koutakuReq.Input

	// Collect all texts to embed
	var texts []string
	if embeddingInput.Text != nil {
		texts = append(texts, *embeddingInput.Text)
	}
	if len(embeddingInput.Texts) > 0 {
		texts = append(texts, embeddingInput.Texts...)
	}

	if len(texts) == 0 {
		return nil
	}

	// Create batch embedding request with one request per text
	batchRequest := &GeminiBatchEmbeddingRequest{
		Requests: make([]GeminiEmbeddingRequest, len(texts)),
	}
	if koutakuReq.Params != nil {
		batchRequest.ExtraParams = koutakuReq.Params.ExtraParams
	}

	// Create individual embedding requests for each text
	for i, text := range texts {
		embeddingReq := GeminiEmbeddingRequest{
			Model: "models/" + koutakuReq.Model,
			Content: &Content{
				Parts: []*Part{
					{
						Text: text,
					},
				},
			},
		}

		// Add parameters if available
		if koutakuReq.Params != nil {
			if koutakuReq.Params.Dimensions != nil {
				embeddingReq.OutputDimensionality = koutakuReq.Params.Dimensions
			}

			// Handle extra parameters
			if koutakuReq.Params.ExtraParams != nil {
				if taskType, ok := schemas.SafeExtractStringPointer(koutakuReq.Params.ExtraParams["taskType"]); ok {
					delete(batchRequest.ExtraParams, "taskType")
					embeddingReq.TaskType = taskType
				}
				if title, ok := schemas.SafeExtractStringPointer(koutakuReq.Params.ExtraParams["title"]); ok {
					delete(batchRequest.ExtraParams, "title")
					embeddingReq.Title = title
				}
			}
		}

		batchRequest.Requests[i] = embeddingReq
	}

	return batchRequest
}

// ToGeminiEmbeddingResponse converts a KoutakuResponse with embedding data to Gemini's embedding response format
func ToGeminiEmbeddingResponse(koutakuResp *schemas.KoutakuEmbeddingResponse) *GeminiEmbeddingResponse {
	if koutakuResp == nil || len(koutakuResp.Data) == 0 {
		return nil
	}

	geminiResp := &GeminiEmbeddingResponse{
		Embeddings: make([]GeminiEmbedding, len(koutakuResp.Data)),
	}

	// Convert each embedding from Koutaku format to Gemini format
	for i, embedding := range koutakuResp.Data {
		var values []float64

		// Extract embedding values from KoutakuEmbeddingResponse
		if embedding.Embedding.EmbeddingArray != nil {
			values = append([]float64(nil), embedding.Embedding.EmbeddingArray...)
		} else if len(embedding.Embedding.Embedding2DArray) > 0 {
			// If it's a 2D array, take the first array
			values = append([]float64(nil), embedding.Embedding.Embedding2DArray[0]...)
		}

		geminiEmbedding := GeminiEmbedding{
			Values: values,
		}

		// Add statistics if available (token count from usage metadata)
		if koutakuResp.Usage != nil {
			geminiEmbedding.Statistics = &ContentEmbeddingStatistics{
				TokenCount: int32(koutakuResp.Usage.PromptTokens),
			}
		}

		geminiResp.Embeddings[i] = geminiEmbedding
	}

	// Set metadata if available (for Vertex API compatibility)
	if koutakuResp.Usage != nil {
		geminiResp.Metadata = &EmbedContentMetadata{
			BillableCharacterCount: int32(koutakuResp.Usage.PromptTokens),
		}
	}

	return geminiResp
}

// ToKoutakuEmbeddingResponse converts a Gemini embedding response to KoutakuEmbeddingResponse format
func ToKoutakuEmbeddingResponse(geminiResp *GeminiEmbeddingResponse, model string) *schemas.KoutakuEmbeddingResponse {
	if geminiResp == nil || len(geminiResp.Embeddings) == 0 {
		return nil
	}

	koutakuResp := &schemas.KoutakuEmbeddingResponse{
		Data:   make([]schemas.EmbeddingData, len(geminiResp.Embeddings)),
		Model:  model,
		Object: "list",
	}

	// Convert each embedding from Gemini format to Koutaku format
	for i, geminiEmbedding := range geminiResp.Embeddings {
		embeddingData := schemas.EmbeddingData{
			Index:  i,
			Object: "embedding",
			Embedding: schemas.EmbeddingStruct{
				EmbeddingArray: geminiEmbedding.Values,
			},
		}

		koutakuResp.Data[i] = embeddingData
	}

	// Convert usage metadata if available
	if geminiResp.Metadata != nil || (len(geminiResp.Embeddings) > 0 && geminiResp.Embeddings[0].Statistics != nil) {
		koutakuResp.Usage = &schemas.KoutakuLLMUsage{}

		// Use statistics from the first embedding if available
		if geminiResp.Embeddings[0].Statistics != nil {
			koutakuResp.Usage.PromptTokens = int(geminiResp.Embeddings[0].Statistics.TokenCount)
		} else if geminiResp.Metadata != nil {
			// Fall back to metadata if statistics are not available
			koutakuResp.Usage.PromptTokens = int(geminiResp.Metadata.BillableCharacterCount)
		}

		// Set total tokens same as prompt tokens for embeddings
		koutakuResp.Usage.TotalTokens = koutakuResp.Usage.PromptTokens
	}

	return koutakuResp
}

// ToKoutakuEmbeddingRequest converts a GeminiGenerationRequest to KoutakuEmbeddingRequest format
func (request *GeminiGenerationRequest) ToKoutakuEmbeddingRequest(ctx *schemas.KoutakuContext) *schemas.KoutakuEmbeddingRequest {
	if request == nil {
		return nil
	}

	provider, model := schemas.ParseModelString(request.Model, utils.CheckAndSetDefaultProvider(ctx, schemas.Gemini))

	// Create the embedding request
	koutakuReq := &schemas.KoutakuEmbeddingRequest{
		Provider:  provider,
		Model:     model,
		Fallbacks: schemas.ParseFallbacks(request.Fallbacks),
	}

	// SDK batch embedding request contains multiple embedding requests with same parameters but different text fields.
	if len(request.Requests) > 0 {
		var texts []string
		for _, req := range request.Requests {
			if req.Content != nil && len(req.Content.Parts) > 0 {
				for _, part := range req.Content.Parts {
					if part != nil && part.Text != "" {
						texts = append(texts, part.Text)
					}
				}
			}
		}
		if len(texts) > 0 {
			koutakuReq.Input = &schemas.EmbeddingInput{}
			if len(texts) == 1 {
				koutakuReq.Input.Text = &texts[0]
			} else {
				koutakuReq.Input.Texts = texts
			}
		}

		embeddingRequest := request.Requests[0]

		// Convert parameters
		if embeddingRequest.OutputDimensionality != nil || embeddingRequest.TaskType != nil || embeddingRequest.Title != nil {
			koutakuReq.Params = &schemas.EmbeddingParameters{}

			if embeddingRequest.OutputDimensionality != nil {
				koutakuReq.Params.Dimensions = embeddingRequest.OutputDimensionality
			}

			// Handle extra parameters
			if embeddingRequest.TaskType != nil || embeddingRequest.Title != nil {
				koutakuReq.Params.ExtraParams = make(map[string]interface{})
				if embeddingRequest.TaskType != nil {
					koutakuReq.Params.ExtraParams["taskType"] = embeddingRequest.TaskType
				}
				if embeddingRequest.Title != nil {
					koutakuReq.Params.ExtraParams["title"] = embeddingRequest.Title
				}
			}
		}
	}

	// Generation-style requests (e.g., non-Imagen :predict) carry text in contents[].parts[].
	// If no SDK requests[] were provided, derive embedding input from contents.
	if koutakuReq.Input == nil {
		var texts []string
		for _, content := range request.Contents {
			for _, part := range content.Parts {
				if part != nil && part.Text != "" {
					texts = append(texts, part.Text)
				}
			}
		}
		if len(texts) > 0 {
			koutakuReq.Input = &schemas.EmbeddingInput{}
			if len(texts) == 1 {
				koutakuReq.Input.Text = &texts[0]
			} else {
				koutakuReq.Input.Texts = texts
			}
		}
	}

	return koutakuReq
}
