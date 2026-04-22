package runway

import (
	"strings"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	schemas "github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

// parseRunwayError parses Runway API error responses and converts them to KoutakuError.
func parseRunwayError(resp *fasthttp.Response) *schemas.KoutakuError {
	// Parse as RunwayAPIError
	var errorResp RunwayAPIError
	koutakuErr := providerUtils.HandleProviderAPIError(resp, &errorResp)

	// Set error message if available
	if errorResp.Error != "" {
		if koutakuErr.Error == nil {
			koutakuErr.Error = &schemas.ErrorField{}
		}
		koutakuErr.Error.Message = errorResp.Error
	} else if koutakuErr.Error != nil && koutakuErr.Error.Message == "" {
		// If no error message was extracted, use a generic one
		koutakuErr.Error.Message = "Runway API request failed"
	} else if koutakuErr.Error == nil {
		koutakuErr.Error = &schemas.ErrorField{
			Message: "Runway API request failed",
		}
	}

	// Remove trailing newlines
	if koutakuErr.Error != nil && koutakuErr.Error.Message != "" {
		koutakuErr.Error.Message = strings.TrimRight(koutakuErr.Error.Message, "\n")
	}

	return koutakuErr
}
