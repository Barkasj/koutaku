package handlers

import (
	"fmt"
	"strconv"

	"github.com/fasthttp/router"
	koutaku "github.com/koutaku/koutaku/core"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/koutaku/koutaku/framework/logstore"
	"github.com/koutaku/koutaku/transports/koutaku-http/lib"
	"github.com/valyala/fasthttp"
)

// --- HTTP Handler ---

// AsyncHandler handles async job HTTP endpoints.
type AsyncHandler struct {
	client       *koutaku.Koutaku
	executor     *logstore.AsyncJobExecutor
	handlerStore lib.HandlerStore
	config       *lib.Config
}

// AsyncPathToTypeMapping maps exact paths to request types (only for non-parameterized paths)
// Parameterized paths are set per-route in RegisterRoutes
var AsyncPathToTypeMapping = map[string]schemas.RequestType{
	"/v1/async/completions":          schemas.TextCompletionRequest,
	"/v1/async/chat/completions":     schemas.ChatCompletionRequest,
	"/v1/async/responses":            schemas.ResponsesRequest,
	"/v1/async/embeddings":           schemas.EmbeddingRequest,
	"/v1/async/audio/speech":         schemas.SpeechRequest,
	"/v1/async/audio/transcriptions": schemas.TranscriptionRequest,
	"/v1/async/images/generations":   schemas.ImageGenerationRequest,
	"/v1/async/images/edits":         schemas.ImageEditRequest,
	"/v1/async/images/variations":    schemas.ImageVariationRequest,
	"/v1/async/rerank":               schemas.RerankRequest,
	"/v1/async/ocr":                  schemas.OCRRequest,
}

// RegisterAsyncRequestTypeMiddleware handles exact path matching for non-parameterized routes
func RegisterAsyncRequestTypeMiddleware(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		path := string(ctx.Path())
		if requestType, ok := AsyncPathToTypeMapping[path]; ok {
			ctx.SetUserValue(schemas.KoutakuContextKeyHTTPRequestType, requestType)
		}
		next(ctx)
	}
}

// NewAsyncHandler creates a new AsyncHandler.
// If the async job executor is not available (e.g., LogsStore or governance plugin not configured),
// the handler is created with a nil executor and RegisterRoutes will skip async route registration.
func NewAsyncHandler(client *koutaku.Koutaku, config *lib.Config) *AsyncHandler {
	return &AsyncHandler{
		client:       client,
		executor:     config.GetAsyncJobExecutor(),
		handlerStore: config,
		config:       config,
	}
}

// RegisterRoutes registers async job endpoints.
func (h *AsyncHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.KoutakuHTTPMiddleware) {
	if h.executor == nil {
		return // LogStore not configured, skip async routes
	}

	baseMiddlewares := append([]schemas.KoutakuHTTPMiddleware{RegisterAsyncRequestTypeMiddleware}, middlewares...)

	// Async submission endpoints (non-parameterized, request type set via AsyncPathToTypeMapping)
	r.POST("/v1/async/completions", lib.ChainMiddlewares(h.asyncTextCompletion, baseMiddlewares...))
	r.POST("/v1/async/chat/completions", lib.ChainMiddlewares(h.asyncChatCompletion, baseMiddlewares...))
	r.POST("/v1/async/responses", lib.ChainMiddlewares(h.asyncResponses, baseMiddlewares...))
	r.POST("/v1/async/embeddings", lib.ChainMiddlewares(h.asyncEmbeddings, baseMiddlewares...))
	r.POST("/v1/async/audio/speech", lib.ChainMiddlewares(h.asyncSpeech, baseMiddlewares...))
	r.POST("/v1/async/audio/transcriptions", lib.ChainMiddlewares(h.asyncTranscription, baseMiddlewares...))
	r.POST("/v1/async/images/generations", lib.ChainMiddlewares(h.asyncImageGeneration, baseMiddlewares...))
	r.POST("/v1/async/images/edits", lib.ChainMiddlewares(h.asyncImageEdit, baseMiddlewares...))
	r.POST("/v1/async/images/variations", lib.ChainMiddlewares(h.asyncImageVariation, baseMiddlewares...))
	r.POST("/v1/async/rerank", lib.ChainMiddlewares(h.asyncRerank, baseMiddlewares...))
	r.POST("/v1/async/ocr", lib.ChainMiddlewares(h.asyncOCR, baseMiddlewares...))

	// Async job retrieval endpoints
	r.GET("/v1/async/completions/{job_id}", lib.ChainMiddlewares(h.getJob(schemas.TextCompletionRequest), middlewares...))
	r.GET("/v1/async/chat/completions/{job_id}", lib.ChainMiddlewares(h.getJob(schemas.ChatCompletionRequest), middlewares...))
	r.GET("/v1/async/responses/{job_id}", lib.ChainMiddlewares(h.getJob(schemas.ResponsesRequest), middlewares...))
	r.GET("/v1/async/embeddings/{job_id}", lib.ChainMiddlewares(h.getJob(schemas.EmbeddingRequest), middlewares...))
	r.GET("/v1/async/audio/speech/{job_id}", lib.ChainMiddlewares(h.getJob(schemas.SpeechRequest), middlewares...))
	r.GET("/v1/async/audio/transcriptions/{job_id}", lib.ChainMiddlewares(h.getJob(schemas.TranscriptionRequest), middlewares...))
	r.GET("/v1/async/images/generations/{job_id}", lib.ChainMiddlewares(h.getJob(schemas.ImageGenerationRequest), middlewares...))
	r.GET("/v1/async/images/edits/{job_id}", lib.ChainMiddlewares(h.getJob(schemas.ImageEditRequest), middlewares...))
	r.GET("/v1/async/images/variations/{job_id}", lib.ChainMiddlewares(h.getJob(schemas.ImageVariationRequest), middlewares...))
	r.GET("/v1/async/rerank/{job_id}", lib.ChainMiddlewares(h.getJob(schemas.RerankRequest), middlewares...))
	r.GET("/v1/async/ocr/{job_id}", lib.ChainMiddlewares(h.getJob(schemas.OCRRequest), middlewares...))
}

// --- Async submission handlers ---

// asyncTextCompletion handles POST /v1/async/completions
func (h *AsyncHandler) asyncTextCompletion(ctx *fasthttp.RequestCtx) {
	req, koutakuTextReq, err := prepareTextCompletionRequest(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	if req.Stream != nil && *req.Stream {
		SendError(ctx, fasthttp.StatusBadRequest, "stream is not supported for async text completions")
		return
	}

	koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, h.handlerStore.ShouldAllowDirectKeys(), h.config.GetHeaderMatcher(), h.config.GetMCPHeaderCombinedAllowlist())
	if koutakuCtx == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
		return
	}
	defer cancel()

	resultTTL := getResultTTLFromHeaderWithDefault(ctx, h.config.ClientConfig.AsyncJobResultTTL)

	job, err := h.executor.SubmitJob(
		koutakuCtx,
		resultTTL,
		func(bgCtx *schemas.KoutakuContext) (interface{}, *schemas.KoutakuError) {
			return h.client.TextCompletionRequest(bgCtx, koutakuTextReq)
		},
		schemas.TextCompletionRequest,
	)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, err.Error())
		return
	}
	SendJSONWithStatus(ctx, job.ToResponse(), fasthttp.StatusAccepted)
}

// asyncChatCompletion handles POST /v1/async/chat/completions
func (h *AsyncHandler) asyncChatCompletion(ctx *fasthttp.RequestCtx) {
	req, koutakuChatReq, err := prepareChatCompletionRequest(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	if req.Stream != nil && *req.Stream {
		SendError(ctx, fasthttp.StatusBadRequest, "stream is not supported for async chat completions")
		return
	}

	koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, h.handlerStore.ShouldAllowDirectKeys(), h.config.GetHeaderMatcher(), h.config.GetMCPHeaderCombinedAllowlist())
	if koutakuCtx == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
		return
	}
	defer cancel()

	resultTTL := getResultTTLFromHeaderWithDefault(ctx, h.config.ClientConfig.AsyncJobResultTTL)

	job, err := h.executor.SubmitJob(
		koutakuCtx,
		resultTTL,
		func(bgCtx *schemas.KoutakuContext) (interface{}, *schemas.KoutakuError) {
			return h.client.ChatCompletionRequest(bgCtx, koutakuChatReq)
		},
		schemas.ChatCompletionRequest,
	)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	SendJSONWithStatus(ctx, job.ToResponse(), fasthttp.StatusAccepted)
}

// asyncResponses handles POST /v1/async/responses
func (h *AsyncHandler) asyncResponses(ctx *fasthttp.RequestCtx) {
	req, koutakuResponsesReq, err := prepareResponsesRequest(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	if req.Stream != nil && *req.Stream {
		SendError(ctx, fasthttp.StatusBadRequest, "stream is not supported for async responses")
		return
	}

	koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, h.handlerStore.ShouldAllowDirectKeys(), h.config.GetHeaderMatcher(), h.config.GetMCPHeaderCombinedAllowlist())
	if koutakuCtx == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
		return
	}
	defer cancel()

	resultTTL := getResultTTLFromHeaderWithDefault(ctx, h.config.ClientConfig.AsyncJobResultTTL)

	job, err := h.executor.SubmitJob(
		koutakuCtx,
		resultTTL,
		func(bgCtx *schemas.KoutakuContext) (interface{}, *schemas.KoutakuError) {
			return h.client.ResponsesRequest(bgCtx, koutakuResponsesReq)
		},
		schemas.ResponsesRequest,
	)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, fmt.Sprintf("Failed to create async job: %v", err))
		return
	}

	SendJSONWithStatus(ctx, job.ToResponse(), fasthttp.StatusAccepted)
}

// asyncEmbeddings handles POST /v1/async/embeddings
func (h *AsyncHandler) asyncEmbeddings(ctx *fasthttp.RequestCtx) {
	_, koutakuEmbeddingReq, err := prepareEmbeddingRequest(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, h.handlerStore.ShouldAllowDirectKeys(), h.config.GetHeaderMatcher(), h.config.GetMCPHeaderCombinedAllowlist())
	if koutakuCtx == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
		return
	}
	defer cancel()

	resultTTL := getResultTTLFromHeaderWithDefault(ctx, h.config.ClientConfig.AsyncJobResultTTL)

	job, err := h.executor.SubmitJob(
		koutakuCtx,
		resultTTL,
		func(bgCtx *schemas.KoutakuContext) (interface{}, *schemas.KoutakuError) {
			return h.client.EmbeddingRequest(bgCtx, koutakuEmbeddingReq)
		},
		schemas.EmbeddingRequest,
	)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	SendJSONWithStatus(ctx, job.ToResponse(), fasthttp.StatusAccepted)
}

// asyncSpeech handles POST /v1/async/audio/speech
func (h *AsyncHandler) asyncSpeech(ctx *fasthttp.RequestCtx) {
	req, koutakuSpeechReq, err := prepareSpeechRequest(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	if req.StreamFormat != nil && *req.StreamFormat == "sse" {
		SendError(ctx, fasthttp.StatusBadRequest, "stream is not supported for async speech")
		return
	}

	koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, h.handlerStore.ShouldAllowDirectKeys(), h.config.GetHeaderMatcher(), h.config.GetMCPHeaderCombinedAllowlist())
	if koutakuCtx == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
		return
	}
	defer cancel()

	resultTTL := getResultTTLFromHeaderWithDefault(ctx, h.config.ClientConfig.AsyncJobResultTTL)

	job, err := h.executor.SubmitJob(
		koutakuCtx,
		resultTTL,
		func(bgCtx *schemas.KoutakuContext) (interface{}, *schemas.KoutakuError) {
			return h.client.SpeechRequest(bgCtx, koutakuSpeechReq)
		},
		schemas.SpeechRequest,
	)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	SendJSONWithStatus(ctx, job.ToResponse(), fasthttp.StatusAccepted)
}

// asyncTranscription handles POST /v1/async/audio/transcriptions
func (h *AsyncHandler) asyncTranscription(ctx *fasthttp.RequestCtx) {
	koutakuTranscriptionReq, stream, err := prepareTranscriptionRequest(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	if stream {
		SendError(ctx, fasthttp.StatusBadRequest, "stream is not supported for async transcriptions")
		return
	}

	koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, h.handlerStore.ShouldAllowDirectKeys(), h.config.GetHeaderMatcher(), h.config.GetMCPHeaderCombinedAllowlist())
	if koutakuCtx == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
		return
	}
	defer cancel()

	resultTTL := getResultTTLFromHeaderWithDefault(ctx, h.config.ClientConfig.AsyncJobResultTTL)

	job, err := h.executor.SubmitJob(
		koutakuCtx,
		resultTTL,
		func(bgCtx *schemas.KoutakuContext) (interface{}, *schemas.KoutakuError) {
			return h.client.TranscriptionRequest(bgCtx, koutakuTranscriptionReq)
		},
		schemas.TranscriptionRequest,
	)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	SendJSONWithStatus(ctx, job.ToResponse(), fasthttp.StatusAccepted)
}

// asyncImageGeneration handles POST /v1/async/images/generations
func (h *AsyncHandler) asyncImageGeneration(ctx *fasthttp.RequestCtx) {
	req, koutakuReq, err := prepareImageGenerationRequest(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	if req.KoutakuParams.Stream != nil && *req.KoutakuParams.Stream {
		SendError(ctx, fasthttp.StatusBadRequest, "stream is not supported for async image generations")
		return
	}

	koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, h.handlerStore.ShouldAllowDirectKeys(), h.config.GetHeaderMatcher(), h.config.GetMCPHeaderCombinedAllowlist())
	if koutakuCtx == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
		return
	}
	defer cancel()

	resultTTL := getResultTTLFromHeaderWithDefault(ctx, h.config.ClientConfig.AsyncJobResultTTL)

	job, err := h.executor.SubmitJob(
		koutakuCtx,
		resultTTL,
		func(bgCtx *schemas.KoutakuContext) (interface{}, *schemas.KoutakuError) {
			return h.client.ImageGenerationRequest(bgCtx, koutakuReq)
		},
		schemas.ImageGenerationRequest,
	)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	SendJSONWithStatus(ctx, job.ToResponse(), fasthttp.StatusAccepted)
}

// asyncImageEdit handles POST /v1/async/images/edits
func (h *AsyncHandler) asyncImageEdit(ctx *fasthttp.RequestCtx) {
	req, koutakuReq, err := prepareImageEditRequest(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	if req.Stream != nil && *req.Stream {
		SendError(ctx, fasthttp.StatusBadRequest, "stream is not supported for async image edits")
		return
	}

	koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, h.handlerStore.ShouldAllowDirectKeys(), h.config.GetHeaderMatcher(), h.config.GetMCPHeaderCombinedAllowlist())
	if koutakuCtx == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
		return
	}
	defer cancel()

	resultTTL := getResultTTLFromHeaderWithDefault(ctx, h.config.ClientConfig.AsyncJobResultTTL)

	job, err := h.executor.SubmitJob(
		koutakuCtx,
		resultTTL,
		func(bgCtx *schemas.KoutakuContext) (interface{}, *schemas.KoutakuError) {
			return h.client.ImageEditRequest(bgCtx, koutakuReq)
		},
		schemas.ImageEditRequest,
	)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	SendJSONWithStatus(ctx, job.ToResponse(), fasthttp.StatusAccepted)
}

// asyncImageVariation handles POST /v1/async/images/variations
func (h *AsyncHandler) asyncImageVariation(ctx *fasthttp.RequestCtx) {
	koutakuReq, err := prepareImageVariationRequest(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, h.handlerStore.ShouldAllowDirectKeys(), h.config.GetHeaderMatcher(), h.config.GetMCPHeaderCombinedAllowlist())
	if koutakuCtx == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
		return
	}
	defer cancel()

	resultTTL := getResultTTLFromHeaderWithDefault(ctx, h.config.ClientConfig.AsyncJobResultTTL)

	job, err := h.executor.SubmitJob(
		koutakuCtx,
		resultTTL,
		func(bgCtx *schemas.KoutakuContext) (interface{}, *schemas.KoutakuError) {
			return h.client.ImageVariationRequest(bgCtx, koutakuReq)
		},
		schemas.ImageVariationRequest,
	)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}
	SendJSONWithStatus(ctx, job.ToResponse(), fasthttp.StatusAccepted)
}

// asyncRerank handles POST /v1/async/rerank
func (h *AsyncHandler) asyncRerank(ctx *fasthttp.RequestCtx) {
	_, koutakuReq, err := prepareRerankRequest(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, h.handlerStore.ShouldAllowDirectKeys(), h.config.GetHeaderMatcher(), h.config.GetMCPHeaderCombinedAllowlist())
	if koutakuCtx == nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "Failed to convert context")
		return
	}
	defer cancel()

	resultTTL := getResultTTLFromHeaderWithDefault(ctx, h.config.ClientConfig.AsyncJobResultTTL)

	job, err := h.executor.SubmitJob(
		koutakuCtx,
		resultTTL,
		func(bgCtx *schemas.KoutakuContext) (interface{}, *schemas.KoutakuError) {
			return h.client.RerankRequest(bgCtx, koutakuReq)
		},
		schemas.RerankRequest,
	)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, err.Error())
		return
	}
	SendJSONWithStatus(ctx, job.ToResponse(), fasthttp.StatusAccepted)
}

// asyncOCR handles POST /v1/async/ocr
func (h *AsyncHandler) asyncOCR(ctx *fasthttp.RequestCtx) {
	_, koutakuReq, err := prepareOCRRequest(ctx)
	if err != nil {
		SendError(ctx, fasthttp.StatusBadRequest, err.Error())
		return
	}

	koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, h.handlerStore.ShouldAllowDirectKeys(), h.config.GetHeaderMatcher(), h.config.GetMCPHeaderCombinedAllowlist())
	if koutakuCtx == nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "Failed to convert context")
		return
	}
	defer cancel()

	resultTTL := getResultTTLFromHeaderWithDefault(ctx, h.config.ClientConfig.AsyncJobResultTTL)

	job, err := h.executor.SubmitJob(
		koutakuCtx,
		resultTTL,
		func(bgCtx *schemas.KoutakuContext) (interface{}, *schemas.KoutakuError) {
			return h.client.OCRRequest(bgCtx, koutakuReq)
		},
		schemas.OCRRequest,
	)
	if err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, err.Error())
		return
	}
	SendJSONWithStatus(ctx, job.ToResponse(), fasthttp.StatusAccepted)
}

// --- Job retrieval handler ---

// getJob handles GET /v1/async/{type}/{job_id}
func (h *AsyncHandler) getJob(operationType schemas.RequestType) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		jobID, ok := ctx.UserValue("job_id").(string)
		if !ok || jobID == "" {
			SendError(ctx, fasthttp.StatusBadRequest, "job_id is required")
			return
		}

		// Get the requesting user's VK for auth check
		koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, h.handlerStore.ShouldAllowDirectKeys(), h.config.GetHeaderMatcher(), h.config.GetMCPHeaderCombinedAllowlist())
		if koutakuCtx == nil {
			SendError(ctx, fasthttp.StatusBadRequest, "Failed to convert context")
			return
		}
		defer cancel()

		job, err := h.executor.RetrieveJob(koutakuCtx, jobID, getVirtualKeyFromContext(koutakuCtx), operationType)
		if err != nil {
			SendError(ctx, fasthttp.StatusNotFound, err.Error())
			return
		}

		resp := job.ToResponse()

		// Return 202 for pending/processing, 200 for completed/failed
		switch job.Status {
		case schemas.AsyncJobStatusPending, schemas.AsyncJobStatusProcessing:
			SendJSONWithStatus(ctx, resp, fasthttp.StatusAccepted)
		default:
			SendJSON(ctx, resp)
		}
	}
}

// --- Helper functions ---

// getVirtualKeyFromContext extracts the virtual key value from context.
// Returns nil if no VK is present (e.g., direct key mode or no governance).
func getVirtualKeyFromContext(ctx *schemas.KoutakuContext) *string {
	vkValue := koutaku.GetStringFromContext(ctx, schemas.KoutakuContextKeyVirtualKey)
	if vkValue == "" {
		return nil
	}
	return &vkValue
}

func getResultTTLFromHeaderWithDefault(ctx *fasthttp.RequestCtx, defaultTTL int) int {
	resultTTL := string(ctx.Request.Header.Peek(schemas.AsyncHeaderResultTTL))
	if resultTTL == "" {
		return defaultTTL
	}
	resultTTLInt, err := strconv.Atoi(resultTTL)
	if err != nil || resultTTLInt < 0 {
		return defaultTTL
	}
	return resultTTLInt
}
