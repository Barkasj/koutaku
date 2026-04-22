package cohere

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/schemas"
)

// ToKoutakuResponsesRequest converts a Cohere count tokens request to Koutaku format.
func (req *CohereCountTokensRequest) ToKoutakuResponsesRequest(ctx *schemas.KoutakuContext) *schemas.KoutakuResponsesRequest {
	if req == nil {
		return nil
	}

	provider, model := schemas.ParseModelString(req.Model, utils.CheckAndSetDefaultProvider(ctx, schemas.Cohere))

	userRole := schemas.ResponsesInputMessageRoleUser
	return &schemas.KoutakuResponsesRequest{
		Provider: provider,
		Model:    model,
		Input: []schemas.ResponsesMessage{
			{
				Role: &userRole,
				Content: &schemas.ResponsesMessageContent{
					ContentStr: &req.Text,
				},
			},
		},
	}
}

// ToCohereCountTokensRequest converts a Koutaku count tokens request to Cohere's tokenize payload.
func ToCohereCountTokensRequest(koutakuReq *schemas.KoutakuResponsesRequest) (*CohereCountTokensRequest, error) {
	if koutakuReq == nil {
		return nil, nil
	}

	if koutakuReq.Input == nil {
		return nil, fmt.Errorf("count tokens input is not provided")
	}

	text := buildCohereCountTokensText(koutakuReq.Input)
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, fmt.Errorf("count tokens text is empty after conversion")
	}
	runeCount := utf8.RuneCountInString(trimmed)
	if runeCount < cohereTokenizeMinTextLength || runeCount > cohereTokenizeMaxTextLength {
		return nil, fmt.Errorf("count tokens text length must be between %d and %d characters", cohereTokenizeMinTextLength, cohereTokenizeMaxTextLength)
	}

	cohereReq := &CohereCountTokensRequest{
		Model: koutakuReq.Model,
		Text:  trimmed,
	}
	if koutakuReq.Params != nil {
		cohereReq.ExtraParams = koutakuReq.Params.ExtraParams
	}

	return cohereReq, nil
}

// ToKoutakuCountTokensResponse converts a Cohere tokenize response to Koutaku format.
func (resp *CohereCountTokensResponse) ToKoutakuCountTokensResponse(model string) *schemas.KoutakuCountTokensResponse {
	if resp == nil {
		return nil
	}

	inputTokens := len(resp.Tokens)
	if inputTokens == 0 && len(resp.TokenStrings) > 0 {
		inputTokens = len(resp.TokenStrings)
	}
	totalTokens := inputTokens

	return &schemas.KoutakuCountTokensResponse{
		Model:        model,
		InputTokens:  inputTokens,
		TotalTokens:  &totalTokens,
		TokenStrings: resp.TokenStrings,
		Tokens:       resp.Tokens,
		Object:       "response.input_tokens",
	}
}

// buildCohereCountTokensText flattens Responses messages into a plain text payload for tokenization.
func buildCohereCountTokensText(messages []schemas.ResponsesMessage) string {
	var parts []string

	for _, msg := range messages {
		var contentParts []string

		if msg.Content != nil {
			if msg.Content.ContentStr != nil {
				contentParts = append(contentParts, *msg.Content.ContentStr)
			}
			for _, block := range msg.Content.ContentBlocks {
				if block.Text != nil {
					contentParts = append(contentParts, *block.Text)
				}
				if block.ResponsesOutputMessageContentRefusal != nil && block.ResponsesOutputMessageContentRefusal.Refusal != "" {
					contentParts = append(contentParts, block.ResponsesOutputMessageContentRefusal.Refusal)
				}
			}
		}

		if msg.ResponsesReasoning != nil {
			for _, summary := range msg.ResponsesReasoning.Summary {
				if summary.Text != "" {
					contentParts = append(contentParts, summary.Text)
				}
			}
		}

		if len(contentParts) == 0 {
			continue
		}

		parts = append(parts, strings.Join(contentParts, "\n"))
	}

	return strings.TrimSpace(strings.Join(parts, "\n"))
}
