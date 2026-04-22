package cohere

import (
	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

func parseCohereError(resp *fasthttp.Response) *schemas.KoutakuError {
	var errorResp CohereError
	koutakuErr := providerUtils.HandleProviderAPIError(resp, &errorResp)
	koutakuErr.Type = &errorResp.Type
	if koutakuErr.Error == nil {
		koutakuErr.Error = &schemas.ErrorField{}
	}
	koutakuErr.Error.Message = errorResp.Message
	if errorResp.Code != nil {
		koutakuErr.Error.Code = errorResp.Code
	}
	return koutakuErr
}
