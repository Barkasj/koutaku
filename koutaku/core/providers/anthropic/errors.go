package anthropic

import (
	"fmt"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	schemas "github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

// ToAnthropicChatCompletionError converts a KoutakuError to AnthropicMessageError
func ToAnthropicChatCompletionError(koutakuErr *schemas.KoutakuError) *AnthropicMessageError {
	if koutakuErr == nil {
		return nil
	}

	// Safely extract type and message from nested error
	errorType := "api_error"
	message := ""
	if koutakuErr.Error != nil {
		if koutakuErr.Error.Type != nil && *koutakuErr.Error.Type != "" {
			errorType = *koutakuErr.Error.Type
		}
		message = koutakuErr.Error.Message
	}

	// Handle nested error fields with nil checks
	errorStruct := AnthropicMessageErrorStruct{
		Type:    errorType,
		Message: message,
	}

	return &AnthropicMessageError{
		Type:  "error", // always "error" for Anthropic
		Error: errorStruct,
	}
}

// ToAnthropicResponsesStreamError converts a KoutakuError to Anthropic responses streaming error in SSE format
func ToAnthropicResponsesStreamError(koutakuErr *schemas.KoutakuError) string {
	if koutakuErr == nil {
		return ""
	}

	anthropicErr := ToAnthropicChatCompletionError(koutakuErr)

	// Marshal to JSON
	jsonData, err := providerUtils.MarshalSorted(anthropicErr)
	if err != nil {
		return ""
	}

	// Format as Anthropic SSE error event
	return fmt.Sprintf("event: error\ndata: %s\n\n", jsonData)
}

func parseAnthropicError(resp *fasthttp.Response) *schemas.KoutakuError {
	var errorResp AnthropicError
	koutakuErr := providerUtils.HandleProviderAPIError(resp, &errorResp)
	if errorResp.Error != nil {
		if koutakuErr.Error == nil {
			koutakuErr.Error = &schemas.ErrorField{}
		}
		koutakuErr.Error.Type = &errorResp.Error.Type
		koutakuErr.Error.Message = errorResp.Error.Message
	}
	return koutakuErr
}
