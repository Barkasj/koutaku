package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	koutaku "github.com/koutaku/koutaku/core"
	"github.com/koutaku/koutaku/core/schemas"
	bfws "github.com/koutaku/koutaku/transports/koutaku-http/websocket"
)

func newRealtimeTurnContext(
	baseCtx *schemas.KoutakuContext,
	requestID string,
	sessionID string,
	providerSessionID string,
	source realtimeTurnSource,
	eventType schemas.RealtimeEventType,
	key *schemas.Key,
) *schemas.KoutakuContext {
	ctx := schemas.NewKoutakuContext(context.Background(), schemas.NoDeadline)
	if baseCtx != nil {
		// Realtime post-hook contexts must preserve plugin-private values written in
		// pre-hooks (for example telemetry start timestamps), not just public keys.
		for ctxKey, value := range baseCtx.GetUserValues() {
			if value != nil {
				ctx.SetValue(ctxKey, value)
			}
		}
	}

	ctx.SetValue(schemas.KoutakuContextKeyHTTPRequestType, schemas.RealtimeRequest)
	if requestID == "" {
		requestID = uuid.NewString()
	}
	ctx.SetValue(schemas.KoutakuContextKeyRequestID, requestID)
	resolvedSessionID := strings.TrimSpace(providerSessionID)
	if resolvedSessionID == "" {
		resolvedSessionID = strings.TrimSpace(sessionID)
	}
	if baseCtx != nil {
		if externalSessionID, ok := baseCtx.Value(schemas.KoutakuContextKeyParentRequestID).(string); ok && strings.TrimSpace(externalSessionID) != "" {
			resolvedSessionID = strings.TrimSpace(externalSessionID)
		}
	}
	if resolvedSessionID != "" {
		ctx.SetValue(schemas.KoutakuContextKeyParentRequestID, resolvedSessionID)
	}
	if strings.TrimSpace(providerSessionID) != "" {
		ctx.SetValue(schemas.KoutakuContextKeyRealtimeSessionID, providerSessionID)
		ctx.SetValue(schemas.KoutakuContextKeyRealtimeProviderSessionID, providerSessionID)
	}
	if source != "" {
		ctx.SetValue(schemas.KoutakuContextKeyRealtimeSource, string(source))
	}
	if eventType != "" {
		ctx.SetValue(schemas.KoutakuContextKeyRealtimeEventType, string(eventType))
	}
	if key != nil {
		if strings.TrimSpace(key.ID) != "" {
			ctx.SetValue(schemas.KoutakuContextKeySelectedKeyID, key.ID)
		}
		if strings.TrimSpace(key.Name) != "" {
			ctx.SetValue(schemas.KoutakuContextKeySelectedKeyName, key.Name)
		}
	}
	return ctx
}

func applyRealtimeTurnContextValues(ctx *schemas.KoutakuContext, values map[any]any) {
	if ctx == nil || len(values) == 0 {
		return
	}
	for ctxKey, value := range values {
		switch ctxKey {
		case schemas.KoutakuContextKeyRequestID,
			schemas.KoutakuContextKeyParentRequestID,
			schemas.KoutakuContextKeyRealtimeSessionID,
			schemas.KoutakuContextKeyRealtimeProviderSessionID,
			schemas.KoutakuContextKeyRealtimeSource,
			schemas.KoutakuContextKeyRealtimeEventType,
			schemas.KoutakuContextKeyStreamStartTime,
			schemas.KoutakuContextKeyStreamEndIndicator:
			continue
		}
		if value != nil {
			ctx.SetValue(ctxKey, value)
		}
	}
}

func setRealtimeTurnStreamContext(ctx *schemas.KoutakuContext, startedAt time.Time, isFinal bool) {
	if ctx == nil {
		return
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	ctx.SetValue(schemas.KoutakuContextKeyStreamStartTime, startedAt)
	if isFinal {
		ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
	}
}

func buildRealtimeTurnPreRequest(provider schemas.ModelProvider, model string, turnInputs []bfws.RealtimeTurnInput) *schemas.KoutakuRequest {
	input := make([]schemas.ResponsesMessage, 0, len(turnInputs))
	for _, turnInput := range turnInputs {
		summary := strings.TrimSpace(turnInput.Summary)
		if summary == "" {
			continue
		}
		switch turnInput.Role {
		case string(schemas.ChatMessageRoleTool):
			itemType := schemas.ResponsesMessageTypeFunctionCallOutput
			output := &schemas.ResponsesToolMessageOutputStruct{
				ResponsesToolCallOutputStr: schemas.Ptr(summary),
			}
			input = append(input, schemas.ResponsesMessage{
				Type:                 &itemType,
				ResponsesToolMessage: &schemas.ResponsesToolMessage{Output: output},
			})
		case string(schemas.ChatMessageRoleUser):
			itemType := schemas.ResponsesMessageTypeMessage
			role := schemas.ResponsesInputMessageRoleUser
			input = append(input, schemas.ResponsesMessage{
				Type:    &itemType,
				Role:    &role,
				Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr(summary)},
			})
		}
	}

	return &schemas.KoutakuRequest{
		RequestType: schemas.RealtimeRequest,
		ResponsesRequest: &schemas.KoutakuResponsesRequest{
			Provider: provider,
			Model:    model,
			Input:    input,
		},
	}
}

func buildRealtimeTurnPostResponse(
	rtProvider schemas.RealtimeProvider,
	provider schemas.ModelProvider,
	model string,
	rawRequest string,
	rawResponse []byte,
	contentOverride string,
	latency int64,
) *schemas.KoutakuResponse {
	output := buildRealtimeTurnOutputMessages(rtProvider, rawResponse, contentOverride)
	resp := &schemas.KoutakuResponsesResponse{
		Object: "response",
		Model:  model,
		Output: output,
		ExtraFields: schemas.KoutakuResponseExtraFields{
			RequestType:            schemas.RealtimeRequest,
			Provider:               provider,
			OriginalModelRequested: model,
			Latency:                latency,
		},
	}
	if usage := extractRealtimeTurnUsage(rtProvider, rawResponse); usage != nil {
		resp.Usage = buildRealtimeResponsesUsage(usage)
	}
	if strings.TrimSpace(rawRequest) != "" {
		resp.ExtraFields.RawRequest = rawRequest
	}
	if len(rawResponse) > 0 {
		resp.ExtraFields.RawResponse = string(rawResponse)
	}

	return &schemas.KoutakuResponse{ResponsesResponse: resp}
}

func buildRealtimeTurnOutputMessages(rtProvider schemas.RealtimeProvider, rawResponse []byte, contentOverride string) []schemas.ResponsesMessage {
	outputs := make([]schemas.ResponsesMessage, 0)
	if outputMessage := extractRealtimeTurnOutputMessage(rtProvider, rawResponse, contentOverride); outputMessage != nil {
		outputs = append(outputs, buildRealtimeResponsesMessagesFromChat(outputMessage, contentOverride)...)
	}

	if len(outputs) > 0 {
		return outputs
	}

	var parsed realtimeResponseDoneEnvelope
	if len(rawResponse) > 0 && schemas.Unmarshal(rawResponse, &parsed) == nil {
		for _, item := range parsed.Response.Output {
			switch item.Type {
			case "message":
				content := strings.TrimSpace(contentOverride)
				if content == "" {
					content = extractRealtimeResponseDoneContentText(item.Content)
				}
				itemType := schemas.ResponsesMessageTypeMessage
				role := schemas.ResponsesInputMessageRoleAssistant
				msg := schemas.ResponsesMessage{
					Type:   &itemType,
					Role:   &role,
					Status: schemas.Ptr("completed"),
				}
				if strings.TrimSpace(item.ID) != "" {
					msg.ID = schemas.Ptr(strings.TrimSpace(item.ID))
				}
				if content != "" {
					msg.Content = &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr(content)}
				}
				outputs = append(outputs, msg)
			case "function_call":
				itemType := schemas.ResponsesMessageTypeFunctionCall
				msg := schemas.ResponsesMessage{
					Type:   &itemType,
					Status: schemas.Ptr("completed"),
					ResponsesToolMessage: &schemas.ResponsesToolMessage{
						Name:      schemas.Ptr(strings.TrimSpace(item.Name)),
						Arguments: schemas.Ptr(item.Arguments),
					},
				}
				if strings.TrimSpace(item.ID) != "" {
					msg.ID = schemas.Ptr(strings.TrimSpace(item.ID))
				}
				if strings.TrimSpace(item.CallID) != "" {
					msg.CallID = schemas.Ptr(strings.TrimSpace(item.CallID))
				}
				outputs = append(outputs, msg)
			}
		}
	}

	if len(outputs) == 0 && strings.TrimSpace(contentOverride) != "" {
		itemType := schemas.ResponsesMessageTypeMessage
		role := schemas.ResponsesInputMessageRoleAssistant
		outputs = append(outputs, schemas.ResponsesMessage{
			Type:    &itemType,
			Role:    &role,
			Status:  schemas.Ptr("completed"),
			Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr(strings.TrimSpace(contentOverride))},
		})
	}

	return outputs
}

func buildRealtimeResponsesMessagesFromChat(message *schemas.ChatMessage, contentOverride string) []schemas.ResponsesMessage {
	if message == nil {
		return nil
	}

	outputs := make([]schemas.ResponsesMessage, 0, 1)
	content := strings.TrimSpace(contentOverride)
	if content == "" && message.Content != nil && message.Content.ContentStr != nil {
		content = strings.TrimSpace(*message.Content.ContentStr)
	}
	if content != "" {
		itemType := schemas.ResponsesMessageTypeMessage
		role := schemas.ResponsesInputMessageRoleAssistant
		outputs = append(outputs, schemas.ResponsesMessage{
			Type:    &itemType,
			Role:    &role,
			Status:  schemas.Ptr("completed"),
			Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr(content)},
		})
	}

	if message.ChatAssistantMessage == nil {
		return outputs
	}

	for _, toolCall := range message.ChatAssistantMessage.ToolCalls {
		itemType := schemas.ResponsesMessageTypeFunctionCall
		msg := schemas.ResponsesMessage{
			Type:   &itemType,
			Status: schemas.Ptr("completed"),
			ResponsesToolMessage: &schemas.ResponsesToolMessage{
				Arguments: schemas.Ptr(toolCall.Function.Arguments),
			},
		}
		if toolCall.Function.Name != nil {
			msg.ResponsesToolMessage.Name = schemas.Ptr(strings.TrimSpace(*toolCall.Function.Name))
		}
		if toolCall.ID != nil {
			msg.CallID = schemas.Ptr(strings.TrimSpace(*toolCall.ID))
			msg.ID = schemas.Ptr(strings.TrimSpace(*toolCall.ID))
		}
		outputs = append(outputs, msg)
	}

	return outputs
}

func extractRealtimeResponseDoneContentText(content []realtimeResponseDoneContent) string {
	for _, block := range content {
		switch {
		case strings.TrimSpace(block.Text) != "":
			return strings.TrimSpace(block.Text)
		case strings.TrimSpace(block.Transcript) != "":
			return strings.TrimSpace(block.Transcript)
		case strings.TrimSpace(block.Refusal) != "":
			return strings.TrimSpace(block.Refusal)
		}
	}
	return ""
}

func buildRealtimeResponsesUsage(usage *schemas.KoutakuLLMUsage) *schemas.ResponsesResponseUsage {
	if usage == nil {
		return nil
	}
	result := &schemas.ResponsesResponseUsage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
		TotalTokens:  usage.TotalTokens,
	}
	if usage.PromptTokensDetails != nil {
		result.InputTokensDetails = &schemas.ResponsesResponseInputTokens{
			TextTokens:        usage.PromptTokensDetails.TextTokens,
			AudioTokens:       usage.PromptTokensDetails.AudioTokens,
			ImageTokens:       usage.PromptTokensDetails.ImageTokens,
			CachedReadTokens:  usage.PromptTokensDetails.CachedReadTokens,
			CachedWriteTokens: usage.PromptTokensDetails.CachedWriteTokens,
		}
	}
	if usage.CompletionTokensDetails != nil {
		result.OutputTokensDetails = &schemas.ResponsesResponseOutputTokens{
			TextTokens:               usage.CompletionTokensDetails.TextTokens,
			AcceptedPredictionTokens: usage.CompletionTokensDetails.AcceptedPredictionTokens,
			AudioTokens:              usage.CompletionTokensDetails.AudioTokens,
			ImageTokens:              usage.CompletionTokensDetails.ImageTokens,
			ReasoningTokens:          usage.CompletionTokensDetails.ReasoningTokens,
			RejectedPredictionTokens: usage.CompletionTokensDetails.RejectedPredictionTokens,
			CitationTokens:           usage.CompletionTokensDetails.CitationTokens,
			NumSearchQueries:         usage.CompletionTokensDetails.NumSearchQueries,
		}
	}
	return result
}

func newRealtimeTurnErrorEventPayload(koutakuErr *schemas.KoutakuError) []byte {
	if koutakuErr == nil {
		return []byte(`{"type":"error","error":{"type":"server_error","message":"internal server error"}}`)
	}

	errorType, errorCode, errorMessage, errorParam := mapRealtimeWireErrorFields(koutakuErr)
	payload := schemas.KoutakuRealtimeEvent{
		Type: schemas.RTEventError,
		Error: &schemas.RealtimeError{
			Type:    errorType,
			Code:    errorCode,
			Message: errorMessage,
			Param:   errorParam,
		},
	}
	if data, err := schemas.Marshal(payload); err == nil {
		return data
	}
	return []byte(`{"type":"error","error":{"type":"server_error","message":"internal server error"}}`)
}

// isBudgetOrBillingError returns true if the lowercased value indicates a budget or billing exhaustion error.
// Quota/rate-limit patterns (quota_exceeded, quota exceeded, etc.) are already covered by koutaku.IsRateLimitErrorMessage.
func isBudgetOrBillingError(lower string) bool {
	return strings.Contains(lower, "budget_exceeded") ||
		strings.Contains(lower, "budget exceeded") ||
		strings.Contains(lower, "insufficient_quota") ||
		strings.Contains(lower, "hard limit reached") ||
		strings.Contains(lower, "billing hard limit")
}

func mapRealtimeWireErrorFields(koutakuErr *schemas.KoutakuError) (string, string, string, string) {
	errorType := "server_error"
	errorCode := "server_error"
	errorMessage := "internal server error"
	errorParam := ""

	if koutakuErr == nil {
		return errorType, errorCode, errorMessage, errorParam
	}

	var values []string
	if koutakuErr.Type != nil {
		values = append(values, strings.TrimSpace(*koutakuErr.Type))
	}
	if koutakuErr.Error != nil {
		if koutakuErr.Error.Type != nil {
			values = append(values, strings.TrimSpace(*koutakuErr.Error.Type))
		}
		if koutakuErr.Error.Code != nil {
			values = append(values, strings.TrimSpace(*koutakuErr.Error.Code))
		}
		if strings.TrimSpace(koutakuErr.Error.Message) != "" {
			errorMessage = strings.TrimSpace(koutakuErr.Error.Message)
			values = append(values, errorMessage)
		}
		if koutakuErr.Error.Param != nil {
			errorParam = strings.TrimSpace(fmt.Sprint(koutakuErr.Error.Param))
		}
	}

	for _, value := range values {
		lower := strings.ToLower(value)
		switch {
		case lower == "":
			continue
		case strings.Contains(lower, "invalid_request_error"):
			return "invalid_request_error", "invalid_request_error", errorMessage, errorParam
		case isBudgetOrBillingError(lower):
			return "insufficient_quota", "insufficient_quota", errorMessage, errorParam
		case koutaku.IsRateLimitErrorMessage(lower):
			return "rate_limit_exceeded", "rate_limit_exceeded", errorMessage, errorParam
		}
	}

	return errorType, errorCode, errorMessage, errorParam
}

func shouldGracefullyDisconnectRealtime(koutakuErr *schemas.KoutakuError) bool {
	if koutakuErr == nil {
		return false
	}

	var values []string
	if koutakuErr.Type != nil {
		values = append(values, strings.TrimSpace(*koutakuErr.Type))
	}
	if koutakuErr.Error != nil {
		if koutakuErr.Error.Type != nil {
			values = append(values, strings.TrimSpace(*koutakuErr.Error.Type))
		}
		if koutakuErr.Error.Code != nil {
			values = append(values, strings.TrimSpace(*koutakuErr.Error.Code))
		}
		values = append(values, strings.TrimSpace(koutakuErr.Error.Message))
	}

	for _, value := range values {
		lower := strings.ToLower(value)
		if lower == "" {
			continue
		}
		if isBudgetOrBillingError(lower) || koutaku.IsRateLimitErrorMessage(lower) {
			return true
		}
	}

	return false
}

func startRealtimeTurnHooks(
	client *koutaku.Koutaku,
	baseCtx *schemas.KoutakuContext,
	session *bfws.Session,
	rtProvider schemas.RealtimeProvider,
	provider schemas.ModelProvider,
	model string,
	key *schemas.Key,
	startEventType schemas.RealtimeEventType,
) *schemas.KoutakuError {
	if client == nil || session == nil {
		return &schemas.KoutakuError{
			Type:       schemas.Ptr("server_error"),
			StatusCode: schemas.Ptr(500),
			Error: &schemas.ErrorField{
				Type:    schemas.Ptr("server_error"),
				Message: "realtime turn pipeline is unavailable",
			},
		}
	}
	if !session.TryBeginRealtimeTurnHooks() {
		return &schemas.KoutakuError{
			Type:       schemas.Ptr("invalid_request_error"),
			StatusCode: schemas.Ptr(400),
			Error: &schemas.ErrorField{
				Type:    schemas.Ptr("invalid_request_error"),
				Message: "Conversation already has an active response in progress.",
			},
		}
	}
	committed := false
	defer func() {
		if !committed {
			session.AbortRealtimeTurnHooks()
		}
	}()

	startedAt := time.Now()
	turnCtx := newRealtimeTurnContext(baseCtx, "", session.ID(), session.ProviderSessionID(), realtimeTurnSourceEI, startEventType, key)
	setRealtimeTurnStreamContext(turnCtx, startedAt, false)
	req := buildRealtimeTurnPreRequest(provider, model, session.PeekRealtimeTurnInputs())
	hooks, koutakuErr := client.RunRealtimeTurnPreHooks(turnCtx, req)
	if koutakuErr != nil {
		// RunRealtimeTurnPreHooks already executed post-hooks and flushed the trace
		// for this turn-start failure. Clear buffered turn state so transport-close
		// fallback finalization does not emit the same error a second time.
		session.ConsumeRealtimeTurnInputs()
		session.ConsumeRealtimeOutputText()
		return koutakuErr
	}

	requestID, _ := turnCtx.Value(schemas.KoutakuContextKeyRequestID).(string)
	session.SetRealtimeTurnHooks(&bfws.RealtimeTurnPluginState{
		PostHookRunner: hooks.PostHookRunner,
		Cleanup:        hooks.Cleanup,
		RequestID:      requestID,
		StartedAt:      startedAt,
		PreHookValues:  turnCtx.GetUserValues(),
	})
	committed = true
	return nil
}

func finalizeRealtimeTurnHooks(
	client *koutaku.Koutaku,
	baseCtx *schemas.KoutakuContext,
	session *bfws.Session,
	rtProvider schemas.RealtimeProvider,
	provider schemas.ModelProvider,
	model string,
	key *schemas.Key,
	rawResponse []byte,
	contentOverride string,
) *schemas.KoutakuError {
	if client == nil || session == nil {
		return nil
	}

	turnInputs := session.ConsumeRealtimeTurnInputs()
	rawRequest := combineRealtimeInputRaw(turnInputs)

	if activeHooks := session.ConsumeRealtimeTurnHooks(); activeHooks != nil {
		defer func() {
			if activeHooks.Cleanup != nil {
				activeHooks.Cleanup()
			}
		}()
		postResponse := buildRealtimeTurnPostResponse(
			rtProvider,
			provider,
			model,
			rawRequest,
			rawResponse,
			contentOverride,
			time.Since(activeHooks.StartedAt).Milliseconds(),
		)
		postCtx := newRealtimeTurnContext(baseCtx, activeHooks.RequestID, session.ID(), session.ProviderSessionID(), realtimeTurnSourceLM, rtProvider.RealtimeTurnFinalEvent(), key)
		applyRealtimeTurnContextValues(postCtx, activeHooks.PreHookValues)
		setRealtimeTurnStreamContext(postCtx, activeHooks.StartedAt, true)
		_, koutakuErr := activeHooks.PostHookRunner(postCtx, postResponse, nil)
		completeRealtimeTurnTrace(postCtx)
		return koutakuErr
	}

	startedAt := time.Now()
	preCtx := newRealtimeTurnContext(baseCtx, "", session.ID(), session.ProviderSessionID(), realtimeTurnSourceEI, "", key)
	setRealtimeTurnStreamContext(preCtx, startedAt, false)
	preReq := buildRealtimeTurnPreRequest(provider, model, turnInputs)
	hooks, koutakuErr := client.RunRealtimeTurnPreHooks(preCtx, preReq)
	if koutakuErr != nil {
		return koutakuErr
	}
	if hooks.Cleanup != nil {
		defer hooks.Cleanup()
	}

	requestID, _ := preCtx.Value(schemas.KoutakuContextKeyRequestID).(string)
	postResponse := buildRealtimeTurnPostResponse(
		rtProvider,
		provider,
		model,
		rawRequest,
		rawResponse,
		contentOverride,
		time.Since(startedAt).Milliseconds(),
	)
	postCtx := newRealtimeTurnContext(baseCtx, requestID, session.ID(), session.ProviderSessionID(), realtimeTurnSourceLM, rtProvider.RealtimeTurnFinalEvent(), key)
	applyRealtimeTurnContextValues(postCtx, preCtx.GetUserValues())
	setRealtimeTurnStreamContext(postCtx, startedAt, true)
	_, koutakuErr = hooks.PostHookRunner(postCtx, postResponse, nil)
	completeRealtimeTurnTrace(postCtx)
	return koutakuErr
}

func finalizeRealtimeTurnHooksWithError(
	client *koutaku.Koutaku,
	baseCtx *schemas.KoutakuContext,
	session *bfws.Session,
	provider schemas.ModelProvider,
	model string,
	key *schemas.Key,
	eventType schemas.RealtimeEventType,
	rawResponse []byte,
	koutakuErr *schemas.KoutakuError,
) *schemas.KoutakuError {
	if session == nil || koutakuErr == nil {
		return nil
	}

	turnInputs := session.ConsumeRealtimeTurnInputs()
	rawRequest := combineRealtimeInputRaw(turnInputs)
	session.ConsumeRealtimeOutputText()

	if activeHooks := session.ConsumeRealtimeTurnHooks(); activeHooks != nil {
		defer func() {
			if activeHooks.Cleanup != nil {
				activeHooks.Cleanup()
			}
		}()
		postErr := buildRealtimeTurnPostError(
			provider,
			model,
			rawRequest,
			rawResponse,
			koutakuErr,
		)
		postCtx := newRealtimeTurnContext(baseCtx, activeHooks.RequestID, session.ID(), session.ProviderSessionID(), realtimeTurnSourceLM, eventType, key)
		applyRealtimeTurnContextValues(postCtx, activeHooks.PreHookValues)
		setRealtimeTurnStreamContext(postCtx, activeHooks.StartedAt, true)
		_, hookErr := activeHooks.PostHookRunner(postCtx, nil, postErr)
		completeRealtimeTurnTrace(postCtx)
		return hookErr
	}

	if len(turnInputs) == 0 {
		return nil
	}

	if client == nil {
		return nil
	}

	startedAt := time.Now()
	preCtx := newRealtimeTurnContext(baseCtx, "", session.ID(), session.ProviderSessionID(), realtimeTurnSourceEI, "", key)
	setRealtimeTurnStreamContext(preCtx, startedAt, false)
	preReq := buildRealtimeTurnPreRequest(provider, model, turnInputs)
	hooks, hookPreErr := client.RunRealtimeTurnPreHooks(preCtx, preReq)
	if hookPreErr != nil {
		return hookPreErr
	}
	if hooks.Cleanup != nil {
		defer hooks.Cleanup()
	}

	requestID, _ := preCtx.Value(schemas.KoutakuContextKeyRequestID).(string)
	postErr := buildRealtimeTurnPostError(
		provider,
		model,
		rawRequest,
		rawResponse,
		koutakuErr,
	)
	postCtx := newRealtimeTurnContext(baseCtx, requestID, session.ID(), session.ProviderSessionID(), realtimeTurnSourceLM, eventType, key)
	applyRealtimeTurnContextValues(postCtx, preCtx.GetUserValues())
	setRealtimeTurnStreamContext(postCtx, startedAt, true)
	_, hookErr := hooks.PostHookRunner(postCtx, nil, postErr)
	completeRealtimeTurnTrace(postCtx)
	return hookErr
}

func buildRealtimeTurnPostError(
	provider schemas.ModelProvider,
	model string,
	rawRequest string,
	rawResponse []byte,
	koutakuErr *schemas.KoutakuError,
) *schemas.KoutakuError {
	if koutakuErr == nil {
		return nil
	}

	copied := *koutakuErr
	copied.ExtraFields = koutakuErr.ExtraFields
	if koutakuErr.Error != nil {
		errorCopy := *koutakuErr.Error
		copied.Error = &errorCopy
	}
	copied.ExtraFields.RequestType = schemas.RealtimeRequest
	if copied.ExtraFields.Provider == "" {
		copied.ExtraFields.Provider = provider
	}
	if strings.TrimSpace(copied.ExtraFields.OriginalModelRequested) == "" {
		copied.ExtraFields.OriginalModelRequested = model
	}
	if strings.TrimSpace(rawRequest) != "" && copied.ExtraFields.RawRequest == nil {
		copied.ExtraFields.RawRequest = rawRequest
	}
	if len(rawResponse) > 0 && copied.ExtraFields.RawResponse == nil {
		copied.ExtraFields.RawResponse = json.RawMessage(append([]byte(nil), rawResponse...))
	}
	return &copied
}

func newKoutakuErrorFromRealtimeError(
	provider schemas.ModelProvider,
	model string,
	rawResponse []byte,
	realtimeErr *schemas.RealtimeError,
) *schemas.KoutakuError {
	if realtimeErr == nil {
		return nil
	}

	statusCode := 500
	values := []string{
		strings.TrimSpace(realtimeErr.Type),
		strings.TrimSpace(realtimeErr.Code),
		strings.TrimSpace(realtimeErr.Message),
	}
	for _, value := range values {
		lower := strings.ToLower(value)
		switch {
		case lower == "":
			continue
		case strings.Contains(lower, "invalid_request_error"):
			statusCode = 400
		case isBudgetOrBillingError(lower), koutaku.IsRateLimitErrorMessage(lower):
			statusCode = 429
		}
	}

	errType := strings.TrimSpace(realtimeErr.Type)
	if errType == "" {
		errType = "server_error"
	}
	errCode := strings.TrimSpace(realtimeErr.Code)
	if errCode == "" {
		errCode = errType
	}
	message := strings.TrimSpace(realtimeErr.Message)
	if message == "" {
		message = "realtime turn failed"
	}

	koutakuErr := &schemas.KoutakuError{
		IsKoutakuError: true,
		StatusCode:     schemas.Ptr(statusCode),
		Type:           schemas.Ptr(errType),
		Error: &schemas.ErrorField{
			Type:    schemas.Ptr(errType),
			Code:    schemas.Ptr(errCode),
			Message: message,
		},
		ExtraFields: schemas.KoutakuErrorExtraFields{
			Provider:               provider,
			OriginalModelRequested: model,
			RequestType:            schemas.RealtimeRequest,
		},
	}
	if strings.TrimSpace(realtimeErr.Param) != "" {
		koutakuErr.Error.Param = realtimeErr.Param
	}
	if len(rawResponse) > 0 {
		koutakuErr.ExtraFields.RawResponse = json.RawMessage(append([]byte(nil), rawResponse...))
	}
	return koutakuErr
}

func completeRealtimeTurnTrace(ctx *schemas.KoutakuContext) {
	if ctx == nil {
		return
	}
	traceID, _ := ctx.Value(schemas.KoutakuContextKeyTraceID).(string)
	if strings.TrimSpace(traceID) == "" {
		return
	}
	tracer, _ := ctx.Value(schemas.KoutakuContextKeyTracer).(schemas.Tracer)
	if tracer == nil {
		return
	}
	tracer.CompleteAndFlushTrace(strings.TrimSpace(traceID))
}

func finalizeRealtimeTurnHooksOnTransportError(
	client *koutaku.Koutaku,
	baseCtx *schemas.KoutakuContext,
	session *bfws.Session,
	provider schemas.ModelProvider,
	model string,
	key *schemas.Key,
	status int,
	code string,
	message string,
) *schemas.KoutakuError {
	return finalizeRealtimeTurnHooksWithError(
		client,
		baseCtx,
		session,
		provider,
		model,
		key,
		schemas.RTEventError,
		nil,
		newRealtimeWireKoutakuError(status, code, message),
	)
}
