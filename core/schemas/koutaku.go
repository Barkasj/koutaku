// Package schemas defines the core schemas and types used by the Koutaku system.
package schemas

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

const (
	DefaultInitialPoolSize = 5000
)

type KeySelector func(ctx *KoutakuContext, keys []Key, providerKey ModelProvider, model string) (Key, error)

// KoutakuConfig represents the configuration for initializing a Koutaku instance.
// It contains the necessary components for setting up the system including account details,
// plugins, logging, and initial pool size.
type KoutakuConfig struct {
	Account            Account
	LLMPlugins         []LLMPlugin
	MCPPlugins         []MCPPlugin
	OAuth2Provider     OAuth2Provider
	Logger             Logger
	Tracer             Tracer      // Tracer for distributed tracing (nil = NoOpTracer)
	InitialPoolSize    int         // Initial pool size for sync pools in Koutaku. Higher values will reduce memory allocations but will increase memory usage.
	DropExcessRequests bool        // If true, in cases where the queue is full, requests will not wait for the queue to be empty and will be dropped instead.
	MCPConfig          *MCPConfig  // MCP (Model Context Protocol) configuration for tool integration
	KeySelector        KeySelector // Custom key selector function
	KVStore            KVStore     // shared KV store for clustering/session stickiness; nil = disabled
}

// ModelProvider represents the different AI model providers supported by Koutaku.
type ModelProvider string

const (
	OpenAI      ModelProvider = "openai"
	Azure       ModelProvider = "azure"
	Anthropic   ModelProvider = "anthropic"
	Bedrock     ModelProvider = "bedrock"
	Cohere      ModelProvider = "cohere"
	Vertex      ModelProvider = "vertex"
	Mistral     ModelProvider = "mistral"
	Ollama      ModelProvider = "ollama"
	Groq        ModelProvider = "groq"
	SGL         ModelProvider = "sgl"
	Parasail    ModelProvider = "parasail"
	Perplexity  ModelProvider = "perplexity"
	Cerebras    ModelProvider = "cerebras"
	Gemini      ModelProvider = "gemini"
	OpenRouter  ModelProvider = "openrouter"
	Elevenlabs  ModelProvider = "elevenlabs"
	HuggingFace ModelProvider = "huggingface"
	Nebius      ModelProvider = "nebius"
	XAI         ModelProvider = "xai"
	Replicate   ModelProvider = "replicate"
	VLLM        ModelProvider = "vllm"
	Runway      ModelProvider = "runway"
	Fireworks   ModelProvider = "fireworks"
)

// SupportedBaseProviders is the list of base providers allowed for custom providers.
var SupportedBaseProviders = []ModelProvider{
	Anthropic,
	Bedrock,
	Cohere,
	Gemini,
	OpenAI,
	HuggingFace,
	Replicate,
}

// StandardProviders is the list of all built-in (non-custom) providers.
var StandardProviders = []ModelProvider{
	Anthropic,
	Azure,
	Bedrock,
	Cerebras,
	Cohere,
	Gemini,
	Groq,
	Mistral,
	Ollama,
	OpenAI,
	Parasail,
	Perplexity,
	SGL,
	Vertex,
	OpenRouter,
	Elevenlabs,
	HuggingFace,
	Nebius,
	XAI,
	Replicate,
	VLLM,
	Runway,
	Fireworks,
}

// RequestType represents the type of request being made to a provider.
type RequestType string

const (
	ListModelsRequest            RequestType = "list_models"
	TextCompletionRequest        RequestType = "text_completion"
	TextCompletionStreamRequest  RequestType = "text_completion_stream"
	ChatCompletionRequest        RequestType = "chat_completion"
	ChatCompletionStreamRequest  RequestType = "chat_completion_stream"
	ResponsesRequest             RequestType = "responses"
	ResponsesStreamRequest       RequestType = "responses_stream"
	EmbeddingRequest             RequestType = "embedding"
	SpeechRequest                RequestType = "speech"
	SpeechStreamRequest          RequestType = "speech_stream"
	TranscriptionRequest         RequestType = "transcription"
	TranscriptionStreamRequest   RequestType = "transcription_stream"
	ImageGenerationRequest       RequestType = "image_generation"
	ImageGenerationStreamRequest RequestType = "image_generation_stream"
	ImageEditRequest             RequestType = "image_edit"
	ImageEditStreamRequest       RequestType = "image_edit_stream"
	ImageVariationRequest        RequestType = "image_variation"
	VideoGenerationRequest       RequestType = "video_generation"
	VideoRetrieveRequest         RequestType = "video_retrieve"
	VideoDownloadRequest         RequestType = "video_download"
	VideoDeleteRequest           RequestType = "video_delete"
	VideoListRequest             RequestType = "video_list"
	VideoRemixRequest            RequestType = "video_remix"
	BatchCreateRequest           RequestType = "batch_create"
	BatchListRequest             RequestType = "batch_list"
	BatchRetrieveRequest         RequestType = "batch_retrieve"
	BatchCancelRequest           RequestType = "batch_cancel"
	BatchResultsRequest          RequestType = "batch_results"
	BatchDeleteRequest           RequestType = "batch_delete"
	FileUploadRequest            RequestType = "file_upload"
	FileListRequest              RequestType = "file_list"
	FileRetrieveRequest          RequestType = "file_retrieve"
	FileDeleteRequest            RequestType = "file_delete"
	FileContentRequest           RequestType = "file_content"
	ContainerCreateRequest       RequestType = "container_create"
	ContainerListRequest         RequestType = "container_list"
	ContainerRetrieveRequest     RequestType = "container_retrieve"
	ContainerDeleteRequest       RequestType = "container_delete"
	ContainerFileCreateRequest   RequestType = "container_file_create"
	ContainerFileListRequest     RequestType = "container_file_list"
	ContainerFileRetrieveRequest RequestType = "container_file_retrieve"
	ContainerFileContentRequest  RequestType = "container_file_content"
	ContainerFileDeleteRequest   RequestType = "container_file_delete"
	RerankRequest                RequestType = "rerank"
	OCRRequest                   RequestType = "ocr"
	CountTokensRequest           RequestType = "count_tokens"
	MCPToolExecutionRequest      RequestType = "mcp_tool_execution"
	PassthroughRequest           RequestType = "passthrough"
	PassthroughStreamRequest     RequestType = "passthrough_stream"
	UnknownRequest               RequestType = "unknown"
	WebSocketResponsesRequest    RequestType = "websocket_responses"
	RealtimeRequest              RequestType = "realtime"
)

// KoutakuContextKey is a type for context keys used in Koutaku.
type KoutakuContextKey string

// KoutakuContextKeyRequestType is a context key for the request type.
const (
	KoutakuContextKeySessionToken      KoutakuContextKey = "koutaku-session-token" // string (session token for authentication - set by auth middleware)
	KoutakuContextKeyVirtualKey        KoutakuContextKey = "x-bf-vk"               // string
	KoutakuContextKeyAPIKeyName        KoutakuContextKey = "x-bf-api-key"          // string (explicit key name selection)
	KoutakuContextKeyAPIKeyID          KoutakuContextKey = "x-bf-api-key-id"       // string (explicit key ID selection, takes priority over name)
	KoutakuContextKeyRequestID         KoutakuContextKey = "request-id"            // string
	KoutakuContextKeyFallbackRequestID KoutakuContextKey = "fallback-request-id"   // string
	KoutakuContextKeyDirectKey         KoutakuContextKey = "koutaku-direct-key"    // Key struct

	// NOTE: []string is used for both keys, and by default all clients/tools are included (when nil).
	// If "*" is present, all clients/tools are included, and [] means no clients/tools are included.
	// Request context filtering takes priority over client config - context can override client exclusions.
	MCPContextKeyIncludeClients KoutakuContextKey = "mcp-include-clients" // Context key for whitelist client filtering
	MCPContextKeyIncludeTools   KoutakuContextKey = "mcp-include-tools"   // Context key for whitelist tool filtering (Note: toolName should be in "clientName-toolName" format for individual tools, or "clientName-*" for wildcard)

	KoutakuContextKeySelectedKeyID                       KoutakuContextKey = "koutaku-selected-key-id"               // string (to store the selected key ID (set by koutaku governance plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeySelectedKeyName                     KoutakuContextKey = "koutaku-selected-key-name"             // string (to store the selected key name (set by koutaku governance plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyGovernanceVirtualKeyID              KoutakuContextKey = "koutaku-governance-virtual-key-id"     // string (to store the virtual key ID (set by koutaku governance plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyGovernanceVirtualKeyName            KoutakuContextKey = "koutaku-governance-virtual-key-name"   // string (to store the virtual key name (set by koutaku governance plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyGovernanceTeamID                    KoutakuContextKey = "koutaku-governance-team-id"            // string (to store the team ID (set by koutaku governance plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyGovernanceTeamName                  KoutakuContextKey = "koutaku-governance-team-name"          // string (to store the team name (set by koutaku governance plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyGovernanceCustomerID                KoutakuContextKey = "koutaku-governance-customer-id"        // string (to store the customer ID (set by koutaku governance plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyGovernanceCustomerName              KoutakuContextKey = "koutaku-governance-customer-name"      // string (to store the customer name (set by koutaku governance plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyGovernanceBusinessUnitID            KoutakuContextKey = "koutaku-governance-business-unit-id"   // string (to store the business unit ID (set by enterprise governance plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyGovernanceBusinessUnitName          KoutakuContextKey = "koutaku-governance-business-unit-name" // string (to store the business unit name (set by enterprise governance plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyGovernanceRoutingRuleID             KoutakuContextKey = "koutaku-governance-routing-rule-id"    // string (to store the routing rule ID (set by koutaku governance plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyGovernanceRoutingRuleName           KoutakuContextKey = "koutaku-governance-routing-rule-name"  // string (to store the routing rule name (set by koutaku governance plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeySelectedPromptName                  KoutakuContextKey = "koutaku-selected-prompt-name"          // string (display name of the selected prompt (set by prompts plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeySelectedPromptVersion               KoutakuContextKey = "koutaku-selected-prompt-version"       // string (numeric version as string, e.g. "3" (set by prompts plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeySelectedPromptID                    KoutakuContextKey = "koutaku-selected-prompt-id"            // string (id of the selected prompt (set by prompts plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyGovernanceIncludeOnlyKeys           KoutakuContextKey = "bf-governance-include-only-keys"       // []string (to store the include-only key IDs for provider config routing (set by koutaku governance plugin - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyNumberOfRetries                     KoutakuContextKey = "koutaku-number-of-retries"             // int (to store the number of retries (set by koutaku - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyFallbackIndex                       KoutakuContextKey = "koutaku-fallback-index"                // int (to store the fallback index (set by koutaku - DO NOT SET THIS MANUALLY)) 0 for primary, 1 for first fallback, etc.
	KoutakuContextKeyStreamEndIndicator                  KoutakuContextKey = "koutaku-stream-end-indicator"          // bool (set by koutaku - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyStreamIdleTimeout                   KoutakuContextKey = "koutaku-stream-idle-timeout"           // time.Duration (per-chunk idle timeout for streaming)
	KoutakuContextKeySkipKeySelection                    KoutakuContextKey = "koutaku-skip-key-selection"            // bool (will pass an empty key to the provider)
	KoutakuContextKeyExtraHeaders                        KoutakuContextKey = "koutaku-extra-headers"                 // map[string][]string
	KoutakuContextKeyURLPath                             KoutakuContextKey = "koutaku-extra-url-path"                // string
	KoutakuContextKeyUseRawRequestBody                   KoutakuContextKey = "koutaku-use-raw-request-body"
	KoutakuContextKeyChangeRequestType                   KoutakuContextKey = "koutaku-change-request-type"                      // RequestType (set by plugins to trigger request type conversion in core, e.g. text->chat or chat->responses)
	KoutakuContextKeySendBackRawRequest                  KoutakuContextKey = "koutaku-send-back-raw-request"                    // bool (per-request override — read by koutaku.go, never overwritten)
	KoutakuContextKeySendBackRawResponse                 KoutakuContextKey = "koutaku-send-back-raw-response"                   // bool (per-request override — read by koutaku.go, never overwritten)
	KoutakuContextKeyIntegrationType                     KoutakuContextKey = "koutaku-integration-type"                         // integration used in gateway (e.g. openai, anthropic, bedrock, etc.)
	KoutakuContextKeyIsResponsesToChatCompletionFallback KoutakuContextKey = "koutaku-is-responses-to-chat-completion-fallback" // bool (set by koutaku - DO NOT SET THIS MANUALLY))
	KoutakuMCPAgentOriginalRequestID                     KoutakuContextKey = "koutaku-mcp-agent-original-request-id"            // string (to store the original request ID for MCP agent mode)
	KoutakuContextKeyParentMCPRequestID                  KoutakuContextKey = "bf-parent-mcp-request-id"                         // string (parent request ID for nested tool calls from executeCode)
	KoutakuContextKeyStructuredOutputToolName            KoutakuContextKey = "koutaku-structured-output-tool-name"              // string (to store the name of the structured output tool (set by koutaku))
	KoutakuContextKeyUserAgent                           KoutakuContextKey = "koutaku-user-agent"                               // string (set by koutaku)
	KoutakuContextKeyTraceID                             KoutakuContextKey = "koutaku-trace-id"                                 // string (trace ID for distributed tracing - set by tracing middleware)
	KoutakuContextKeySpanID                              KoutakuContextKey = "koutaku-span-id"                                  // string (current span ID for child span creation - set by tracer)
	KoutakuContextKeyParentSpanID                        KoutakuContextKey = "koutaku-parent-span-id"                           // string (parent span ID from W3C traceparent header - set by tracing middleware)
	KoutakuContextKeyStreamStartTime                     KoutakuContextKey = "koutaku-stream-start-time"                        // time.Time (start time for streaming TTFT calculation - set by koutaku)
	KoutakuContextKeyTracer                              KoutakuContextKey = "koutaku-tracer"                                   // Tracer (tracer instance for completing deferred spans - set by koutaku)
	KoutakuContextKeyDeferTraceCompletion                KoutakuContextKey = "koutaku-defer-trace-completion"                   // bool (signals trace completion should be deferred for streaming - set by streaming handlers)
	KoutakuContextKeyTraceCompleter                      KoutakuContextKey = "koutaku-trace-completer"                          // func([]PluginLogEntry) (callback to complete trace after streaming, receives transport plugin logs - set by tracing middleware)
	KoutakuContextKeyAccumulatorID                       KoutakuContextKey = "koutaku-accumulator-id"                           // string (ID for streaming accumulator lookup - set by tracer for accumulator operations)
	KoutakuContextKeyMCPUserSession                      KoutakuContextKey = "koutaku-mcp-user-session"                         // string (per-user OAuth session token, automatically generated by koutaku)
	KoutakuContextKeyMCPUserID                           KoutakuContextKey = "koutaku-mcp-user-id"                              // string (per-user OAuth user identifier from X-Bf-User-Id header)
	KoutakuContextKeyOAuthRedirectURI                    KoutakuContextKey = "koutaku-oauth-redirect-uri"                       // string (OAuth callback URL, e.g. https://host/api/oauth/callback - set by HTTP middleware)
	KoutakuContextKeyIsMCPGateway                        KoutakuContextKey = "koutaku-is-mcp-gateway"                           // bool (true when request is being handled via the MCP gateway path)
	KoutakuContextKeyHasEmittedMessageDelta              KoutakuContextKey = "koutaku-has-emitted-message-delta"                // bool (tracks whether message_delta was already emitted during streaming - avoids duplicates)
	KoutakuContextKeySkipDBUpdate                        KoutakuContextKey = "koutaku-skip-db-update"                           // bool (set by koutaku - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyGovernancePluginName                KoutakuContextKey = "governance-plugin-name"                           // string (name of the governance plugin that processed the request - set by koutaku)
	KoutakuContextKeyPromptsPluginName                   KoutakuContextKey = "prompts-plugin-name"                              // string (name of the prompts plugin to use - set by koutaku - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyIsEnterprise                        KoutakuContextKey = "is-enterprise"                                    // bool (set by koutaku - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyAvailableProviders                  KoutakuContextKey = "available-providers"                              // []ModelProvider (set by koutaku - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyStoreRawRequestResponse             KoutakuContextKey = "koutaku-store-raw-request-response"               // bool (per-request override — read by koutaku.go, never overwritten)
	KoutakuContextKeyCaptureRawRequest                   KoutakuContextKey = "koutaku-capture-raw-request"                      // bool (set by koutaku - DO NOT SET THIS MANUALLY) — true when providers should capture raw request bytes
	KoutakuContextKeyCaptureRawResponse                  KoutakuContextKey = "koutaku-capture-raw-response"                     // bool (set by koutaku - DO NOT SET THIS MANUALLY) — true when providers should capture raw response bytes
	KoutakuContextKeyDropRawRequestFromClient            KoutakuContextKey = "koutaku-drop-raw-request-from-client"             // bool (set by koutaku - DO NOT SET THIS MANUALLY) — true when raw request should be stripped from the client-facing response
	KoutakuContextKeyDropRawResponseFromClient           KoutakuContextKey = "koutaku-drop-raw-response-from-client"            // bool (set by koutaku - DO NOT SET THIS MANUALLY) — true when raw response should be stripped from the client-facing response
	KoutakuContextKeyShouldStoreRawInLogs                KoutakuContextKey = "koutaku-should-store-raw-in-logs"                 // bool (set by koutaku - DO NOT SET THIS MANUALLY) — true when raw request/response should be persisted in log records
	KoutakuContextKeyRetryDBFetch                        KoutakuContextKey = "koutaku-retry-db-fetch"                           // bool (set by koutaku - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyIsCustomProvider                    KoutakuContextKey = "koutaku-is-custom-provider"                       // bool (set by koutaku - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyHTTPRequestType                     KoutakuContextKey = "koutaku-http-request-type"                        // RequestType (set by koutaku - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyPassthroughExtraParams              KoutakuContextKey = "koutaku-passthrough-extra-params"                 // bool
	KoutakuContextKeyRoutingEnginesUsed                  KoutakuContextKey = "koutaku-routing-engines-used"                     // []string (set by koutaku - DO NOT SET THIS MANUALLY) - list of routing engines used ("routing-rule", "governance", "loadbalancing", etc.)
	KoutakuContextKeyRoutingEngineLogs                   KoutakuContextKey = "koutaku-routing-engine-logs"                      // []RoutingEngineLogEntry (set by koutaku - DO NOT SET THIS MANUALLY) - list of routing engine log entries
	KoutakuContextKeyTransportPluginLogs                 KoutakuContextKey = "koutaku-transport-plugin-logs"                    // []PluginLogEntry (transport-layer plugin logs accumulated during HTTP transport hooks)
	KoutakuContextKeyTransportPostHookCompleter          KoutakuContextKey = "koutaku-transport-posthook-completer"             // func() (callback to run HTTPTransportPostHook after streaming - set by transport interceptor middleware)
	KoutakuContextKeySkipPluginPipeline                  KoutakuContextKey = "koutaku-skip-plugin-pipeline"                     // bool - skip plugin pipeline for the request
	KoutakuContextKeyParentRequestID                     KoutakuContextKey = "koutaku-parent-request-id"                        // string (parent linkage for grouped request logs like realtime turns)
	KoutakuContextKeyRealtimeSessionID                   KoutakuContextKey = "koutaku-realtime-session-id"                      // string
	KoutakuContextKeyRealtimeProviderSessionID           KoutakuContextKey = "koutaku-realtime-provider-session-id"             // string
	KoutakuContextKeyRealtimeSource                      KoutakuContextKey = "koutaku-realtime-source"                          // string ("ei" or "lm")
	KoutakuContextKeyRealtimeEventType                   KoutakuContextKey = "koutaku-realtime-event-type"                      // string
	KoutakuIsAsyncRequest                                KoutakuContextKey = "koutaku-is-async-request"                         // bool (set by koutaku - DO NOT SET THIS MANUALLY)) - whether the request is an async request (only used in gateway)
	KoutakuContextKeyRequestHeaders                      KoutakuContextKey = "koutaku-request-headers"                          // map[string]string (all request headers with lowercased keys)
	KoutakuContextKeySkipListModelsGovernanceFiltering   KoutakuContextKey = "koutaku-skip-list-models-governance-filtering"    // bool (set by koutaku - DO NOT SET THIS MANUALLY))
	KoutakuContextKeySCIMClaims                          KoutakuContextKey = "scim_claims"
	KoutakuContextKeyUserID                              KoutakuContextKey = "koutaku-user-id"   // string (to store the user ID (set by enterprise auth middleware - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyUserName                            KoutakuContextKey = "koutaku-user-name" // string (to store the user name (set by enterprise auth middleware - DO NOT SET THIS MANUALLY))
	KoutakuContextKeyTargetUserID                        KoutakuContextKey = "target_user_id"
	KoutakuContextKeyIsAzureUserAgent                    KoutakuContextKey = "koutaku-is-azure-user-agent" // bool (set by koutaku - DO NOT SET THIS MANUALLY)) - whether the request is an Azure user agent (only used in gateway)
	KoutakuContextKeyVideoOutputRequested                KoutakuContextKey = "koutaku-video-output-requested"
	KoutakuContextKeyValidateKeys                        KoutakuContextKey = "koutaku-validate-keys"                      // bool (triggers additional key validation during provider add/update)
	KoutakuContextKeyProviderResponseHeaders             KoutakuContextKey = "koutaku-provider-response-headers"          // map[string]string (set by provider handlers for response header forwarding)
	KoutakuContextKeyMCPAddedTools                       KoutakuContextKey = "koutaku-mcp-added-tools"                    // []string (set by koutaku - DO NOT SET THIS MANUALLY)) - list of tools added to the request by MCP, all the tool are in the format "clientName-toolName"
	KoutakuContextKeyLargePayloadMode                    KoutakuContextKey = "koutaku-large-payload-mode"                 // bool (set by koutaku - DO NOT SET THIS MANUALLY)) indicates large payload streaming mode is active
	KoutakuContextKeyLargePayloadReader                  KoutakuContextKey = "koutaku-large-payload-reader"               // io.Reader (set by koutaku - DO NOT SET THIS MANUALLY)) upstream reader for large payloads
	KoutakuContextKeyLargePayloadContentLength           KoutakuContextKey = "koutaku-large-payload-content-length"       // int (set by koutaku - DO NOT SET THIS MANUALLY)) content length for large payloads
	KoutakuContextKeyLargePayloadContentType             KoutakuContextKey = "koutaku-large-payload-content-type"         // string (set by enterprise - DO NOT SET THIS MANUALLY)) original content type for large payload passthrough
	KoutakuContextKeyLargePayloadMetadata                KoutakuContextKey = "koutaku-large-payload-metadata"             // *LargePayloadMetadata (set by koutaku - DO NOT SET THIS MANUALLY)) routing metadata for large payloads
	KoutakuContextKeyLargePayloadRequestThreshold        KoutakuContextKey = "koutaku-large-payload-request-threshold"    // int64 (set by enterprise - DO NOT SET THIS MANUALLY)) request threshold used by transport heuristics
	KoutakuContextKeyLargeResponseMode                   KoutakuContextKey = "koutaku-large-response-mode"                // bool (set by koutaku - DO NOT SET THIS MANUALLY)) indicates large response streaming mode is active
	KoutakuContextKeyLargePayloadRequestPreview          KoutakuContextKey = "koutaku-large-payload-request-preview"      // string (set by koutaku - DO NOT SET THIS MANUALLY)) truncated request body preview for logging
	KoutakuContextKeyLargePayloadResponsePreview         KoutakuContextKey = "koutaku-large-payload-response-preview"     // string (set by koutaku - DO NOT SET THIS MANUALLY)) truncated response body preview for logging
	KoutakuContextKeyLargeResponseReader                 KoutakuContextKey = "koutaku-large-response-reader"              // io.ReadCloser (set by koutaku - DO NOT SET THIS MANUALLY)) upstream reader for large responses
	KoutakuContextKeyLargeResponseContentLength          KoutakuContextKey = "koutaku-large-response-content-length"      // int (set by koutaku - DO NOT SET THIS MANUALLY)) content length for large responses
	KoutakuContextKeyLargeResponseContentType            KoutakuContextKey = "koutaku-large-response-content-type"        // string (set by koutaku - DO NOT SET THIS MANUALLY)) upstream content type for large responses
	KoutakuContextKeyLargeResponseContentDisposition     KoutakuContextKey = "koutaku-large-response-content-disposition" // string (set by koutaku - DO NOT SET THIS MANUALLY)) downstream content disposition for large responses
	KoutakuContextKeyLargeResponseThreshold              KoutakuContextKey = "koutaku-large-response-threshold"           // int64 (set by enterprise - DO NOT SET THIS MANUALLY)) threshold for response streaming
	KoutakuContextKeyLargePayloadPrefetchSize            KoutakuContextKey = "koutaku-large-payload-prefetch-size"        // int (set by enterprise - DO NOT SET THIS MANUALLY)) prefetch buffer size for metadata extraction from large responses
	KoutakuContextKeyDeferredUsage                       KoutakuContextKey = "koutaku-deferred-usage"                     // chan *KoutakuLLMUsage (set by provider Phase B — delivers usage after response streaming completes)
	KoutakuContextKeyDeferredLargePayloadMetadata        KoutakuContextKey = "koutaku-deferred-large-payload-metadata"    // <-chan *LargePayloadMetadata (set by enterprise Phase B request — delivers metadata after body streaming)
	KoutakuContextKeySSEReaderFactory                    KoutakuContextKey = "koutaku-sse-reader-factory"                 // *providerUtils.SSEReaderFactory (set by enterprise — replaces default bufio.Scanner SSE readers with streaming readers)
	KoutakuContextKeySessionID                           KoutakuContextKey = "koutaku-session-id"                         // string session ID for the request (session stickiness)
	KoutakuContextKeySessionTTL                          KoutakuContextKey = "koutaku-session-ttl"                        // time.Duration session TTL for the request (session stickiness)
	KoutakuContextKeyMCPExtraHeaders                     KoutakuContextKey = "koutaku-mcp-extra-headers"                  // map[string][]string (these headers are forwarded only to the MCP while tool execution if they are in the allowlist of the MCP client)
	KoutakuContextKeyMCPLogID                            KoutakuContextKey = "koutaku-mcp-log-id"                         // string (unique UUID for each MCP tool log entry - set per goroutine by agent executor - DO NOT SET THIS MANUALLY)
	KoutakuContextKeyCompatConvertTextToChat             KoutakuContextKey = "koutaku-compat-convert-text-to-chat"        // bool (per-request override from x-bf-compat header)
	KoutakuContextKeyCompatConvertChatToResponses        KoutakuContextKey = "koutaku-compat-convert-chat-to-responses"   // bool (per-request override from x-bf-compat header)
	KoutakuContextKeyCompatShouldDropParams              KoutakuContextKey = "koutaku-compat-should-drop-params"          // bool (per-request override from x-bf-compat header)
	KoutakuContextKeyCompatShouldConvertParams           KoutakuContextKey = "koutaku-compat-should-convert-params"       // bool (per-request override from x-bf-compat header)
	KoutakuContextKeyAttemptTrail                        KoutakuContextKey = "koutaku-attempt-trail"                      // []KeyAttemptRecord (set by koutaku - DO NOT SET THIS MANUALLY) - per-attempt key selection history
)

const (
	// DefaultLargePayloadRequestThresholdBytes is the default request-size heuristic
	// used by transport guards when no enterprise threshold is present on context.
	DefaultLargePayloadRequestThresholdBytes = 10 * 1024 * 1024 // 10MB
)

// RoutingEngine constants
const (
	RoutingEngineGovernance    = "governance"
	RoutingEngineRoutingRule   = "routing-rule"
	RoutingEngineLoadbalancing = "loadbalancing"
)

// KeyAttemptRecord captures the outcome of a single request attempt within executeRequestWithRetries.
// One record is appended per attempt regardless of whether the key changed between attempts.
// FailReason is supplementary retry metadata: it is populated only when another retry will be
// attempted (i.e. a non-terminal attempt), and is nil on any terminal attempt — including success,
// non-retryable failure, or a retryable error when no retries remain.
type KeyAttemptRecord struct {
	Attempt    int     `json:"attempt"`
	KeyID      string  `json:"key_id"`
	KeyName    string  `json:"key_name"`
	FailReason *string `json:"fail_reason,omitempty"`
}

// RoutingEngineLogEntry represents a log entry from a routing engine
// format: [timestamp] [engine] - message
type RoutingEngineLogEntry struct {
	Engine    string // e.g., "governance", "routing-rule", "openrouter"
	Message   string // Human-readable decision/action message
	Timestamp int64  // Unix milliseconds
}

// PluginLogEntry represents a structured log entry emitted by a plugin via ctx.Log().
type PluginLogEntry struct {
	PluginName string   `json:"plugin_name"`
	Level      LogLevel `json:"level"`
	Message    string   `json:"message"`
	Timestamp  int64    `json:"timestamp"` // Unix milliseconds
}

// GroupPluginLogsByName groups a flat slice of plugin log entries by plugin name.
// Returns nil if the input is empty.
func GroupPluginLogsByName(logs []PluginLogEntry) map[string][]PluginLogEntry {
	if len(logs) == 0 {
		return nil
	}
	grouped := make(map[string][]PluginLogEntry, min(len(logs), 4))
	for _, entry := range logs {
		grouped[entry.PluginName] = append(grouped[entry.PluginName], entry)
	}
	return grouped
}

// NOTE: for custom plugin implementation dealing with streaming short circuit,
// make sure to mark KoutakuContextKeyStreamEndIndicator as true at the end of the stream.

// LargePayloadMetadata holds routing-relevant metadata selectively extracted from large payloads.
// This is used when the full request body is too large to parse (e.g., 400MB video upload).
// Only small routing/observability fields are extracted; the body itself streams through unchanged.
type LargePayloadMetadata struct {
	ResponseModalities []string // e.g., ["AUDIO"] for speech, ["IMAGE"] for image generation
	SpeechConfig       bool     // true if generationConfig.speechConfig is present
	Model              string   // model extracted without full body parsing (openai/anthropic multipart/json)
	StreamRequested    *bool    // stream flag when available in request payload metadata
}

//* Request Structs

// Fallback represents a fallback model to be used if the primary model is not available.
type Fallback struct {
	Provider ModelProvider `json:"provider"`
	Model    string        `json:"model"`
}

// KoutakuRequest is the request struct for all koutaku requests.
// only ONE of the following fields should be set:
// - ListModelsRequest
// - TextCompletionRequest
// - ChatRequest
// - ResponsesRequest
// - CountTokensRequest
// - EmbeddingRequest
// - RerankRequest
// - SpeechRequest
// - TranscriptionRequest
// - ImageGenerationRequest
// NOTE: Koutaku Request is submitted back to pool after every use so DO NOT keep references to this struct after use, especially in go routines.
type KoutakuRequest struct {
	RequestType RequestType

	ListModelsRequest            *KoutakuListModelsRequest
	TextCompletionRequest        *KoutakuTextCompletionRequest
	ChatRequest                  *KoutakuChatRequest
	ResponsesRequest             *KoutakuResponsesRequest
	CountTokensRequest           *KoutakuResponsesRequest
	EmbeddingRequest             *KoutakuEmbeddingRequest
	RerankRequest                *KoutakuRerankRequest
	OCRRequest                   *KoutakuOCRRequest
	SpeechRequest                *KoutakuSpeechRequest
	TranscriptionRequest         *KoutakuTranscriptionRequest
	ImageGenerationRequest       *KoutakuImageGenerationRequest
	ImageEditRequest             *KoutakuImageEditRequest
	ImageVariationRequest        *KoutakuImageVariationRequest
	VideoGenerationRequest       *KoutakuVideoGenerationRequest
	VideoRetrieveRequest         *KoutakuVideoRetrieveRequest
	VideoDownloadRequest         *KoutakuVideoDownloadRequest
	VideoListRequest             *KoutakuVideoListRequest
	VideoRemixRequest            *KoutakuVideoRemixRequest
	VideoDeleteRequest           *KoutakuVideoDeleteRequest
	FileUploadRequest            *KoutakuFileUploadRequest
	FileListRequest              *KoutakuFileListRequest
	FileRetrieveRequest          *KoutakuFileRetrieveRequest
	FileDeleteRequest            *KoutakuFileDeleteRequest
	FileContentRequest           *KoutakuFileContentRequest
	BatchCreateRequest           *KoutakuBatchCreateRequest
	BatchListRequest             *KoutakuBatchListRequest
	BatchRetrieveRequest         *KoutakuBatchRetrieveRequest
	BatchCancelRequest           *KoutakuBatchCancelRequest
	BatchResultsRequest          *KoutakuBatchResultsRequest
	BatchDeleteRequest           *KoutakuBatchDeleteRequest
	ContainerCreateRequest       *KoutakuContainerCreateRequest
	ContainerListRequest         *KoutakuContainerListRequest
	ContainerRetrieveRequest     *KoutakuContainerRetrieveRequest
	ContainerDeleteRequest       *KoutakuContainerDeleteRequest
	ContainerFileCreateRequest   *KoutakuContainerFileCreateRequest
	ContainerFileListRequest     *KoutakuContainerFileListRequest
	ContainerFileRetrieveRequest *KoutakuContainerFileRetrieveRequest
	ContainerFileContentRequest  *KoutakuContainerFileContentRequest
	ContainerFileDeleteRequest   *KoutakuContainerFileDeleteRequest
	PassthroughRequest           *KoutakuPassthroughRequest
}

// GetRequestFields returns the provider, model, and fallbacks from the request.
func (br *KoutakuRequest) GetRequestFields() (provider ModelProvider, model string, fallbacks []Fallback) {
	switch {
	case br.ListModelsRequest != nil:
		return br.ListModelsRequest.Provider, "", nil
	case br.TextCompletionRequest != nil:
		return br.TextCompletionRequest.Provider, br.TextCompletionRequest.Model, br.TextCompletionRequest.Fallbacks
	case br.ChatRequest != nil:
		return br.ChatRequest.Provider, br.ChatRequest.Model, br.ChatRequest.Fallbacks
	case br.ResponsesRequest != nil:
		return br.ResponsesRequest.Provider, br.ResponsesRequest.Model, br.ResponsesRequest.Fallbacks
	case br.CountTokensRequest != nil:
		return br.CountTokensRequest.Provider, br.CountTokensRequest.Model, br.CountTokensRequest.Fallbacks
	case br.EmbeddingRequest != nil:
		return br.EmbeddingRequest.Provider, br.EmbeddingRequest.Model, br.EmbeddingRequest.Fallbacks
	case br.RerankRequest != nil:
		return br.RerankRequest.Provider, br.RerankRequest.Model, br.RerankRequest.Fallbacks
	case br.OCRRequest != nil:
		return br.OCRRequest.Provider, br.OCRRequest.Model, br.OCRRequest.Fallbacks
	case br.SpeechRequest != nil:
		return br.SpeechRequest.Provider, br.SpeechRequest.Model, br.SpeechRequest.Fallbacks
	case br.TranscriptionRequest != nil:
		return br.TranscriptionRequest.Provider, br.TranscriptionRequest.Model, br.TranscriptionRequest.Fallbacks
	case br.ImageGenerationRequest != nil:
		return br.ImageGenerationRequest.Provider, br.ImageGenerationRequest.Model, br.ImageGenerationRequest.Fallbacks
	case br.ImageEditRequest != nil:
		return br.ImageEditRequest.Provider, br.ImageEditRequest.Model, br.ImageEditRequest.Fallbacks
	case br.ImageVariationRequest != nil:
		return br.ImageVariationRequest.Provider, br.ImageVariationRequest.Model, br.ImageVariationRequest.Fallbacks
	case br.VideoGenerationRequest != nil:
		return br.VideoGenerationRequest.Provider, br.VideoGenerationRequest.Model, br.VideoGenerationRequest.Fallbacks
	case br.VideoRetrieveRequest != nil:
		return br.VideoRetrieveRequest.Provider, "", nil
	case br.VideoDownloadRequest != nil:
		return br.VideoDownloadRequest.Provider, "", nil
	case br.VideoListRequest != nil:
		return br.VideoListRequest.Provider, "", nil
	case br.VideoDeleteRequest != nil:
		return br.VideoDeleteRequest.Provider, "", nil
	case br.VideoRemixRequest != nil:
		return br.VideoRemixRequest.Provider, "", nil
	case br.FileUploadRequest != nil:
		if br.FileUploadRequest.Model != nil {
			return br.FileUploadRequest.Provider, *br.FileUploadRequest.Model, nil
		}
		return br.FileUploadRequest.Provider, "", nil
	case br.FileListRequest != nil:
		if br.FileListRequest.Model != nil {
			return br.FileListRequest.Provider, *br.FileListRequest.Model, nil
		}
		return br.FileListRequest.Provider, "", nil
	case br.FileRetrieveRequest != nil:
		if br.FileRetrieveRequest.Model != nil {
			return br.FileRetrieveRequest.Provider, *br.FileRetrieveRequest.Model, nil
		}
		return br.FileRetrieveRequest.Provider, "", nil
	case br.FileDeleteRequest != nil:
		if br.FileDeleteRequest.Model != nil {
			return br.FileDeleteRequest.Provider, *br.FileDeleteRequest.Model, nil
		}
		return br.FileDeleteRequest.Provider, "", nil
	case br.FileContentRequest != nil:
		if br.FileContentRequest.Model != nil {
			return br.FileContentRequest.Provider, *br.FileContentRequest.Model, nil
		}
		return br.FileContentRequest.Provider, "", nil
	case br.BatchCreateRequest != nil:
		if br.BatchCreateRequest.Model != nil {
			return br.BatchCreateRequest.Provider, *br.BatchCreateRequest.Model, nil
		}
		return br.BatchCreateRequest.Provider, "", nil
	case br.BatchListRequest != nil:
		if br.BatchListRequest.Model != nil {
			return br.BatchListRequest.Provider, *br.BatchListRequest.Model, nil
		}
		return br.BatchListRequest.Provider, "", nil
	case br.BatchRetrieveRequest != nil:
		if br.BatchRetrieveRequest.Model != nil {
			return br.BatchRetrieveRequest.Provider, *br.BatchRetrieveRequest.Model, nil
		}
		return br.BatchRetrieveRequest.Provider, "", nil
	case br.BatchCancelRequest != nil:
		if br.BatchCancelRequest.Model != nil {
			return br.BatchCancelRequest.Provider, *br.BatchCancelRequest.Model, nil
		}
		return br.BatchCancelRequest.Provider, "", nil
	case br.BatchResultsRequest != nil:
		if br.BatchResultsRequest.Model != nil {
			return br.BatchResultsRequest.Provider, *br.BatchResultsRequest.Model, nil
		}
		return br.BatchResultsRequest.Provider, "", nil
	case br.BatchDeleteRequest != nil:
		if br.BatchDeleteRequest.Model != nil {
			return br.BatchDeleteRequest.Provider, *br.BatchDeleteRequest.Model, nil
		}
		return br.BatchDeleteRequest.Provider, "", nil
	case br.ContainerCreateRequest != nil:
		return br.ContainerCreateRequest.Provider, "", nil
	case br.ContainerListRequest != nil:
		return br.ContainerListRequest.Provider, "", nil
	case br.ContainerRetrieveRequest != nil:
		return br.ContainerRetrieveRequest.Provider, "", nil
	case br.ContainerDeleteRequest != nil:
		return br.ContainerDeleteRequest.Provider, "", nil
	case br.ContainerFileCreateRequest != nil:
		return br.ContainerFileCreateRequest.Provider, "", nil
	case br.ContainerFileListRequest != nil:
		return br.ContainerFileListRequest.Provider, "", nil
	case br.ContainerFileRetrieveRequest != nil:
		return br.ContainerFileRetrieveRequest.Provider, "", nil
	case br.ContainerFileContentRequest != nil:
		return br.ContainerFileContentRequest.Provider, "", nil
	case br.ContainerFileDeleteRequest != nil:
		return br.ContainerFileDeleteRequest.Provider, "", nil
	case br.PassthroughRequest != nil:
		return br.PassthroughRequest.Provider, br.PassthroughRequest.Model, nil
	}
	return "", "", nil
}

func (br *KoutakuRequest) SetProvider(provider ModelProvider) {
	switch {
	case br.ListModelsRequest != nil:
		br.ListModelsRequest.Provider = provider
	case br.TextCompletionRequest != nil:
		br.TextCompletionRequest.Provider = provider
	case br.ChatRequest != nil:
		br.ChatRequest.Provider = provider
	case br.ResponsesRequest != nil:
		br.ResponsesRequest.Provider = provider
	case br.CountTokensRequest != nil:
		br.CountTokensRequest.Provider = provider
	case br.EmbeddingRequest != nil:
		br.EmbeddingRequest.Provider = provider
	case br.RerankRequest != nil:
		br.RerankRequest.Provider = provider
	case br.OCRRequest != nil:
		br.OCRRequest.Provider = provider
	case br.SpeechRequest != nil:
		br.SpeechRequest.Provider = provider
	case br.TranscriptionRequest != nil:
		br.TranscriptionRequest.Provider = provider
	case br.ImageGenerationRequest != nil:
		br.ImageGenerationRequest.Provider = provider
	case br.ImageEditRequest != nil:
		br.ImageEditRequest.Provider = provider
	case br.ImageVariationRequest != nil:
		br.ImageVariationRequest.Provider = provider
	case br.VideoGenerationRequest != nil:
		br.VideoGenerationRequest.Provider = provider
	case br.VideoRetrieveRequest != nil:
		br.VideoRetrieveRequest.Provider = provider
	case br.VideoDownloadRequest != nil:
		br.VideoDownloadRequest.Provider = provider
	case br.VideoListRequest != nil:
		br.VideoListRequest.Provider = provider
	case br.VideoDeleteRequest != nil:
		br.VideoDeleteRequest.Provider = provider
	case br.VideoRemixRequest != nil:
		br.VideoRemixRequest.Provider = provider
	}
}

func (br *KoutakuRequest) SetModel(model string) {
	switch {
	case br.TextCompletionRequest != nil:
		br.TextCompletionRequest.Model = model
	case br.ChatRequest != nil:
		br.ChatRequest.Model = model
	case br.ResponsesRequest != nil:
		br.ResponsesRequest.Model = model
	case br.CountTokensRequest != nil:
		br.CountTokensRequest.Model = model
	case br.EmbeddingRequest != nil:
		br.EmbeddingRequest.Model = model
	case br.RerankRequest != nil:
		br.RerankRequest.Model = model
	case br.OCRRequest != nil:
		br.OCRRequest.Model = model
	case br.SpeechRequest != nil:
		br.SpeechRequest.Model = model
	case br.TranscriptionRequest != nil:
		br.TranscriptionRequest.Model = model
	case br.ImageGenerationRequest != nil:
		br.ImageGenerationRequest.Model = model
	case br.ImageEditRequest != nil:
		br.ImageEditRequest.Model = model
	case br.ImageVariationRequest != nil:
		br.ImageVariationRequest.Model = model
	case br.VideoGenerationRequest != nil:
		br.VideoGenerationRequest.Model = model
	case br.BatchCreateRequest != nil:
		if br.BatchCreateRequest.Model != nil {
			br.BatchCreateRequest.Model = new(model)
		}
	}
}

func (br *KoutakuRequest) SetFallbacks(fallbacks []Fallback) {
	switch {
	case br.TextCompletionRequest != nil:
		br.TextCompletionRequest.Fallbacks = fallbacks
	case br.ChatRequest != nil:
		br.ChatRequest.Fallbacks = fallbacks
	case br.ResponsesRequest != nil:
		br.ResponsesRequest.Fallbacks = fallbacks
	case br.CountTokensRequest != nil:
		br.CountTokensRequest.Fallbacks = fallbacks
	case br.EmbeddingRequest != nil:
		br.EmbeddingRequest.Fallbacks = fallbacks
	case br.RerankRequest != nil:
		br.RerankRequest.Fallbacks = fallbacks
	case br.OCRRequest != nil:
		br.OCRRequest.Fallbacks = fallbacks
	case br.SpeechRequest != nil:
		br.SpeechRequest.Fallbacks = fallbacks
	case br.TranscriptionRequest != nil:
		br.TranscriptionRequest.Fallbacks = fallbacks
	case br.ImageGenerationRequest != nil:
		br.ImageGenerationRequest.Fallbacks = fallbacks
	case br.ImageEditRequest != nil:
		br.ImageEditRequest.Fallbacks = fallbacks
	case br.ImageVariationRequest != nil:
		br.ImageVariationRequest.Fallbacks = fallbacks
	case br.VideoGenerationRequest != nil:
		br.VideoGenerationRequest.Fallbacks = fallbacks
	}
}

func (br *KoutakuRequest) SetRawRequestBody(rawRequestBody []byte) {
	switch {
	case br.TextCompletionRequest != nil:
		br.TextCompletionRequest.RawRequestBody = rawRequestBody
	case br.ChatRequest != nil:
		br.ChatRequest.RawRequestBody = rawRequestBody
	case br.ResponsesRequest != nil:
		br.ResponsesRequest.RawRequestBody = rawRequestBody
	case br.CountTokensRequest != nil:
		br.CountTokensRequest.RawRequestBody = rawRequestBody
	case br.EmbeddingRequest != nil:
		br.EmbeddingRequest.RawRequestBody = rawRequestBody
	case br.RerankRequest != nil:
		br.RerankRequest.RawRequestBody = rawRequestBody
	case br.OCRRequest != nil:
		br.OCRRequest.RawRequestBody = rawRequestBody
	case br.SpeechRequest != nil:
		br.SpeechRequest.RawRequestBody = rawRequestBody
	case br.TranscriptionRequest != nil:
		br.TranscriptionRequest.RawRequestBody = rawRequestBody
	case br.ImageGenerationRequest != nil:
		br.ImageGenerationRequest.RawRequestBody = rawRequestBody
	case br.ImageEditRequest != nil:
		br.ImageEditRequest.RawRequestBody = rawRequestBody
	case br.ImageVariationRequest != nil:
		br.ImageVariationRequest.RawRequestBody = rawRequestBody
	case br.VideoGenerationRequest != nil:
		br.VideoGenerationRequest.RawRequestBody = rawRequestBody
	case br.VideoRemixRequest != nil:
		br.VideoRemixRequest.RawRequestBody = rawRequestBody
	}
}

type MCPRequestType string

const (
	MCPRequestTypeChatToolCall      MCPRequestType = "chat_tool_call"      // Chat API format
	MCPRequestTypeResponsesToolCall MCPRequestType = "responses_tool_call" // Responses API format
)

// KoutakuMCPRequest is the request struct for all MCP requests.
// only ONE of the following fields should be set:
// - ChatAssistantMessageToolCall
// - ResponsesToolMessage
type KoutakuMCPRequest struct {
	RequestType MCPRequestType

	*ChatAssistantMessageToolCall
	*ResponsesToolMessage
}

func (r *KoutakuMCPRequest) GetToolName() string {
	if r.ChatAssistantMessageToolCall != nil {
		if r.ChatAssistantMessageToolCall.Function.Name != nil {
			return *r.ChatAssistantMessageToolCall.Function.Name
		}
	}
	if r.ResponsesToolMessage != nil {
		if r.ResponsesToolMessage.Name != nil {
			return *r.ResponsesToolMessage.Name
		}
	}
	return ""
}

func (r *KoutakuMCPRequest) GetToolArguments() interface{} {
	if r.ChatAssistantMessageToolCall != nil {
		return r.ChatAssistantMessageToolCall.Function.Arguments
	}
	if r.ResponsesToolMessage != nil {
		return r.ResponsesToolMessage.Arguments
	}
	return nil
}

//* Response Structs

// KoutakuResponse represents the complete result from any koutaku request.
type KoutakuResponse struct {
	ListModelsResponse            *KoutakuListModelsResponse
	TextCompletionResponse        *KoutakuTextCompletionResponse
	ChatResponse                  *KoutakuChatResponse
	ResponsesResponse             *KoutakuResponsesResponse
	ResponsesStreamResponse       *KoutakuResponsesStreamResponse
	CountTokensResponse           *KoutakuCountTokensResponse
	EmbeddingResponse             *KoutakuEmbeddingResponse
	RerankResponse                *KoutakuRerankResponse
	OCRResponse                   *KoutakuOCRResponse
	SpeechResponse                *KoutakuSpeechResponse
	SpeechStreamResponse          *KoutakuSpeechStreamResponse
	TranscriptionResponse         *KoutakuTranscriptionResponse
	TranscriptionStreamResponse   *KoutakuTranscriptionStreamResponse
	ImageGenerationResponse       *KoutakuImageGenerationResponse
	ImageGenerationStreamResponse *KoutakuImageGenerationStreamResponse
	VideoGenerationResponse       *KoutakuVideoGenerationResponse
	VideoDownloadResponse         *KoutakuVideoDownloadResponse
	VideoListResponse             *KoutakuVideoListResponse
	VideoDeleteResponse           *KoutakuVideoDeleteResponse
	FileUploadResponse            *KoutakuFileUploadResponse
	FileListResponse              *KoutakuFileListResponse
	FileRetrieveResponse          *KoutakuFileRetrieveResponse
	FileDeleteResponse            *KoutakuFileDeleteResponse
	FileContentResponse           *KoutakuFileContentResponse
	BatchCreateResponse           *KoutakuBatchCreateResponse
	BatchListResponse             *KoutakuBatchListResponse
	BatchRetrieveResponse         *KoutakuBatchRetrieveResponse
	BatchCancelResponse           *KoutakuBatchCancelResponse
	BatchResultsResponse          *KoutakuBatchResultsResponse
	BatchDeleteResponse           *KoutakuBatchDeleteResponse
	ContainerCreateResponse       *KoutakuContainerCreateResponse
	ContainerListResponse         *KoutakuContainerListResponse
	ContainerRetrieveResponse     *KoutakuContainerRetrieveResponse
	ContainerDeleteResponse       *KoutakuContainerDeleteResponse
	ContainerFileCreateResponse   *KoutakuContainerFileCreateResponse
	ContainerFileListResponse     *KoutakuContainerFileListResponse
	ContainerFileRetrieveResponse *KoutakuContainerFileRetrieveResponse
	ContainerFileContentResponse  *KoutakuContainerFileContentResponse
	ContainerFileDeleteResponse   *KoutakuContainerFileDeleteResponse
	PassthroughResponse           *KoutakuPassthroughResponse
}

func (r *KoutakuResponse) GetExtraFields() *KoutakuResponseExtraFields {
	switch {
	case r.ListModelsResponse != nil:
		return &r.ListModelsResponse.ExtraFields
	case r.TextCompletionResponse != nil:
		return &r.TextCompletionResponse.ExtraFields
	case r.ChatResponse != nil:
		return &r.ChatResponse.ExtraFields
	case r.ResponsesResponse != nil:
		return &r.ResponsesResponse.ExtraFields
	case r.ResponsesStreamResponse != nil:
		return &r.ResponsesStreamResponse.ExtraFields
	case r.CountTokensResponse != nil:
		return &r.CountTokensResponse.ExtraFields
	case r.EmbeddingResponse != nil:
		return &r.EmbeddingResponse.ExtraFields
	case r.RerankResponse != nil:
		return &r.RerankResponse.ExtraFields
	case r.OCRResponse != nil:
		return &r.OCRResponse.ExtraFields
	case r.SpeechResponse != nil:
		return &r.SpeechResponse.ExtraFields
	case r.SpeechStreamResponse != nil:
		return &r.SpeechStreamResponse.ExtraFields
	case r.TranscriptionResponse != nil:
		return &r.TranscriptionResponse.ExtraFields
	case r.TranscriptionStreamResponse != nil:
		return &r.TranscriptionStreamResponse.ExtraFields
	case r.ImageGenerationResponse != nil:
		return &r.ImageGenerationResponse.ExtraFields
	case r.ImageGenerationStreamResponse != nil:
		return &r.ImageGenerationStreamResponse.ExtraFields
	case r.FileUploadResponse != nil:
		return &r.FileUploadResponse.ExtraFields
	case r.FileListResponse != nil:
		return &r.FileListResponse.ExtraFields
	case r.FileRetrieveResponse != nil:
		return &r.FileRetrieveResponse.ExtraFields
	case r.FileDeleteResponse != nil:
		return &r.FileDeleteResponse.ExtraFields
	case r.FileContentResponse != nil:
		return &r.FileContentResponse.ExtraFields
	case r.VideoGenerationResponse != nil:
		return &r.VideoGenerationResponse.ExtraFields
	case r.VideoDownloadResponse != nil:
		return &r.VideoDownloadResponse.ExtraFields
	case r.VideoListResponse != nil:
		return &r.VideoListResponse.ExtraFields
	case r.VideoDeleteResponse != nil:
		return &r.VideoDeleteResponse.ExtraFields
	case r.BatchCreateResponse != nil:
		return &r.BatchCreateResponse.ExtraFields
	case r.BatchListResponse != nil:
		return &r.BatchListResponse.ExtraFields
	case r.BatchRetrieveResponse != nil:
		return &r.BatchRetrieveResponse.ExtraFields
	case r.BatchCancelResponse != nil:
		return &r.BatchCancelResponse.ExtraFields
	case r.BatchDeleteResponse != nil:
		return &r.BatchDeleteResponse.ExtraFields
	case r.BatchResultsResponse != nil:
		return &r.BatchResultsResponse.ExtraFields
	case r.ContainerCreateResponse != nil:
		return &r.ContainerCreateResponse.ExtraFields
	case r.ContainerListResponse != nil:
		return &r.ContainerListResponse.ExtraFields
	case r.ContainerRetrieveResponse != nil:
		return &r.ContainerRetrieveResponse.ExtraFields
	case r.ContainerDeleteResponse != nil:
		return &r.ContainerDeleteResponse.ExtraFields
	case r.ContainerFileCreateResponse != nil:
		return &r.ContainerFileCreateResponse.ExtraFields
	case r.ContainerFileListResponse != nil:
		return &r.ContainerFileListResponse.ExtraFields
	case r.ContainerFileRetrieveResponse != nil:
		return &r.ContainerFileRetrieveResponse.ExtraFields
	case r.ContainerFileContentResponse != nil:
		return &r.ContainerFileContentResponse.ExtraFields
	case r.ContainerFileDeleteResponse != nil:
		return &r.ContainerFileDeleteResponse.ExtraFields
	case r.PassthroughResponse != nil:
		return &r.PassthroughResponse.ExtraFields
	}

	return &KoutakuResponseExtraFields{}
}

func (r *KoutakuResponse) PopulateExtraFields(requestType RequestType, provider ModelProvider, originalModelRequested string, resolvedModelUsed string) {
	if r == nil {
		return
	}
	resolvedModel := resolvedModelUsed
	if resolvedModel == "" {
		resolvedModel = originalModelRequested
	}
	switch {
	case r.ListModelsResponse != nil:
		r.ListModelsResponse.ExtraFields.RequestType = requestType
		r.ListModelsResponse.ExtraFields.Provider = provider
		r.ListModelsResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.ListModelsResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.TextCompletionResponse != nil:
		r.TextCompletionResponse.ExtraFields.RequestType = requestType
		r.TextCompletionResponse.ExtraFields.Provider = provider
		r.TextCompletionResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.TextCompletionResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.ChatResponse != nil:
		r.ChatResponse.ExtraFields.RequestType = requestType
		r.ChatResponse.ExtraFields.Provider = provider
		r.ChatResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.ChatResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.ResponsesResponse != nil:
		r.ResponsesResponse.ExtraFields.RequestType = requestType
		r.ResponsesResponse.ExtraFields.Provider = provider
		r.ResponsesResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.ResponsesResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.ResponsesStreamResponse != nil:
		r.ResponsesStreamResponse.ExtraFields.RequestType = requestType
		r.ResponsesStreamResponse.ExtraFields.Provider = provider
		r.ResponsesStreamResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.ResponsesStreamResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.CountTokensResponse != nil:
		r.CountTokensResponse.ExtraFields.RequestType = requestType
		r.CountTokensResponse.ExtraFields.Provider = provider
		r.CountTokensResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.CountTokensResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.EmbeddingResponse != nil:
		r.EmbeddingResponse.ExtraFields.RequestType = requestType
		r.EmbeddingResponse.ExtraFields.Provider = provider
		r.EmbeddingResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.EmbeddingResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.RerankResponse != nil:
		r.RerankResponse.ExtraFields.RequestType = requestType
		r.RerankResponse.ExtraFields.Provider = provider
		r.RerankResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.RerankResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.SpeechResponse != nil:
		r.SpeechResponse.ExtraFields.RequestType = requestType
		r.SpeechResponse.ExtraFields.Provider = provider
		r.SpeechResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.SpeechResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.SpeechStreamResponse != nil:
		r.SpeechStreamResponse.ExtraFields.RequestType = requestType
		r.SpeechStreamResponse.ExtraFields.Provider = provider
		r.SpeechStreamResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.SpeechStreamResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.TranscriptionResponse != nil:
		r.TranscriptionResponse.ExtraFields.RequestType = requestType
		r.TranscriptionResponse.ExtraFields.Provider = provider
		r.TranscriptionResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.TranscriptionResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.TranscriptionStreamResponse != nil:
		r.TranscriptionStreamResponse.ExtraFields.RequestType = requestType
		r.TranscriptionStreamResponse.ExtraFields.Provider = provider
		r.TranscriptionStreamResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.TranscriptionStreamResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.ImageGenerationResponse != nil:
		r.ImageGenerationResponse.ExtraFields.RequestType = requestType
		r.ImageGenerationResponse.ExtraFields.Provider = provider
		r.ImageGenerationResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.ImageGenerationResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.ImageGenerationStreamResponse != nil:
		r.ImageGenerationStreamResponse.ExtraFields.RequestType = requestType
		r.ImageGenerationStreamResponse.ExtraFields.Provider = provider
		r.ImageGenerationStreamResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.ImageGenerationStreamResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.VideoGenerationResponse != nil:
		r.VideoGenerationResponse.ExtraFields.RequestType = requestType
		r.VideoGenerationResponse.ExtraFields.Provider = provider
		r.VideoGenerationResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.VideoGenerationResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.VideoDownloadResponse != nil:
		r.VideoDownloadResponse.ExtraFields.RequestType = requestType
		r.VideoDownloadResponse.ExtraFields.Provider = provider
		r.VideoDownloadResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.VideoDownloadResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.VideoListResponse != nil:
		r.VideoListResponse.ExtraFields.RequestType = requestType
		r.VideoListResponse.ExtraFields.Provider = provider
		r.VideoListResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.VideoListResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.VideoDeleteResponse != nil:
		r.VideoDeleteResponse.ExtraFields.RequestType = requestType
		r.VideoDeleteResponse.ExtraFields.Provider = provider
		r.VideoDeleteResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.VideoDeleteResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.FileUploadResponse != nil:
		r.FileUploadResponse.ExtraFields.RequestType = requestType
		r.FileUploadResponse.ExtraFields.Provider = provider
		r.FileUploadResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.FileUploadResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.FileListResponse != nil:
		r.FileListResponse.ExtraFields.RequestType = requestType
		r.FileListResponse.ExtraFields.Provider = provider
		r.FileListResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.FileListResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.FileRetrieveResponse != nil:
		r.FileRetrieveResponse.ExtraFields.RequestType = requestType
		r.FileRetrieveResponse.ExtraFields.Provider = provider
		r.FileRetrieveResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.FileRetrieveResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.FileDeleteResponse != nil:
		r.FileDeleteResponse.ExtraFields.RequestType = requestType
		r.FileDeleteResponse.ExtraFields.Provider = provider
		r.FileDeleteResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.FileDeleteResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.FileContentResponse != nil:
		r.FileContentResponse.ExtraFields.RequestType = requestType
		r.FileContentResponse.ExtraFields.Provider = provider
		r.FileContentResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.FileContentResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.BatchCreateResponse != nil:
		r.BatchCreateResponse.ExtraFields.RequestType = requestType
		r.BatchCreateResponse.ExtraFields.Provider = provider
		r.BatchCreateResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.BatchCreateResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.BatchListResponse != nil:
		r.BatchListResponse.ExtraFields.RequestType = requestType
		r.BatchListResponse.ExtraFields.Provider = provider
		r.BatchListResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.BatchListResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.BatchRetrieveResponse != nil:
		r.BatchRetrieveResponse.ExtraFields.RequestType = requestType
		r.BatchRetrieveResponse.ExtraFields.Provider = provider
		r.BatchRetrieveResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.BatchRetrieveResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.BatchCancelResponse != nil:
		r.BatchCancelResponse.ExtraFields.RequestType = requestType
		r.BatchCancelResponse.ExtraFields.Provider = provider
		r.BatchCancelResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.BatchCancelResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.BatchDeleteResponse != nil:
		r.BatchDeleteResponse.ExtraFields.RequestType = requestType
		r.BatchDeleteResponse.ExtraFields.Provider = provider
		r.BatchDeleteResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.BatchDeleteResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.BatchResultsResponse != nil:
		r.BatchResultsResponse.ExtraFields.RequestType = requestType
		r.BatchResultsResponse.ExtraFields.Provider = provider
		r.BatchResultsResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.BatchResultsResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.ContainerCreateResponse != nil:
		r.ContainerCreateResponse.ExtraFields.RequestType = requestType
		r.ContainerCreateResponse.ExtraFields.Provider = provider
		r.ContainerCreateResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.ContainerCreateResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.ContainerListResponse != nil:
		r.ContainerListResponse.ExtraFields.RequestType = requestType
		r.ContainerListResponse.ExtraFields.Provider = provider
		r.ContainerListResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.ContainerListResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.ContainerRetrieveResponse != nil:
		r.ContainerRetrieveResponse.ExtraFields.RequestType = requestType
		r.ContainerRetrieveResponse.ExtraFields.Provider = provider
		r.ContainerRetrieveResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.ContainerRetrieveResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.ContainerDeleteResponse != nil:
		r.ContainerDeleteResponse.ExtraFields.RequestType = requestType
		r.ContainerDeleteResponse.ExtraFields.Provider = provider
		r.ContainerDeleteResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.ContainerDeleteResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.ContainerFileCreateResponse != nil:
		r.ContainerFileCreateResponse.ExtraFields.RequestType = requestType
		r.ContainerFileCreateResponse.ExtraFields.Provider = provider
		r.ContainerFileCreateResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.ContainerFileCreateResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.ContainerFileListResponse != nil:
		r.ContainerFileListResponse.ExtraFields.RequestType = requestType
		r.ContainerFileListResponse.ExtraFields.Provider = provider
		r.ContainerFileListResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.ContainerFileListResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.ContainerFileRetrieveResponse != nil:
		r.ContainerFileRetrieveResponse.ExtraFields.RequestType = requestType
		r.ContainerFileRetrieveResponse.ExtraFields.Provider = provider
		r.ContainerFileRetrieveResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.ContainerFileRetrieveResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.ContainerFileContentResponse != nil:
		r.ContainerFileContentResponse.ExtraFields.RequestType = requestType
		r.ContainerFileContentResponse.ExtraFields.Provider = provider
		r.ContainerFileContentResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.ContainerFileContentResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.ContainerFileDeleteResponse != nil:
		r.ContainerFileDeleteResponse.ExtraFields.RequestType = requestType
		r.ContainerFileDeleteResponse.ExtraFields.Provider = provider
		r.ContainerFileDeleteResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.ContainerFileDeleteResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.OCRResponse != nil:
		r.OCRResponse.ExtraFields.RequestType = requestType
		r.OCRResponse.ExtraFields.Provider = provider
		r.OCRResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.OCRResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	case r.PassthroughResponse != nil:
		r.PassthroughResponse.ExtraFields.RequestType = requestType
		r.PassthroughResponse.ExtraFields.Provider = provider
		r.PassthroughResponse.ExtraFields.OriginalModelRequested = originalModelRequested
		r.PassthroughResponse.ExtraFields.ResolvedModelUsed = resolvedModel
	}
}

// KoutakuMCPResponse is the response struct for all MCP responses.
// only ONE of the following fields should be set:
// - ChatMessage
// - ResponsesMessage
type KoutakuMCPResponse struct {
	ChatMessage      *ChatMessage
	ResponsesMessage *ResponsesMessage
	ExtraFields      KoutakuMCPResponseExtraFields
}

// KoutakuResponseExtraFields contains additional fields in a response.
type KoutakuResponseExtraFields struct {
	RequestType               RequestType        `json:"request_type"`
	Provider                  ModelProvider      `json:"provider,omitempty"`
	OriginalModelRequested    string             `json:"original_model_requested,omitempty"` // the model alias the caller sent in the request
	ResolvedModelUsed         string             `json:"resolved_model_used,omitempty"`      // the actual provider API identifier used (equals OriginalModelRequested when no alias mapping exists)
	Latency                   int64              `json:"latency"`                            // in milliseconds (for streaming responses this will be each chunk latency, and the last chunk latency will be the total latency)
	ChunkIndex                int                `json:"chunk_index"`                        // used for streaming responses to identify the chunk index, will be 0 for non-streaming responses
	RawRequest                interface{}        `json:"raw_request,omitempty"`
	RawResponse               interface{}        `json:"raw_response,omitempty"`
	CacheDebug                *KoutakuCacheDebug `json:"cache_debug,omitempty"`
	ParseErrors               []BatchError       `json:"parse_errors,omitempty"` // errors encountered while parsing JSONL batch results
	ConvertedRequestType      RequestType        `json:"converted_request_type,omitempty"`
	DroppedCompatPluginParams []string           `json:"dropped_compat_plugin_params,omitempty"` // params dropped by the compat plugin based on model catalog
	ProviderResponseHeaders   map[string]string  `json:"provider_response_headers,omitempty"`    // HTTP response headers from the provider (filtered to exclude transport-level headers)
}

type KoutakuMCPResponseExtraFields struct {
	ClientName string `json:"client_name"`
	ToolName   string `json:"tool_name"`
	Latency    int64  `json:"latency"` // in milliseconds
}

// KoutakuCacheDebug represents debug information about the cache.
type KoutakuCacheDebug struct {
	CacheHit bool `json:"cache_hit"`

	CacheID *string `json:"cache_id,omitempty"`
	HitType *string `json:"hit_type,omitempty"`

	RequestedProvider *string `json:"requested_provider,omitempty"`
	RequestedModel    *string `json:"requested_model,omitempty"`

	// Semantic cache only (provider, model, and input tokens will be present for semantic cache, even if cache is not hit)
	ProviderUsed *string `json:"provider_used,omitempty"`
	ModelUsed    *string `json:"model_used,omitempty"`
	InputTokens  *int    `json:"input_tokens,omitempty"`

	// Semantic cache only (only when cache is hit)
	Threshold  *float64 `json:"threshold,omitempty"`
	Similarity *float64 `json:"similarity,omitempty"`
}

const (
	RequestCancelled = "request_cancelled"
	RequestTimedOut  = "request_timed_out"
)

// KoutakuStreamChunk represents a stream of responses from the Koutaku system.
// Either KoutakuResponse or KoutakuError will be non-nil.
type KoutakuStreamChunk struct {
	*KoutakuTextCompletionResponse
	*KoutakuChatResponse
	*KoutakuResponsesStreamResponse
	*KoutakuSpeechStreamResponse
	*KoutakuTranscriptionStreamResponse
	*KoutakuImageGenerationStreamResponse
	*KoutakuPassthroughResponse
	*KoutakuError
}

// MarshalJSON implements custom JSON marshaling for KoutakuStreamChunk.
// This ensures that only the non-nil embedded struct is marshaled,
func (bs KoutakuStreamChunk) MarshalJSON() ([]byte, error) {
	if bs.KoutakuTextCompletionResponse != nil {
		return MarshalSorted(bs.KoutakuTextCompletionResponse)
	} else if bs.KoutakuChatResponse != nil {
		return MarshalSorted(bs.KoutakuChatResponse)
	} else if bs.KoutakuResponsesStreamResponse != nil {
		return MarshalSorted(bs.KoutakuResponsesStreamResponse)
	} else if bs.KoutakuSpeechStreamResponse != nil {
		return MarshalSorted(bs.KoutakuSpeechStreamResponse)
	} else if bs.KoutakuTranscriptionStreamResponse != nil {
		return MarshalSorted(bs.KoutakuTranscriptionStreamResponse)
	} else if bs.KoutakuImageGenerationStreamResponse != nil {
		return MarshalSorted(bs.KoutakuImageGenerationStreamResponse)
	} else if bs.KoutakuPassthroughResponse != nil {
		return MarshalSorted(bs.KoutakuPassthroughResponse)
	} else if bs.KoutakuError != nil {
		return MarshalSorted(bs.KoutakuError)
	}
	// Return empty object if both are nil (shouldn't happen in practice)
	return []byte("{}"), nil
}

// KoutakuError represents an error from the Koutaku system.
//
// PLUGIN DEVELOPERS: When creating KoutakuError in PreLLMHook or PostLLMHook, you can set AllowFallbacks:
// - AllowFallbacks = &true: Koutaku will try fallback providers if available
// - AllowFallbacks = &false: Koutaku will return this error immediately, no fallbacks
// - AllowFallbacks = nil: Treated as true by default (fallbacks allowed for resilience)
type KoutakuError struct {
	EventID        *string                 `json:"event_id,omitempty"`
	Type           *string                 `json:"type,omitempty"`
	IsKoutakuError bool                    `json:"is_koutaku_error"`
	StatusCode     *int                    `json:"status_code,omitempty"`
	Error          *ErrorField             `json:"error"`
	AllowFallbacks *bool                   `json:"-"` // Optional: Controls fallback behavior (nil = true by default)
	StreamControl  *StreamControl          `json:"-"` // Optional: Controls stream behavior
	ExtraFields    KoutakuErrorExtraFields `json:"extra_fields"`
}

func (e *KoutakuError) PopulateExtraFields(requestType RequestType, provider ModelProvider, originalModelRequested string, resolvedModelUsed string) {
	if e == nil {
		return
	}
	e.ExtraFields.RequestType = requestType
	e.ExtraFields.Provider = provider
	e.ExtraFields.OriginalModelRequested = originalModelRequested
	if resolvedModelUsed != "" {
		e.ExtraFields.ResolvedModelUsed = resolvedModelUsed
	} else {
		e.ExtraFields.ResolvedModelUsed = originalModelRequested
	}
}

// StreamControl represents stream control options.
type StreamControl struct {
	LogError   *bool `json:"log_error,omitempty"`   // Optional: Controls logging of error
	SkipStream *bool `json:"skip_stream,omitempty"` // Optional: Controls skipping of stream chunk
}

// ErrorField represents detailed error information.
type ErrorField struct {
	Type    *string     `json:"type,omitempty"`
	Code    *string     `json:"code,omitempty"`
	Message string      `json:"message"`
	Error   error       `json:"-"`
	Param   interface{} `json:"param,omitempty"`
	EventID *string     `json:"event_id,omitempty"`
}

// MarshalJSON implements custom JSON marshaling for ErrorField.
// It converts the Error field (error interface) to a string.
func (e *ErrorField) MarshalJSON() ([]byte, error) {
	type Alias ErrorField
	aux := &struct {
		Error *string `json:"error,omitempty"`
		*Alias
	}{
		Alias: (*Alias)(e),
	}

	if e.Error != nil {
		errStr := e.Error.Error()
		aux.Error = &errStr
	}

	return json.Marshal(aux)
}

func (e *ErrorField) UnmarshalJSON(data []byte) error {
	aux := &struct {
		Type    *string     `json:"type,omitempty"`
		Code    interface{} `json:"code,omitempty"`
		Message string      `json:"message"`
		Error   *string     `json:"error,omitempty"`
		Param   interface{} `json:"param,omitempty"`
		EventID *string     `json:"event_id,omitempty"`
	}{}

	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}

	e.Type = aux.Type
	e.Message = aux.Message
	e.Param = aux.Param
	e.EventID = aux.EventID
	if aux.Error != nil {
		e.Error = errors.New(*aux.Error)
	}
	if aux.Code != nil {
		switch v := aux.Code.(type) {
		case string:
			e.Code = &v
		case float64:
			s := strconv.FormatInt(int64(v), 10)
			e.Code = &s
		default:
			s := fmt.Sprint(aux.Code)
			e.Code = &s
		}
	}
	return nil
}

// KoutakuErrorExtraFields contains additional fields in an error response.
type KoutakuErrorExtraFields struct {
	Provider                  ModelProvider              `json:"provider,omitempty"`
	OriginalModelRequested    string                     `json:"original_model_requested,omitempty"`
	ResolvedModelUsed         string                     `json:"resolved_model_used,omitempty"`
	RequestType               RequestType                `json:"request_type,omitempty"`
	RawRequest                interface{}                `json:"raw_request,omitempty"`
	RawResponse               interface{}                `json:"raw_response,omitempty"`
	ConvertedRequestType      RequestType                `json:"converted_request_type,omitempty"`
	DroppedCompatPluginParams []string                   `json:"dropped_compat_plugin_params,omitempty"`
	KeyStatuses               []KeyStatus                `json:"key_statuses,omitempty"`
	MCPAuthRequired           *MCPUserOAuthRequiredError `json:"mcp_auth_required,omitempty"` // Set when a per-user OAuth MCP tool requires authentication
}
