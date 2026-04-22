package vllm

import (
	"github.com/bytedance/sonic"
	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	schemas "github.com/koutaku/koutaku/core/schemas"
)

func HandleVLLMResponse[T any](responseBody []byte, response *T, requestBody []byte, sendBackRawRequest bool, sendBackRawResponse bool) (rawRequest interface{}, rawResponse interface{}, koutakuErr *schemas.KoutakuError) {
	var errorResp schemas.KoutakuError
	rawRequest, rawResponse, koutakuErr = providerUtils.HandleProviderResponse(responseBody, response, requestBody, sendBackRawRequest, sendBackRawResponse)
	if koutakuErr != nil {
		return rawRequest, rawResponse, koutakuErr
	}
	if err := sonic.Unmarshal(responseBody, &errorResp); err == nil && errorResp.Error != nil && errorResp.Error.Message != "" {
		return rawRequest, rawResponse, &errorResp
	}
	return rawRequest, rawResponse, nil
}
