package gemini

import (
	"fmt"
	"strings"

	"github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// ToKoutakuTranscriptionRequest converts a GeminiGenerationRequest to a KoutakuTranscriptionRequest
func (request *GeminiGenerationRequest) ToKoutakuTranscriptionRequest(ctx *schemas.KoutakuContext) (*schemas.KoutakuTranscriptionRequest, error) {
	provider, model := schemas.ParseModelString(request.Model, utils.CheckAndSetDefaultProvider(ctx, schemas.Gemini))

	koutakuReq := &schemas.KoutakuTranscriptionRequest{
		Provider: provider,
		Model:    model,
	}

	// Extract audio data and prompt from contents
	var promptText string
	var audioData []byte
	var audioMimeType string

	for _, content := range request.Contents {
		for _, part := range content.Parts {
			// Extract text prompt
			if part.Text != "" {
				if promptText != "" {
					promptText += " "
				}
				promptText += part.Text
			}

			// Extract audio data from inline data
			if part.InlineData != nil && strings.HasPrefix(strings.ToLower(part.InlineData.MIMEType), "audio/") {
				decodedData, err := decodeBase64StringToBytes(part.InlineData.Data)
				if err != nil {
					return nil, fmt.Errorf("failed to decode base64 audio data: %v", err)
				}
				audioData = append(audioData, decodedData...)
				if audioMimeType == "" {
					audioMimeType = part.InlineData.MIMEType
				}
			}

			// Extract audio data from file data (would need to be fetched separately in real scenario)
			// For now, we just note the file URI in extra params
			if part.FileData != nil && strings.HasPrefix(strings.ToLower(part.FileData.MIMEType), "audio/") {
				if koutakuReq.Params == nil {
					koutakuReq.Params = &schemas.TranscriptionParameters{}
				}
				if koutakuReq.Params.ExtraParams == nil {
					koutakuReq.Params.ExtraParams = make(map[string]interface{})
				}
				koutakuReq.Params.ExtraParams["file_uri"] = part.FileData.FileURI
				if audioMimeType == "" {
					audioMimeType = part.FileData.MIMEType
				}
			}
		}
	}

	// Set the audio input
	koutakuReq.Input = &schemas.TranscriptionInput{
		File: audioData,
	}

	// Set parameters
	if koutakuReq.Params == nil {
		koutakuReq.Params = &schemas.TranscriptionParameters{}
	}

	// Set prompt if provided
	if promptText != "" {
		koutakuReq.Params.Prompt = &promptText
	}

	// Handle safety settings from request
	if len(request.SafetySettings) > 0 {
		if koutakuReq.Params.ExtraParams == nil {
			koutakuReq.Params.ExtraParams = make(map[string]interface{})
		}
		koutakuReq.Params.ExtraParams["safety_settings"] = request.SafetySettings
	}

	// Handle cached content
	if request.CachedContent != "" {
		if koutakuReq.Params.ExtraParams == nil {
			koutakuReq.Params.ExtraParams = make(map[string]interface{})
		}
		koutakuReq.Params.ExtraParams["cached_content"] = request.CachedContent
	}

	// Handle labels
	if len(request.Labels) > 0 {
		if koutakuReq.Params.ExtraParams == nil {
			koutakuReq.Params.ExtraParams = make(map[string]interface{})
		}
		koutakuReq.Params.ExtraParams["labels"] = request.Labels
	}

	return koutakuReq, nil
}

func ToGeminiTranscriptionRequest(koutakuReq *schemas.KoutakuTranscriptionRequest) *GeminiGenerationRequest {
	if koutakuReq == nil {
		return nil
	}

	// Create the base Gemini generation request
	geminiReq := &GeminiGenerationRequest{
		Model: koutakuReq.Model,
	}

	// Convert parameters to generation config
	if koutakuReq.Params != nil {
		geminiReq.ExtraParams = koutakuReq.Params.ExtraParams
		// Handle extra parameters
		if koutakuReq.Params.ExtraParams != nil {
			// Safety settings
			if safetySettings, ok := schemas.SafeExtractFromMap(koutakuReq.Params.ExtraParams, "safety_settings"); ok {
				delete(geminiReq.ExtraParams, "safety_settings")
				if settings, ok := SafeExtractSafetySettings(safetySettings); ok {
					geminiReq.SafetySettings = settings
				}
			}

			// Cached content
			if cachedContent, ok := schemas.SafeExtractString(koutakuReq.Params.ExtraParams["cached_content"]); ok {
				delete(geminiReq.ExtraParams, "cached_content")
				geminiReq.CachedContent = cachedContent
			}

			// Labels
			if labels, ok := schemas.SafeExtractFromMap(koutakuReq.Params.ExtraParams, "labels"); ok {
				if labelMap, ok := schemas.SafeExtractStringMap(labels); ok {
					delete(geminiReq.ExtraParams, "labels")
					geminiReq.Labels = labelMap
				}
			}
		}
	}

	// Determine the prompt text
	var prompt string
	if koutakuReq.Params != nil && koutakuReq.Params.Prompt != nil {
		prompt = *koutakuReq.Params.Prompt
	} else {
		prompt = "Generate a transcript of the speech."
	}

	// Create parts for the transcription request
	parts := []*Part{
		{
			Text: prompt,
		},
	}

	// Add audio file if present
	if len(koutakuReq.Input.File) > 0 {
		parts = append(parts, &Part{
			InlineData: &Blob{
				MIMEType: utils.DetectAudioMimeType(koutakuReq.Input.File),
				Data:     encodeBytesToBase64String(koutakuReq.Input.File),
			},
		})
	}

	geminiReq.Contents = []Content{
		{
			Parts: parts,
		},
	}

	return geminiReq
}

// ToKoutakuTranscriptionResponse converts a GenerateContentResponse to a KoutakuTranscriptionResponse
func (response *GenerateContentResponse) ToKoutakuTranscriptionResponse() *schemas.KoutakuTranscriptionResponse {
	koutakuResp := &schemas.KoutakuTranscriptionResponse{}

	// Process candidates to extract text content
	if len(response.Candidates) > 0 {
		candidate := response.Candidates[0]
		if candidate.Content != nil && len(candidate.Content.Parts) > 0 {
			var textContent string

			// Extract text content from all parts
			for _, part := range candidate.Content.Parts {
				if part.Text != "" {
					textContent += part.Text
				}
			}

			if textContent != "" {
				koutakuResp.Text = textContent
				koutakuResp.Task = schemas.Ptr("transcribe")

				// Set usage information with modality details
				koutakuResp.Usage = convertGeminiUsageMetadataToTranscriptionUsage(response.UsageMetadata)
			}
		}
	}

	return koutakuResp
}

// ToGeminiTranscriptionResponse converts a KoutakuTranscriptionResponse to Gemini's GenerateContentResponse
func ToGeminiTranscriptionResponse(koutakuResp *schemas.KoutakuTranscriptionResponse) *GenerateContentResponse {
	if koutakuResp == nil {
		return nil
	}

	genaiResp := &GenerateContentResponse{}

	candidate := &Candidate{
		Content: &Content{
			Parts: []*Part{
				{
					Text: koutakuResp.Text,
				},
			},
			Role: string(RoleModel),
		},
	}

	// Set usage metadata from transcription usage with modality details
	genaiResp.UsageMetadata = convertKoutakuTranscriptionUsageToGeminiUsageMetadata(koutakuResp.Usage)

	genaiResp.Candidates = []*Candidate{candidate}
	return genaiResp
}
