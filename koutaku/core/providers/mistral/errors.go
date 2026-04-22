package mistral

import (
	"fmt"
	"strings"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

// MistralErrorResponse captures both Mistral's top-level error shape and nested OpenAI-style errors.
type MistralErrorResponse struct {
	Object  string              `json:"object,omitempty"`
	Message string              `json:"message,omitempty"`
	Type    string              `json:"type,omitempty"`
	Code    string              `json:"code,omitempty"`
	Error   *schemas.ErrorField `json:"error,omitempty"`
}

// ParseMistralError parses Mistral-specific error responses.
func ParseMistralError(resp *fasthttp.Response) *schemas.KoutakuError {
	var errorResp MistralErrorResponse
	koutakuErr := providerUtils.HandleProviderAPIError(resp, &errorResp)
	if koutakuErr == nil {
		return nil
	}

	if koutakuErr.Error == nil {
		koutakuErr.Error = &schemas.ErrorField{}
	}

	if errorResp.Error != nil {
		if strings.TrimSpace(errorResp.Error.Message) != "" {
			koutakuErr.Error.Message = errorResp.Error.Message
		}
		if errorResp.Error.Type != nil && strings.TrimSpace(*errorResp.Error.Type) != "" {
			koutakuErr.Error.Type = errorResp.Error.Type
			koutakuErr.Type = errorResp.Error.Type
		}
		if errorResp.Error.Code != nil && strings.TrimSpace(*errorResp.Error.Code) != "" {
			koutakuErr.Error.Code = errorResp.Error.Code
		}
		koutakuErr.Error.Param = errorResp.Error.Param
		if errorResp.Error.EventID != nil {
			koutakuErr.Error.EventID = errorResp.Error.EventID
		}
	}

	if strings.TrimSpace(errorResp.Message) != "" {
		koutakuErr.Error.Message = errorResp.Message
	}
	if strings.TrimSpace(errorResp.Type) != "" {
		errorType := schemas.Ptr(errorResp.Type)
		koutakuErr.Error.Type = errorType
		koutakuErr.Type = errorType
	}
	if strings.TrimSpace(errorResp.Code) != "" {
		koutakuErr.Error.Code = schemas.Ptr(errorResp.Code)
	}

	if strings.TrimSpace(koutakuErr.Error.Message) == "" {
		if koutakuErr.StatusCode != nil {
			koutakuErr.Error.Message = fmt.Sprintf("provider API error (status %d)", *koutakuErr.StatusCode)
		} else {
			koutakuErr.Error.Message = "provider API error"
		}
	}

	return koutakuErr
}
