package anthropic

import (
	"github.com/koutaku/koutaku/core/schemas"
)

// ToKoutakuCountTokensResponse converts an Anthropic count tokens response to Koutaku format
func (resp *AnthropicCountTokensResponse) ToKoutakuCountTokensResponse(model string) *schemas.KoutakuCountTokensResponse {
	if resp == nil {
		return nil
	}

	totalTokens := resp.InputTokens

	koutakuResp := &schemas.KoutakuCountTokensResponse{
		Model:       model,
		InputTokens: resp.InputTokens,
		TotalTokens: &totalTokens,
		Object:      "response.input_tokens",
	}

	return koutakuResp
}

// ToAnthropicCountTokensResponse converts a Koutaku count tokens response to Anthropic format.
func ToAnthropicCountTokensResponse(koutakuResp *schemas.KoutakuCountTokensResponse) *AnthropicCountTokensResponse {
	if koutakuResp == nil {
		return nil
	}

	return &AnthropicCountTokensResponse{
		InputTokens: koutakuResp.InputTokens,
	}
}
