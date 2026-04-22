package huggingface

import (
	"fmt"

	"github.com/koutaku/koutaku/core/schemas"
)

// ToHuggingFaceResponsesRequest converts a Koutaku Responses request into the Hugging Face
// chat-completions payload that the provider already understands.
func ToHuggingFaceResponsesRequest(koutakuReq *schemas.KoutakuResponsesRequest) (*HuggingFaceChatRequest, error) {
	if koutakuReq == nil {
		return nil, nil
	}

	chatReq := koutakuReq.ToChatRequest()
	if chatReq == nil {
		return nil, fmt.Errorf("failed to convert responses request to chat request")
	}

	hfReq, err := ToHuggingFaceChatCompletionRequest(chatReq)
	if err != nil {
		return nil, err
	}
	if hfReq == nil {
		return nil, fmt.Errorf("failed to convert chat request to Hugging Face request")
	}

	return hfReq, nil
}

// ToKoutakuResponsesResponseFromHuggingFace converts a Koutaku chat response into the
// Koutaku Responses response shape, preserving provider metadata.
func ToKoutakuResponsesResponseFromHuggingFace(resp *schemas.KoutakuChatResponse, requestedModel string) (*schemas.KoutakuResponsesResponse, error) {
	if resp == nil {
		return nil, nil
	}

	// Ensure model is set
	if resp.Model == "" {
		resp.Model = requestedModel
	}

	responsesResp := resp.ToKoutakuResponsesResponse()
	if responsesResp != nil {
	}

	return responsesResp, nil
}
