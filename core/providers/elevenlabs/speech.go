package elevenlabs

import (
	"github.com/koutaku/koutaku/core/schemas"
)

func ToElevenlabsSpeechRequest(koutakuReq *schemas.KoutakuSpeechRequest) *ElevenlabsSpeechRequest {
	if koutakuReq == nil || koutakuReq.Input == nil {
		return nil
	}

	elevenlabsReq := &ElevenlabsSpeechRequest{
		ModelID: koutakuReq.Model,
		Text:    koutakuReq.Input.Input,
	}

	if koutakuReq.Params != nil {
		elevenlabsReq.ExtraParams = koutakuReq.Params.ExtraParams
		voiceSettings := ElevenlabsVoiceSettings{}
		hasVoiceSettings := false

		if koutakuReq.Params.Speed != nil {
			voiceSettings.Speed = *koutakuReq.Params.Speed
			hasVoiceSettings = true
		}

		if koutakuReq.Params.ExtraParams != nil {
			if stability, ok := schemas.SafeExtractFloat64Pointer(koutakuReq.Params.ExtraParams["stability"]); ok {
				delete(elevenlabsReq.ExtraParams, "stability")
				voiceSettings.Stability = *stability
				hasVoiceSettings = true
			}
			if useSpeakerBoost, ok := schemas.SafeExtractBoolPointer(koutakuReq.Params.ExtraParams["use_speaker_boost"]); ok {
				delete(elevenlabsReq.ExtraParams, "use_speaker_boost")
				voiceSettings.UseSpeakerBoost = *useSpeakerBoost
				hasVoiceSettings = true
			}
			if similarityBoost, ok := schemas.SafeExtractFloat64Pointer(koutakuReq.Params.ExtraParams["similarity_boost"]); ok {
				delete(elevenlabsReq.ExtraParams, "similarity_boost")
				voiceSettings.SimilarityBoost = *similarityBoost
				hasVoiceSettings = true
			}
			if style, ok := schemas.SafeExtractFloat64Pointer(koutakuReq.Params.ExtraParams["style"]); ok {
				delete(elevenlabsReq.ExtraParams, "style")
				voiceSettings.Style = *style
				hasVoiceSettings = true
			}
			if seed, ok := schemas.SafeExtractIntPointer(koutakuReq.Params.ExtraParams["seed"]); ok {
				delete(elevenlabsReq.ExtraParams, "seed")
				elevenlabsReq.Seed = seed
			}
			if previousText, ok := schemas.SafeExtractStringPointer(koutakuReq.Params.ExtraParams["previous_text"]); ok {
				delete(elevenlabsReq.ExtraParams, "previous_text")
				elevenlabsReq.PreviousText = previousText
			}
			if nextText, ok := schemas.SafeExtractStringPointer(koutakuReq.Params.ExtraParams["next_text"]); ok {
				delete(elevenlabsReq.ExtraParams, "next_text")
				elevenlabsReq.NextText = nextText
			}
			if previousRequestIDs, ok := schemas.SafeExtractStringSlice(koutakuReq.Params.ExtraParams["previous_request_ids"]); ok {
				delete(elevenlabsReq.ExtraParams, "previous_request_ids")
				elevenlabsReq.PreviousRequestIDs = previousRequestIDs
			}
			if nextRequestIDs, ok := schemas.SafeExtractStringSlice(koutakuReq.Params.ExtraParams["next_request_ids"]); ok {
				delete(elevenlabsReq.ExtraParams, "next_request_ids")
				elevenlabsReq.NextRequestIDs = nextRequestIDs
			}
			if applyTextNormalization, ok := schemas.SafeExtractStringPointer(koutakuReq.Params.ExtraParams["apply_text_normalization"]); ok {
				delete(elevenlabsReq.ExtraParams, "apply_text_normalization")
				elevenlabsReq.ApplyTextNormalization = applyTextNormalization
			}
			if applyLanguageTextNormalization, ok := schemas.SafeExtractBoolPointer(koutakuReq.Params.ExtraParams["apply_language_text_normalization"]); ok {
				delete(elevenlabsReq.ExtraParams, "apply_language_text_normalization")
				elevenlabsReq.ApplyLanguageTextNormalization = applyLanguageTextNormalization
			}
			if usePVCAsIVC, ok := schemas.SafeExtractBoolPointer(koutakuReq.Params.ExtraParams["use_pvc_as_ivc"]); ok {
				delete(elevenlabsReq.ExtraParams, "use_pvc_as_ivc")
				elevenlabsReq.UsePVCAsIVC = usePVCAsIVC
			}
		}

		if hasVoiceSettings {
			elevenlabsReq.VoiceSettings = &voiceSettings
		}

		if koutakuReq.Params.LanguageCode != nil {
			elevenlabsReq.LanguageCode = koutakuReq.Params.LanguageCode
		}

		if len(koutakuReq.Params.PronunciationDictionaryLocators) > 0 {
			elevenlabsReq.PronunciationDictionaryLocators = make([]ElevenlabsPronunciationDictionaryLocator, len(koutakuReq.Params.PronunciationDictionaryLocators))
			for i, locator := range koutakuReq.Params.PronunciationDictionaryLocators {
				elevenlabsReq.PronunciationDictionaryLocators[i] = ElevenlabsPronunciationDictionaryLocator{
					PronunciationDictionaryID: locator.PronunciationDictionaryID,
					VersionID:                 locator.VersionID,
				}
			}
		}
	}

	return elevenlabsReq
}
