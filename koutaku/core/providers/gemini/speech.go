package gemini

import (
	"context"
	"fmt"
	"strings"

	"github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// ToKoutakuSpeechRequest converts a GeminiGenerationRequest to a KoutakuSpeechRequest
func (request *GeminiGenerationRequest) ToKoutakuSpeechRequest(ctx *schemas.KoutakuContext) *schemas.KoutakuSpeechRequest {
	provider, model := schemas.ParseModelString(request.Model, utils.CheckAndSetDefaultProvider(ctx, schemas.Gemini))

	koutakuReq := &schemas.KoutakuSpeechRequest{
		Provider: provider,
		Model:    model,
	}

	// Extract text input from contents
	var textInput string
	for _, content := range request.Contents {
		for _, part := range content.Parts {
			if part.Text != "" {
				textInput += part.Text
			}
		}
	}

	koutakuReq.Input = &schemas.SpeechInput{
		Input: textInput,
	}

	// Convert generation config to parameters
	if request.GenerationConfig.SpeechConfig != nil || len(request.GenerationConfig.ResponseModalities) > 0 {
		koutakuReq.Params = &schemas.SpeechParameters{}

		// Extract voice config from speech config
		if request.GenerationConfig.SpeechConfig != nil {
			// Handle single-speaker voice config
			if request.GenerationConfig.SpeechConfig.VoiceConfig != nil {
				koutakuReq.Params.VoiceConfig = &schemas.SpeechVoiceInput{}

				if request.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig != nil {
					voiceName := request.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName
					koutakuReq.Params.VoiceConfig.Voice = &voiceName
				}
			} else if request.GenerationConfig.SpeechConfig.MultiSpeakerVoiceConfig != nil {
				// Handle multi-speaker voice config
				// Convert to Koutaku's MultiVoiceConfig format
				if len(request.GenerationConfig.SpeechConfig.MultiSpeakerVoiceConfig.SpeakerVoiceConfigs) > 0 {
					koutakuReq.Params.VoiceConfig = &schemas.SpeechVoiceInput{}
					multiVoiceConfig := make([]schemas.VoiceConfig, 0, len(request.GenerationConfig.SpeechConfig.MultiSpeakerVoiceConfig.SpeakerVoiceConfigs))

					for _, speakerConfig := range request.GenerationConfig.SpeechConfig.MultiSpeakerVoiceConfig.SpeakerVoiceConfigs {
						if speakerConfig.VoiceConfig != nil && speakerConfig.VoiceConfig.PrebuiltVoiceConfig != nil {
							multiVoiceConfig = append(multiVoiceConfig, schemas.VoiceConfig{
								Speaker: speakerConfig.Speaker,
								Voice:   speakerConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName,
							})
						}
					}

					koutakuReq.Params.VoiceConfig.MultiVoiceConfig = multiVoiceConfig
				}
			}
		}

		// Store response modalities in extra params if needed
		if len(request.GenerationConfig.ResponseModalities) > 0 {
			if koutakuReq.Params.ExtraParams == nil {
				koutakuReq.Params.ExtraParams = make(map[string]interface{})
			}
			modalities := make([]string, len(request.GenerationConfig.ResponseModalities))
			for i, mod := range request.GenerationConfig.ResponseModalities {
				modalities[i] = string(mod)
			}
			koutakuReq.Params.ExtraParams["response_modalities"] = modalities
		}
	}

	return koutakuReq
}

// ToGeminiSpeechRequest converts a KoutakuSpeechRequest to a GeminiGenerationRequest
func ToGeminiSpeechRequest(koutakuReq *schemas.KoutakuSpeechRequest) (*GeminiGenerationRequest, error) {
	if koutakuReq == nil {
		return nil, fmt.Errorf("koutakuReq is nil")
	}
	// Here we confirm if the response_format is wav or empty string
	// If its anything else, we will return an error
	if koutakuReq.Params != nil && koutakuReq.Params.ResponseFormat != "" && koutakuReq.Params.ResponseFormat != "wav" {
		return nil, fmt.Errorf("gemini does not support response_format: %s. Only wav or empty string is supported which defaults to wav", koutakuReq.Params.ResponseFormat)
	}
	// Create the base Gemini generation request
	geminiReq := &GeminiGenerationRequest{
		Model: koutakuReq.Model,
	}
	// Convert parameters to generation config
	geminiReq.GenerationConfig.ResponseModalities = []Modality{ModalityAudio}
	// Convert speech input to Gemini format
	if koutakuReq.Input != nil && koutakuReq.Input.Input != "" {
		geminiReq.Contents = []Content{
			{
				Parts: []*Part{
					{
						Text: koutakuReq.Input.Input,
					},
				},
			},
		}
		// Add speech config to generation config if voice config is provided
		if koutakuReq.Params != nil && koutakuReq.Params.VoiceConfig != nil {
			// Handle both single voice and multi-voice configurations
			if koutakuReq.Params.VoiceConfig.Voice != nil || len(koutakuReq.Params.VoiceConfig.MultiVoiceConfig) > 0 {
				addSpeechConfigToGenerationConfig(&geminiReq.GenerationConfig, koutakuReq.Params.VoiceConfig)
			}
			geminiReq.ExtraParams = koutakuReq.Params.ExtraParams
		}
	}
	return geminiReq, nil
}

// ToKoutakuSpeechResponse converts a GenerateContentResponse to a KoutakuSpeechResponse
func (response *GenerateContentResponse) ToKoutakuSpeechResponse(ctx context.Context) (*schemas.KoutakuSpeechResponse, error) {
	koutakuResp := &schemas.KoutakuSpeechResponse{}

	// Process candidates to extract audio content
	if len(response.Candidates) > 0 {
		candidate := response.Candidates[0]
		if candidate.Content != nil && len(candidate.Content.Parts) > 0 {
			var audioData []byte
			// Extract audio data from all parts
			for _, part := range candidate.Content.Parts {
				if part.InlineData != nil && len(part.InlineData.Data) > 0 {
					// Check if this is audio data
					if strings.HasPrefix(part.InlineData.MIMEType, "audio/") {
						decodedData, err := decodeBase64StringToBytes(part.InlineData.Data)
						if err != nil {
							return nil, fmt.Errorf("failed to decode base64 audio data: %v", err)
						}
						audioData = append(audioData, decodedData...)
					}
				}
			}
			if len(audioData) > 0 {
				responseFormat := ctx.Value(KoutakuContextKeyResponseFormat).(string)
				// Gemini returns PCM audio (s16le, 24000 Hz, mono)
				// Convert to WAV for standard playable output format
				if responseFormat == "wav" {
					wavData, err := utils.ConvertPCMToWAV(audioData, utils.DefaultGeminiPCMConfig())
					if err != nil {
						return nil, fmt.Errorf("failed to convert PCM to WAV: %v", err)
					}
					koutakuResp.Audio = wavData
				} else {
					koutakuResp.Audio = audioData
				}
			}

			// Set usage information
			if response.UsageMetadata != nil {
				koutakuResp.Usage = convertGeminiUsageMetadataToSpeechUsage(response.UsageMetadata)
			}
		}
	}
	return koutakuResp, nil
}

// ToGeminiSpeechResponse converts a KoutakuSpeechResponse to Gemini's GenerateContentResponse
func ToGeminiSpeechResponse(koutakuResp *schemas.KoutakuSpeechResponse) *GenerateContentResponse {
	if koutakuResp == nil {
		return nil
	}

	genaiResp := &GenerateContentResponse{}

	candidate := &Candidate{
		Content: &Content{
			Parts: []*Part{
				{
					InlineData: &Blob{
						Data:     encodeBytesToBase64String(koutakuResp.Audio),
						MIMEType: utils.DetectAudioMimeType(koutakuResp.Audio),
					},
				},
			},
			Role: string(RoleModel),
		},
	}

	// Set usage metadata if present
	if koutakuResp.Usage != nil {
		genaiResp.UsageMetadata = convertKoutakuSpeechUsageToGeminiUsageMetadata(koutakuResp.Usage)
	}

	genaiResp.Candidates = []*Candidate{candidate}
	return genaiResp
}
