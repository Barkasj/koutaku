// Package integrations provides a generic router framework for handling different LLM provider APIs.
//
// CENTRALIZED STREAMING ARCHITECTURE:
//
// This package implements a centralized streaming approach where all stream handling logic
// is consolidated in the GenericRouter, eliminating the need for provider-specific StreamHandler
// implementations. The key components are:
//
// 1. StreamConfig: Defines streaming configuration for each route, including:
//   - ResponseConverter: Converts KoutakuResponse to provider-specific streaming format
//   - ErrorConverter: Converts KoutakuError to provider-specific streaming error format
//
// 2. Centralized Stream Processing: The GenericRouter handles all streaming logic:
//   - SSE header management
//   - Stream channel processing
//   - Error handling and conversion
//   - Response formatting and flushing
//   - Stream closure (handled automatically by provider implementation)
//
// 3. Provider-Specific Type Conversion: Integration types.go files only handle type conversion:
//   - Derive{Provider}StreamFromKoutakuResponse: Convert responses to streaming format
//   - Derive{Provider}StreamFromKoutakuError: Convert errors to streaming error format
//
// BENEFITS:
// - Eliminates code duplication across provider-specific stream handlers
// - Centralizes streaming logic for consistency and maintainability
// - Separates concerns: routing logic vs type conversion
// - Automatic stream closure management by provider implementations
// - Consistent error handling across all providers
//
// USAGE EXAMPLE:
//
//	routes := []RouteConfig{
//	  {
//	    Path: "/openai/chat/completions",
//	    Method: "POST",
//	    // ... other configs ...
//	    StreamConfig: &StreamConfig{
//	      ResponseConverter: func(resp *schemas.KoutakuResponse) (interface{}, error) {
//	        return DeriveOpenAIStreamFromKoutakuResponse(resp), nil
//	      },
//	      ErrorConverter: func(err *schemas.KoutakuError) interface{} {
//	        return DeriveOpenAIStreamFromKoutakuError(err)
//	      },
//	    },
//	  },
//	}
package integrations

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"errors"
	"mime"
	"mime/multipart"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/bytedance/sonic"
	"github.com/fasthttp/router"
	koutaku "github.com/koutaku/koutaku/core"
	"github.com/koutaku/koutaku/core/providers/bedrock"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/koutaku/koutaku/framework/logstore"
	"github.com/koutaku/koutaku/transports/koutaku-http/lib"
	"github.com/valyala/fasthttp"
)

// ExtensionRouter defines the interface that all integration routers must implement
// to register their routes with the main HTTP router.
type ExtensionRouter interface {
	RegisterRoutes(r *router.Router, middlewares ...schemas.KoutakuHTTPMiddleware)
}

// StreamingRequest interface for requests that support streaming
type StreamingRequest interface {
	IsStreamingRequested() bool
}

// RequestWithSettableExtraParams is implemented by request types that accept
// provider-specific extra parameters via the extra_params JSON key. The
// integration router extracts extra_params from the raw request body and
// passes them through so they propagate to the downstream provider.
type RequestWithSettableExtraParams interface {
	SetExtraParams(params map[string]interface{})
}

// BatchRequest wraps a Koutaku batch request with its type information.
type BatchRequest struct {
	Type            schemas.RequestType
	CreateRequest   *schemas.KoutakuBatchCreateRequest
	ListRequest     *schemas.KoutakuBatchListRequest
	RetrieveRequest *schemas.KoutakuBatchRetrieveRequest
	CancelRequest   *schemas.KoutakuBatchCancelRequest
	DeleteRequest   *schemas.KoutakuBatchDeleteRequest
	ResultsRequest  *schemas.KoutakuBatchResultsRequest
}

// FileRequest wraps a Koutaku file request with its type information.
type FileRequest struct {
	Type            schemas.RequestType
	UploadRequest   *schemas.KoutakuFileUploadRequest
	ListRequest     *schemas.KoutakuFileListRequest
	RetrieveRequest *schemas.KoutakuFileRetrieveRequest
	DeleteRequest   *schemas.KoutakuFileDeleteRequest
	ContentRequest  *schemas.KoutakuFileContentRequest
}

// ContainerRequest wraps a Koutaku container request with its type information.
type ContainerRequest struct {
	Type            schemas.RequestType
	CreateRequest   *schemas.KoutakuContainerCreateRequest
	ListRequest     *schemas.KoutakuContainerListRequest
	RetrieveRequest *schemas.KoutakuContainerRetrieveRequest
	DeleteRequest   *schemas.KoutakuContainerDeleteRequest
}

// ContainerFileRequest is a wrapper for Koutaku container file requests.
type ContainerFileRequest struct {
	Type            schemas.RequestType
	CreateRequest   *schemas.KoutakuContainerFileCreateRequest
	ListRequest     *schemas.KoutakuContainerFileListRequest
	RetrieveRequest *schemas.KoutakuContainerFileRetrieveRequest
	ContentRequest  *schemas.KoutakuContainerFileContentRequest
	DeleteRequest   *schemas.KoutakuContainerFileDeleteRequest
}

// BatchRequestConverter is a function that converts integration-specific batch requests to Koutaku format.
type BatchRequestConverter func(ctx *schemas.KoutakuContext, req interface{}) (*BatchRequest, error)

// FileRequestConverter is a function that converts integration-specific file requests to Koutaku format.
type FileRequestConverter func(ctx *schemas.KoutakuContext, req interface{}) (*FileRequest, error)

// ContainerRequestConverter is a function that converts integration-specific container requests to Koutaku format.
type ContainerRequestConverter func(ctx *schemas.KoutakuContext, req interface{}) (*ContainerRequest, error)

// ContainerFileRequestConverter is a function that converts integration-specific container file requests to Koutaku format.
type ContainerFileRequestConverter func(ctx *schemas.KoutakuContext, req interface{}) (*ContainerFileRequest, error)

// RequestConverter is a function that converts integration-specific requests to Koutaku format.
// It takes the parsed request object and returns a KoutakuRequest ready for processing.
type RequestConverter func(ctx *schemas.KoutakuContext, req interface{}) (*schemas.KoutakuRequest, error)

// ListModelsResponseConverter is a function that converts KoutakuListModelsResponse to integration-specific format.
// It takes a KoutakuListModelsResponse and returns the format expected by the specific integration.
type ListModelsResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuListModelsResponse) (interface{}, error)

// TextResponseConverter is a function that converts KoutakuTextCompletionResponse to integration-specific format.
// It takes a KoutakuTextCompletionResponse and returns the format expected by the specific integration.
type TextResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuTextCompletionResponse) (interface{}, error)

// ChatResponseConverter is a function that converts KoutakuChatResponse to integration-specific format.
// It takes a KoutakuChatResponse and returns the format expected by the specific integration.
type ChatResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuChatResponse) (interface{}, error)

// AsyncChatResponseConverter is a function that converts an async job response to an integration-specific format.
// It takes an async job response and a method to convert the chat response, and returns the integration-specific format, extra headers, and an error.
type AsyncChatResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.AsyncJobResponse, chatResponseConverter ChatResponseConverter) (interface{}, map[string]string, error)

// ResponsesResponseConverter is a function that converts KoutakuResponsesResponse to integration-specific format.
// It takes a KoutakuResponsesResponse and returns the format expected by the specific integration.
type ResponsesResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuResponsesResponse) (interface{}, error)

// AsyncResponsesResponseConverter is a function that converts an async job response to an integration-specific format.
// It takes an async job response and a method to convert the responses response, and returns the integration-specific format, extra headers, and an error.
type AsyncResponsesResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.AsyncJobResponse, responsesResponseConverter ResponsesResponseConverter) (interface{}, map[string]string, error)

// EmbeddingResponseConverter is a function that converts KoutakuEmbeddingResponse to integration-specific format.
// It takes a KoutakuEmbeddingResponse and returns the format expected by the specific integration.
type EmbeddingResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuEmbeddingResponse) (interface{}, error)

// RerankResponseConverter is a function that converts KoutakuRerankResponse to integration-specific format.
// It takes a KoutakuRerankResponse and returns the format expected by the specific integration.
type RerankResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuRerankResponse) (interface{}, error)

// OCRResponseConverter is a function that converts KoutakuOCRResponse to integration-specific format.
// It takes a KoutakuOCRResponse and returns the format expected by the specific integration.
type OCRResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuOCRResponse) (interface{}, error)

// SpeechResponseConverter is a function that converts KoutakuSpeechResponse to integration-specific format.
// It takes a KoutakuSpeechResponse and returns the format expected by the specific integration.
type SpeechResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuSpeechResponse) (interface{}, error)

// TranscriptionResponseConverter is a function that converts KoutakuTranscriptionResponse to integration-specific format.
// It takes a KoutakuTranscriptionResponse and returns the format expected by the specific integration.
type TranscriptionResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuTranscriptionResponse) (interface{}, error)

// BatchCreateResponseConverter is a function that converts KoutakuBatchCreateResponse to integration-specific format.
// It takes a KoutakuBatchCreateResponse and returns the format expected by the specific integration.
type BatchCreateResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuBatchCreateResponse) (interface{}, error)

// BatchListResponseConverter is a function that converts KoutakuBatchListResponse to integration-specific format.
// It takes a KoutakuBatchListResponse and returns the format expected by the specific integration.
type BatchListResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuBatchListResponse) (interface{}, error)

// BatchRetrieveResponseConverter is a function that converts KoutakuBatchRetrieveResponse to integration-specific format.
// It takes a KoutakuBatchRetrieveResponse and returns the format expected by the specific integration.
type BatchRetrieveResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuBatchRetrieveResponse) (interface{}, error)

// BatchCancelResponseConverter is a function that converts KoutakuBatchCancelResponse to integration-specific format.
// It takes a KoutakuBatchCancelResponse and returns the format expected by the specific integration.
type BatchCancelResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuBatchCancelResponse) (interface{}, error)

// BatchResultsResponseConverter is a function that converts KoutakuBatchResultsResponse to integration-specific format.
// It takes a KoutakuBatchResultsResponse and returns the format expected by the specific integration.
type BatchResultsResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuBatchResultsResponse) (interface{}, error)

// BatchDeleteResponseConverter is a function that converts KoutakuBatchDeleteResponse to integration-specific format.
// It takes a KoutakuBatchDeleteResponse and returns the format expected by the specific integration.
type BatchDeleteResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuBatchDeleteResponse) (interface{}, error)

// FileUploadResponseConverter is a function that converts KoutakuFileUploadResponse to integration-specific format.
// It takes a KoutakuFileUploadResponse and returns the format expected by the specific integration.
type FileUploadResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuFileUploadResponse) (interface{}, error)

// FileListResponseConverter is a function that converts KoutakuFileListResponse to integration-specific format.
// It takes a KoutakuFileListResponse and returns the format expected by the specific integration.
type FileListResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuFileListResponse) (interface{}, error)

// FileRetrieveResponseConverter is a function that converts KoutakuFileRetrieveResponse to integration-specific format.
// It takes a KoutakuFileRetrieveResponse and returns the format expected by the specific integration.
type FileRetrieveResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuFileRetrieveResponse) (interface{}, error)

// FileDeleteResponseConverter is a function that converts KoutakuFileDeleteResponse to integration-specific format.
// It takes a KoutakuFileDeleteResponse and returns the format expected by the specific integration.
type FileDeleteResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuFileDeleteResponse) (interface{}, error)

// FileContentResponseConverter is a function that converts KoutakuFileContentResponse to integration-specific format.
// It takes a KoutakuFileContentResponse and returns the format expected by the specific integration.
// Note: This may return binary data or a wrapper object depending on the integration.
type FileContentResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuFileContentResponse) (interface{}, error)

// ContainerCreateResponseConverter is a function that converts KoutakuContainerCreateResponse to integration-specific format.
// It takes a KoutakuContainerCreateResponse and returns the format expected by the specific integration.
type ContainerCreateResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuContainerCreateResponse) (interface{}, error)

// ContainerListResponseConverter is a function that converts KoutakuContainerListResponse to integration-specific format.
// It takes a KoutakuContainerListResponse and returns the format expected by the specific integration.
type ContainerListResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuContainerListResponse) (interface{}, error)

// ContainerRetrieveResponseConverter is a function that converts KoutakuContainerRetrieveResponse to integration-specific format.
// It takes a KoutakuContainerRetrieveResponse and returns the format expected by the specific integration.
type ContainerRetrieveResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuContainerRetrieveResponse) (interface{}, error)

// ContainerDeleteResponseConverter is a function that converts KoutakuContainerDeleteResponse to integration-specific format.
// It takes a KoutakuContainerDeleteResponse and returns the format expected by the specific integration.
type ContainerDeleteResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuContainerDeleteResponse) (interface{}, error)

// ContainerFileCreateResponseConverter is a function that converts KoutakuContainerFileCreateResponse to integration-specific format.
// It takes a KoutakuContainerFileCreateResponse and returns the format expected by the specific integration.
type ContainerFileCreateResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuContainerFileCreateResponse) (interface{}, error)

// ContainerFileListResponseConverter is a function that converts KoutakuContainerFileListResponse to integration-specific format.
// It takes a KoutakuContainerFileListResponse and returns the format expected by the specific integration.
type ContainerFileListResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuContainerFileListResponse) (interface{}, error)

// ContainerFileRetrieveResponseConverter is a function that converts KoutakuContainerFileRetrieveResponse to integration-specific format.
// It takes a KoutakuContainerFileRetrieveResponse and returns the format expected by the specific integration.
type ContainerFileRetrieveResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuContainerFileRetrieveResponse) (interface{}, error)

// ContainerFileContentResponseConverter is a function that converts KoutakuContainerFileContentResponse to integration-specific format.
// It takes a KoutakuContainerFileContentResponse and returns the format expected by the specific integration.
type ContainerFileContentResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuContainerFileContentResponse) (interface{}, error)

// ContainerFileDeleteResponseConverter is a function that converts KoutakuContainerFileDeleteResponse to integration-specific format.
// It takes a KoutakuContainerFileDeleteResponse and returns the format expected by the specific integration.
type ContainerFileDeleteResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuContainerFileDeleteResponse) (interface{}, error)

// CountTokensResponseConverter is a function that converts KoutakuCountTokensResponse to integration-specific format.
// It takes a KoutakuCountTokensResponse and returns the format expected by the specific integration.
type CountTokensResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuCountTokensResponse) (interface{}, error)

// TextStreamResponseConverter is a function that converts KoutakuTextCompletionResponse to integration-specific streaming format.
// It takes a KoutakuTextCompletionResponse and returns the event type and the streaming format expected by the specific integration.
type TextStreamResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuTextCompletionResponse) (string, interface{}, error)

// ChatStreamResponseConverter is a function that converts KoutakuChatResponse to integration-specific streaming format.
// It takes a KoutakuChatResponse and returns the event type and the streaming format expected by the specific integration.
type ChatStreamResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuChatResponse) (string, interface{}, error)

// ResponsesStreamResponseConverter is a function that converts KoutakuResponsesStreamResponse to integration-specific streaming format.
// It takes a KoutakuResponsesStreamResponse and returns a single event type and payload, which can itself encode one or more SSE events if needed by the integration.
type ResponsesStreamResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuResponsesStreamResponse) (string, interface{}, error)

// SpeechStreamResponseConverter is a function that converts KoutakuSpeechStreamResponse to integration-specific streaming format.
// It takes a KoutakuSpeechStreamResponse and returns the event type and the streaming format expected by the specific integration.
type SpeechStreamResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuSpeechStreamResponse) (string, interface{}, error)

// TranscriptionStreamResponseConverter is a function that converts KoutakuTranscriptionStreamResponse to integration-specific streaming format.
// It takes a KoutakuTranscriptionStreamResponse and returns the event type and the streaming format expected by the specific integration.
type TranscriptionStreamResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuTranscriptionStreamResponse) (string, interface{}, error)

// ImageGenerationResponseConverter is a function that converts KoutakuImageGenerationResponse to integration-specific format.
// It takes a KoutakuImageGenerationResponse and returns the format expected by the specific integration.
type ImageGenerationResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuImageGenerationResponse) (interface{}, error)

// ImageGenerationStreamResponseConverter is a function that converts KoutakuImageGenerationStreamResponse to integration-specific streaming format.
// It takes a KoutakuImageGenerationStreamResponse and returns the event type and the streaming format expected by the specific integration.
type ImageGenerationStreamResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuImageGenerationStreamResponse) (string, interface{}, error)

// ImageEditResponseConverter is a function that converts KoutakuImageGenerationResponse to integration-specific format.
// It takes a KoutakuImageGenerationResponse and returns the format expected by the specific integration.
type ImageEditResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuImageGenerationResponse) (interface{}, error)

// VideoGenerationResponseConverter is a function that converts KoutakuVideoGenerationResponse to integration-specific format.
// It takes a KoutakuVideoGenerationResponse and returns the format expected by the specific integration.
type VideoGenerationResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuVideoGenerationResponse) (interface{}, error)

// VideoDownloadResponseConverter is a function that converts KoutakuVideoDownloadResponse to integration-specific format.
// It takes a KoutakuVideoDownloadResponse and returns the format expected by the specific integration.
type VideoDownloadResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuVideoDownloadResponse) (interface{}, error)

// VideoRetrieveAsDownloadConverter is a function that converts KoutakuVideoGenerationResponse to integration-specific format.
// It takes a KoutakuVideoGenerationResponse and returns the format expected by the specific integration.
type VideoRetrieveAsDownloadConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuVideoGenerationResponse) (interface{}, error)

// VideoDeleteResponseConverter is a function that converts KoutakuVideoDeleteResponse to integration-specific format.
// It takes a KoutakuVideoDeleteResponse and returns the format expected by the specific integration.
type VideoDeleteResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuVideoDeleteResponse) (interface{}, error)

// VideoListResponseConverter is a function that converts KoutakuVideoListResponse to integration-specific format.
// It takes a KoutakuVideoListResponse and returns the format expected by the specific integration.
type VideoListResponseConverter func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuVideoListResponse) (interface{}, error)

// ErrorConverter is a function that converts KoutakuError to integration-specific format.
// It takes a KoutakuError and returns the format expected by the specific integration.
type ErrorConverter func(ctx *schemas.KoutakuContext, err *schemas.KoutakuError) interface{}

// StreamErrorConverter is a function that converts KoutakuError to integration-specific streaming error format.
// It takes a KoutakuError and returns the streaming error format expected by the specific integration.
type StreamErrorConverter func(ctx *schemas.KoutakuContext, err *schemas.KoutakuError) interface{}

// RequestParser is a function that handles custom request body parsing.
// It replaces the default JSON parsing when configured (e.g., for multipart/form-data).
// The parser should populate the provided request object from the fasthttp context.
// If it returns an error, the request processing stops.
type RequestParser func(ctx *fasthttp.RequestCtx, req interface{}) error

// PreRequestCallback is called after parsing the request but before processing through Koutaku.
// It can be used to modify the request object (e.g., extract model from URL parameters)
// or perform validation. If it returns an error, the request processing stops.
// It can also modify the koutaku context based on the request context before it is given to Koutaku.
type PreRequestCallback func(ctx *fasthttp.RequestCtx, koutakuCtx *schemas.KoutakuContext, req interface{}) error

// PostRequestCallback is called after processing the request but before sending the response.
// It can be used to modify the response or perform additional logging/metrics.
// If it returns an error, an error response is sent instead of the success response.
type PostRequestCallback func(ctx *fasthttp.RequestCtx, req interface{}, resp interface{}) error

// HTTPRequestTypeGetter is a function type that accepts only a *fasthttp.RequestCtx and
// returns a schemas.RequestType indicating the HTTP request type derived from the context.
type HTTPRequestTypeGetter func(ctx *fasthttp.RequestCtx) schemas.RequestType

// ShortCircuit is a function that determines if the request should be short-circuited.
type ShortCircuit func(ctx *fasthttp.RequestCtx, koutakuCtx *schemas.KoutakuContext, req interface{}) (bool, error)

// StreamConfig defines streaming-specific configuration for an integration
//
// SSE FORMAT BEHAVIOR:
//
// The ResponseConverter and ErrorConverter functions in StreamConfig can return either:
//
// 1. OBJECTS (interface{} that's not a string):
//   - Will be JSON marshaled and sent as standard SSE: data: {json}\n\n
//   - Use this for most providers (OpenAI, Google, etc.)
//   - Example: return map[string]interface{}{"delta": {"content": "hello"}}
//   - Result: data: {"delta":{"content":"hello"}}\n\n
//
// 2. STRINGS:
//   - Will be sent directly as-is without any modification
//   - Use this for providers requiring custom SSE event types (Anthropic, etc.)
//   - Example: return "event: content_block_delta\ndata: {\"type\":\"text\"}\n\n"
//   - Result: event: content_block_delta
//     data: {"type":"text"}
//
// Choose the appropriate return type based on your provider's SSE specification.
type StreamConfig struct {
	TextStreamResponseConverter            TextStreamResponseConverter            // Function to convert KoutakuTextCompletionResponse to streaming format
	ChatStreamResponseConverter            ChatStreamResponseConverter            // Function to convert KoutakuChatResponse to streaming format
	ResponsesStreamResponseConverter       ResponsesStreamResponseConverter       // Function to convert KoutakuResponsesResponse to streaming format
	SpeechStreamResponseConverter          SpeechStreamResponseConverter          // Function to convert KoutakuSpeechResponse to streaming format
	TranscriptionStreamResponseConverter   TranscriptionStreamResponseConverter   // Function to convert KoutakuTranscriptionResponse to streaming format
	ImageGenerationStreamResponseConverter ImageGenerationStreamResponseConverter // Function to convert KoutakuImageGenerationStreamResponse to streaming format
	ErrorConverter                         StreamErrorConverter                   // Function to convert KoutakuError to streaming error format
}

type RouteConfigType string

const (
	RouteConfigTypeOpenAI    RouteConfigType = "openai"
	RouteConfigTypeAnthropic RouteConfigType = "anthropic"
	RouteConfigTypeGenAI     RouteConfigType = "genai"
	RouteConfigTypeBedrock   RouteConfigType = "bedrock"
	RouteConfigTypeCohere    RouteConfigType = "cohere"
)

// RouteConfig defines the configuration for a single route in an integration.
// It specifies the path, method, and handlers for request/response conversion.
type RouteConfig struct {
	Type                                   RouteConfigType                        // Type of the route
	Path                                   string                                 // HTTP path pattern (e.g., "/openai/v1/chat/completions")
	Method                                 string                                 // HTTP method (POST, GET, PUT, DELETE)
	GetHTTPRequestType                     HTTPRequestTypeGetter                  // Function to get the HTTP request type from the context (SHOULD NOT BE NIL)
	GetRequestTypeInstance                 func(ctx context.Context) interface{}  // Factory function to create request instance (SHOULD NOT BE NIL)
	RequestParser                          RequestParser                          // Optional: custom request parsing (e.g., multipart/form-data)
	RequestConverter                       RequestConverter                       // Function to convert request to KoutakuRequest (for inference requests)
	BatchRequestConverter                  BatchRequestConverter                  // Function to convert request to BatchRequest (for batch operations)
	FileRequestConverter                   FileRequestConverter                   // Function to convert request to FileRequest (for file operations)
	ContainerRequestConverter              ContainerRequestConverter              // Function to convert request to ContainerRequest (for container operations)
	ContainerFileRequestConverter          ContainerFileRequestConverter          // Function to convert request to ContainerFileRequest (for container file operations)
	ListModelsResponseConverter            ListModelsResponseConverter            // Function to convert KoutakuListModelsResponse to integration format (SHOULD NOT BE NIL)
	TextResponseConverter                  TextResponseConverter                  // Function to convert KoutakuTextCompletionResponse to integration format (SHOULD NOT BE NIL)
	ChatResponseConverter                  ChatResponseConverter                  // Function to convert KoutakuChatResponse to integration format (SHOULD NOT BE NIL)
	AsyncChatResponseConverter             AsyncChatResponseConverter             // Function to convert AsyncJobResponse to integration format (SHOULD NOT BE NIL)
	ResponsesResponseConverter             ResponsesResponseConverter             // Function to convert KoutakuResponsesResponse to integration format (SHOULD NOT BE NIL)
	AsyncResponsesResponseConverter        AsyncResponsesResponseConverter        // Function to convert AsyncJobResponse to integration format (SHOULD NOT BE NIL)
	EmbeddingResponseConverter             EmbeddingResponseConverter             // Function to convert KoutakuEmbeddingResponse to integration format (SHOULD NOT BE NIL)
	RerankResponseConverter                RerankResponseConverter                // Function to convert KoutakuRerankResponse to integration format
	OCRResponseConverter                   OCRResponseConverter                   // Function to convert KoutakuOCRResponse to integration format
	SpeechResponseConverter                SpeechResponseConverter                // Function to convert KoutakuSpeechResponse to integration format (SHOULD NOT BE NIL)
	TranscriptionResponseConverter         TranscriptionResponseConverter         // Function to convert KoutakuTranscriptionResponse to integration format (SHOULD NOT BE NIL)
	ImageGenerationResponseConverter       ImageGenerationResponseConverter       // Function to convert KoutakuImageGenerationResponse to integration format (SHOULD NOT BE NIL)
	VideoGenerationResponseConverter       VideoGenerationResponseConverter       // Function to convert KoutakuVideoGenerationResponse to integration format (SHOULD NOT BE NIL)
	VideoDownloadResponseConverter         VideoDownloadResponseConverter         // Function to convert KoutakuVideoDownloadResponse to integration format (SHOULD NOT BE NIL)
	VideoDeleteResponseConverter           VideoDeleteResponseConverter           // Function to convert KoutakuVideoDeleteResponse to integration format (SHOULD NOT BE NIL)
	VideoListResponseConverter             VideoListResponseConverter             // Function to convert KoutakuVideoListResponse to integration format (SHOULD NOT BE NIL)
	BatchCreateResponseConverter           BatchCreateResponseConverter           // Function to convert KoutakuBatchCreateResponse to integration format
	BatchListResponseConverter             BatchListResponseConverter             // Function to convert KoutakuBatchListResponse to integration format
	BatchRetrieveResponseConverter         BatchRetrieveResponseConverter         // Function to convert KoutakuBatchRetrieveResponse to integration format
	BatchCancelResponseConverter           BatchCancelResponseConverter           // Function to convert KoutakuBatchCancelResponse to integration format
	BatchDeleteResponseConverter           BatchDeleteResponseConverter           // Function to convert KoutakuBatchDeleteResponse to integration format
	BatchResultsResponseConverter          BatchResultsResponseConverter          // Function to convert KoutakuBatchResultsResponse to integration format
	FileUploadResponseConverter            FileUploadResponseConverter            // Function to convert KoutakuFileUploadResponse to integration format
	FileListResponseConverter              FileListResponseConverter              // Function to convert KoutakuFileListResponse to integration format
	FileRetrieveResponseConverter          FileRetrieveResponseConverter          // Function to convert KoutakuFileRetrieveResponse to integration format
	FileDeleteResponseConverter            FileDeleteResponseConverter            // Function to convert KoutakuFileDeleteResponse to integration format
	FileContentResponseConverter           FileContentResponseConverter           // Function to convert KoutakuFileContentResponse to integration format
	ContainerCreateResponseConverter       ContainerCreateResponseConverter       // Function to convert KoutakuContainerCreateResponse to integration format
	ContainerListResponseConverter         ContainerListResponseConverter         // Function to convert KoutakuContainerListResponse to integration format
	ContainerRetrieveResponseConverter     ContainerRetrieveResponseConverter     // Function to convert KoutakuContainerRetrieveResponse to integration format
	ContainerDeleteResponseConverter       ContainerDeleteResponseConverter       // Function to convert KoutakuContainerDeleteResponse to integration format
	ContainerFileCreateResponseConverter   ContainerFileCreateResponseConverter   // Function to convert KoutakuContainerFileCreateResponse to integration format
	ContainerFileListResponseConverter     ContainerFileListResponseConverter     // Function to convert KoutakuContainerFileListResponse to integration format
	ContainerFileRetrieveResponseConverter ContainerFileRetrieveResponseConverter // Function to convert KoutakuContainerFileRetrieveResponse to integration format
	ContainerFileContentResponseConverter  ContainerFileContentResponseConverter  // Function to convert KoutakuContainerFileContentResponse to integration format
	ContainerFileDeleteResponseConverter   ContainerFileDeleteResponseConverter   // Function to convert KoutakuContainerFileDeleteResponse to integration format
	CountTokensResponseConverter           CountTokensResponseConverter           // Function to convert KoutakuCountTokensResponse to integration format
	ErrorConverter                         ErrorConverter                         // Function to convert KoutakuError to integration format (SHOULD NOT BE NIL)
	StreamConfig                           *StreamConfig                          // Optional: Streaming configuration (if nil, streaming not supported)
	PreCallback                            PreRequestCallback                     // Optional: called after parsing but before Koutaku processing
	PostCallback                           PostRequestCallback                    // Optional: called after request processing
	ShortCircuit                           ShortCircuit
}

type PassthroughConfig struct {
	Provider         schemas.ModelProvider                                              // which provider's key pool to draw from
	ProviderDetector func(ctx *fasthttp.RequestCtx, model string) schemas.ModelProvider // optional: dynamic provider detection
	StripPrefix      []string                                                           // e.g. "/openai" — stripped before forwarding
}

// LargePayloadHook is called before body parsing to detect and set up large payload streaming.
// If it returns skipBodyParse=true, the router skips JSON parsing of the request body.
// The hook is responsible for setting all relevant context keys (KoutakuContextKeyLargePayloadMode,
// KoutakuContextKeyLargePayloadReader, KoutakuContextKeyLargePayloadContentLength,
// KoutakuContextKeyLargePayloadMetadata) when activating large payload mode.
type LargePayloadHook func(
	ctx *fasthttp.RequestCtx,
	koutakuCtx *schemas.KoutakuContext,
	routeType RouteConfigType,
) (skipBodyParse bool, err error)

// LargeResponseHook is called before streaming a large response body to the client.
// Enterprise uses this to wrap the response reader with Phase B scanning (e.g., usage extraction
// from the full response stream when usage is beyond the Phase A prefetch window).
// The hook receives the koutaku context with KoutakuContextKeyLargeResponseReader already set
// and may replace the reader on context with a wrapped version.
type LargeResponseHook func(
	ctx *fasthttp.RequestCtx,
	koutakuCtx *schemas.KoutakuContext,
)

// GenericRouter provides a reusable router implementation for all integrations.
// It handles the common flow of: parse request → convert to Koutaku → execute → convert response.
// Integration-specific logic is handled through the RouteConfig callbacks and converters.
type GenericRouter struct {
	client            *koutaku.Koutaku // Koutaku client for executing requests
	handlerStore      lib.HandlerStore // Config provider for the router
	routes            []RouteConfig    // List of route configurations
	passthroughCfg    *PassthroughConfig
	logger            schemas.Logger    // Logger for the router
	largePayloadHook  LargePayloadHook  // Optional: enterprise hook for large payload detection
	largeResponseHook LargeResponseHook // Optional: enterprise hook for large response scanning
}

// SetLargePayloadHook sets the hook for large payload detection and streaming.
// This is used by enterprise to inject large payload optimization without
// embedding the logic in the OSS router.
func (g *GenericRouter) SetLargePayloadHook(hook LargePayloadHook) {
	g.largePayloadHook = hook
}

// SetLargeResponseHook sets the hook for large response scanning.
// Enterprise uses this to inject Phase B usage extraction into the response stream
// without embedding scanning logic in the OSS router.
func (g *GenericRouter) SetLargeResponseHook(hook LargeResponseHook) {
	g.largeResponseHook = hook
}

// NewGenericRouter creates a new generic router with the given koutaku client and route configurations.
// Each integration should create their own routes and pass them to this constructor.
func NewGenericRouter(client *koutaku.Koutaku, handlerStore lib.HandlerStore, routes []RouteConfig, passthroughCfg *PassthroughConfig, logger schemas.Logger) *GenericRouter {
	return &GenericRouter{
		client:         client,
		handlerStore:   handlerStore,
		routes:         routes,
		passthroughCfg: passthroughCfg,
		logger:         logger,
	}
}

// RegisterRoutes registers all configured routes on the given fasthttp router.
// This method implements the ExtensionRouter interface.
func (g *GenericRouter) RegisterRoutes(r *router.Router, middlewares ...schemas.KoutakuHTTPMiddleware) {
	for _, route := range g.routes {
		// Validate route configuration at startup to fail fast
		method := strings.ToUpper(route.Method)

		if route.GetRequestTypeInstance == nil {
			g.logger.Warn("route configuration is invalid: GetRequestTypeInstance cannot be nil for route " + route.Path)
			continue
		}

		// Test that GetRequestTypeInstance returns a valid instance
		if testInstance := route.GetRequestTypeInstance(context.Background()); testInstance == nil {
			g.logger.Warn("route configuration is invalid: GetRequestTypeInstance returned nil for route " + route.Path)
			continue
		}

		// Determine route type: inference, batch, file, container, or container file
		isBatchRoute := route.BatchRequestConverter != nil
		isFileRoute := route.FileRequestConverter != nil
		isContainerRoute := route.ContainerRequestConverter != nil
		isContainerFileRoute := route.ContainerFileRequestConverter != nil
		isInferenceRoute := !isBatchRoute && !isFileRoute && !isContainerRoute && !isContainerFileRoute

		// For inference routes, require RequestConverter
		if isInferenceRoute && route.RequestConverter == nil {
			g.logger.Warn("route configuration is invalid: RequestConverter cannot be nil for inference route " + route.Path)
			continue
		}

		if route.ErrorConverter == nil {
			g.logger.Warn("route configuration is invalid: ErrorConverter cannot be nil for route " + route.Path)
			continue
		}

		registerRequestTypeMiddleware := func(next fasthttp.RequestHandler) fasthttp.RequestHandler {
			return func(ctx *fasthttp.RequestCtx) {
				if route.GetHTTPRequestType != nil {
					ctx.SetUserValue(schemas.KoutakuContextKeyHTTPRequestType, route.GetHTTPRequestType(ctx))
				}
				next(ctx)
			}
		}

		// Create a fresh middlewares list for this route (don't mutate the original)
		// This ensures each route only has its own middleware plus the originally passed middlewares
		routeMiddlewares := append([]schemas.KoutakuHTTPMiddleware{registerRequestTypeMiddleware}, middlewares...)

		handler := g.createHandler(route)
		switch method {
		case fasthttp.MethodPost:
			r.POST(route.Path, lib.ChainMiddlewares(handler, routeMiddlewares...))
		case fasthttp.MethodGet:
			r.GET(route.Path, lib.ChainMiddlewares(handler, routeMiddlewares...))
		case fasthttp.MethodPut:
			r.PUT(route.Path, lib.ChainMiddlewares(handler, routeMiddlewares...))
		case fasthttp.MethodDelete:
			r.DELETE(route.Path, lib.ChainMiddlewares(handler, routeMiddlewares...))
		case fasthttp.MethodHead:
			r.HEAD(route.Path, lib.ChainMiddlewares(handler, routeMiddlewares...))
		default:
			r.POST(route.Path, lib.ChainMiddlewares(handler, routeMiddlewares...)) // Default to POST
		}
	}

	if g.passthroughCfg != nil {
		catchAll := lib.ChainMiddlewares(g.handlePassthrough, middlewares...)
		// Register for all methods that need forwarding
		for _, method := range []string{fasthttp.MethodGet, fasthttp.MethodPost, fasthttp.MethodPut, fasthttp.MethodDelete, fasthttp.MethodPatch, fasthttp.MethodHead} {
			for _, prefix := range g.passthroughCfg.StripPrefix {
				r.Handle(method, prefix+"/{path:*}", catchAll)
			}
		}
	}
}

// createHandler creates a fasthttp handler for the given route configuration.
// The handler follows this flow:
// 1. Parse JSON request body into the configured request type (for methods that expect bodies)
// 2. Execute pre-callback (if configured) for request modification/validation
// 3. Convert request to KoutakuRequest using the configured converter
// 4. Execute the request through Koutaku (streaming or non-streaming)
// 5. Execute post-callback (if configured) for response modification
// 6. Convert and send the response using the configured response converter
func (g *GenericRouter) createHandler(config RouteConfig) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		method := string(ctx.Method())

		// Parse request body into the integration-specific request type
		// Note: config validation is performed at startup in RegisterRoutes
		req := config.GetRequestTypeInstance(ctx)
		var rawBody []byte

		// Execute the request through Koutaku
		koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, g.handlerStore.ShouldAllowDirectKeys(), g.handlerStore.GetHeaderMatcher(), g.handlerStore.GetMCPHeaderCombinedAllowlist())

		// Set integration type to context
		koutakuCtx.SetValue(schemas.KoutakuContextKeyIntegrationType, string(config.Type))

		// Set available providers to context
		availableProviders := g.handlerStore.GetAvailableProviders()
		koutakuCtx.SetValue(schemas.KoutakuContextKeyAvailableProviders, availableProviders)

		// Async retrieve: check x-bf-async-id header early (before body parsing)
		if asyncID := string(ctx.Request.Header.Peek(schemas.AsyncHeaderGetID)); asyncID != "" {
			defer cancel()
			g.handleAsyncRetrieve(ctx, config, koutakuCtx)
			return
		}

		// Parse request body based on configuration
		if method != fasthttp.MethodGet && method != fasthttp.MethodHead {
			// Hook executes before JSON parsing so large requests can remain streaming.
			isLargePayload := false
			if g.largePayloadHook != nil {
				var err error
				isLargePayload, err = g.largePayloadHook(ctx, koutakuCtx, config.Type)
				if err != nil {
					cancel()
					g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "large payload detection failed"))
					return
				}
			}

			if isLargePayload {
				// Large payload mode: body streams directly to provider via
				// KoutakuContextKeyLargePayloadReader. Skip all body parsing
				// (JSON and multipart) — metadata was already extracted by the hook.
			} else if config.RequestParser != nil {
				// Use custom parser (e.g., for multipart/form-data)
				if err := config.RequestParser(ctx, req); err != nil {
					cancel()
					g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to parse request"))
					return
				}
			} else {
				// Use default JSON parsing
				rawBody = ctx.Request.Body()
				if len(rawBody) > 0 {
					if err := sonic.Unmarshal(rawBody, req); err != nil {
						cancel()
						g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "Invalid JSON"))
						return
					}
				}
			}

			// Extract the "extra_params" JSON key when passthrough is
			// explicitly enabled via x-bf-passthrough-extra-params: true.
			// Provider-specific fields (e.g. Bedrock guardrailConfig)
			// must be nested under "extra_params" in the request body.
			// Runs after both RequestParser and default JSON paths.
			if !isLargePayload && koutakuCtx.Value(schemas.KoutakuContextKeyPassthroughExtraParams) == true {
				if rws, ok := req.(RequestWithSettableExtraParams); ok {
					if rawBody == nil {
						rawBody = ctx.Request.Body()
					}
					if len(rawBody) > 0 {
						var wrapper struct {
							ExtraParams map[string]interface{} `json:"extra_params"`
						}
						if err := sonic.Unmarshal(rawBody, &wrapper); err == nil && len(wrapper.ExtraParams) > 0 {
							rws.SetExtraParams(wrapper.ExtraParams)
						}
					}
				}
			}
		}

		// Execute pre-request callback if configured
		// This is typically used for extracting data from URL parameters
		// or performing request validation after parsing
		if config.PreCallback != nil {
			if err := config.PreCallback(ctx, koutakuCtx, req); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute pre-request callback: "+err.Error()))
				return
			}
		}

		// Execute short-circuit handler if configured.
		// If it returns handled=true the callback has already written a response
		// to ctx and we return immediately, bypassing the Koutaku flow entirely.
		if config.ShortCircuit != nil {
			handled, err := config.ShortCircuit(ctx, koutakuCtx, req)
			if err != nil {
				defer cancel()
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "short-circuit handler error: "+err.Error()))
				return
			}
			if handled {
				defer cancel()
				return
			}
		}

		// Set direct key from context if available
		if ctx.UserValue(string(schemas.KoutakuContextKeyDirectKey)) != nil {
			key, ok := ctx.UserValue(string(schemas.KoutakuContextKeyDirectKey)).(schemas.Key)
			if ok {
				koutakuCtx.SetValue(schemas.KoutakuContextKeyDirectKey, key)
			}
		}

		// Handle batch requests if BatchRequestConverter is set
		// GenAI has two cases: (1) Dedicated batch routes (list/retrieve) have only BatchRequestConverter — always use batch path.
		// (2) The models path has both BatchRequestConverter and RequestConverter — use batch path only for batch create.
		isGenAIBatchCreate := config.Type == RouteConfigTypeGenAI && koutakuCtx.Value(isGeminiBatchCreateRequestContextKey) != nil
		useBatchPath := config.BatchRequestConverter != nil && (config.RequestConverter == nil || config.Type != RouteConfigTypeGenAI || isGenAIBatchCreate)
		if useBatchPath {
			defer cancel()
			batchReq, err := config.BatchRequestConverter(koutakuCtx, req)
			if err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to convert batch request"))
				return
			}
			if batchReq == nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid batch request"))
				return
			}
			g.handleBatchRequest(ctx, config, req, batchReq, koutakuCtx)
			return
		}
		// Handle file requests if FileRequestConverter is set
		if config.FileRequestConverter != nil {
			defer cancel()
			fileReq, err := config.FileRequestConverter(koutakuCtx, req)
			if err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to convert file request"))
				return
			}
			if fileReq == nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid file request"))
				return
			}
			g.handleFileRequest(ctx, config, req, fileReq, koutakuCtx)
			return
		}

		// Handle container requests if ContainerRequestConverter is set
		if config.ContainerRequestConverter != nil {
			defer cancel()
			containerReq, err := config.ContainerRequestConverter(koutakuCtx, req)
			if err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to convert container request"))
				return
			}
			if containerReq == nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid container request"))
				return
			}
			g.handleContainerRequest(ctx, config, req, containerReq, koutakuCtx)
			return
		}

		// Handle container file requests if ContainerFileRequestConverter is set
		if config.ContainerFileRequestConverter != nil {
			defer cancel()
			containerFileReq, err := config.ContainerFileRequestConverter(koutakuCtx, req)
			if err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to convert container file request"))
				return
			}
			if containerFileReq == nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid container file request"))
				return
			}
			g.handleContainerFileRequest(ctx, config, req, containerFileReq, koutakuCtx)
			return
		}

		// Convert the integration-specific request to Koutaku format (inference requests)
		koutakuReq, err := config.RequestConverter(koutakuCtx, req)
		if err != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to convert request to Koutaku format"))
			return
		}
		if koutakuReq == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid request"))
			return
		}
		if sendRawRequestBody, ok := (*koutakuCtx).Value(schemas.KoutakuContextKeyUseRawRequestBody).(bool); ok && sendRawRequestBody {
			koutakuReq.SetRawRequestBody(rawBody)
		}

		// Extract and parse fallbacks from the request if present
		if err := g.extractAndParseFallbacks(req, koutakuReq); err != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to parse fallbacks: "+err.Error()))
			return
		}

		// Async create: check x-bf-async header (needs parsed koutakuReq)
		if string(ctx.Request.Header.Peek(schemas.AsyncHeaderCreate)) != "" {
			defer cancel()
			g.handleAsyncCreate(ctx, config, req, koutakuReq, koutakuCtx)
			return
		}

		// Check if streaming is requested
		isStreaming := false
		if streamingReq, ok := req.(StreamingRequest); ok {
			isStreaming = streamingReq.IsStreamingRequested()
		}

		if isStreaming {
			g.handleStreamingRequest(ctx, config, koutakuReq, koutakuCtx, cancel)
		} else {
			defer cancel() // Ensure cleanup on function exit
			g.handleNonStreamingRequest(ctx, config, req, koutakuReq, koutakuCtx)
		}
	}
}

// handleNonStreamingRequest handles regular (non-streaming) requests
func (g *GenericRouter) handleNonStreamingRequest(ctx *fasthttp.RequestCtx, config RouteConfig, req interface{}, koutakuReq *schemas.KoutakuRequest, koutakuCtx *schemas.KoutakuContext) {
	// Use the cancellable context from ConvertToKoutakuContext
	// While we can't detect client disconnects until we try to write, having a cancellable context
	// allows providers that check ctx.Done() to cancel early if needed. This is less critical than
	// streaming requests (where we actively detect write errors), but still provides a mechanism
	// for providers to respect cancellation.
	var response interface{}

	var err error

	var providerResponseHeaders map[string]string

	switch {
	case koutakuReq.ListModelsRequest != nil:
		// Determine provider: explicit header overrides request field; otherwise
		// fall back to the request field and finally to list-all behavior.
		listModelsProvider := strings.ToLower(string(ctx.Request.Header.Peek("x-bf-model-provider")))
		switch listModelsProvider {
		case "":
			// keep any provider already set on the request
		case "all":
			koutakuReq.ListModelsRequest.Provider = ""
		default:
			koutakuReq.ListModelsRequest.Provider = schemas.ModelProvider(listModelsProvider)
		}

		var listModelsResponse *schemas.KoutakuListModelsResponse
		var koutakuErr *schemas.KoutakuError

		if koutakuReq.ListModelsRequest.Provider != "" {
			listModelsResponse, koutakuErr = g.client.ListModelsRequest(koutakuCtx, koutakuReq.ListModelsRequest)
		} else {
			listModelsResponse, koutakuErr = g.client.ListAllModels(koutakuCtx, koutakuReq.ListModelsRequest)
		}

		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, listModelsResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if listModelsResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		response, err = config.ListModelsResponseConverter(koutakuCtx, listModelsResponse)
		providerResponseHeaders = listModelsResponse.ExtraFields.ProviderResponseHeaders
	case koutakuReq.TextCompletionRequest != nil:
		textCompletionResponse, koutakuErr := g.client.TextCompletionRequest(koutakuCtx, koutakuReq.TextCompletionRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		// Execute post-request callback if configured
		// This is typically used for response modification or additional processing
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, textCompletionResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if textCompletionResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		// Convert Koutaku response to integration-specific format and send
		response, err = config.TextResponseConverter(koutakuCtx, textCompletionResponse)
		providerResponseHeaders = textCompletionResponse.ExtraFields.ProviderResponseHeaders
	case koutakuReq.ChatRequest != nil:
		chatResponse, koutakuErr := g.client.ChatCompletionRequest(koutakuCtx, koutakuReq.ChatRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		// Execute post-request callback if configured
		// This is typically used for response modification or additional processing
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, chatResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if chatResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		// Convert Koutaku response to integration-specific format and send
		response, err = config.ChatResponseConverter(koutakuCtx, chatResponse)
		providerResponseHeaders = chatResponse.ExtraFields.ProviderResponseHeaders
	case koutakuReq.ResponsesRequest != nil:
		responsesResponse, koutakuErr := g.client.ResponsesRequest(koutakuCtx, koutakuReq.ResponsesRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		// Execute post-request callback if configured
		// This is typically used for response modification or additional processing
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, responsesResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if responsesResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		// Convert Koutaku response to integration-specific format and send
		response, err = config.ResponsesResponseConverter(koutakuCtx, responsesResponse)
		providerResponseHeaders = responsesResponse.ExtraFields.ProviderResponseHeaders
	case koutakuReq.EmbeddingRequest != nil:
		embeddingResponse, koutakuErr := g.client.EmbeddingRequest(koutakuCtx, koutakuReq.EmbeddingRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		// Execute post-request callback if configured
		// This is typically used for response modification or additional processing
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, embeddingResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if embeddingResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}
		providerResponseHeaders = embeddingResponse.ExtraFields.ProviderResponseHeaders
		// Convert Koutaku response to integration-specific format and send
		response, err = config.EmbeddingResponseConverter(koutakuCtx, embeddingResponse)
	case koutakuReq.RerankRequest != nil:
		rerankResponse, koutakuErr := g.client.RerankRequest(koutakuCtx, koutakuReq.RerankRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, rerankResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if rerankResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}
		providerResponseHeaders = rerankResponse.ExtraFields.ProviderResponseHeaders
		if config.RerankResponseConverter != nil {
			response, err = config.RerankResponseConverter(koutakuCtx, rerankResponse)
		} else {
			response = rerankResponse
		}

	case koutakuReq.OCRRequest != nil:
		ocrResponse, koutakuErr := g.client.OCRRequest(koutakuCtx, koutakuReq.OCRRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, ocrResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if ocrResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "koutaku response is nil after post-request callback"))
			return
		}
		providerResponseHeaders = ocrResponse.ExtraFields.ProviderResponseHeaders
		if config.OCRResponseConverter != nil {
			response, err = config.OCRResponseConverter(koutakuCtx, ocrResponse)
		} else {
			response = ocrResponse
		}

	case koutakuReq.SpeechRequest != nil:
		speechResponse, koutakuErr := g.client.SpeechRequest(koutakuCtx, koutakuReq.SpeechRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, speechResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if speechResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		providerResponseHeaders = speechResponse.ExtraFields.ProviderResponseHeaders

		if g.tryStreamLargeResponse(ctx, koutakuCtx) {
			return
		}

		if config.SpeechResponseConverter != nil {
			response, err = config.SpeechResponseConverter(koutakuCtx, speechResponse)
			if err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to convert speech response"))
				return
			}
			g.sendSuccess(ctx, koutakuCtx, config.ErrorConverter, response, nil)
			return
		} else {
			ctx.Response.Header.Set("Content-Type", "audio/mpeg")
			ctx.Response.Header.Set("Content-Disposition", "attachment; filename=speech.mp3")
			ctx.Response.Header.Set("Content-Length", strconv.Itoa(len(speechResponse.Audio)))
			ctx.Response.SetBody(speechResponse.Audio)
			return
		}
	case koutakuReq.TranscriptionRequest != nil:
		transcriptionResponse, koutakuErr := g.client.TranscriptionRequest(koutakuCtx, koutakuReq.TranscriptionRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		// Execute post-request callback if configured
		// This is typically used for response modification or additional processing
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, transcriptionResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if transcriptionResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		if g.tryStreamLargeResponse(ctx, koutakuCtx) {
			return
		}

		// Convert Koutaku response to integration-specific format and send
		response, err = config.TranscriptionResponseConverter(koutakuCtx, transcriptionResponse)
		providerResponseHeaders = transcriptionResponse.ExtraFields.ProviderResponseHeaders

		// If converter returns raw bytes, write directly with provider headers.
		// Used for plain-text transcription formats (text, srt, vtt).
		if err == nil {
			if rawBytes, ok := response.([]byte); ok {
				for key, value := range providerResponseHeaders {
					ctx.Response.Header.Set(key, value)
				}
				ctx.SetStatusCode(fasthttp.StatusOK)
				ctx.SetBody(rawBytes)
				return
			}
		}
	case koutakuReq.ImageGenerationRequest != nil:
		imageGenerationResponse, koutakuErr := g.client.ImageGenerationRequest(koutakuCtx, koutakuReq.ImageGenerationRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		// Execute post-request callback if configured
		// This is typically used for response modification or additional processing
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, imageGenerationResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if imageGenerationResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		if config.ImageGenerationResponseConverter == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "missing ImageGenerationResponseConverter for integration"))
			return
		}

		if g.tryStreamLargeResponse(ctx, koutakuCtx) {
			return
		}

		// Convert Koutaku response to integration-specific format and send
		response, err = config.ImageGenerationResponseConverter(koutakuCtx, imageGenerationResponse)
		providerResponseHeaders = imageGenerationResponse.ExtraFields.ProviderResponseHeaders
	case koutakuReq.ImageEditRequest != nil:
		imageEditResponse, koutakuErr := g.client.ImageEditRequest(koutakuCtx, koutakuReq.ImageEditRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		// Execute post-request callback if configured
		// This is typically used for response modification or additional processing
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, imageEditResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if imageEditResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		if config.ImageGenerationResponseConverter == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "missing ImageGenerationResponseConverter for integration"))
			return
		}

		if g.tryStreamLargeResponse(ctx, koutakuCtx) {
			return
		}

		// Convert Koutaku response to integration-specific format and send
		response, err = config.ImageGenerationResponseConverter(koutakuCtx, imageEditResponse)
		providerResponseHeaders = imageEditResponse.ExtraFields.ProviderResponseHeaders
	case koutakuReq.ImageVariationRequest != nil:
		imageVariationResponse, koutakuErr := g.client.ImageVariationRequest(koutakuCtx, koutakuReq.ImageVariationRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		// Execute post-request callback if configured
		// This is typically used for response modification or additional processing
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, imageVariationResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if imageVariationResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		if config.ImageGenerationResponseConverter == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "missing ImageGenerationResponseConverter for integration"))
			return
		}

		if g.tryStreamLargeResponse(ctx, koutakuCtx) {
			return
		}

		// Convert Koutaku response to integration-specific format and send
		response, err = config.ImageGenerationResponseConverter(koutakuCtx, imageVariationResponse)
		providerResponseHeaders = imageVariationResponse.ExtraFields.ProviderResponseHeaders
	case koutakuReq.VideoGenerationRequest != nil:
		videoGenerationResponse, koutakuErr := g.client.VideoGenerationRequest(koutakuCtx, koutakuReq.VideoGenerationRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, videoGenerationResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if videoGenerationResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		if config.VideoGenerationResponseConverter == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "missing VideoGenerationResponseConverter for integration"))
			return
		}

		response, err = config.VideoGenerationResponseConverter(koutakuCtx, videoGenerationResponse)
		providerResponseHeaders = videoGenerationResponse.ExtraFields.ProviderResponseHeaders
	case koutakuReq.VideoRetrieveRequest != nil:
		videoRetrieveResponse, koutakuErr := g.client.VideoRetrieveRequest(koutakuCtx, koutakuReq.VideoRetrieveRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, videoRetrieveResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if videoRetrieveResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		if config.VideoGenerationResponseConverter == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "missing VideoGenerationResponseConverter for integration"))
			return
		}
		response, err = config.VideoGenerationResponseConverter(koutakuCtx, videoRetrieveResponse)
		providerResponseHeaders = videoRetrieveResponse.ExtraFields.ProviderResponseHeaders
	case koutakuReq.VideoDownloadRequest != nil:
		videoDownloadResponse, koutakuErr := g.client.VideoDownloadRequest(koutakuCtx, koutakuReq.VideoDownloadRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, videoDownloadResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if videoDownloadResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		if config.VideoDownloadResponseConverter == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "missing VideoDownloadResponseConverter for integration"))
			return
		}

		response, err = config.VideoDownloadResponseConverter(koutakuCtx, videoDownloadResponse)
		providerResponseHeaders = videoDownloadResponse.ExtraFields.ProviderResponseHeaders

		// If converter returns binary content, write directly with content-type.
		if err == nil {
			if rawBytes, ok := response.([]byte); ok {
				contentType := videoDownloadResponse.ContentType
				if contentType == "" {
					contentType = "application/octet-stream"
				}
				ctx.Response.Header.Set("Content-Type", contentType)
				ctx.Response.Header.Set("Content-Length", strconv.Itoa(len(rawBytes)))
				ctx.Response.SetBody(rawBytes)
				return
			}
		}
	case koutakuReq.VideoDeleteRequest != nil:
		videoDeleteResponse, koutakuErr := g.client.VideoDeleteRequest(koutakuCtx, koutakuReq.VideoDeleteRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, videoDeleteResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if videoDeleteResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		if config.VideoDeleteResponseConverter == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "missing VideoDeleteResponseConverter for integration"))
			return
		}

		response, err = config.VideoDeleteResponseConverter(koutakuCtx, videoDeleteResponse)
		providerResponseHeaders = videoDeleteResponse.ExtraFields.ProviderResponseHeaders
	case koutakuReq.VideoRemixRequest != nil:
		videoRemixResponse, koutakuErr := g.client.VideoRemixRequest(koutakuCtx, koutakuReq.VideoRemixRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, videoRemixResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if videoRemixResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		if config.VideoGenerationResponseConverter == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "missing VideoGenerationResponseConverter for integration"))
			return
		}

		response, err = config.VideoGenerationResponseConverter(koutakuCtx, videoRemixResponse)
		providerResponseHeaders = videoRemixResponse.ExtraFields.ProviderResponseHeaders
	case koutakuReq.VideoListRequest != nil:

		// extract provider from header
		providerHeader := strings.ToLower(string(ctx.Request.Header.Peek("x-bf-video-list-provider")))
		if providerHeader != "" {
			koutakuReq.VideoListRequest.Provider = schemas.ModelProvider(providerHeader)
		} else if koutakuReq.VideoListRequest.Provider == "" {
			koutakuReq.VideoListRequest.Provider = schemas.OpenAI
		}
		videoListResponse, koutakuErr := g.client.VideoListRequest(koutakuCtx, koutakuReq.VideoListRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, videoListResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if videoListResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		if config.VideoListResponseConverter == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "missing VideoListResponseConverter for integration"))
			return
		}

		response, err = config.VideoListResponseConverter(koutakuCtx, videoListResponse)
		providerResponseHeaders = videoListResponse.ExtraFields.ProviderResponseHeaders

	case koutakuReq.CountTokensRequest != nil:
		countTokensResponse, koutakuErr := g.client.CountTokensRequest(koutakuCtx, koutakuReq.CountTokensRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}

		// Execute post-request callback if configured
		// This is typically used for response modification or additional processing
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, countTokensResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}

		if countTokensResponse == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Koutaku response is nil after post-request callback"))
			return
		}

		// Convert Koutaku response to integration-specific format and send
		if config.CountTokensResponseConverter == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "CountTokensResponseConverter not configured"))
			return
		}
		response, err = config.CountTokensResponseConverter(koutakuCtx, countTokensResponse)
		providerResponseHeaders = countTokensResponse.ExtraFields.ProviderResponseHeaders
	default:
		g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Invalid request type"))
		return
	}

	if err != nil {
		g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to encode response"))
		return
	}

	// Forward provider response headers only after conversion succeeds
	for key, value := range providerResponseHeaders {
		ctx.Response.Header.Set(key, value)
	}

	if g.tryStreamLargeResponse(ctx, koutakuCtx) {
		return
	}

	g.sendSuccess(ctx, koutakuCtx, config.ErrorConverter, response, nil)
}

// --- Async integration handlers ---

// handleAsyncCreate submits an async job for the current inference request.
// It stores the raw Koutaku response in the DB; the response converter is applied at retrieval time.
func (g *GenericRouter) handleAsyncCreate(
	ctx *fasthttp.RequestCtx,
	config RouteConfig,
	req interface{},
	koutakuReq *schemas.KoutakuRequest,
	koutakuCtx *schemas.KoutakuContext,
) {
	executor := g.handlerStore.GetAsyncJobExecutor()
	if executor == nil {
		g.sendError(ctx, koutakuCtx, config.ErrorConverter,
			newKoutakuError(nil, "async operations not available: logs store not configured"))
		return
	}

	// Reject streaming + async
	if streamingReq, ok := req.(StreamingRequest); ok && streamingReq.IsStreamingRequested() {
		g.sendError(ctx, koutakuCtx, config.ErrorConverter,
			newKoutakuErrorWithCode(nil, "streaming is not supported for async requests", fasthttp.StatusBadRequest))
		return
	}

	// Reject non-inference routes (batch, file, container)
	if config.BatchRequestConverter != nil || config.FileRequestConverter != nil ||
		config.ContainerRequestConverter != nil || config.ContainerFileRequestConverter != nil {
		g.sendError(ctx, koutakuCtx, config.ErrorConverter,
			newKoutakuError(nil, "async is not supported for batch, file, or container operations"))
		return
	}

	switch config.GetHTTPRequestType(ctx) {
	case schemas.ChatCompletionRequest:
		if config.AsyncChatResponseConverter == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "async operation is not supported on this route"))
			return
		}
	case schemas.ResponsesRequest:
		if config.AsyncResponsesResponseConverter == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "async operation is not supported on this route"))
			return
		}
	default:
		g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "async operation is not supported on this route"))
		return
	}

	operationType := config.GetHTTPRequestType(ctx)
	resultTTL := getResultTTLFromHeaderWithDefault(ctx, g.handlerStore.GetAsyncJobResultTTL())

	// The operation closure runs the Koutaku client call in the background.
	// It returns the raw typed Koutaku response (NOT provider-converted).
	// The response converter is applied at retrieval time via handleAsyncRetrieve.
	operation := func(bgCtx *schemas.KoutakuContext) (interface{}, *schemas.KoutakuError) {
		switch {
		case koutakuReq.ChatRequest != nil:
			return g.client.ChatCompletionRequest(bgCtx, koutakuReq.ChatRequest)
		case koutakuReq.ResponsesRequest != nil:
			return g.client.ResponsesRequest(bgCtx, koutakuReq.ResponsesRequest)
		default:
			return nil, newKoutakuError(nil, "unsupported request type for async execution")
		}
	}

	job, err := executor.SubmitJob(koutakuCtx, resultTTL, operation, operationType)
	if err != nil {
		g.sendError(ctx, koutakuCtx, config.ErrorConverter,
			newKoutakuError(err, "failed to create async job"))
		return
	}

	g.handleAsyncJobResponse(ctx, koutakuCtx, config, job)
}

// handleAsyncRetrieve retrieves an async job by ID and returns the response
// using the route's response converter for completed jobs.
func (g *GenericRouter) handleAsyncRetrieve(
	ctx *fasthttp.RequestCtx,
	config RouteConfig,
	koutakuCtx *schemas.KoutakuContext,
) {
	executor := g.handlerStore.GetAsyncJobExecutor()
	if executor == nil {
		g.sendError(ctx, koutakuCtx, config.ErrorConverter,
			newKoutakuError(nil, "async operations not available: logs store not configured"))
		return
	}

	jobID := string(ctx.Request.Header.Peek(schemas.AsyncHeaderGetID))
	if jobID == "" {
		g.sendError(ctx, koutakuCtx, config.ErrorConverter,
			newKoutakuError(nil, "x-bf-async-id header value is empty"))
		return
	}

	vkValue := getVirtualKeyFromKoutakuContext(koutakuCtx)

	job, err := executor.RetrieveJob(koutakuCtx, jobID, vkValue, config.GetHTTPRequestType(ctx))
	if err != nil {
		if errors.Is(err, logstore.ErrJobInternal) {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter,
				newKoutakuErrorWithCode(err, "failed to retrieve async job", fasthttp.StatusInternalServerError))
		} else {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter,
				newKoutakuErrorWithCode(err, "job not found or expired", fasthttp.StatusNotFound))
		}
		return
	}

	g.handleAsyncJobResponse(ctx, koutakuCtx, config, job)
	return
}

func (g *GenericRouter) handleAsyncJobResponse(ctx *fasthttp.RequestCtx, koutakuCtx *schemas.KoutakuContext, config RouteConfig, job *logstore.AsyncJob) {
	ctx.SetContentType("application/json")

	resp := job.ToResponse()

	switch job.Status {
	case schemas.AsyncJobStatusPending, schemas.AsyncJobStatusProcessing, schemas.AsyncJobStatusCompleted:
		switch job.RequestType {
		case schemas.ChatCompletionRequest:
			if config.AsyncChatResponseConverter == nil || config.ChatResponseConverter == nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "async operation is not supported on this route"))
				return
			}
			response, extraHeaders, err := config.AsyncChatResponseConverter(koutakuCtx, resp, config.ChatResponseConverter)
			if err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to convert async chat response"))
				return
			}
			g.sendSuccess(ctx, koutakuCtx, config.ErrorConverter, response, extraHeaders)
			return
		case schemas.ResponsesRequest:
			if config.AsyncResponsesResponseConverter == nil || config.ResponsesResponseConverter == nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "either async responses response converter or responses response converter not configured"))
				return
			}
			response, extraHeaders, err := config.AsyncResponsesResponseConverter(koutakuCtx, resp, config.ResponsesResponseConverter)
			if err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to convert async responses response"))
				return
			}
			g.sendSuccess(ctx, koutakuCtx, config.ErrorConverter, response, extraHeaders)
			return
		default:
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "unknown request type"))
			return
		}

	case schemas.AsyncJobStatusFailed:
		var err schemas.KoutakuError
		// Deserialize the stored KoutakuError and send through provider error converter
		if job.Error != "" {
			if unmarshalErr := sonic.Unmarshal([]byte(job.Error), &err); unmarshalErr != nil {
				// If unmarshal fails, create a basic error with the raw error string
				err = schemas.KoutakuError{
					Error: &schemas.ErrorField{
						Message: job.Error,
					},
				}
			}
		}
		g.sendError(ctx, koutakuCtx, config.ErrorConverter, &err)
	}
}

// handleBatchRequest handles batch API requests (create, list, retrieve, cancel, results)
func (g *GenericRouter) handleBatchRequest(ctx *fasthttp.RequestCtx, config RouteConfig, req interface{}, batchReq *BatchRequest, koutakuCtx *schemas.KoutakuContext) {
	var response interface{}
	var err error

	switch batchReq.Type {
	case schemas.BatchCreateRequest:
		if batchReq.CreateRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid batch create request"))
			return
		}
		batchResponse, koutakuErr := g.client.BatchCreateRequest(koutakuCtx, batchReq.CreateRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, batchResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.BatchCreateResponseConverter != nil {
			response, err = config.BatchCreateResponseConverter(koutakuCtx, batchResponse)
		} else {
			response = batchResponse
		}

	case schemas.BatchListRequest:
		if batchReq.ListRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid batch list request"))
			return
		}
		batchResponse, koutakuErr := g.client.BatchListRequest(koutakuCtx, batchReq.ListRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, batchResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.BatchListResponseConverter != nil {
			response, err = config.BatchListResponseConverter(koutakuCtx, batchResponse)
		} else {
			response = batchResponse
		}

	case schemas.BatchRetrieveRequest:
		if batchReq.RetrieveRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid batch retrieve request"))
			return
		}
		batchResponse, koutakuErr := g.client.BatchRetrieveRequest(koutakuCtx, batchReq.RetrieveRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, batchResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.BatchRetrieveResponseConverter != nil {
			response, err = config.BatchRetrieveResponseConverter(koutakuCtx, batchResponse)
		} else {
			response = batchResponse
		}

	case schemas.BatchCancelRequest:
		if batchReq.CancelRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid batch cancel request"))
			return
		}
		batchResponse, koutakuErr := g.client.BatchCancelRequest(koutakuCtx, batchReq.CancelRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, batchResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.BatchCancelResponseConverter != nil {
			response, err = config.BatchCancelResponseConverter(koutakuCtx, batchResponse)
		} else {
			response = batchResponse
		}
	case schemas.BatchDeleteRequest:
		if batchReq.DeleteRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid batch delete request"))
			return
		}
		batchResponse, koutakuErr := g.client.BatchDeleteRequest(koutakuCtx, batchReq.DeleteRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, batchResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.BatchDeleteResponseConverter != nil {
			response, err = config.BatchDeleteResponseConverter(koutakuCtx, batchResponse)
		} else {
			response = batchResponse
		}

	case schemas.BatchResultsRequest:
		if batchReq.ResultsRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid batch results request"))
			return
		}
		batchResponse, koutakuErr := g.client.BatchResultsRequest(koutakuCtx, batchReq.ResultsRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, batchResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.BatchResultsResponseConverter != nil {
			response, err = config.BatchResultsResponseConverter(koutakuCtx, batchResponse)
		} else {
			response = batchResponse
		}

	default:
		g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Unknown batch request type"))
		return
	}

	if err != nil {
		g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to convert batch response"))
		return
	}

	g.sendSuccess(ctx, koutakuCtx, config.ErrorConverter, response, nil)
}

// handleFileRequest handles file API requests (upload, list, retrieve, delete, content)
func (g *GenericRouter) handleFileRequest(ctx *fasthttp.RequestCtx, config RouteConfig, req interface{}, fileReq *FileRequest, koutakuCtx *schemas.KoutakuContext) {
	var response interface{}
	var err error

	switch fileReq.Type {
	case schemas.FileUploadRequest:
		if fileReq.UploadRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid file upload request"))
			return
		}
		fileResponse, koutakuErr := g.client.FileUploadRequest(koutakuCtx, fileReq.UploadRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, fileResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.FileUploadResponseConverter != nil {
			response, err = config.FileUploadResponseConverter(koutakuCtx, fileResponse)
		} else {
			response = fileResponse
		}

	case schemas.FileListRequest:
		if fileReq.ListRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid file list request"))
			return
		}
		fileResponse, koutakuErr := g.client.FileListRequest(koutakuCtx, fileReq.ListRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, fileResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.FileListResponseConverter != nil {
			response, err = config.FileListResponseConverter(koutakuCtx, fileResponse)
			if err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to convert file list response"))
				return
			}
			// Handle raw byte responses (e.g., XML for S3 APIs)
			if rawBytes, ok := response.([]byte); ok {
				ctx.SetBody(rawBytes)
				return
			}
		} else {
			response = fileResponse
		}

	case schemas.FileRetrieveRequest:
		if fileReq.RetrieveRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid file retrieve request"))
			return
		}
		fileResponse, koutakuErr := g.client.FileRetrieveRequest(koutakuCtx, fileReq.RetrieveRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, fileResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.FileRetrieveResponseConverter != nil {
			response, err = config.FileRetrieveResponseConverter(koutakuCtx, fileResponse)
		} else {
			response = fileResponse
		}

	case schemas.FileDeleteRequest:
		if fileReq.DeleteRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid file delete request"))
			return
		}
		fileResponse, koutakuErr := g.client.FileDeleteRequest(koutakuCtx, fileReq.DeleteRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, fileResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.FileDeleteResponseConverter != nil {
			response, err = config.FileDeleteResponseConverter(koutakuCtx, fileResponse)
		} else {
			response = fileResponse
		}

	case schemas.FileContentRequest:
		if fileReq.ContentRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid file content request"))
			return
		}
		fileResponse, koutakuErr := g.client.FileContentRequest(koutakuCtx, fileReq.ContentRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, fileResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		// For file content, handle binary response specially if no converter is set
		if config.FileContentResponseConverter != nil {
			response, err = config.FileContentResponseConverter(koutakuCtx, fileResponse)
			if err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to convert file content response"))
				return
			}
			// Check if response is raw bytes - write directly without JSON encoding
			if rawBytes, ok := response.([]byte); ok {
				ctx.Response.Header.Set("Content-Type", fileResponse.ContentType)
				ctx.Response.Header.Set("Content-Length", strconv.Itoa(len(rawBytes)))
				ctx.Response.SetBody(rawBytes)
			} else {
				g.sendSuccess(ctx, koutakuCtx, config.ErrorConverter, response, nil)
			}
		} else {
			// Return raw file content
			ctx.Response.Header.Set("Content-Type", fileResponse.ContentType)
			ctx.Response.Header.Set("Content-Length", strconv.Itoa(len(fileResponse.Content)))
			ctx.Response.SetBody(fileResponse.Content)
		}
		return

	default:
		g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Unknown file request type"))
		return
	}

	if err != nil {
		g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to convert file response"))
		return
	}

	// If response is nil, PostCallback has set headers/status - return without body
	if response == nil {
		return
	}

	g.sendSuccess(ctx, koutakuCtx, config.ErrorConverter, response, nil)
}

// handleContainerRequest handles container API requests (create, list, retrieve, delete)
func (g *GenericRouter) handleContainerRequest(ctx *fasthttp.RequestCtx, config RouteConfig, req interface{}, containerReq *ContainerRequest, koutakuCtx *schemas.KoutakuContext) {
	var response interface{}
	var err error

	switch containerReq.Type {
	case schemas.ContainerCreateRequest:
		if containerReq.CreateRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid container create request"))
			return
		}
		containerResponse, koutakuErr := g.client.ContainerCreateRequest(koutakuCtx, containerReq.CreateRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, containerResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.ContainerCreateResponseConverter != nil {
			response, err = config.ContainerCreateResponseConverter(koutakuCtx, containerResponse)
		} else {
			response = containerResponse
		}

	case schemas.ContainerListRequest:
		if containerReq.ListRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid container list request"))
			return
		}
		containerResponse, koutakuErr := g.client.ContainerListRequest(koutakuCtx, containerReq.ListRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, containerResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.ContainerListResponseConverter != nil {
			response, err = config.ContainerListResponseConverter(koutakuCtx, containerResponse)
		} else {
			response = containerResponse
		}

	case schemas.ContainerRetrieveRequest:
		if containerReq.RetrieveRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid container retrieve request"))
			return
		}
		containerResponse, koutakuErr := g.client.ContainerRetrieveRequest(koutakuCtx, containerReq.RetrieveRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, containerResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.ContainerRetrieveResponseConverter != nil {
			response, err = config.ContainerRetrieveResponseConverter(koutakuCtx, containerResponse)
		} else {
			response = containerResponse
		}

	case schemas.ContainerDeleteRequest:
		if containerReq.DeleteRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid container delete request"))
			return
		}
		containerResponse, koutakuErr := g.client.ContainerDeleteRequest(koutakuCtx, containerReq.DeleteRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, containerResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.ContainerDeleteResponseConverter != nil {
			response, err = config.ContainerDeleteResponseConverter(koutakuCtx, containerResponse)
		} else {
			response = containerResponse
		}

	default:
		g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Unknown container request type"))
		return
	}

	if err != nil {
		g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to convert container response"))
		return
	}

	g.sendSuccess(ctx, koutakuCtx, config.ErrorConverter, response, nil)
}

// handleContainerFileRequest handles container file API requests (create, list, retrieve, content, delete)
func (g *GenericRouter) handleContainerFileRequest(ctx *fasthttp.RequestCtx, config RouteConfig, req interface{}, containerFileReq *ContainerFileRequest, koutakuCtx *schemas.KoutakuContext) {
	var response interface{}
	var err error

	switch containerFileReq.Type {
	case schemas.ContainerFileCreateRequest:
		if containerFileReq.CreateRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid container file create request"))
			return
		}
		containerFileResponse, koutakuErr := g.client.ContainerFileCreateRequest(koutakuCtx, containerFileReq.CreateRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, containerFileResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.ContainerFileCreateResponseConverter != nil {
			response, err = config.ContainerFileCreateResponseConverter(koutakuCtx, containerFileResponse)
		} else {
			response = containerFileResponse
		}

	case schemas.ContainerFileListRequest:
		if containerFileReq.ListRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid container file list request"))
			return
		}
		containerFileResponse, koutakuErr := g.client.ContainerFileListRequest(koutakuCtx, containerFileReq.ListRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, containerFileResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.ContainerFileListResponseConverter != nil {
			response, err = config.ContainerFileListResponseConverter(koutakuCtx, containerFileResponse)
		} else {
			response = containerFileResponse
		}

	case schemas.ContainerFileRetrieveRequest:
		if containerFileReq.RetrieveRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid container file retrieve request"))
			return
		}
		containerFileResponse, koutakuErr := g.client.ContainerFileRetrieveRequest(koutakuCtx, containerFileReq.RetrieveRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, containerFileResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.ContainerFileRetrieveResponseConverter != nil {
			response, err = config.ContainerFileRetrieveResponseConverter(koutakuCtx, containerFileResponse)
		} else {
			response = containerFileResponse
		}

	case schemas.ContainerFileContentRequest:
		if containerFileReq.ContentRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid container file content request"))
			return
		}
		containerFileResponse, koutakuErr := g.client.ContainerFileContentRequest(koutakuCtx, containerFileReq.ContentRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, containerFileResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		// For content requests, handle binary response specially if converter is set
		if config.ContainerFileContentResponseConverter != nil {
			response, err = config.ContainerFileContentResponseConverter(koutakuCtx, containerFileResponse)
			if err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to convert container file content response"))
				return
			}
			// Check if response is raw bytes - write directly without JSON encoding
			if rawBytes, ok := response.([]byte); ok {
				ctx.Response.Header.Set("Content-Type", containerFileResponse.ContentType)
				ctx.Response.Header.Set("Content-Length", strconv.Itoa(len(rawBytes)))
				ctx.Response.SetBody(rawBytes)
			} else {
				g.sendSuccess(ctx, koutakuCtx, config.ErrorConverter, response, nil)
			}
		} else {
			// Return raw binary content
			ctx.Response.Header.Set("Content-Type", containerFileResponse.ContentType)
			ctx.Response.Header.Set("Content-Length", strconv.Itoa(len(containerFileResponse.Content)))
			ctx.Response.SetBody(containerFileResponse.Content)
		}
		return

	case schemas.ContainerFileDeleteRequest:
		if containerFileReq.DeleteRequest == nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "invalid container file delete request"))
			return
		}
		containerFileResponse, koutakuErr := g.client.ContainerFileDeleteRequest(koutakuCtx, containerFileReq.DeleteRequest)
		if koutakuErr != nil {
			g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
			return
		}
		if config.PostCallback != nil {
			if err := config.PostCallback(ctx, req, containerFileResponse); err != nil {
				g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to execute post-request callback"))
				return
			}
		}
		if config.ContainerFileDeleteResponseConverter != nil {
			response, err = config.ContainerFileDeleteResponseConverter(koutakuCtx, containerFileResponse)
		} else {
			response = containerFileResponse
		}

	default:
		g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "Unknown container file request type"))
		return
	}

	if err != nil {
		g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(err, "failed to convert container file response"))
		return
	}

	g.sendSuccess(ctx, koutakuCtx, config.ErrorConverter, response, nil)
}

// handleStreamingRequest handles streaming requests using Server-Sent Events (SSE)
func (g *GenericRouter) handleStreamingRequest(ctx *fasthttp.RequestCtx, config RouteConfig, koutakuReq *schemas.KoutakuRequest, koutakuCtx *schemas.KoutakuContext, cancel context.CancelFunc) {
	// Use the cancellable context from ConvertToKoutakuContext
	// ctx.Done() never fires here in practice: fasthttp.RequestCtx.Done only closes when the whole server shuts down, not when an individual connection drops.
	// As a result we'll leave the provider stream running until it naturally completes, even if the client went away (write error, network drop, etc.).
	// That keeps goroutines and upstream tokens alive long after the SSE writer has exited.
	//
	// We now get a cancellable context from ConvertToKoutakuContext so we can cancel the upstream stream immediately when the client disconnects.
	var stream chan *schemas.KoutakuStreamChunk
	var koutakuErr *schemas.KoutakuError

	// Handle different request types
	if koutakuReq.TextCompletionRequest != nil {
		stream, koutakuErr = g.client.TextCompletionStreamRequest(koutakuCtx, koutakuReq.TextCompletionRequest)
	} else if koutakuReq.ChatRequest != nil {
		stream, koutakuErr = g.client.ChatCompletionStreamRequest(koutakuCtx, koutakuReq.ChatRequest)
	} else if koutakuReq.ResponsesRequest != nil {
		stream, koutakuErr = g.client.ResponsesStreamRequest(koutakuCtx, koutakuReq.ResponsesRequest)
	} else if koutakuReq.SpeechRequest != nil {
		stream, koutakuErr = g.client.SpeechStreamRequest(koutakuCtx, koutakuReq.SpeechRequest)
	} else if koutakuReq.TranscriptionRequest != nil {
		stream, koutakuErr = g.client.TranscriptionStreamRequest(koutakuCtx, koutakuReq.TranscriptionRequest)
	} else if koutakuReq.ImageGenerationRequest != nil {
		stream, koutakuErr = g.client.ImageGenerationStreamRequest(koutakuCtx, koutakuReq.ImageGenerationRequest)
	} else if koutakuReq.ImageEditRequest != nil {
		stream, koutakuErr = g.client.ImageEditStreamRequest(koutakuCtx, koutakuReq.ImageEditRequest)
	}

	// Provider error before streaming started — return proper HTTP error status
	// (SSE headers not yet committed, so we can still set status code + JSON body)
	if koutakuErr != nil {
		cancel()
		g.sendError(ctx, koutakuCtx, config.ErrorConverter, koutakuErr)
		return
	}

	// No request type matched — stream is nil. Return error without spawning
	// a drain goroutine (for-range on nil channel blocks forever).
	if stream == nil {
		cancel()
		g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "streaming is not supported for this request type"))
		return
	}

	// Forward provider response headers stored in context by streaming handlers
	if headers, ok := koutakuCtx.Value(schemas.KoutakuContextKeyProviderResponseHeaders).(map[string]string); ok {
		for key, value := range headers {
			ctx.Response.Header.Set(key, value)
		}
	}

	// Large payload streaming passthrough — bypass SSE event processing, pipe raw upstream
	if g.tryStreamLargeResponse(ctx, koutakuCtx) {
		ctx.Response.Header.Set("Cache-Control", "no-cache")
		ctx.Response.Header.Set("Connection", "keep-alive")
		ctx.Response.Header.Set("Access-Control-Allow-Origin", "*")
		cancel()
		go func() {
			for range stream {
			}
		}()
		return
	}

	// Check if streaming is configured for this route
	if config.StreamConfig == nil {
		cancel()
		// Drain the stream channel to prevent goroutine leaks
		go func() {
			for range stream {
			}
		}()
		g.sendError(ctx, koutakuCtx, config.ErrorConverter, newKoutakuError(nil, "streaming is not supported for this integration"))
		return
	}

	// SSE headers set only after successful stream setup — errors above get proper HTTP status codes
	if config.Type == RouteConfigTypeBedrock {
		ctx.SetContentType("application/vnd.amazon.eventstream")
		ctx.Response.Header.Set("x-amzn-bedrock-content-type", "application/json")
	} else {
		ctx.SetContentType("text/event-stream")
	}

	ctx.Response.Header.Set("Cache-Control", "no-cache")
	ctx.Response.Header.Set("Connection", "keep-alive")
	ctx.Response.Header.Set("Access-Control-Allow-Origin", "*")

	// Handle streaming using the centralized approach
	// Pass cancel function so it can be called when the writer exits (errors, completion, etc.)
	g.handleStreaming(ctx, koutakuCtx, config, stream, cancel)
}

// handleStreaming processes a stream of KoutakuResponse objects and sends them as Server-Sent Events (SSE).
// It handles both successful responses and errors in the streaming format.
//
// SSE FORMAT HANDLING:
//
// By default, all responses and errors are sent in the standard SSE format:
//
//	data: {"response": "content"}\n\n
//
// However, some providers (like Anthropic) require custom SSE event formats with explicit event types:
//
//	event: content_block_delta
//	data: {"type": "content_block_delta", "delta": {...}}
//
//	event: message_stop
//	data: {"type": "message_stop"}
//
// STREAMCONFIG CONVERTER BEHAVIOR:
//
// The StreamConfig.ResponseConverter and StreamConfig.ErrorConverter functions can return:
//
// 1. OBJECTS (default behavior):
//   - Return any Go struct/map/interface{}
//   - Will be JSON marshaled and wrapped as: data: {json}\n\n
//   - Example: return map[string]interface{}{"content": "hello"}
//   - Result: data: {"content":"hello"}\n\n
//
// 2. STRINGS (custom SSE format):
//   - Return a complete SSE string with custom event types and formatting
//   - Will be sent directly without any wrapping or modification
//   - Example: return "event: content_block_delta\ndata: {\"type\":\"text\"}\n\n"
//   - Result: event: content_block_delta
//     data: {"type":"text"}
//
// IMPLEMENTATION GUIDELINES:
//
// For standard providers (OpenAI, etc.): Return objects from converters
// For custom SSE providers (Anthropic, etc.): Return pre-formatted SSE strings
//
// When returning strings, ensure they:
// - Include proper event: lines (if needed)
// - Include data: lines with JSON content
// - End with \n\n for proper SSE formatting
// - Follow the provider's specific SSE event specification
//
// CONTEXT CANCELLATION:
//
// The cancel function is called ONLY when client disconnects are detected via write errors.
// Koutaku handles cleanup internally for normal completion and errors, so we only cancel
// upstream streams when write errors indicate the client has disconnected.
func (g *GenericRouter) handleStreaming(ctx *fasthttp.RequestCtx, koutakuCtx *schemas.KoutakuContext, config RouteConfig, streamChan chan *schemas.KoutakuStreamChunk, cancel context.CancelFunc) {
	// Signal to tracing middleware that trace completion should be deferred
	// The streaming callback will complete the trace after the stream ends
	ctx.SetUserValue(schemas.KoutakuContextKeyDeferTraceCompletion, true)

	// Get the trace completer function for use in the streaming callback.
	// Signature is func([]schemas.PluginLogEntry) so the callback never reads from
	// ctx.UserValue (ctx may be recycled by fasthttp by the time this fires).
	// Router path has no transport post-hook phase, so we always pass nil.
	traceCompleter, _ := ctx.UserValue(schemas.KoutakuContextKeyTraceCompleter).(func([]schemas.PluginLogEntry))

	// Get stream chunk interceptor for plugin hooks
	interceptor := g.handlerStore.GetStreamChunkInterceptor()
	var httpReq *schemas.HTTPRequest
	if interceptor != nil {
		httpReq = lib.BuildHTTPRequestFromFastHTTP(ctx)
	}

	// Use SSEStreamReader to bypass fasthttp's internal pipe (fasthttputil.PipeConns)
	// which batches multiple SSE events into single TCP segments.
	reader := lib.NewSSEStreamReader()
	ctx.Response.SetBodyStream(reader, -1)

	// Producer goroutine: processes the stream channel, formats events, sends to reader
	go func() {
		// Separate defers ensure each cleanup runs even if an earlier one panics (LIFO order)
		defer reader.Done()
		defer schemas.ReleaseHTTPRequest(httpReq)
		defer func() {
			// Complete the trace after streaming finishes
			// This ensures all spans (including llm.call) are properly ended before the trace is sent to OTEL
			if traceCompleter != nil {
				traceCompleter(nil)
			}
		}()

		// Create encoder for AWS Event Stream if needed
		var eventStreamEncoder *eventstream.Encoder
		if config.Type == RouteConfigTypeBedrock {
			eventStreamEncoder = eventstream.NewEncoder()
		}

		shouldSendDoneMarker := true
		if config.Type == RouteConfigTypeAnthropic || strings.Contains(config.Path, "/responses") || strings.Contains(config.Path, "/images/generations") {
			shouldSendDoneMarker = false
		}

		// Process streaming responses
		for chunk := range streamChan {
			if chunk == nil {
				continue
			}

			// Note: We no longer check ctx.Done() here because fasthttp.RequestCtx.Done()
			// only closes when the whole server shuts down, not when an individual client disconnects.
			// Client disconnects are detected via write errors on reader.Send(), which returns false.

			// Handle errors
			if chunk.KoutakuError != nil {
				var errorResponse interface{}

				// Use stream error converter if available, otherwise fallback to regular error converter
				if config.StreamConfig != nil && config.StreamConfig.ErrorConverter != nil {
					errorResponse = config.StreamConfig.ErrorConverter(koutakuCtx, chunk.KoutakuError)
				} else if config.ErrorConverter != nil {
					errorResponse = config.ErrorConverter(koutakuCtx, chunk.KoutakuError)
				} else {
					// Default error response
					errorResponse = map[string]interface{}{
						"error": map[string]interface{}{
							"type":    "internal_error",
							"message": "An error occurred while processing your request",
						},
					}
				}

				// Check if the error converter returned a raw SSE string or JSON object
				if sseErrorString, ok := errorResponse.(string); ok {
					// CUSTOM SSE FORMAT: The converter returned a complete SSE string
					// This is used by providers like Anthropic that need custom event types
					reader.Send([]byte(sseErrorString))
				} else {
					// STANDARD SSE FORMAT: The converter returned an object
					errorJSON, err := sonic.Marshal(errorResponse)
					if err != nil {
						// Fallback to basic error if marshaling fails
						basicError := map[string]interface{}{
							"error": map[string]interface{}{
								"type":    "internal_error",
								"message": "An error occurred while processing your request",
							},
						}
						if errorJSON, err = sonic.Marshal(basicError); err != nil {
							cancel()
							return
						}
					}

					// Send error as SSE data
					reader.SendEvent("", errorJSON)
				}

				return // End stream on error, Koutaku handles cleanup internally
			} else {
				// Allow plugins to modify/filter the chunk via StreamChunkInterceptor
				if interceptor != nil {
					var err error
					chunk, err = interceptor.InterceptChunk(koutakuCtx, httpReq, chunk)
					if err != nil {
						if chunk == nil {
							errorJSON, marshalErr := sonic.Marshal(map[string]string{"error": err.Error()})
							if marshalErr != nil {
								cancel()
								return
							}
							// Return error event and stop streaming
							reader.SendError(errorJSON)
							cancel()
							return
						}
						// Else add warn log and continue
						g.logger.Warn("%v", err)
					}
					if chunk == nil {
						// Skip chunk if plugin wants to skip it
						continue
					}
				}
				// Handle successful responses
				// Convert response to integration-specific streaming format
				var eventType string
				var convertedResponse interface{}
				var err error

				switch {
				case chunk.KoutakuTextCompletionResponse != nil:
					eventType, convertedResponse, err = config.StreamConfig.TextStreamResponseConverter(koutakuCtx, chunk.KoutakuTextCompletionResponse)
				case chunk.KoutakuChatResponse != nil:
					eventType, convertedResponse, err = config.StreamConfig.ChatStreamResponseConverter(koutakuCtx, chunk.KoutakuChatResponse)
				case chunk.KoutakuResponsesStreamResponse != nil:
					eventType, convertedResponse, err = config.StreamConfig.ResponsesStreamResponseConverter(koutakuCtx, chunk.KoutakuResponsesStreamResponse)
				case chunk.KoutakuSpeechStreamResponse != nil:
					eventType, convertedResponse, err = config.StreamConfig.SpeechStreamResponseConverter(koutakuCtx, chunk.KoutakuSpeechStreamResponse)
				case chunk.KoutakuTranscriptionStreamResponse != nil:
					eventType, convertedResponse, err = config.StreamConfig.TranscriptionStreamResponseConverter(koutakuCtx, chunk.KoutakuTranscriptionStreamResponse)
				case chunk.KoutakuImageGenerationStreamResponse != nil:
					eventType, convertedResponse, err = config.StreamConfig.ImageGenerationStreamResponseConverter(koutakuCtx, chunk.KoutakuImageGenerationStreamResponse)
				default:
					requestType := safeGetRequestType(chunk)
					convertedResponse, err = nil, fmt.Errorf("no response converter found for request type: %s", requestType)
				}

				if convertedResponse == nil && err == nil {
					// Skip streaming chunk if no response is available and no error is returned
					continue
				}

				if err != nil {
					// Log conversion error but continue processing
					g.logger.Warn("Failed to convert streaming response: %v", err)
					continue
				}

				// Handle Bedrock Event Stream format
				if config.Type == RouteConfigTypeBedrock && eventStreamEncoder != nil {
					// We need to cast to BedrockStreamEvent to determine event type and structure
					if bedrockEvent, ok := convertedResponse.(*bedrock.BedrockStreamEvent); ok {
						// Convert to sequence of specific Bedrock events
						events := bedrockEvent.ToEncodedEvents()

						// Send all collected events
						for _, evt := range events {
							jsonData, err := sonic.Marshal(evt.Payload)
							if err != nil {
								g.logger.Warn("Failed to marshal bedrock payload: %v", err)
								continue
							}

							headers := eventstream.Headers{
								{
									Name:  ":content-type",
									Value: eventstream.StringValue("application/json"),
								},
								{
									Name:  ":event-type",
									Value: eventstream.StringValue(evt.EventType),
								},
								{
									Name:  ":message-type",
									Value: eventstream.StringValue("event"),
								},
							}

							message := eventstream.Message{
								Headers: headers,
								Payload: jsonData,
							}

							var msgBuf bytes.Buffer
							if err := eventStreamEncoder.Encode(&msgBuf, message); err != nil {
								g.logger.Warn("[Bedrock Stream] Failed to encode message: %v", err)
								cancel()
								return
							}

							if !reader.Send(msgBuf.Bytes()) {
								g.logger.Warn("[Bedrock Stream] Client disconnected")
								cancel()
								return
							}
						}
					}
					// Continue to next chunk (we handled sending internally)
					continue
				}

				// Build and send SSE event
				var buf []byte
				var sent bool
				if sseString, ok := convertedResponse.(string); ok {
					if strings.HasPrefix(sseString, "data: ") || strings.HasPrefix(sseString, "event: ") {
						// Pre-formatted SSE string (e.g. Anthropic custom event types)
						if eventType != "" {
							// Prepend event type line to pre-formatted data
							buf = make([]byte, 0, 7+len(eventType)+1+len(sseString))
							buf = append(buf, "event: "...)
							buf = append(buf, eventType...)
							buf = append(buf, '\n')
							buf = append(buf, sseString...)
							sent = reader.Send(buf)
						} else {
							sent = reader.Send([]byte(sseString))
						}
					} else {
						sent = reader.SendEvent(eventType, []byte(sseString))
					}
				} else {
					responseJSON, err := sonic.Marshal(convertedResponse)
					if err != nil {
						g.logger.Warn("Failed to marshal streaming response: %v", err)
						continue
					}
					sent = reader.SendEvent(eventType, responseJSON)
				}

				if !sent {
					cancel() // Client disconnected, cancel upstream stream
					return
				}
			}
		}

		// Only send the [DONE] marker for plain SSE APIs that expect it.
		// Do NOT send [DONE] for the following cases:
		//   - OpenAI "responses" API and Anthropic messages API: they signal completion by simply closing the stream, not sending [DONE].
		//   - Bedrock: uses AWS Event Stream format rather than SSE with [DONE].
		// Koutaku handles any additional cleanup internally on normal stream completion.
		if shouldSendDoneMarker && config.Type != RouteConfigTypeGenAI && config.Type != RouteConfigTypeBedrock {
			if !reader.SendDone() {
				g.logger.Warn("Failed to write SSE done marker: client disconnected")
				cancel()
				return
			}
		}
	}()
}

// extractPassthroughModel extracts the model from the passthrough request path and/or body.
// Path patterns: models/{model}, models/{model}:suffix (GenAI), .../models/{model} (Vertex), tunedModels/{model}.
// Body is pre-parsed by parsePassthroughBody to avoid redundant unmarshaling.
func extractPassthroughModel(path string, bodyModel string) string {
	if model := extractModelFromPath(path); model != "" {
		return model
	}
	return bodyModel
}

func extractModelFromPath(path string) string {
	path = strings.TrimPrefix(path, "/")
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "models" || p == "tunedModels" {
			if i+1 < len(parts) {
				model := parts[i+1]
				// Strip :suffix for GenAI (e.g. :generateContent, :streamGenerateContent)
				if idx := strings.Index(model, ":"); idx > 0 {
					model = model[:idx]
				}
				return strings.TrimSpace(model)
			}
			break
		}
	}
	return ""
}

// parsePassthroughBody extracts model and streaming flag from the request body in a
// single unmarshal pass. Pass the raw Content-Type header value so multipart boundaries
// are resolved from the header rather than scraped from the body bytes.
func parsePassthroughBody(contentType string, body []byte) (model string, isStream bool) {
	if len(body) == 0 {
		return
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err == nil && strings.HasPrefix(mediaType, "multipart/") {
		if boundary := params["boundary"]; boundary != "" {
			return parseMultipartPassthroughBody(body, boundary)
		}
	}
	// JSON (or unknown) body — one unmarshal for both fields.
	var parsed struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := sonic.Unmarshal(body, &parsed); err == nil {
		model = strings.TrimSpace(parsed.Model)
		isStream = parsed.Stream
	}
	return
}

// parseMultipartPassthroughBody scans multipart parts and extracts model and stream.
//   - Form fields (Content-Disposition name="model"/"stream"): read as plain text.
func parseMultipartPassthroughBody(body []byte, boundary string) (model string, isStream bool) {
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		// Plain form field — check by name.
		switch part.FormName() {
		case "model":
			val, _ := io.ReadAll(part)
			part.Close()
			model = strings.TrimSpace(string(val))
		case "stream":
			val, _ := io.ReadAll(part)
			part.Close()
			s := strings.TrimSpace(strings.ToLower(string(val)))
			isStream = s == "true" || s == "1"
		default:
			part.Close()
		}

		if model != "" && isStream {
			break
		}
	}
	return
}

func (g *GenericRouter) handlePassthrough(ctx *fasthttp.RequestCtx) {
	cfg := g.passthroughCfg

	safeHeaders := make(map[string]string)
	ctx.Request.Header.All()(func(key, value []byte) bool {
		keyStr := strings.ToLower(string(key))
		switch keyStr {
		case "authorization", "api-key", "x-api-key", "x-goog-api-key",
			"host", "connection", "transfer-encoding", "cookie", "set-cookie", "proxy-authorization", "accept-encoding":
		default:
			if strings.HasPrefix(keyStr, "x-bf-") {
				return true // drop internal gateway headers
			}
			safeHeaders[keyStr] = string(value)
		}
		return true
	})

	koutakuCtx, cancel := lib.ConvertToKoutakuContext(ctx, g.handlerStore.ShouldAllowDirectKeys(), g.handlerStore.GetHeaderMatcher(), g.handlerStore.GetMCPHeaderCombinedAllowlist())
	if directKey := ctx.UserValue(string(schemas.KoutakuContextKeyDirectKey)); directKey != nil {
		if key, ok := directKey.(schemas.Key); ok {
			koutakuCtx.SetValue(schemas.KoutakuContextKeyDirectKey, key)
		}
	}

	path := string(ctx.Path())
	for _, prefix := range g.passthroughCfg.StripPrefix {
		if strings.HasPrefix(path, prefix) {
			path = path[len(prefix):]
			break
		}
	}

	body := ctx.Request.Body()
	// Parse body once to get both model and stream flag.
	contentType := string(ctx.Request.Header.ContentType())
	bodyModel, bodyStream := parsePassthroughBody(contentType, body)
	resolvedModel := extractPassthroughModel(path, bodyModel)
	provider := cfg.Provider
	if cfg.ProviderDetector != nil {
		provider = cfg.ProviderDetector(ctx, resolvedModel)
	}
	provider = getProviderFromHeader(ctx, provider)
	isStreaming := strings.Contains(strings.ToLower(path), "stream") || bodyStream

	passthroughReq := &schemas.KoutakuPassthroughRequest{
		Method:      string(ctx.Method()),
		Path:        path,
		RawQuery:    string(ctx.URI().QueryString()),
		Body:        body,
		SafeHeaders: safeHeaders,
		Provider:    provider,
		Model:       resolvedModel,
	}

	if isStreaming {
		g.handlePassthroughStream(ctx, koutakuCtx, cancel, provider, passthroughReq)
	} else {
		g.handlePassthroughNonStream(ctx, koutakuCtx, cancel, provider, passthroughReq)
	}
}

func (g *GenericRouter) handlePassthroughNonStream(
	ctx *fasthttp.RequestCtx,
	koutakuCtx *schemas.KoutakuContext,
	cancel context.CancelFunc,
	provider schemas.ModelProvider,
	req *schemas.KoutakuPassthroughRequest,
) {
	defer cancel()

	resp, koutakuErr := g.client.Passthrough(koutakuCtx, provider, req)
	if koutakuErr != nil {
		g.sendError(ctx, koutakuCtx, func(_ *schemas.KoutakuContext, err *schemas.KoutakuError) interface{} {
			return err
		}, koutakuErr)
		return
	}

	ctx.SetStatusCode(resp.StatusCode)
	for k, v := range resp.Headers {
		switch strings.ToLower(k) {
		case "connection", "transfer-encoding", "set-cookie", "proxy-authenticate", "www-authenticate":
			// drop
		default:
			ctx.Response.Header.Set(k, v)
		}
	}
	ctx.Response.SetBody(resp.Body)
}

func (g *GenericRouter) handlePassthroughStream(
	ctx *fasthttp.RequestCtx,
	koutakuCtx *schemas.KoutakuContext,
	cancel context.CancelFunc,
	provider schemas.ModelProvider,
	req *schemas.KoutakuPassthroughRequest,
) {
	// Deferred trace completion must be explicitly finalized after streaming ends.
	// Without this, observability injectors (including logging) never flush final state.
	traceCompleter, _ := ctx.UserValue(schemas.KoutakuContextKeyTraceCompleter).(func([]schemas.PluginLogEntry))

	stream, koutakuErr := g.client.PassthroughStream(koutakuCtx, provider, req)
	if koutakuErr != nil {
		cancel()
		g.sendError(ctx, koutakuCtx, func(_ *schemas.KoutakuContext, err *schemas.KoutakuError) interface{} {
			return err
		}, koutakuErr)
		return
	}

	// Read the first chunk to extract status code and headers before streaming begins.
	firstChunk, ok := <-stream
	if !ok {
		cancel()
		g.sendError(ctx, koutakuCtx, func(_ *schemas.KoutakuContext, err *schemas.KoutakuError) interface{} {
			return err
		}, newKoutakuError(nil, "passthrough stream ended before headers were received"))
		return
	}
	if firstChunk == nil {
		cancel()
		g.sendError(ctx, koutakuCtx, func(_ *schemas.KoutakuContext, err *schemas.KoutakuError) interface{} {
			return err
		}, newKoutakuError(nil, "passthrough stream returned nil first chunk"))
		return
	}
	if firstChunk.KoutakuError != nil {
		cancel()
		g.sendError(ctx, koutakuCtx, func(_ *schemas.KoutakuContext, err *schemas.KoutakuError) interface{} {
			return err
		}, firstChunk.KoutakuError)
		return
	}

	passthroughResp := firstChunk.KoutakuPassthroughResponse
	if passthroughResp == nil {
		cancel()
		g.sendError(ctx, koutakuCtx, func(_ *schemas.KoutakuContext, err *schemas.KoutakuError) interface{} {
			return err
		}, newKoutakuError(nil, "passthrough stream returned empty first chunk"))
		return
	}

	// Skip post-hook body materialization — ctx.Response.Body() would buffer the entire stream.
	ctx.SetUserValue(schemas.KoutakuContextKeyDeferTraceCompletion, true)

	ctx.SetStatusCode(passthroughResp.StatusCode)
	ctx.SetConnectionClose()
	for k, v := range passthroughResp.Headers {
		switch strings.ToLower(k) {
		case "connection", "transfer-encoding", "content-length", "set-cookie", "proxy-authenticate", "www-authenticate":
			// drop — content-length is invalid for a streaming response
		default:
			ctx.Response.Header.Set(k, v)
		}
	}

	// Use SSEStreamReader to bypass fasthttp's internal pipe batching
	reader := lib.NewSSEStreamReader()
	ctx.Response.SetBodyStream(reader, -1)

	go func() {
		defer func() {
			if traceCompleter != nil {
				traceCompleter(nil)
			}
			reader.Done()
			cancel()
		}()

		// Write the first chunk's data.
		if len(passthroughResp.Body) > 0 {
			if !reader.Send(passthroughResp.Body) {
				cancel()
				return
			}
		}

		for chunk := range stream {
			if chunk == nil {
				continue
			}
			if chunk.KoutakuError != nil {
				break
			}
			if chunk.KoutakuPassthroughResponse != nil && len(chunk.KoutakuPassthroughResponse.Body) > 0 {
				if !reader.Send(chunk.KoutakuPassthroughResponse.Body) {
					cancel()
					return
				}
			}
		}
	}()
}
