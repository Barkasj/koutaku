package openai

import (
	"fmt"
	"strings"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

// ErrorConverter is a function that converts provider-specific error responses to KoutakuError.
type ErrorConverter func(resp *fasthttp.Response) *schemas.KoutakuError

// ParseOpenAIError parses OpenAI error responses.
func ParseOpenAIError(resp *fasthttp.Response) *schemas.KoutakuError {
	var errorResp schemas.KoutakuError

	koutakuErr := providerUtils.HandleProviderAPIError(resp, &errorResp)

	if errorResp.EventID != nil {
		koutakuErr.EventID = errorResp.EventID
	}

	if errorResp.Error != nil {
		if koutakuErr.Error == nil {
			koutakuErr.Error = &schemas.ErrorField{}
		}
		koutakuErr.Error.Type = errorResp.Error.Type
		koutakuErr.Error.Code = errorResp.Error.Code
		if errorResp.Error.Message != "" {
			koutakuErr.Error.Message = errorResp.Error.Message
		}
		koutakuErr.Error.Param = errorResp.Error.Param
		if errorResp.Error.EventID != nil {
			koutakuErr.Error.EventID = errorResp.Error.EventID
		}
	}

	if koutakuErr.Error == nil {
		koutakuErr.Error = &schemas.ErrorField{}
	}
	if strings.TrimSpace(koutakuErr.Error.Message) == "" {
		if koutakuErr.StatusCode != nil {
			koutakuErr.Error.Message = fmt.Sprintf("provider API error (status %d)", *koutakuErr.StatusCode)
		} else {
			koutakuErr.Error.Message = "provider API error"
		}
	}

	// Set ExtraFields unconditionally so provider/model/request metadata is always attached

	return koutakuErr
}
