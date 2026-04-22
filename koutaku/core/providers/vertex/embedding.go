package vertex

import (
	"github.com/koutaku/koutaku/core/schemas"
)

// ToVertexEmbeddingRequest converts a Koutaku embedding request to Vertex AI format
func ToVertexEmbeddingRequest(koutakuReq *schemas.KoutakuEmbeddingRequest) *VertexEmbeddingRequest {
	if koutakuReq == nil || koutakuReq.Input == nil || (koutakuReq.Input.Text == nil && koutakuReq.Input.Texts == nil) {
		return nil
	}
	// Create the request
	vertexReq := &VertexEmbeddingRequest{}
	if koutakuReq.Params != nil {
		vertexReq.ExtraParams = koutakuReq.Params.ExtraParams
	}
	var texts []string
	if koutakuReq.Input.Text != nil {
		texts = []string{*koutakuReq.Input.Text}
	} else {
		texts = koutakuReq.Input.Texts
	}

	// Create instances for each text
	instances := make([]VertexEmbeddingInstance, 0, len(texts))
	for _, text := range texts {
		instance := VertexEmbeddingInstance{
			Content: text,
		}

		// Add optional task_type and title from params
		if koutakuReq.Params != nil {
			if taskTypeStr, ok := schemas.SafeExtractStringPointer(koutakuReq.Params.ExtraParams["task_type"]); ok {
				delete(vertexReq.ExtraParams, "task_type")
				instance.TaskType = taskTypeStr
			}
			if title, ok := schemas.SafeExtractStringPointer(koutakuReq.Params.ExtraParams["title"]); ok {
				delete(vertexReq.ExtraParams, "title")
				instance.Title = title
			}
		}

		instances = append(instances, instance)
	}
	vertexReq.Instances = instances
	// Add parameters if present
	if koutakuReq.Params != nil {
		parameters := &VertexEmbeddingParameters{}

		// Set autoTruncate (defaults to true)
		autoTruncate := true
		if koutakuReq.Params.ExtraParams != nil {
			if autoTruncateVal, ok := schemas.SafeExtractBool(koutakuReq.Params.ExtraParams["autoTruncate"]); ok {
				delete(vertexReq.ExtraParams, "autoTruncate")
				autoTruncate = autoTruncateVal
			}
		}
		parameters.AutoTruncate = &autoTruncate

		// Add outputDimensionality if specified
		if koutakuReq.Params.Dimensions != nil {
			delete(vertexReq.ExtraParams, "dimensions")
			parameters.OutputDimensionality = koutakuReq.Params.Dimensions
		}

		vertexReq.Parameters = parameters
	}

	return vertexReq
}

// ToKoutakuEmbeddingResponse converts a Vertex AI embedding response to Koutaku format
func (response *VertexEmbeddingResponse) ToKoutakuEmbeddingResponse() *schemas.KoutakuEmbeddingResponse {
	if response == nil || len(response.Predictions) == 0 {
		return nil
	}

	// Convert predictions to Koutaku embeddings
	embeddings := make([]schemas.EmbeddingData, 0, len(response.Predictions))
	var usage *schemas.KoutakuLLMUsage

	for i, prediction := range response.Predictions {
		if prediction.Embeddings == nil || len(prediction.Embeddings.Values) == 0 {
			continue
		}

		// Create embedding object
		embedding := schemas.EmbeddingData{
			Object: "embedding",
			Embedding: schemas.EmbeddingStruct{
				EmbeddingArray: append([]float64(nil), prediction.Embeddings.Values...),
			},
			Index: i,
		}

		// Extract statistics if available
		if prediction.Embeddings.Statistics != nil {
			if usage == nil {
				usage = &schemas.KoutakuLLMUsage{}
			}
			usage.TotalTokens += prediction.Embeddings.Statistics.TokenCount
			usage.PromptTokens += prediction.Embeddings.Statistics.TokenCount
		}

		embeddings = append(embeddings, embedding)
	}

	return &schemas.KoutakuEmbeddingResponse{
		Object: "list",
		Data:   embeddings,
		Usage:  usage,
		ExtraFields: schemas.KoutakuResponseExtraFields{
		},
	}
}
