package elevenlabs

var (
	// Maps provider-specific finish reasons to Koutaku format
	koutakuToElevenlabsSpeechFormat = map[string]string{
		"":     "mp3_44100_128",
		"mp3":  "mp3_44100_128",
		"opus": "opus_48000_128",
		"wav":  "pcm_44100",
		"pcm":  "pcm_44100",
	}

	// Maps Koutaku finish reasons to provider-specific format
	elevenlabsSpeechFormatToKoutaku = map[string]string{
		"mp3_44100_128":  "mp3",
		"opus_48000_128": "opus",
		"pcm_44100":      "wav",
	}
)

// ConvertKoutakuSpeechFormatToElevenlabs converts Koutaku speech format to Elevenlabs format
func ConvertKoutakuSpeechFormatToElevenlabs(format string) string {
	if elevenlabsFormat, ok := koutakuToElevenlabsSpeechFormat[format]; ok {
		return elevenlabsFormat
	}
	return format
}

// ConvertElevenlabsSpeechFormatToKoutaku converts Elevenlabs speech format to Koutaku format
func ConvertElevenlabsSpeechFormatToKoutaku(format string) string {
	if koutakuFormat, ok := elevenlabsSpeechFormatToKoutaku[format]; ok {
		return koutakuFormat
	}
	return format
}
