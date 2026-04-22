package xai

import (
	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

// XAIErrorResponse represents xAI's error response format
type XAIErrorResponse struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// ParseXAIError parses xAI-specific error responses.
// xAI returns errors in format: {"code": "...", "error": "..."}
// Unlike OpenAI which uses: {"error": {"message": "...", "type": "...", "code": "..."}}
func ParseXAIError(resp *fasthttp.Response) *schemas.KoutakuError {
	// Try to parse xAI error format
	var xaiErr XAIErrorResponse
	koutakuErr := providerUtils.HandleProviderAPIError(resp, &xaiErr)

	if koutakuErr == nil {
		return nil
	}

	// If we successfully parsed xAI format, extract the fields
	if xaiErr.Error != "" {
		if koutakuErr.Error == nil {
			koutakuErr.Error = &schemas.ErrorField{}
		}
		koutakuErr.Error.Message = xaiErr.Error
		if xaiErr.Code != "" {
			koutakuErr.Error.Code = schemas.Ptr(xaiErr.Code)
		}
	}

	return koutakuErr
}
