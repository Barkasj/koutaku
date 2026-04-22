package bedrock

import (
	"net/http"

	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

func parseBedrockHTTPError(statusCode int, headers http.Header, body []byte) *schemas.KoutakuError {
	fastResp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(fastResp)

	fastResp.SetStatusCode(statusCode)
	for k, values := range headers {
		for _, value := range values {
			fastResp.Header.Add(k, value)
		}
	}
	fastResp.SetBody(body)

	var errorResp BedrockError
	koutakuErr := providerUtils.HandleProviderAPIError(fastResp, &errorResp)
	if errorResp.Message != "" {
		if koutakuErr.Error == nil {
			koutakuErr.Error = &schemas.ErrorField{}
		}
		koutakuErr.Error.Message = errorResp.Message
		koutakuErr.Error.Code = errorResp.Code
	}

	return koutakuErr
}
