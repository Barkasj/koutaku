package replicate

import (
	"github.com/bytedance/sonic"
	schemas "github.com/koutaku/koutaku/core/schemas"
)

// parseReplicateError parses Replicate API error response
func parseReplicateError(body []byte, statusCode int) *schemas.KoutakuError {
	var replicateErr ReplicateError
	if err := sonic.Unmarshal(body, &replicateErr); err == nil && replicateErr.Detail != "" {
		return &schemas.KoutakuError{
			IsKoutakuError: false,
			StatusCode:     &statusCode,
			Error: &schemas.ErrorField{
				Message: replicateErr.Detail,
			},
		}
	}

	// Fallback to generic error
	return &schemas.KoutakuError{
		IsKoutakuError: false,
		StatusCode:     &statusCode,
		Error: &schemas.ErrorField{
			Message: string(body),
		},
	}
}
