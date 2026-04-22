package gemini

import (
	"strconv"
	"strings"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

// ToGeminiError derives a GeminiGenerationError from a KoutakuError
func ToGeminiError(koutakuErr *schemas.KoutakuError) *GeminiGenerationError {
	if koutakuErr == nil {
		return nil
	}
	code := 500
	status := ""
	if koutakuErr.Error != nil && koutakuErr.Error.Type != nil {
		status = *koutakuErr.Error.Type
	}
	message := ""
	if koutakuErr.Error != nil && koutakuErr.Error.Message != "" {
		message = koutakuErr.Error.Message
	}
	if koutakuErr.StatusCode != nil {
		code = *koutakuErr.StatusCode
	}
	return &GeminiGenerationError{
		Error: &GeminiGenerationErrorStruct{
			Code:    code,
			Message: message,
			Status:  status,
		},
	}
}

// parseGeminiError parses Gemini error responses
func parseGeminiError(resp *fasthttp.Response) *schemas.KoutakuError {
	// Try to parse as []GeminiGenerationError
	var errorResps []GeminiGenerationError
	koutakuErr := providerUtils.HandleProviderAPIError(resp, &errorResps)
	if len(errorResps) > 0 {
		var message string
		var firstError *GeminiGenerationErrorStruct
		for _, errorResp := range errorResps {
			if errorResp.Error != nil {
				if firstError == nil {
					firstError = errorResp.Error
				}
				message = message + errorResp.Error.Message + "\n"
			}
		}
		// Trim trailing newline
		message = strings.TrimSuffix(message, "\n")
		if koutakuErr.Error == nil {
			koutakuErr.Error = &schemas.ErrorField{}
		}
		// Set Code from first error if available
		if firstError != nil {
			koutakuErr.Error.Code = schemas.Ptr(strconv.Itoa(firstError.Code))
		}
		// Set Message to trimmed concatenated message
		koutakuErr.Error.Message = message
		return koutakuErr
	}

	// Try to parse as GeminiGenerationError
	var errorResp GeminiGenerationError
	koutakuErr = providerUtils.HandleProviderAPIError(resp, &errorResp)
	if errorResp.Error != nil {
		if koutakuErr.Error == nil {
			koutakuErr.Error = &schemas.ErrorField{}
		}
		koutakuErr.Error.Code = schemas.Ptr(strconv.Itoa(errorResp.Error.Code))
		koutakuErr.Error.Message = errorResp.Error.Message
	}
	return koutakuErr
}
