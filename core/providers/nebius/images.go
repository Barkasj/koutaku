package nebius

import (
	"fmt"
	"strconv"
	"strings"

	schemas "github.com/koutaku/koutaku/core/schemas"
)

// ToNebiusImageGenerationRequest converts a koutaku image generation request to nebius format.
func (provider *NebiusProvider) ToNebiusImageGenerationRequest(koutakuReq *schemas.KoutakuImageGenerationRequest) (*NebiusImageGenerationRequest, error) {
	if koutakuReq == nil || koutakuReq.Input == nil {
		return nil, fmt.Errorf("koutaku request is nil or input is nil")
	}

	req := &NebiusImageGenerationRequest{
		Model:  &koutakuReq.Model,
		Prompt: &koutakuReq.Input.Prompt,
	}

	if koutakuReq.Params != nil {

		if koutakuReq.Params.ResponseFormat != nil {
			req.ResponseFormat = koutakuReq.Params.ResponseFormat
		}

		if koutakuReq.Params.Size != nil && strings.TrimSpace(strings.ToLower(*koutakuReq.Params.Size)) != "auto" {
			size := strings.Split(strings.TrimSpace(strings.ToLower(*koutakuReq.Params.Size)), "x")
			if len(size) != 2 {
				return nil, fmt.Errorf("invalid size format: expected 'WIDTHxHEIGHT', got %q", *koutakuReq.Params.Size)
			}

			width, err := strconv.Atoi(size[0])
			if err != nil {
				return nil, fmt.Errorf("invalid width in size %q: %w", *koutakuReq.Params.Size, err)
			}

			height, err := strconv.Atoi(size[1])
			if err != nil {
				return nil, fmt.Errorf("invalid height in size %q: %w", *koutakuReq.Params.Size, err)
			}

			req.Width = &width
			req.Height = &height
		}
		if koutakuReq.Params.OutputFormat != nil {
			req.ResponseExtension = koutakuReq.Params.OutputFormat
		}
		if req.ResponseExtension != nil && strings.ToLower(*req.ResponseExtension) == "jpeg" {
			req.ResponseExtension = schemas.Ptr("jpg")
		}
		if koutakuReq.Params.Seed != nil {
			req.Seed = koutakuReq.Params.Seed
		}
		if koutakuReq.Params.NegativePrompt != nil {
			req.NegativePrompt = koutakuReq.Params.NegativePrompt
		}
		if koutakuReq.Params.NumInferenceSteps != nil {
			req.NumInferenceSteps = koutakuReq.Params.NumInferenceSteps
		}
		// Handle extra params
		if koutakuReq.Params.ExtraParams != nil {
			req.ExtraParams = koutakuReq.Params.ExtraParams
			// Map guidance_scale
			if v, ok := schemas.SafeExtractIntPointer(koutakuReq.Params.ExtraParams["guidance_scale"]); ok {
				delete(req.ExtraParams, "guidance_scale")
				req.GuidanceScale = v
			}

			// Map loras in array format [{"url": "...", "scale": ...}]
			if lorasValue, exists := koutakuReq.Params.ExtraParams["loras"]; exists && lorasValue != nil {
				delete(req.ExtraParams, "loras")
				// Check if lorasValue is an array of maps
				if lorasArray, ok := lorasValue.([]interface{}); ok {
					for _, item := range lorasArray {
						if loraMap, ok := item.(map[string]interface{}); ok {
							if url, ok := schemas.SafeExtractString(loraMap["url"]); ok {
								if scale, ok := schemas.SafeExtractInt(loraMap["scale"]); ok {
									req.Loras = append(req.Loras, NebiusLora{URL: url, Scale: scale})
								}
							}
						}
					}
				}
			}
		}
	}
	return req, nil
}

// ToKoutakuImageResponse converts a nebius image generation response to koutaku format.
func ToKoutakuImageResponse(nebiusResponse *NebiusImageGenerationResponse) *schemas.KoutakuImageGenerationResponse {
	if nebiusResponse == nil {
		return nil
	}

	data := make([]schemas.ImageData, len(nebiusResponse.Data))
	for i, img := range nebiusResponse.Data {
		data[i] = schemas.ImageData{
			URL:           img.URL,
			B64JSON:       img.B64JSON,
			RevisedPrompt: img.RevisedPrompt,
			Index:         i,
		}
	}
	return &schemas.KoutakuImageGenerationResponse{
		ID:   nebiusResponse.Id,
		Data: data,
	}
}
