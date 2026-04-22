package nebius

import (
	"strings"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

// parseNebiusImageError parses Nebius error responses
func parseNebiusImageError(resp *fasthttp.Response) *schemas.KoutakuError {
	var nebiusErr NebiusError
	koutakuErr := providerUtils.HandleProviderAPIError(resp, &nebiusErr)

	if koutakuErr.Error == nil {
		koutakuErr.Error = &schemas.ErrorField{}
	}

	// Extract error message
	var message string
	if nebiusErr.Detail != nil {
		if nebiusErr.Detail.Message != nil {
			message = *nebiusErr.Detail.Message
		}

		if len(nebiusErr.Detail.ValidationErrors) > 0 {
			var messages []string
			var locations []string

			if message != "" {
				messages = append(messages, message)
			}

			for _, detail := range nebiusErr.Detail.ValidationErrors {
				if detail.Msg != "" {
					messages = append(messages, detail.Msg)
				}
				if len(detail.Loc) > 0 {
					locations = append(locations, strings.Join(detail.Loc, "."))
				}
			}

			if len(messages) > 0 {
				message = strings.Join(messages, "; ")
			}
			if len(locations) > 0 {
				locationStr := strings.Join(locations, ", ")
				if message == "" {
					message = "[" + locationStr + "]"
				} else {
					message = message + " [" + locationStr + "]"
				}
			}
		}
	}

	// Use the extracted message if available
	if message != "" {
		koutakuErr.Error.Message = message
	}

	return koutakuErr
}
