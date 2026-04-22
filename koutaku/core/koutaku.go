// Package koutaku provides the core implementation of the Koutaku system.
// Koutaku is a unified interface for interacting with various AI model providers,
// managing concurrent requests, and handling provider-specific configurations.
package koutaku

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"

	"github.com/koutaku/koutaku/core/keyselectors"
	"github.com/koutaku/koutaku/core/mcp"
	"github.com/koutaku/koutaku/core/mcp/codemode/starlark"
	"github.com/koutaku/koutaku/core/providers/anthropic"
	"github.com/koutaku/koutaku/core/providers/azure"
	"github.com/koutaku/koutaku/core/providers/bedrock"
	"github.com/koutaku/koutaku/core/providers/cerebras"
	"github.com/koutaku/koutaku/core/providers/cohere"
	"github.com/koutaku/koutaku/core/providers/elevenlabs"
	"github.com/koutaku/koutaku/core/providers/fireworks"
	"github.com/koutaku/koutaku/core/providers/gemini"
	"github.com/koutaku/koutaku/core/providers/groq"
	"github.com/koutaku/koutaku/core/providers/huggingface"
	"github.com/koutaku/koutaku/core/providers/mistral"
	"github.com/koutaku/koutaku/core/providers/nebius"
	"github.com/koutaku/koutaku/core/providers/ollama"
	"github.com/koutaku/koutaku/core/providers/openai"
	"github.com/koutaku/koutaku/core/providers/openrouter"
	"github.com/koutaku/koutaku/core/providers/parasail"
	"github.com/koutaku/koutaku/core/providers/perplexity"
	"github.com/koutaku/koutaku/core/providers/replicate"
	"github.com/koutaku/koutaku/core/providers/runway"
	"github.com/koutaku/koutaku/core/providers/sgl"
	providerUtils "github.com/koutaku/koutaku/core/providers/utils"
	"github.com/koutaku/koutaku/core/providers/vertex"
	"github.com/koutaku/koutaku/core/providers/vllm"
	"github.com/koutaku/koutaku/core/providers/xai"
	schemas "github.com/koutaku/koutaku/core/schemas"
	"github.com/valyala/fasthttp"
)

// ChannelMessage represents a message passed through the request channel.
// It contains the request, response and error channels, and the request type.
type ChannelMessage struct {
	schemas.KoutakuRequest
	Context        *schemas.KoutakuContext
	Response       chan *schemas.KoutakuResponse
	ResponseStream chan chan *schemas.KoutakuStreamChunk
	Err            chan schemas.KoutakuError
}

// Koutaku manages providers and maintains specified open channels for concurrent processing.
// It handles request routing, provider management, and response processing.
type Koutaku struct {
	ctx                 *schemas.KoutakuContext
	cancel              context.CancelFunc
	account             schemas.Account                     // account interface
	llmPlugins          atomic.Pointer[[]schemas.LLMPlugin] // list of llm plugins
	mcpPlugins          atomic.Pointer[[]schemas.MCPPlugin] // list of mcp plugins
	providers           atomic.Pointer[[]schemas.Provider]  // list of providers
	requestQueues       sync.Map                            // provider request queues (thread-safe), stores *ProviderQueue
	waitGroups          sync.Map                            // wait groups for each provider (thread-safe)
	providerMutexes     sync.Map                            // mutexes for each provider to prevent concurrent updates (thread-safe)
	channelMessagePool  sync.Pool                           // Pool for ChannelMessage objects, initial pool size is set in Init
	responseChannelPool sync.Pool                           // Pool for response channels, initial pool size is set in Init
	errorChannelPool    sync.Pool                           // Pool for error channels, initial pool size is set in Init
	responseStreamPool  sync.Pool                           // Pool for response stream channels, initial pool size is set in Init
	pluginPipelinePool  sync.Pool                           // Pool for PluginPipeline objects
	koutakuRequestPool  sync.Pool                           // Pool for KoutakuRequest objects
	mcpRequestPool      sync.Pool                           // Pool for KoutakuMCPRequest objects
	oauth2Provider      schemas.OAuth2Provider              // OAuth provider instance
	logger              schemas.Logger                      // logger instance, default logger is used if not provided
	tracer              atomic.Value                        // tracer for distributed tracing (stores schemas.Tracer, NoOpTracer if not configured)
	MCPManager          mcp.MCPManagerInterface             // MCP integration manager (nil if MCP not configured)
	mcpInitOnce         sync.Once                           // Ensures MCP manager is initialized only once
	dropExcessRequests  atomic.Bool                         // If true, in cases where the queue is full, requests will not wait for the queue to be empty and will be dropped instead.
	keySelector         schemas.KeySelector                 // Custom key selector function
	kvStore             schemas.KVStore                     // optional KV store for session stickiness (nil = disabled)
}

// ProviderQueue wraps a provider's request channel with lifecycle management
// to prevent "send on closed channel" panics during provider removal/update.
// Producers must check the closing flag or select on the done channel before sending.
//
// Why pq.queue is NEVER closed:
//
// Closing a channel in Go causes any concurrent send to that channel to panic
// ("send on closed channel"). There is always a TOCTOU window between a
// producer's isClosing() check and its select { case pq.queue <- msg: ... }:
// the producer could pass isClosing() while the queue is open, get preempted,
// and resume only after the queue is closed. Go's selectgo evaluates select
// cases in a random order, so even having case <-pq.done: in the same select
// does not protect against this — if selectgo evaluates the send case first on
// a closed channel it panics immediately via goto sclose, before reaching done.
//
// To close pq.queue safely you would need a sender-side WaitGroup so that
// signalClosing could wait for every in-flight producer to finish. That adds
// non-trivial overhead on the hot request path.
//
// Instead, pq.done is the sole shutdown signal. Receiving from a closed channel
// is always safe (returns the zero value immediately), so:
//   - Workers exit via case <-pq.done: — safe
//   - Producers bail via case <-pq.done: — safe
//   - drainQueueWithErrors handles any messages that slip through the TOCTOU window
//
// pq.queue is garbage collected automatically:
//   - RemoveProvider calls requestQueues.Delete, dropping the map's reference.
//   - UpdateProvider calls requestQueues.Store with a new queue, dropping the
//     map's reference to oldPq. Shutdown does not Delete at all — the whole
//     Koutaku instance is torn down.
//     In all cases, once no producer goroutine holds a reference to the
//     ProviderQueue, both the struct and pq.queue are eligible for GC.
//     No explicit close is needed.
type ProviderQueue struct {
	queue      chan *ChannelMessage // the actual request queue channel — never closed, see above
	done       chan struct{}        // closed by signalClosing() to signal shutdown; never written to otherwise
	closing    uint32               // atomic: 0 = open, 1 = closing
	signalOnce sync.Once
}

func isLargePayloadPassthrough(ctx *schemas.KoutakuContext) bool {
	if ctx == nil {
		return false
	}
	// Large payload mode intentionally skips JSON->Koutaku input materialization.
	// Example: a 400MB multipart/audio upload sets Input=nil by design; strict
	// non-nil validation here would reject valid passthrough requests.
	isLargePayload, _ := ctx.Value(schemas.KoutakuContextKeyLargePayloadMode).(bool)
	if !isLargePayload {
		return false
	}
	// Verify reader is present (flag and reader are always set together by middleware)
	reader := ctx.Value(schemas.KoutakuContextKeyLargePayloadReader)
	return reader != nil
}

// signalClosing signals the closing of the provider queue.
// This is lock-free: uses atomic store and sync.Once to safely signal shutdown.
func (pq *ProviderQueue) signalClosing() {
	pq.signalOnce.Do(func() {
		atomic.StoreUint32(&pq.closing, 1)
		close(pq.done)
	})
}

// isClosing returns true if the provider queue is closing.
// Uses atomic load for lock-free checking.
func (pq *ProviderQueue) isClosing() bool {
	return atomic.LoadUint32(&pq.closing) == 1
}

// PluginPipeline encapsulates the execution of plugin PreHooks and PostHooks, tracks how many plugins ran, and manages short-circuiting and error aggregation.
type PluginPipeline struct {
	llmPlugins []schemas.LLMPlugin
	mcpPlugins []schemas.MCPPlugin
	logger     schemas.Logger
	tracer     schemas.Tracer

	// Number of PreHooks that were executed (used to determine which PostHooks to run in reverse order)
	executedPreHooks int
	// Errors from PreHooks and PostHooks
	preHookErrors  []error
	postHookErrors []error

	// streamingMu guards the streaming post-hook accumulators below. Per-chunk
	// writes (accumulatePluginTiming) run in the provider goroutine while the
	// end-of-stream finalizer (FinalizeStreamingPostHookSpans) and
	// resetPluginPipeline can run in a different goroutine, so unsynchronised
	// access triggers "concurrent map read and map write" panics.
	streamingMu         sync.Mutex
	postHookTimings     map[string]*pluginTimingAccumulator // keyed by plugin name
	postHookPluginOrder []string                            // order in which post-hooks ran (for nested span creation)
	chunkCount          int

	// Plugin logging: cached scoped contexts for streaming post-hooks (reused across chunks)
	streamScopedCtxs map[string]*schemas.KoutakuContext
}

// pluginTimingAccumulator accumulates timing information for a plugin across streaming chunks
type pluginTimingAccumulator struct {
	totalDuration time.Duration
	invocations   int
	errors        int
}

// tracerWrapper wraps a Tracer to ensure atomic.Value stores consistent types.
// This is necessary because atomic.Value.Store() panics if called with values
// of different concrete types, even if they implement the same interface.
type tracerWrapper struct {
	tracer schemas.Tracer
}

// INITIALIZATION

// Init initializes a new Koutaku instance with the given configuration.
// It sets up the account, plugins, object pools, and initializes providers.
// Returns an error if initialization fails.
// Initial Memory Allocations happens here as per the initial pool size.
func Init(ctx context.Context, config schemas.KoutakuConfig) (*Koutaku, error) {
	if config.Account == nil {
		return nil, fmt.Errorf("account is required to initialize Koutaku")
	}

	if config.Logger == nil {
		config.Logger = NewDefaultLogger(schemas.LogLevelInfo)
	}
	providerUtils.SetLogger(config.Logger)

	// Initialize tracer (use NoOpTracer if not provided)
	tracer := config.Tracer
	if tracer == nil {
		tracer = schemas.DefaultTracer()
	}

	koutakuCtx, cancel := schemas.NewKoutakuContextWithCancel(ctx)
	koutaku := &Koutaku{
		ctx:            koutakuCtx,
		cancel:         cancel,
		account:        config.Account,
		llmPlugins:     atomic.Pointer[[]schemas.LLMPlugin]{},
		mcpPlugins:     atomic.Pointer[[]schemas.MCPPlugin]{},
		requestQueues:  sync.Map{},
		waitGroups:     sync.Map{},
		keySelector:    config.KeySelector,
		oauth2Provider: config.OAuth2Provider,
		logger:         config.Logger,
		kvStore:        config.KVStore,
	}
	koutaku.tracer.Store(&tracerWrapper{tracer: tracer})
	if config.LLMPlugins == nil {
		config.LLMPlugins = make([]schemas.LLMPlugin, 0)
	}
	if config.MCPPlugins == nil {
		config.MCPPlugins = make([]schemas.MCPPlugin, 0)
	}
	koutaku.llmPlugins.Store(&config.LLMPlugins)
	koutaku.mcpPlugins.Store(&config.MCPPlugins)

	// Initialize providers slice
	koutaku.providers.Store(&[]schemas.Provider{})

	koutaku.dropExcessRequests.Store(config.DropExcessRequests)

	if koutaku.keySelector == nil {
		koutaku.keySelector = keyselectors.WeightedRandom
	}

	// Initialize object pools
	koutaku.channelMessagePool = sync.Pool{
		New: func() interface{} {
			return &ChannelMessage{}
		},
	}
	koutaku.responseChannelPool = sync.Pool{
		New: func() interface{} {
			return make(chan *schemas.KoutakuResponse, 1)
		},
	}
	koutaku.errorChannelPool = sync.Pool{
		New: func() interface{} {
			return make(chan schemas.KoutakuError, 1)
		},
	}
	koutaku.responseStreamPool = sync.Pool{
		New: func() interface{} {
			return make(chan chan *schemas.KoutakuStreamChunk, 1)
		},
	}
	koutaku.pluginPipelinePool = sync.Pool{
		New: func() interface{} {
			return &PluginPipeline{
				preHookErrors:  make([]error, 0),
				postHookErrors: make([]error, 0),
			}
		},
	}
	koutaku.koutakuRequestPool = sync.Pool{
		New: func() interface{} {
			return &schemas.KoutakuRequest{}
		},
	}
	koutaku.mcpRequestPool = sync.Pool{
		New: func() interface{} {
			return &schemas.KoutakuMCPRequest{}
		},
	}
	// Prewarm pools with multiple objects
	for range config.InitialPoolSize {
		// Create and put new objects directly into pools
		koutaku.channelMessagePool.Put(&ChannelMessage{})
		koutaku.responseChannelPool.Put(make(chan *schemas.KoutakuResponse, 1))
		koutaku.errorChannelPool.Put(make(chan schemas.KoutakuError, 1))
		koutaku.responseStreamPool.Put(make(chan chan *schemas.KoutakuStreamChunk, 1))
		koutaku.pluginPipelinePool.Put(&PluginPipeline{
			preHookErrors:  make([]error, 0),
			postHookErrors: make([]error, 0),
		})
		koutaku.koutakuRequestPool.Put(&schemas.KoutakuRequest{})
		koutaku.mcpRequestPool.Put(&schemas.KoutakuMCPRequest{})
	}

	providerKeys, err := koutaku.account.GetConfiguredProviders()
	if err != nil {
		return nil, err
	}

	// Initialize MCP manager if configured
	if config.MCPConfig != nil {
		koutaku.mcpInitOnce.Do(func() {
			// Set up plugin pipeline provider functions for executeCode tool hooks
			mcpConfig := *config.MCPConfig
			mcpConfig.PluginPipelineProvider = func() interface{} {
				return koutaku.getPluginPipeline()
			}
			mcpConfig.ReleasePluginPipeline = func(pipeline interface{}) {
				if pp, ok := pipeline.(*PluginPipeline); ok {
					koutaku.releasePluginPipeline(pp)
				}
			}
			// Create Starlark CodeMode for code execution
			var codeModeConfig *mcp.CodeModeConfig
			if mcpConfig.ToolManagerConfig != nil {
				codeModeConfig = &mcp.CodeModeConfig{
					BindingLevel:         mcpConfig.ToolManagerConfig.CodeModeBindingLevel,
					ToolExecutionTimeout: mcpConfig.ToolManagerConfig.ToolExecutionTimeout,
				}
			}
			codeMode := starlark.NewStarlarkCodeMode(codeModeConfig, koutaku.logger)
			koutaku.MCPManager = mcp.NewMCPManager(koutakuCtx, mcpConfig, koutaku.oauth2Provider, koutaku.logger, codeMode)
			koutaku.logger.Info("MCP integration initialized successfully")
		})
	}

	// Create buffered channels for each provider and start workers
	for _, providerKey := range providerKeys {
		if strings.TrimSpace(string(providerKey)) == "" {
			koutaku.logger.Warn("provider key is empty, skipping init")
			continue
		}

		config, err := koutaku.account.GetConfigForProvider(providerKey)
		if err != nil {
			koutaku.logger.Warn("failed to get config for provider, skipping init: %v", err)
			continue
		}
		if config == nil {
			koutaku.logger.Warn("config is nil for provider %s, skipping init", providerKey)
			continue
		}

		// Lock the provider mutex during initialization
		providerMutex := koutaku.getProviderMutex(providerKey)
		providerMutex.Lock()
		err = koutaku.prepareProvider(providerKey, config)
		providerMutex.Unlock()

		if err != nil {
			koutaku.logger.Warn("failed to prepare provider %s: %v", providerKey, err)
		}
	}
	return koutaku, nil
}

// SetTracer sets the tracer for the Koutaku instance.
func (koutaku *Koutaku) SetTracer(tracer schemas.Tracer) {
	if tracer == nil {
		// Fall back to no-op tracer if not provided
		tracer = schemas.DefaultTracer()
	}
	koutaku.tracer.Store(&tracerWrapper{tracer: tracer})
}

// getTracer returns the tracer from atomic storage with type assertion.
func (koutaku *Koutaku) getTracer() schemas.Tracer {
	return koutaku.tracer.Load().(*tracerWrapper).tracer
}

// ReloadConfig reloads the config from DB
// Currently we update account, drop excess requests, and plugin lists
// We will keep on adding other aspects as required
func (koutaku *Koutaku) ReloadConfig(config schemas.KoutakuConfig) error {
	koutaku.dropExcessRequests.Store(config.DropExcessRequests)
	return nil
}

// PUBLIC API METHODS

// ListModelsRequest sends a list models request to the specified provider.
func (koutaku *Koutaku) ListModelsRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuListModelsRequest) (*schemas.KoutakuListModelsResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "list models request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ListModelsRequest,
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for list models request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ListModelsRequest,
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ListModelsRequest
	koutakuReq.ListModelsRequest = req

	resp, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}

	return resp.ListModelsResponse, nil
}

// ListAllModels lists all models from all configured providers.
// It accumulates responses from all providers with a limit of 1000 per provider to get all results.
func (koutaku *Koutaku) ListAllModels(ctx *schemas.KoutakuContext, req *schemas.KoutakuListModelsRequest) (*schemas.KoutakuListModelsResponse, *schemas.KoutakuError) {
	if req == nil {
		req = &schemas.KoutakuListModelsRequest{}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	providerKeys, err := koutaku.GetConfiguredProviders()
	if err != nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: err.Error(),
				Error:   err,
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ListModelsRequest,
			},
		}
	}

	startTime := time.Now()

	// Result structure for collecting provider responses
	type providerResult struct {
		provider    schemas.ModelProvider
		models      []schemas.Model
		keyStatuses []schemas.KeyStatus
		err         *schemas.KoutakuError
	}

	results := make(chan providerResult, len(providerKeys))
	var wg sync.WaitGroup

	// Launch concurrent requests for all providers
	for _, providerKey := range providerKeys {
		if strings.TrimSpace(string(providerKey)) == "" {
			continue
		}

		wg.Add(1)
		go func(providerKey schemas.ModelProvider) {
			defer wg.Done()

			providerCtx := schemas.NewKoutakuContext(ctx, schemas.NoDeadline)
			providerCtx.SetValue(schemas.KoutakuContextKeyRequestID, uuid.New().String())

			providerModels := make([]schemas.Model, 0)
			var providerKeyStatuses []schemas.KeyStatus
			var providerErr *schemas.KoutakuError

			// Create request for this provider with limit of 1000
			providerRequest := &schemas.KoutakuListModelsRequest{
				Provider:   providerKey,
				PageSize:   schemas.DefaultPageSize,
				Unfiltered: req.Unfiltered,
			}

			iterations := 0
			for {
				// check for context cancellation
				select {
				case <-ctx.Done():
					koutaku.logger.Warn("context cancelled for provider %s", providerKey)
					return
				default:
				}

				iterations++
				if iterations > schemas.MaxPaginationRequests {
					koutaku.logger.Warn("reached maximum pagination requests (%d) for provider %s, please increase the page size", schemas.MaxPaginationRequests, providerKey)
					break
				}

				response, koutakuErr := koutaku.ListModelsRequest(providerCtx, providerRequest)
				if koutakuErr != nil {
					// Skip logging "no keys found" and "not supported" errors as they are expected when a provider is not configured
					if !strings.Contains(koutakuErr.Error.Message, "no keys found") &&
						!strings.Contains(koutakuErr.Error.Message, "not supported") {
						providerErr = koutakuErr
						koutaku.logger.Warn("failed to list models for provider %s: %s", providerKey, GetErrorMessage(koutakuErr))
					}
					// Collect key statuses from error (failure case)
					if len(koutakuErr.ExtraFields.KeyStatuses) > 0 {
						providerKeyStatuses = append(providerKeyStatuses, koutakuErr.ExtraFields.KeyStatuses...)
					}
					break
				}

				if response == nil || len(response.Data) == 0 {
					break
				}

				providerModels = append(providerModels, response.Data...)

				if len(response.KeyStatuses) > 0 {
					providerKeyStatuses = append(providerKeyStatuses, response.KeyStatuses...)
				}

				// Check if there are more pages
				if response.NextPageToken == "" {
					break
				}

				// Set the page token for the next request
				providerRequest.PageToken = response.NextPageToken
			}

			results <- providerResult{
				provider:    providerKey,
				models:      providerModels,
				keyStatuses: providerKeyStatuses,
				err:         providerErr,
			}
		}(providerKey)
	}

	// Wait for all goroutines to complete
	wg.Wait()
	close(results)

	// Accumulate all models and key statuses from all providers
	allModels := make([]schemas.Model, 0)
	allKeyStatuses := make([]schemas.KeyStatus, 0)
	var firstError *schemas.KoutakuError

	for result := range results {
		if len(result.models) > 0 {
			allModels = append(allModels, result.models...)
		}
		if len(result.keyStatuses) > 0 {
			allKeyStatuses = append(allKeyStatuses, result.keyStatuses...)
		}
		if result.err != nil && firstError == nil {
			firstError = result.err
		}
	}

	// If we couldn't get any models from any provider, return the first error
	if len(allModels) == 0 && firstError != nil {
		// Attach all key statuses to the error
		firstError.ExtraFields.KeyStatuses = allKeyStatuses
		return nil, firstError
	}

	// Sort models alphabetically by ID
	sort.Slice(allModels, func(i, j int) bool {
		return allModels[i].ID < allModels[j].ID
	})

	// Return aggregated response with accumulated latency and key statuses
	response := &schemas.KoutakuListModelsResponse{
		Data:        allModels,
		KeyStatuses: allKeyStatuses,
		ExtraFields: schemas.KoutakuResponseExtraFields{
			RequestType: schemas.ListModelsRequest,
			Latency:     time.Since(startTime).Milliseconds(),
		},
	}

	response = response.ApplyPagination(req.PageSize, req.PageToken)

	return response, nil
}

// TextCompletionRequest sends a text completion request to the specified provider.
func (koutaku *Koutaku) TextCompletionRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuTextCompletionRequest) (*schemas.KoutakuTextCompletionResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "text completion request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.TextCompletionRequest,
			},
		}
	}
	if (req.Input == nil || (req.Input.PromptStr == nil && req.Input.PromptArray == nil)) && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "prompt not provided for text completion request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.TextCompletionRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}
	// Preparing request
	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.TextCompletionRequest
	koutakuReq.TextCompletionRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	// TODO: Release the response
	return response.TextCompletionResponse, nil
}

// TextCompletionStreamRequest sends a streaming text completion request to the specified provider.
func (koutaku *Koutaku) TextCompletionStreamRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuTextCompletionRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "text completion stream request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.TextCompletionStreamRequest,
			},
		}
	}
	if (req.Input == nil || (req.Input.PromptStr == nil && req.Input.PromptArray == nil)) && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "text not provided for text completion stream request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.TextCompletionStreamRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}
	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.TextCompletionStreamRequest
	koutakuReq.TextCompletionRequest = req
	return koutaku.handleStreamRequest(ctx, koutakuReq)
}

func (koutaku *Koutaku) makeChatCompletionRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuChatRequest) (*schemas.KoutakuChatResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "chat completion request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ChatCompletionRequest,
			},
		}
	}
	if req.Input == nil && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "chats not provided for chat completion request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.ChatCompletionRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ChatCompletionRequest
	koutakuReq.ChatRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}

	return response.ChatResponse, nil
}

// ChatCompletionRequest sends a chat completion request to the specified provider.
func (koutaku *Koutaku) ChatCompletionRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuChatRequest) (*schemas.KoutakuChatResponse, *schemas.KoutakuError) {
	// If ctx is nil, use the koutaku context (defensive check for mcp agent mode)
	if ctx == nil {
		ctx = koutaku.ctx
	}

	response, err := koutaku.makeChatCompletionRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	// Check if we should enter agent mode
	if koutaku.MCPManager != nil {
		return koutaku.MCPManager.CheckAndExecuteAgentForChatRequest(
			ctx,
			req,
			response,
			koutaku.makeChatCompletionRequest,
			koutaku.executeMCPToolWithHooks,
		)
	}

	return response, nil
}

// ChatCompletionStreamRequest sends a chat completion stream request to the specified provider.
func (koutaku *Koutaku) ChatCompletionStreamRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuChatRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "chat completion stream request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ChatCompletionStreamRequest,
			},
		}
	}
	if req.Input == nil && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "chats not provided for chat completion request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.ChatCompletionStreamRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ChatCompletionStreamRequest
	koutakuReq.ChatRequest = req

	return koutaku.handleStreamRequest(ctx, koutakuReq)
}

func (koutaku *Koutaku) makeResponsesRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuResponsesRequest) (*schemas.KoutakuResponsesResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "responses request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ResponsesRequest,
			},
		}
	}
	// In large payload mode, Input is intentionally nil — body streams directly to upstream
	if req.Input == nil {
		isLargePayload, _ := ctx.Value(schemas.KoutakuContextKeyLargePayloadMode).(bool)
		if !isLargePayload {
			return nil, &schemas.KoutakuError{
				IsKoutakuError: false,
				Error: &schemas.ErrorField{
					Message: "responses not provided for responses request",
				},
				ExtraFields: schemas.KoutakuErrorExtraFields{
					RequestType:            schemas.ResponsesRequest,
					Provider:               req.Provider,
					OriginalModelRequested: req.Model,
					ResolvedModelUsed:      req.Model,
				},
			}
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ResponsesRequest
	koutakuReq.ResponsesRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.ResponsesResponse, nil
}

// ResponsesRequest sends a responses request to the specified provider.
func (koutaku *Koutaku) ResponsesRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuResponsesRequest) (*schemas.KoutakuResponsesResponse, *schemas.KoutakuError) {
	// If ctx is nil, use the koutaku context (defensive check for mcp agent mode)
	if ctx == nil {
		ctx = koutaku.ctx
	}

	response, err := koutaku.makeResponsesRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	// Check if we should enter agent mode
	if koutaku.MCPManager != nil {
		return koutaku.MCPManager.CheckAndExecuteAgentForResponsesRequest(
			ctx,
			req,
			response,
			koutaku.makeResponsesRequest,
			koutaku.executeMCPToolWithHooks,
		)
	}

	return response, nil
}

// ResponsesStreamRequest sends a responses stream request to the specified provider.
func (koutaku *Koutaku) ResponsesStreamRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuResponsesRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "responses stream request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ResponsesStreamRequest,
			},
		}
	}
	// In large payload mode, Input is intentionally nil — body streams directly to upstream
	if req.Input == nil {
		isLargePayload, _ := ctx.Value(schemas.KoutakuContextKeyLargePayloadMode).(bool)
		if !isLargePayload {
			return nil, &schemas.KoutakuError{
				IsKoutakuError: false,
				Error: &schemas.ErrorField{
					Message: "responses not provided for responses stream request",
				},
				ExtraFields: schemas.KoutakuErrorExtraFields{
					RequestType:            schemas.ResponsesStreamRequest,
					Provider:               req.Provider,
					OriginalModelRequested: req.Model,
					ResolvedModelUsed:      req.Model,
				},
			}
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ResponsesStreamRequest
	koutakuReq.ResponsesRequest = req

	return koutaku.handleStreamRequest(ctx, koutakuReq)
}

// CountTokensRequest sends a count tokens request to the specified provider.
func (koutaku *Koutaku) CountTokensRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuResponsesRequest) (*schemas.KoutakuCountTokensResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "count tokens request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.CountTokensRequest,
			},
		}
	}
	if req.Input == nil && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "input not provided for count tokens request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.CountTokensRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.CountTokensRequest
	koutakuReq.CountTokensRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}

	return response.CountTokensResponse, nil
}

// EmbeddingRequest sends an embedding request to the specified provider.
func (koutaku *Koutaku) EmbeddingRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuEmbeddingRequest) (*schemas.KoutakuEmbeddingResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "embedding request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.EmbeddingRequest,
			},
		}
	}
	hasExtraInputs := req.Params != nil && req.Params.ExtraParams != nil &&
		(req.Params.ExtraParams["inputs"] != nil || req.Params.ExtraParams["images"] != nil)
	if (req.Input == nil || (req.Input.Text == nil && req.Input.Texts == nil && req.Input.Embedding == nil && req.Input.Embeddings == nil)) && !hasExtraInputs && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "embedding input not provided for embedding request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.EmbeddingRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.EmbeddingRequest
	koutakuReq.EmbeddingRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	// TODO: Release the response
	return response.EmbeddingResponse, nil
}

// RerankRequest sends a rerank request to the specified provider.
func (koutaku *Koutaku) RerankRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuRerankRequest) (*schemas.KoutakuRerankResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "rerank request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.RerankRequest,
			},
		}
	}
	if strings.TrimSpace(req.Query) == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "query not provided for rerank request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.RerankRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}
	if len(req.Documents) == 0 {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "documents not provided for rerank request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.RerankRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}
	for i, doc := range req.Documents {
		if strings.TrimSpace(doc.Text) == "" {
			return nil, &schemas.KoutakuError{
				IsKoutakuError: false,
				Error: &schemas.ErrorField{
					Message: fmt.Sprintf("document text is empty at index %d", i),
				},
				ExtraFields: schemas.KoutakuErrorExtraFields{
					RequestType:            schemas.RerankRequest,
					Provider:               req.Provider,
					OriginalModelRequested: req.Model,
					ResolvedModelUsed:      req.Model,
				},
			}
		}
	}
	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.RerankRequest
	koutakuReq.RerankRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.RerankResponse, nil
}

// OCRRequest sends an OCR request to the specified provider.
func (koutaku *Koutaku) OCRRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuOCRRequest) (*schemas.KoutakuOCRResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "ocr request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.OCRRequest,
			},
		}
	}
	if strings.TrimSpace(string(req.Document.Type)) == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "document type not provided for ocr request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.OCRRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}
	if req.Document.Type == schemas.OCRDocumentTypeDocumentURL && (req.Document.DocumentURL == nil || strings.TrimSpace(*req.Document.DocumentURL) == "") {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "document_url not provided for document_url type ocr request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.OCRRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}
	if req.Document.Type == schemas.OCRDocumentTypeImageURL && (req.Document.ImageURL == nil || strings.TrimSpace(*req.Document.ImageURL) == "") {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "image_url not provided for image_url type ocr request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.OCRRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}
	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.OCRRequest
	koutakuReq.OCRRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.OCRResponse, nil
}

// SpeechRequest sends a speech request to the specified provider.
func (koutaku *Koutaku) SpeechRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuSpeechRequest) (*schemas.KoutakuSpeechResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "speech request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.SpeechRequest,
			},
		}
	}
	if (req.Input == nil || req.Input.Input == "") && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "speech input not provided for speech request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.SpeechRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.SpeechRequest
	koutakuReq.SpeechRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	// TODO: Release the response
	return response.SpeechResponse, nil
}

// SpeechStreamRequest sends a speech stream request to the specified provider.
func (koutaku *Koutaku) SpeechStreamRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuSpeechRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "speech stream request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.SpeechStreamRequest,
			},
		}
	}
	if (req.Input == nil || req.Input.Input == "") && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "speech input not provided for speech stream request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.SpeechStreamRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.SpeechStreamRequest
	koutakuReq.SpeechRequest = req

	return koutaku.handleStreamRequest(ctx, koutakuReq)
}

// TranscriptionRequest sends a transcription request to the specified provider.
func (koutaku *Koutaku) TranscriptionRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuTranscriptionRequest) (*schemas.KoutakuTranscriptionResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "transcription request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.TranscriptionRequest,
			},
		}
	}
	if (req.Input == nil || req.Input.File == nil) && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "transcription input not provided for transcription request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.TranscriptionRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.TranscriptionRequest
	koutakuReq.TranscriptionRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	// TODO: Release the response
	return response.TranscriptionResponse, nil
}

// TranscriptionStreamRequest sends a transcription stream request to the specified provider.
func (koutaku *Koutaku) TranscriptionStreamRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuTranscriptionRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "transcription stream request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.TranscriptionStreamRequest,
			},
		}
	}
	if (req.Input == nil || req.Input.File == nil) && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "transcription input not provided for transcription stream request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.TranscriptionStreamRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.TranscriptionStreamRequest
	koutakuReq.TranscriptionRequest = req

	return koutaku.handleStreamRequest(ctx, koutakuReq)
}

// ImageGenerationRequest sends an image generation request to the specified provider.
func (koutaku *Koutaku) ImageGenerationRequest(ctx *schemas.KoutakuContext,
	req *schemas.KoutakuImageGenerationRequest,
) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "image generation request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ImageGenerationRequest,
			},
		}
	}
	if (req.Input == nil || req.Input.Prompt == "") && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "prompt not provided for image generation request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.ImageGenerationRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ImageGenerationRequest
	koutakuReq.ImageGenerationRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	if response == nil || response.ImageGenerationResponse == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "received nil response from provider",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.ImageGenerationRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	return response.ImageGenerationResponse, nil
}

// ImageGenerationStreamRequest sends an image generation stream request to the specified provider.
func (koutaku *Koutaku) ImageGenerationStreamRequest(ctx *schemas.KoutakuContext,
	req *schemas.KoutakuImageGenerationRequest,
) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "image generation stream request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ImageGenerationStreamRequest,
			},
		}
	}
	if (req.Input == nil || req.Input.Prompt == "") && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "prompt not provided for image generation stream request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.ImageGenerationStreamRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ImageGenerationStreamRequest
	koutakuReq.ImageGenerationRequest = req

	return koutaku.handleStreamRequest(ctx, koutakuReq)
}

// ImageEditRequest sends an image edit request to the specified provider.
func (koutaku *Koutaku) ImageEditRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuImageEditRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "image edit request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ImageEditRequest,
			},
		}
	}
	if (req.Input == nil || req.Input.Images == nil || len(req.Input.Images) == 0) && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "images not provided for image edit request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.ImageEditRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}
	// Prompt is not required for certain operation types that work without a text prompt
	var imageEditParamsType *string
	if req.Params != nil {
		imageEditParamsType = req.Params.Type
	}
	if !isPromptOptionalImageEditType(imageEditParamsType) &&
		(req.Input == nil || req.Input.Prompt == "") && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "prompt not provided for image edit request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.ImageEditRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ImageEditRequest
	koutakuReq.ImageEditRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}

	if response == nil || response.ImageGenerationResponse == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "received nil response from provider",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.ImageEditRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	return response.ImageGenerationResponse, nil
}

// ImageEditStreamRequest sends an image edit stream request to the specified provider.
func (koutaku *Koutaku) ImageEditStreamRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuImageEditRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "image edit stream request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ImageEditStreamRequest,
			},
		}
	}
	if (req.Input == nil || req.Input.Images == nil || len(req.Input.Images) == 0) && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "images not provided for image edit stream request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.ImageEditStreamRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}
	// Prompt is not required for certain operation types that work without a text prompt
	var imageEditStreamParamsType *string
	if req.Params != nil {
		imageEditStreamParamsType = req.Params.Type
	}
	if !isPromptOptionalImageEditType(imageEditStreamParamsType) &&
		(req.Input == nil || req.Input.Prompt == "") && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "prompt not provided for image edit stream request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.ImageEditStreamRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ImageEditStreamRequest
	koutakuReq.ImageEditRequest = req

	return koutaku.handleStreamRequest(ctx, koutakuReq)
}

// ImageVariationRequest sends an image variation request to the specified provider.
func (koutaku *Koutaku) ImageVariationRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuImageVariationRequest) (*schemas.KoutakuImageGenerationResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "image variation request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ImageVariationRequest,
			},
		}
	}
	if (req.Input == nil || req.Input.Image.Image == nil || len(req.Input.Image.Image) == 0) && !isLargePayloadPassthrough(ctx) {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "image not provided for image variation request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.ImageVariationRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ImageVariationRequest
	koutakuReq.ImageVariationRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}

	if response == nil || response.ImageGenerationResponse == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "received nil response from provider",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.ImageVariationRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	return response.ImageGenerationResponse, nil
}

// VideoGenerationRequest sends a video generation request to the specified provider.
func (koutaku *Koutaku) VideoGenerationRequest(ctx *schemas.KoutakuContext,
	req *schemas.KoutakuVideoGenerationRequest,
) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "video generation request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoGenerationRequest,
			},
		}
	}
	if req.Input == nil || req.Input.Prompt == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "prompt not provided for video generation request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.VideoGenerationRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.VideoGenerationRequest
	koutakuReq.VideoGenerationRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	if response == nil || response.VideoGenerationResponse == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "received nil response from provider",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            schemas.VideoGenerationRequest,
				Provider:               req.Provider,
				OriginalModelRequested: req.Model,
				ResolvedModelUsed:      req.Model,
			},
		}
	}

	return response.VideoGenerationResponse, nil
}

func (koutaku *Koutaku) VideoRetrieveRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuVideoRetrieveRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "video retrieve request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoRetrieveRequest,
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for video retrieve request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoRetrieveRequest,
			},
		}
	}
	if req.ID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "video_id is required for video retrieve request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoRetrieveRequest,
				Provider:    req.Provider,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.VideoRetrieveRequest
	koutakuReq.VideoRetrieveRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	if response == nil || response.VideoGenerationResponse == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "received nil response from provider",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoRetrieveRequest,
				Provider:    req.Provider,
			},
		}
	}
	return response.VideoGenerationResponse, nil
}

// VideoDownloadRequest downloads video content from the provider.
func (koutaku *Koutaku) VideoDownloadRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuVideoDownloadRequest) (*schemas.KoutakuVideoDownloadResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "video download request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoDownloadRequest,
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for video download request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoDownloadRequest,
			},
		}
	}
	if req.ID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "video_id is required for video download request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoDownloadRequest,
				Provider:    req.Provider,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.VideoDownloadRequest
	koutakuReq.VideoDownloadRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.VideoDownloadResponse, nil
}

func (koutaku *Koutaku) VideoRemixRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuVideoRemixRequest) (*schemas.KoutakuVideoGenerationResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "video remix request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoRemixRequest,
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for video remix request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoRemixRequest,
			},
		}
	}
	if req.ID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "video_id is required for video remix request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoRemixRequest,
				Provider:    req.Provider,
			},
		}
	}
	if req.Input == nil || req.Input.Prompt == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "prompt is required for video remix request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoRemixRequest,
				Provider:    req.Provider,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.VideoRemixRequest
	koutakuReq.VideoRemixRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	if response == nil || response.VideoGenerationResponse == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "received nil response from provider",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoRemixRequest,
				Provider:    req.Provider,
			},
		}
	}
	return response.VideoGenerationResponse, nil
}

func (koutaku *Koutaku) VideoListRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuVideoListRequest) (*schemas.KoutakuVideoListResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "video list request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoListRequest,
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for video list request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoListRequest,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.VideoListRequest
	koutakuReq.VideoListRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.VideoListResponse, nil
}

func (koutaku *Koutaku) VideoDeleteRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuVideoDeleteRequest) (*schemas.KoutakuVideoDeleteResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "video delete request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoDeleteRequest,
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for video delete request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoDeleteRequest,
			},
		}
	}
	if req.ID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "video_id is required for video delete request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.VideoDeleteRequest,
				Provider:    req.Provider,
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.VideoDeleteRequest
	koutakuReq.VideoDeleteRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.VideoDeleteResponse, nil
}

// BatchCreateRequest creates a new batch job for asynchronous processing.
func (koutaku *Koutaku) BatchCreateRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuBatchCreateRequest) (*schemas.KoutakuBatchCreateResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "batch create request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for batch create request",
			},
		}
	}
	if req.InputFileID == "" && len(req.Requests) == 0 {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "either input_file_id or requests is required for batch create request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	provider := koutaku.getProviderByKey(req.Provider)
	if provider == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider not found for batch create request",
			},
		}
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.BatchCreateRequest
	koutakuReq.BatchCreateRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.BatchCreateResponse, nil
}

// BatchListRequest lists batch jobs for the specified provider.
func (koutaku *Koutaku) BatchListRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuBatchListRequest) (*schemas.KoutakuBatchListResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "batch list request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for batch list request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.BatchListRequest
	koutakuReq.BatchListRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.BatchListResponse, nil
}

// BatchRetrieveRequest retrieves a specific batch job.
func (koutaku *Koutaku) BatchRetrieveRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuBatchRetrieveRequest) (*schemas.KoutakuBatchRetrieveResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "batch retrieve request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for batch retrieve request",
			},
		}
	}
	if req.BatchID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "batch_id is required for batch retrieve request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.BatchRetrieveRequest
	koutakuReq.BatchRetrieveRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.BatchRetrieveResponse, nil
}

// BatchCancelRequest cancels a batch job.
func (koutaku *Koutaku) BatchCancelRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuBatchCancelRequest) (*schemas.KoutakuBatchCancelResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "batch cancel request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for batch cancel request",
			},
		}
	}
	if req.BatchID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "batch_id is required for batch cancel request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.BatchCancelRequest
	koutakuReq.BatchCancelRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.BatchCancelResponse, nil
}

// BatchDeleteRequest deletes a batch job.
func (koutaku *Koutaku) BatchDeleteRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuBatchDeleteRequest) (*schemas.KoutakuBatchDeleteResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "batch delete request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for batch delete request",
			},
		}
	}
	if req.BatchID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "batch_id is required for batch delete request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.BatchDeleteRequest
	koutakuReq.BatchDeleteRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.BatchDeleteResponse, nil
}

// BatchResultsRequest retrieves results from a completed batch job.
func (koutaku *Koutaku) BatchResultsRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuBatchResultsRequest) (*schemas.KoutakuBatchResultsResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "batch results request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.BatchResultsRequest,
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for batch results request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.BatchResultsRequest,
			},
		}
	}
	if req.BatchID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "batch_id is required for batch results request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.BatchResultsRequest,
				Provider:    req.Provider,
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.BatchResultsRequest
	koutakuReq.BatchResultsRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.BatchResultsResponse, nil
}

// FileUploadRequest uploads a file to the specified provider.
func (koutaku *Koutaku) FileUploadRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuFileUploadRequest) (*schemas.KoutakuFileUploadResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "file upload request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.FileUploadRequest,
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for file upload request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.FileUploadRequest,
			},
		}
	}
	if len(req.File) == 0 {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "file content is required for file upload request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.FileUploadRequest,
				Provider:    req.Provider,
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.FileUploadRequest
	koutakuReq.FileUploadRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.FileUploadResponse, nil
}

// FileListRequest lists files from the specified provider.
func (koutaku *Koutaku) FileListRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuFileListRequest) (*schemas.KoutakuFileListResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "file list request is nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.FileListRequest,
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for file list request",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.FileListRequest,
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.FileListRequest
	koutakuReq.FileListRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.FileListResponse, nil
}

// FileRetrieveRequest retrieves file metadata from the specified provider.
func (koutaku *Koutaku) FileRetrieveRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuFileRetrieveRequest) (*schemas.KoutakuFileRetrieveResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "file retrieve request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for file retrieve request",
			},
		}
	}
	if req.FileID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "file_id is required for file retrieve request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.FileRetrieveRequest
	koutakuReq.FileRetrieveRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.FileRetrieveResponse, nil
}

// FileDeleteRequest deletes a file from the specified provider.
func (koutaku *Koutaku) FileDeleteRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuFileDeleteRequest) (*schemas.KoutakuFileDeleteResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "file delete request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for file delete request",
			},
		}
	}
	if req.FileID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "file_id is required for file delete request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.FileDeleteRequest
	koutakuReq.FileDeleteRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.FileDeleteResponse, nil
}

// FileContentRequest downloads file content from the specified provider.
func (koutaku *Koutaku) FileContentRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuFileContentRequest) (*schemas.KoutakuFileContentResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "file content request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for file content request",
			},
		}
	}
	if req.FileID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "file_id is required for file content request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.FileContentRequest
	koutakuReq.FileContentRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.FileContentResponse, nil
}

func (koutaku *Koutaku) Passthrough(
	ctx *schemas.KoutakuContext,
	provider schemas.ModelProvider,
	req *schemas.KoutakuPassthroughRequest,
) (*schemas.KoutakuPassthroughResponse, *schemas.KoutakuError) {
	if req == nil {
		sc := fasthttp.StatusBadRequest
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			StatusCode:     &sc,
			Error:          &schemas.ErrorField{Message: "passthrough request is nil"},
		}
	}

	req.Provider = provider

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.PassthroughRequest
	koutakuReq.PassthroughRequest = req

	resp, koutakuErr := koutaku.handleRequest(ctx, koutakuReq)
	if koutakuErr != nil {
		return nil, koutakuErr
	}
	if resp == nil || resp.PassthroughResponse == nil {
		sc := fasthttp.StatusBadGateway
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			StatusCode:     &sc,
			Error:          &schemas.ErrorField{Message: "provider returned nil passthrough response"},
		}
	}
	return resp.PassthroughResponse, nil
}

func (koutaku *Koutaku) PassthroughStream(
	ctx *schemas.KoutakuContext,
	provider schemas.ModelProvider,
	req *schemas.KoutakuPassthroughRequest,
) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	if req == nil {
		sc := fasthttp.StatusBadRequest
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			StatusCode:     &sc,
			Error:          &schemas.ErrorField{Message: "passthrough request is nil"},
		}
	}

	req.Provider = provider

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.PassthroughStreamRequest
	koutakuReq.PassthroughRequest = req

	return koutaku.handleStreamRequest(ctx, koutakuReq)
}

// ExecuteChatMCPTool executes an MCP tool call and returns the result as a chat message.
// This is the main public API for manual MCP tool execution in Chat format.
//
// Parameters:
//   - ctx: Execution context
//   - toolCall: The tool call to execute (from assistant message)
//
// Returns:
//   - *schemas.ChatMessage: Tool message with execution result
//   - *schemas.KoutakuError: Any execution error
func (koutaku *Koutaku) ExecuteChatMCPTool(ctx *schemas.KoutakuContext, toolCall *schemas.ChatAssistantMessageToolCall) (*schemas.ChatMessage, *schemas.KoutakuError) {
	// Handle nil context early to prevent issues downstream
	if ctx == nil {
		ctx = koutaku.ctx
	}

	// Validate toolCall is not nil
	if toolCall == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "toolCall cannot be nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ChatCompletionRequest,
			},
		}
	}

	// Get MCP request from pool and populate
	mcpRequest := koutaku.getMCPRequest()
	mcpRequest.RequestType = schemas.MCPRequestTypeChatToolCall
	mcpRequest.ChatAssistantMessageToolCall = toolCall
	defer koutaku.releaseMCPRequest(mcpRequest)

	// Execute with common handler
	result, err := koutaku.handleMCPToolExecution(ctx, mcpRequest, schemas.ChatCompletionRequest)
	if err != nil {
		return nil, err
	}

	// Validate and extract chat message from result
	if result == nil || result.ChatMessage == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "MCP tool execution returned nil chat message",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ChatCompletionRequest,
			},
		}
	}

	return result.ChatMessage, nil
}

// ExecuteResponsesMCPTool executes an MCP tool call and returns the result as a responses message.
// This is the main public API for manual MCP tool execution in Responses format.
//
// Parameters:
//   - ctx: Execution context
//   - toolCall: The tool call to execute (from assistant message)
//
// Returns:
//   - *schemas.ResponsesMessage: Tool message with execution result
//   - *schemas.KoutakuError: Any execution error
func (koutaku *Koutaku) ExecuteResponsesMCPTool(ctx *schemas.KoutakuContext, toolCall *schemas.ResponsesToolMessage) (*schemas.ResponsesMessage, *schemas.KoutakuError) {
	// Handle nil context early to prevent issues downstream
	if ctx == nil {
		ctx = koutaku.ctx
	}

	// Validate toolCall is not nil
	if toolCall == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "toolCall cannot be nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ResponsesRequest,
			},
		}
	}

	// Get MCP request from pool and populate
	mcpRequest := koutaku.getMCPRequest()
	mcpRequest.RequestType = schemas.MCPRequestTypeResponsesToolCall
	mcpRequest.ResponsesToolMessage = toolCall
	defer koutaku.releaseMCPRequest(mcpRequest)

	// Execute with common handler
	result, err := koutaku.handleMCPToolExecution(ctx, mcpRequest, schemas.ResponsesRequest)
	if err != nil {
		return nil, err
	}

	// Validate and extract responses message from result
	if result == nil || result.ResponsesMessage == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "MCP tool execution returned nil responses message",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: schemas.ResponsesRequest,
			},
		}
	}

	return result.ResponsesMessage, nil
}

// ContainerCreateRequest creates a new container.
func (koutaku *Koutaku) ContainerCreateRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuContainerCreateRequest) (*schemas.KoutakuContainerCreateResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container create request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for container create request",
			},
		}
	}
	if req.Name == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "name is required for container create request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ContainerCreateRequest
	koutakuReq.ContainerCreateRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.ContainerCreateResponse, nil
}

// ContainerListRequest lists containers.
func (koutaku *Koutaku) ContainerListRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuContainerListRequest) (*schemas.KoutakuContainerListResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container list request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for container list request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ContainerListRequest
	koutakuReq.ContainerListRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.ContainerListResponse, nil
}

// ContainerRetrieveRequest retrieves a specific container.
func (koutaku *Koutaku) ContainerRetrieveRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuContainerRetrieveRequest) (*schemas.KoutakuContainerRetrieveResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container retrieve request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for container retrieve request",
			},
		}
	}
	if req.ContainerID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container_id is required for container retrieve request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ContainerRetrieveRequest
	koutakuReq.ContainerRetrieveRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.ContainerRetrieveResponse, nil
}

// ContainerDeleteRequest deletes a container.
func (koutaku *Koutaku) ContainerDeleteRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuContainerDeleteRequest) (*schemas.KoutakuContainerDeleteResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container delete request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for container delete request",
			},
		}
	}
	if req.ContainerID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container_id is required for container delete request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ContainerDeleteRequest
	koutakuReq.ContainerDeleteRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.ContainerDeleteResponse, nil
}

// ContainerFileCreateRequest creates a file in a container.
func (koutaku *Koutaku) ContainerFileCreateRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuContainerFileCreateRequest) (*schemas.KoutakuContainerFileCreateResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container file create request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for container file create request",
			},
		}
	}
	if req.ContainerID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container_id is required for container file create request",
			},
		}
	}
	if len(req.File) == 0 && (req.FileID == nil || strings.TrimSpace(*req.FileID) == "") && (req.Path == nil || strings.TrimSpace(*req.Path) == "") {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "one of file, file_id, or path is required for container file create request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ContainerFileCreateRequest
	koutakuReq.ContainerFileCreateRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.ContainerFileCreateResponse, nil
}

// ContainerFileListRequest lists files in a container.
func (koutaku *Koutaku) ContainerFileListRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuContainerFileListRequest) (*schemas.KoutakuContainerFileListResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container file list request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for container file list request",
			},
		}
	}
	if req.ContainerID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container_id is required for container file list request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ContainerFileListRequest
	koutakuReq.ContainerFileListRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.ContainerFileListResponse, nil
}

// ContainerFileRetrieveRequest retrieves a file from a container.
func (koutaku *Koutaku) ContainerFileRetrieveRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuContainerFileRetrieveRequest) (*schemas.KoutakuContainerFileRetrieveResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container file retrieve request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for container file retrieve request",
			},
		}
	}
	if req.ContainerID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container_id is required for container file retrieve request",
			},
		}
	}
	if req.FileID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "file_id is required for container file retrieve request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ContainerFileRetrieveRequest
	koutakuReq.ContainerFileRetrieveRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.ContainerFileRetrieveResponse, nil
}

// ContainerFileContentRequest retrieves the content of a file from a container.
func (koutaku *Koutaku) ContainerFileContentRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuContainerFileContentRequest) (*schemas.KoutakuContainerFileContentResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container file content request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for container file content request",
			},
		}
	}
	if req.ContainerID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container_id is required for container file content request",
			},
		}
	}
	if req.FileID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "file_id is required for container file content request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ContainerFileContentRequest
	koutakuReq.ContainerFileContentRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.ContainerFileContentResponse, nil
}

// ContainerFileDeleteRequest deletes a file from a container.
func (koutaku *Koutaku) ContainerFileDeleteRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuContainerFileDeleteRequest) (*schemas.KoutakuContainerFileDeleteResponse, *schemas.KoutakuError) {
	if req == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container file delete request is nil",
			},
		}
	}
	if req.Provider == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "provider is required for container file delete request",
			},
		}
	}
	if req.ContainerID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "container_id is required for container file delete request",
			},
		}
	}
	if req.FileID == "" {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "file_id is required for container file delete request",
			},
		}
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutakuReq := koutaku.getKoutakuRequest()
	koutakuReq.RequestType = schemas.ContainerFileDeleteRequest
	koutakuReq.ContainerFileDeleteRequest = req

	response, err := koutaku.handleRequest(ctx, koutakuReq)
	if err != nil {
		return nil, err
	}
	return response.ContainerFileDeleteResponse, nil
}

// RemovePlugin removes a plugin from the server.
func (koutaku *Koutaku) RemovePlugin(name string, pluginTypes []schemas.PluginType) error {
	for _, pluginType := range pluginTypes {
		switch pluginType {
		case schemas.PluginTypeLLM:
			err := koutaku.removeLLMPlugin(name)
			if err != nil {
				return err
			}
		case schemas.PluginTypeMCP:
			err := koutaku.removeMCPPlugin(name)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// removeLLMPlugin removes an LLM plugin from the server.
func (koutaku *Koutaku) removeLLMPlugin(name string) error {
	for {
		oldPlugins := koutaku.llmPlugins.Load()
		if oldPlugins == nil {
			return nil
		}
		var pluginToCleanup schemas.LLMPlugin
		found := false
		// Create new slice without the plugin to remove
		newPlugins := make([]schemas.LLMPlugin, 0, len(*oldPlugins))
		for _, p := range *oldPlugins {
			if p.GetName() == name {
				pluginToCleanup = p
				koutaku.logger.Debug("removing LLM plugin %s", name)
				found = true
			} else {
				newPlugins = append(newPlugins, p)
			}
		}
		if !found {
			return nil
		}
		// Atomic compare-and-swap
		if koutaku.llmPlugins.CompareAndSwap(oldPlugins, &newPlugins) {
			// Cleanup the old plugin
			err := pluginToCleanup.Cleanup()
			if err != nil {
				koutaku.logger.Warn("failed to cleanup old LLM plugin %s: %v", pluginToCleanup.GetName(), err)
			}
			return nil
		}
		// Retrying as swapping did not work
	}
}

// removeMCPPlugin removes an MCP plugin from the server.
func (koutaku *Koutaku) removeMCPPlugin(name string) error {
	for {
		oldPlugins := koutaku.mcpPlugins.Load()
		if oldPlugins == nil {
			return nil
		}
		var pluginToCleanup schemas.MCPPlugin
		found := false
		// Create new slice without the plugin to remove
		newPlugins := make([]schemas.MCPPlugin, 0, len(*oldPlugins))
		for _, p := range *oldPlugins {
			if p.GetName() == name {
				pluginToCleanup = p
				koutaku.logger.Debug("removing MCP plugin %s", name)
				found = true
			} else {
				newPlugins = append(newPlugins, p)
			}
		}
		if !found {
			return nil
		}
		// Atomic compare-and-swap
		if koutaku.mcpPlugins.CompareAndSwap(oldPlugins, &newPlugins) {
			// Cleanup the old plugin
			err := pluginToCleanup.Cleanup()
			if err != nil {
				koutaku.logger.Warn("failed to cleanup old MCP plugin %s: %v", pluginToCleanup.GetName(), err)
			}
			return nil
		}
		// Retrying as swapping did not work
	}
}

// ReloadPlugin reloads a plugin with new instance
// During the reload - it's stop the world phase where we take a global lock on the plugin mutex
func (koutaku *Koutaku) ReloadPlugin(plugin schemas.BasePlugin, pluginTypes []schemas.PluginType) error {
	for _, pluginType := range pluginTypes {
		switch pluginType {
		case schemas.PluginTypeLLM:
			llmPlugin, ok := plugin.(schemas.LLMPlugin)
			if !ok {
				return fmt.Errorf("plugin %s is not an LLMPlugin", plugin.GetName())
			}
			err := koutaku.reloadLLMPlugin(llmPlugin)
			if err != nil {
				return err
			}
		case schemas.PluginTypeMCP:
			mcpPlugin, ok := plugin.(schemas.MCPPlugin)
			if !ok {
				return fmt.Errorf("plugin %s is not an MCPPlugin", plugin.GetName())
			}
			err := koutaku.reloadMCPPlugin(mcpPlugin)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// reloadLLMPlugin reloads an LLM plugin with new instance
func (koutaku *Koutaku) reloadLLMPlugin(plugin schemas.LLMPlugin) error {
	for {
		var pluginToCleanup schemas.LLMPlugin
		found := false
		oldPlugins := koutaku.llmPlugins.Load()

		// Create new slice with replaced plugin or initialize empty slice
		var newPlugins []schemas.LLMPlugin
		if oldPlugins == nil {
			// Initialize new empty slice for the first plugin
			newPlugins = make([]schemas.LLMPlugin, 0)
		} else {
			newPlugins = make([]schemas.LLMPlugin, len(*oldPlugins))
			copy(newPlugins, *oldPlugins)
		}

		for i, p := range newPlugins {
			if p.GetName() == plugin.GetName() {
				// Cleaning up old plugin before replacing it
				pluginToCleanup = p
				koutaku.logger.Debug("replacing LLM plugin %s with new instance", plugin.GetName())
				newPlugins[i] = plugin
				found = true
				break
			}
		}
		if !found {
			// This means that user is adding a new plugin
			koutaku.logger.Debug("adding new LLM plugin %s", plugin.GetName())
			newPlugins = append(newPlugins, plugin)
		}
		// Atomic compare-and-swap
		if koutaku.llmPlugins.CompareAndSwap(oldPlugins, &newPlugins) {
			// Cleanup the old plugin
			if found && pluginToCleanup != nil {
				err := pluginToCleanup.Cleanup()
				if err != nil {
					koutaku.logger.Warn("failed to cleanup old LLM plugin %s: %v", pluginToCleanup.GetName(), err)
				}
			}
			return nil
		}
		// Retrying as swapping did not work
	}
}

// reloadMCPPlugin reloads an MCP plugin with new instance
func (koutaku *Koutaku) reloadMCPPlugin(plugin schemas.MCPPlugin) error {
	for {
		var pluginToCleanup schemas.MCPPlugin
		found := false
		oldPlugins := koutaku.mcpPlugins.Load()
		if oldPlugins == nil {
			return nil
		}
		// Create new slice with replaced plugin
		newPlugins := make([]schemas.MCPPlugin, len(*oldPlugins))
		copy(newPlugins, *oldPlugins)
		for i, p := range newPlugins {
			if p.GetName() == plugin.GetName() {
				// Cleaning up old plugin before replacing it
				pluginToCleanup = p
				koutaku.logger.Debug("replacing MCP plugin %s with new instance", plugin.GetName())
				newPlugins[i] = plugin
				found = true
				break
			}
		}
		if !found {
			// This means that user is adding a new plugin
			koutaku.logger.Debug("adding new MCP plugin %s", plugin.GetName())
			newPlugins = append(newPlugins, plugin)
		}
		// Atomic compare-and-swap
		if koutaku.mcpPlugins.CompareAndSwap(oldPlugins, &newPlugins) {
			// Cleanup the old plugin
			if found && pluginToCleanup != nil {
				err := pluginToCleanup.Cleanup()
				if err != nil {
					koutaku.logger.Warn("failed to cleanup old MCP plugin %s: %v", pluginToCleanup.GetName(), err)
				}
			}
			return nil
		}
		// Retrying as swapping did not work
	}
}

// ReorderPlugins reorders all plugin slices (LLM, MCP) to match the given
// base plugin name ordering. This should be called after SortAndRebuildPlugins
// on the config layer to sync the core's execution order.
// Plugins not in the ordering are appended at the end (defensive).
func (koutaku *Koutaku) ReorderPlugins(orderedNames []string) {
	pos := make(map[string]int, len(orderedNames))
	for i, name := range orderedNames {
		pos[name] = i
	}
	reorderAtomicSlice(&koutaku.llmPlugins, pos)
	reorderAtomicSlice(&koutaku.mcpPlugins, pos)
}

// pluginWithName is satisfied by both LLMPlugin and MCPPlugin.
type pluginWithName interface {
	GetName() string
}

// reorderAtomicSlice atomically reorders the plugin slice stored behind ptr
// so that plugins appear in the order given by pos (name → position).
// Uses CAS retry for lock-free safety.
func reorderAtomicSlice[T pluginWithName](ptr *atomic.Pointer[[]T], pos map[string]int) {
	for {
		old := ptr.Load()
		if old == nil || len(*old) == 0 {
			return
		}
		reordered := make([]T, len(*old))
		copy(reordered, *old)
		sort.SliceStable(reordered, func(i, j int) bool {
			iPos, iOk := pos[reordered[i].GetName()]
			jPos, jOk := pos[reordered[j].GetName()]
			if !iOk && !jOk {
				return false
			}
			if !iOk {
				return false
			}
			if !jOk {
				return true
			}
			return iPos < jPos
		})
		if ptr.CompareAndSwap(old, &reordered) {
			return
		}
	}
}

// GetConfiguredProviders returns the configured providers.
//
// Returns:
//   - []schemas.ModelProvider: List of configured providers
//   - error: Any error that occurred during the retrieval process
//
// Example:
//
//	providers, err := koutaku.GetConfiguredProviders()
//	if err != nil {
//		return nil, err
//	}
//	fmt.Println(providers)
func (koutaku *Koutaku) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	providers := koutaku.providers.Load()
	if providers == nil {
		return nil, fmt.Errorf("no providers configured")
	}
	modelProviders := make([]schemas.ModelProvider, len(*providers))
	for i, provider := range *providers {
		modelProviders[i] = provider.GetProviderKey()
	}
	return modelProviders, nil
}

// RemoveProvider removes a provider from the server.
// This method gracefully stops all workers for the provider,
// closes the request queue, and removes the provider from the providers slice.
//
// Parameters:
//   - providerKey: The provider to remove
//
// Returns:
//   - error: Any error that occurred during the removal process
func (koutaku *Koutaku) RemoveProvider(providerKey schemas.ModelProvider) error {
	koutaku.logger.Info("Removing provider %s", providerKey)
	providerMutex := koutaku.getProviderMutex(providerKey)
	providerMutex.Lock()
	defer providerMutex.Unlock()

	// Step 1: Load the ProviderQueue and verify provider exists
	pqValue, exists := koutaku.requestQueues.Load(providerKey)
	if !exists {
		return fmt.Errorf("provider %s not found in request queues", providerKey)
	}
	pq := pqValue.(*ProviderQueue)

	// Step 2: Signal closing. Blocks new producers (isClosing() returns true) and
	// causes idle workers to drain remaining buffered requests with errors then exit.
	pq.signalClosing()
	koutaku.logger.Debug("signaled closing for provider %s", providerKey)

	// Step 3: Wait for all workers to finish in-flight requests and exit.
	waitGroup, exists := koutaku.waitGroups.Load(providerKey)
	if exists {
		waitGroup.(*sync.WaitGroup).Wait()
		koutaku.logger.Debug("all workers for provider %s have stopped", providerKey)
	}

	// Step 3b: Final drain sweep — see drainQueueWithErrors for full explanation.
	koutaku.drainQueueWithErrors(pq)

	// Step 4: Remove the provider from the request queues.
	koutaku.requestQueues.Delete(providerKey)

	// Step 5: Remove the provider from the wait groups.
	koutaku.waitGroups.Delete(providerKey)

	// Step 6: Remove the provider from the providers slice.
	if err := koutaku.removeProviderFromSlice(providerKey); err != nil {
		koutaku.logger.Error(
			"provider %s was removed from queues but could not be removed from the providers slice — "+
				"koutaku.providers is now inconsistent. "+
				"To recover: retry RemoveProvider(%s), or restart Koutaku if that fails.",
			providerKey, providerKey,
		)
		return err
	}

	koutaku.logger.Info("successfully removed provider %s", providerKey)
	schemas.UnregisterKnownProvider(providerKey)
	return nil
}

// UpdateProvider dynamically updates a provider with new configuration.
// This method gracefully recreates the provider instance with updated settings,
// stops existing workers, creates a new queue with updated settings,
// and starts new workers with the updated provider and concurrency configuration.
//
// Parameters:
//   - providerKey: The provider to update
//
// Returns:
//   - error: Any error that occurred during the update process
//
// Note: This operation will temporarily pause request processing for the specified provider
// while the transition occurs. In-flight requests will complete before workers are stopped.
// Buffered requests in the old queue will be transferred to the new queue to prevent loss.
//
// Concurrency safety — no-worker window:
// UpdateProvider holds a per-provider write lock (providerMutex.Lock) for its entire
// duration. All producer paths (tryRequest, tryStreamRequest) acquire the corresponding
// read lock inside getProviderQueue before they can look up or enqueue into any queue.
// This means no producer can observe or enqueue into newPq until UpdateProvider returns
// and releases the write lock — at which point new workers are already running and
// consuming newPq. There is therefore no window where newPq is visible to producers
// but has zero workers.
func (koutaku *Koutaku) UpdateProvider(providerKey schemas.ModelProvider) error {
	koutaku.logger.Info(fmt.Sprintf("Updating provider configuration for provider %s", providerKey))
	// Get the updated configuration from the account
	providerConfig, err := koutaku.account.GetConfigForProvider(providerKey)
	if err != nil {
		return fmt.Errorf("failed to get updated config for provider %s: %v", providerKey, err)
	}
	if providerConfig == nil {
		return fmt.Errorf("config is nil for provider %s", providerKey)
	}
	// Lock the provider to prevent concurrent access during update
	providerMutex := koutaku.getProviderMutex(providerKey)
	providerMutex.Lock()
	defer providerMutex.Unlock()

	// Check if provider currently exists
	oldPqValue, exists := koutaku.requestQueues.Load(providerKey)
	if !exists {
		koutaku.logger.Debug("provider %s not currently active, initializing with new configuration", providerKey)
		// If provider doesn't exist, just prepare it with new configuration
		return koutaku.prepareProvider(providerKey, providerConfig)
	}

	oldPq := oldPqValue.(*ProviderQueue)

	koutaku.logger.Debug("gracefully stopping existing workers for provider %s", providerKey)

	// Step 1: Create new ProviderQueue with updated buffer size
	newPq := &ProviderQueue{
		queue:      make(chan *ChannelMessage, providerConfig.ConcurrencyAndBufferSize.BufferSize),
		done:       make(chan struct{}),
		signalOnce: sync.Once{},
	}

	// Step 2: Atomically replace the queue so new producers immediately use newPq.
	koutaku.requestQueues.Store(providerKey, newPq)
	koutaku.logger.Debug("stored new queue for provider %s, new producers will use it", providerKey)

	// Step 3: Transfer buffered requests from the old queue to the new queue BEFORE
	// signalling workers to stop. This ensures buffered requests are processed by the
	// new workers rather than being drained with errors.
	// Old workers are still running and may consume some items concurrently — that is
	// fine, they process them normally.
	// If newPq is full during transfer, all remaining buffered requests are cancelled
	// immediately rather than blocking — this avoids the deadlock where transfer goroutines
	// wait for space that only opens once new workers start (which can't happen until
	// the transfer completes).
	transferredCount := 0
	cancelledCount := 0
	for {
		select {
		case msg := <-oldPq.queue:
			select {
			case newPq.queue <- msg:
				transferredCount++
			default:
				// newPq is full — cancel this message and all remaining in oldPq.
				cancelMsg := func(r *ChannelMessage) {
					prov, mod, _ := r.KoutakuRequest.GetRequestFields()
					select {
					case r.Err <- schemas.KoutakuError{
						IsKoutakuError: false,
						Error:          &schemas.ErrorField{Message: "request failed during provider concurrency update: queue full"},
						ExtraFields: schemas.KoutakuErrorExtraFields{
							RequestType:            r.RequestType,
							Provider:               prov,
							OriginalModelRequested: mod,
						},
					}:
					case <-r.Context.Done():
					}
				}
				cancelMsg(msg)
				cancelledCount++
				for {
					select {
					case r := <-oldPq.queue:
						cancelMsg(r)
						cancelledCount++
					default:
						goto transferComplete
					}
				}
			}
		default:
			// No more buffered messages
			goto transferComplete
		}
	}

transferComplete:
	if transferredCount > 0 {
		koutaku.logger.Info("transferred %d buffered requests to new queue for provider %s", transferredCount, providerKey)
	}
	if cancelledCount > 0 {
		koutaku.logger.Warn("cancelled %d buffered requests during transfer for provider %s: new queue was full", cancelledCount, providerKey)
	}

	// Step 4: Signal the old queue is closing. Producers that still hold a reference to
	// oldPq will detect this via isClosing() and transparently re-route to newPq.
	// This happens after the transfer so the new queue is already populated before
	// stale producers attempt their re-route.
	oldPq.signalClosing()
	koutaku.logger.Debug("signaled closing for old queue of provider %s", providerKey)

	// Step 5: Wait for all existing workers to finish processing in-flight requests.
	// Workers exit via oldPq.done (signalled above).
	waitGroup, exists := koutaku.waitGroups.Load(providerKey)
	if exists {
		waitGroup.(*sync.WaitGroup).Wait()
		koutaku.logger.Debug("all workers for provider %s have stopped", providerKey)
	}

	// Step 5b: Final drain sweep — see drainQueueWithErrors for full explanation.
	koutaku.drainQueueWithErrors(oldPq)

	// Step 6: Create new wait group for the updated workers.
	koutaku.waitGroups.Store(providerKey, &sync.WaitGroup{})

	// Step 7: Create provider instance.
	provider, err := koutaku.createBaseProvider(providerKey, providerConfig)
	if err != nil {
		// Roll back: signal closing, remove from map, then drain.
		// Order matters: Delete before drainQueueWithErrors so that producers
		// re-routing via requestQueues.Load find nothing and return "provider
		// shutting down" immediately, narrowing the TOCTOU window before the sweep.
		newPq.signalClosing()
		koutaku.requestQueues.Delete(providerKey)
		koutaku.waitGroups.Delete(providerKey)
		koutaku.drainQueueWithErrors(newPq)
		if sliceErr := koutaku.removeProviderFromSlice(providerKey); sliceErr != nil {
			koutaku.logger.Error(
				"UpdateProvider rollback for %s is incomplete — provider was removed from queues "+
					"but could not be removed from the providers slice: %v. "+
					"koutaku.providers is now inconsistent. "+
					"To recover: call RemoveProvider(%s) then AddProvider to re-register it, "+
					"or restart Koutaku if that fails.",
				providerKey, sliceErr, providerKey,
			)
		}
		return fmt.Errorf("provider update for %s failed during initialization; provider has been removed — re-add or retry UpdateProvider to restore it: %v", providerKey, err)
	}

	// Step 8: Atomically replace the provider in the providers slice.
	// This must happen before starting new workers to prevent stale reads
	koutaku.logger.Debug("atomically replacing provider instance in providers slice for %s", providerKey)

	replacementAttempts := 0
	maxReplacementAttempts := 100 // Prevent infinite loops in high-contention scenarios

	for {
		replacementAttempts++
		if replacementAttempts > maxReplacementAttempts {
			newPq.signalClosing()
			koutaku.requestQueues.Delete(providerKey)
			koutaku.waitGroups.Delete(providerKey)
			koutaku.drainQueueWithErrors(newPq)
			if sliceErr := koutaku.removeProviderFromSlice(providerKey); sliceErr != nil {
				koutaku.logger.Error(
					"UpdateProvider rollback for %s is incomplete — provider was removed from queues "+
						"but could not be removed from the providers slice: %v. "+
						"koutaku.providers is now inconsistent. "+
						"To recover: call RemoveProvider(%s) then AddProvider to re-register it, "+
						"or restart Koutaku if that fails.",
					providerKey, sliceErr, providerKey,
				)
			}
			return fmt.Errorf("failed to replace provider %s in providers slice after %d attempts; provider has been removed — re-add or retry UpdateProvider to restore it", providerKey, maxReplacementAttempts)
		}

		oldPtr := koutaku.providers.Load()
		var oldSlice []schemas.Provider
		if oldPtr != nil {
			oldSlice = *oldPtr
		}

		// Create new slice without the old provider of this key
		// Use exact capacity to avoid allocations
		newSlice := make([]schemas.Provider, 0, len(oldSlice))
		oldProviderFound := false

		for _, existingProvider := range oldSlice {
			if existingProvider.GetProviderKey() != providerKey {
				newSlice = append(newSlice, existingProvider)
			} else {
				oldProviderFound = true
			}
		}

		// Add the new provider
		newSlice = append(newSlice, provider)

		if koutaku.providers.CompareAndSwap(oldPtr, &newSlice) {
			if oldProviderFound {
				koutaku.logger.Debug("successfully replaced existing provider instance for %s in providers slice", providerKey)
			} else {
				koutaku.logger.Debug("successfully added new provider instance for %s to providers slice", providerKey)
			}
			break
		}
		// Retrying as swapping did not work (likely due to concurrent modification)
	}

	// Step 9: Start new workers with updated concurrency.
	koutaku.logger.Debug("starting %d new workers for provider %s with buffer size %d",
		providerConfig.ConcurrencyAndBufferSize.Concurrency,
		providerKey,
		providerConfig.ConcurrencyAndBufferSize.BufferSize)

	waitGroupValue, _ := koutaku.waitGroups.Load(providerKey)
	currentWaitGroup := waitGroupValue.(*sync.WaitGroup)

	for range providerConfig.ConcurrencyAndBufferSize.Concurrency {
		currentWaitGroup.Add(1)
		go koutaku.requestWorker(provider, providerConfig, newPq)
	}

	koutaku.logger.Info("successfully updated provider configuration for provider %s", providerKey)
	return nil
}

// GetDropExcessRequests returns the current value of DropExcessRequests
func (koutaku *Koutaku) GetDropExcessRequests() bool {
	return koutaku.dropExcessRequests.Load()
}

// UpdateDropExcessRequests updates the DropExcessRequests setting at runtime.
// This allows for hot-reloading of this configuration value.
func (koutaku *Koutaku) UpdateDropExcessRequests(value bool) {
	koutaku.dropExcessRequests.Store(value)
	koutaku.logger.Info("drop_excess_requests updated to: %v", value)
}

// getProviderMutex gets or creates a mutex for the given provider
func (koutaku *Koutaku) getProviderMutex(providerKey schemas.ModelProvider) *sync.RWMutex {
	mutexValue, _ := koutaku.providerMutexes.LoadOrStore(providerKey, &sync.RWMutex{})
	return mutexValue.(*sync.RWMutex)
}

// removeProviderFromSlice atomically removes the provider with the given key
// from koutaku.providers using a CAS retry loop. Callers hold the per-provider
// write mutex so no concurrent goroutine can re-add this key — contention is
// only from other providers' CAS operations, so the loop converges in at most
// a few iterations under any concurrency level.
// Returns an error if the limit is hit (state will be inconsistent).
func (koutaku *Koutaku) removeProviderFromSlice(providerKey schemas.ModelProvider) error {
	const maxAttempts = 100
	for range maxAttempts {
		oldPtr := koutaku.providers.Load()
		if oldPtr == nil {
			return nil
		}
		oldSlice := *oldPtr
		newSlice := make([]schemas.Provider, 0, len(oldSlice))
		for _, p := range oldSlice {
			if p.GetProviderKey() != providerKey {
				newSlice = append(newSlice, p)
			}
		}
		if koutaku.providers.CompareAndSwap(oldPtr, &newSlice) {
			return nil
		}
	}
	return fmt.Errorf("failed to remove provider %s from providers slice after %d attempts", providerKey, maxAttempts)
}

// MCP PUBLIC API

// RegisterMCPTool registers a typed tool handler with the MCP integration.
// This allows developers to easily add custom tools that will be available
// to all LLM requests processed by this Koutaku instance.
//
// Parameters:
//   - name: Unique tool name
//   - description: Human-readable tool description
//   - handler: Function that handles tool execution
//   - toolSchema: Koutaku tool schema for function calling
//
// Returns:
//   - error: Any registration error
//
// Example:
//
//	type EchoArgs struct {
//	    Message string `json:"message"`
//	}
//
//	err := koutaku.RegisterMCPTool("echo", "Echo a message",
//	    func(args EchoArgs) (string, error) {
//	        return args.Message, nil
//	    }, toolSchema)
func (koutaku *Koutaku) RegisterMCPTool(name, description string, handler func(args any) (string, error), toolSchema schemas.ChatTool) error {
	if koutaku.MCPManager == nil {
		return fmt.Errorf("mcp is not configured in this koutaku instance")
	}

	return koutaku.MCPManager.RegisterTool(name, description, handler, toolSchema)
}

// IMPORTANT: Running the MCP client management operations (GetMCPClients, AddMCPClient, RemoveMCPClient, EditMCPClientTools)
// may temporarily increase latency for incoming requests while the operations are being processed.
// These operations involve network I/O and connection management that require mutex locks
// which can block briefly during execution.

// GetMCPClients returns all MCP clients managed by the Koutaku instance.
//
// Returns:
//   - []schemas.MCPClient: List of all MCP clients
//   - error: Any retrieval error
func (koutaku *Koutaku) GetMCPClients() ([]schemas.MCPClient, error) {
	if koutaku.MCPManager == nil {
		return nil, fmt.Errorf("mcp is not configured in this koutaku instance")
	}

	clients := koutaku.MCPManager.GetClients()
	clientsInConfig := make([]schemas.MCPClient, 0, len(clients))

	for _, client := range clients {
		tools := make([]schemas.ChatToolFunction, 0, len(client.ToolMap))
		for _, tool := range client.ToolMap {
			if tool.Function != nil {
				// Create a deep copy (for name) of the tool function to avoid modifying the original
				toolFunction := schemas.ChatToolFunction{}
				toolFunction.Name = tool.Function.Name
				toolFunction.Description = tool.Function.Description
				toolFunction.Parameters = tool.Function.Parameters
				toolFunction.Strict = tool.Function.Strict
				// Remove the client prefix from the tool name
				toolFunction.Name = strings.TrimPrefix(toolFunction.Name, client.ExecutionConfig.Name+"-")
				tools = append(tools, toolFunction)
			}
		}

		sort.Slice(tools, func(i, j int) bool {
			return tools[i].Name < tools[j].Name
		})

		clientsInConfig = append(clientsInConfig, schemas.MCPClient{
			Config: client.ExecutionConfig,
			Tools:  tools,
			State:  client.State,
		})
	}

	return clientsInConfig, nil
}

// GetAvailableTools returns the available tools for the given context.
//
// Returns:
//   - []schemas.ChatTool: List of available tools
func (koutaku *Koutaku) GetAvailableMCPTools(ctx *schemas.KoutakuContext) []schemas.ChatTool {
	if koutaku.MCPManager == nil {
		return nil
	}
	return koutaku.MCPManager.GetAvailableTools(ctx)
}

// AddMCPClient adds a new MCP client to the Koutaku instance.
// This allows for dynamic MCP client management at runtime.
//
// Parameters:
//   - config: MCP client configuration
//
// Returns:
//   - error: Any registration error
//
// Example:
//
//	err := koutaku.AddMCPClient(schemas.MCPClientConfig{
//	    Name: "my-mcp-client",
//	    ConnectionType: schemas.MCPConnectionTypeHTTP,
//	    ConnectionString: &url,
//	})
func (koutaku *Koutaku) AddMCPClient(config *schemas.MCPClientConfig) error {
	if koutaku.MCPManager == nil {
		// Use sync.Once to ensure thread-safe initialization
		koutaku.mcpInitOnce.Do(func() {
			// Initialize with empty config - client will be added via AddClient below
			mcpConfig := schemas.MCPConfig{
				ClientConfigs: []*schemas.MCPClientConfig{},
			}
			// Set up plugin pipeline provider functions for executeCode tool hooks
			mcpConfig.PluginPipelineProvider = func() interface{} {
				return koutaku.getPluginPipeline()
			}
			mcpConfig.ReleasePluginPipeline = func(pipeline interface{}) {
				if pp, ok := pipeline.(*PluginPipeline); ok {
					koutaku.releasePluginPipeline(pp)
				}
			}
			// Create Starlark CodeMode for code execution (with default config)
			codeMode := starlark.NewStarlarkCodeMode(nil, koutaku.logger)
			koutaku.MCPManager = mcp.NewMCPManager(koutaku.ctx, mcpConfig, koutaku.oauth2Provider, koutaku.logger, codeMode)
		})
	}

	// Handle case where initialization succeeded elsewhere but manager is still nil
	if koutaku.MCPManager == nil {
		return fmt.Errorf("MCP manager is not initialized")
	}

	return koutaku.MCPManager.AddClient(config)
}

// RemoveMCPClient removes an MCP client from the Koutaku instance.
// This allows for dynamic MCP client management at runtime.
//
// Parameters:
//   - id: ID of the client to remove
//
// Returns:
//   - error: Any removal error
//
// Example:
//
//	err := koutaku.RemoveMCPClient("my-mcp-client-id")
//	if err != nil {
//	    log.Fatalf("Failed to remove MCP client: %v", err)
//	}
func (koutaku *Koutaku) RemoveMCPClient(id string) error {
	if koutaku.MCPManager == nil {
		return fmt.Errorf("mcp is not configured in this koutaku instance")
	}

	return koutaku.MCPManager.RemoveClient(id)
}

// SetMCPManager sets the MCP manager for this Koutaku instance.
// This allows injecting a custom MCP manager implementation (e.g., for enterprise features).
// If the provided manager is a concrete *mcp.MCPManager, Koutaku's plugin pipeline is injected
// into the manager's CodeMode so that nested tool calls run through the plugin hooks.
//
// Parameters:
//   - manager: The MCP manager to set (must implement MCPManagerInterface)
func (koutaku *Koutaku) SetMCPManager(manager mcp.MCPManagerInterface) {
	koutaku.MCPManager = manager
	// Inject Koutaku's plugin pipeline into the manager's CodeMode so that
	// nested tool calls (e.g. via Starlark executeCode) run through plugin hooks.
	if m, ok := manager.(*mcp.MCPManager); ok {
		m.SetPluginPipeline(
			func() mcp.PluginPipeline {
				pipeline := koutaku.getPluginPipeline()
				if pp, ok := any(pipeline).(mcp.PluginPipeline); ok {
					return pp
				}
				return nil
			},
			func(pipeline mcp.PluginPipeline) {
				if pp, ok := pipeline.(*PluginPipeline); ok {
					koutaku.releasePluginPipeline(pp)
				}
			},
		)
	}
}

// UpdateMCPClient updates the MCP client.
// This allows for dynamic MCP client tool management at runtime.
//
// Parameters:
//   - id: ID of the client to edit
//   - updatedConfig: Updated MCP client configuration
//
// Returns:
//   - error: Any edit error
//
// Example:
//
//	err := koutaku.UpdateMCPClient("my-mcp-client-id", schemas.MCPClientConfig{
//	    Name:           "my-mcp-client-name",
//	    ToolsToExecute: []string{"tool1", "tool2"},
//	})
func (koutaku *Koutaku) UpdateMCPClient(id string, updatedConfig *schemas.MCPClientConfig) error {
	if koutaku.MCPManager == nil {
		return fmt.Errorf("mcp is not configured in this koutaku instance")
	}

	return koutaku.MCPManager.UpdateClient(id, updatedConfig)
}

// ReconnectMCPClient attempts to reconnect an MCP client if it is disconnected.
//
// Parameters:
//   - id: ID of the client to reconnect
//
// Returns:
//   - error: Any reconnection error
func (koutaku *Koutaku) ReconnectMCPClient(id string) error {
	if koutaku.MCPManager == nil {
		return fmt.Errorf("mcp is not configured in this koutaku instance")
	}

	return koutaku.MCPManager.ReconnectClient(id)
}

// VerifyPerUserOAuthConnection delegates to the MCP manager to verify an MCP
// server using a temporary access token and discover available tools. The
// connection is closed after verification. If the MCP manager is not yet
// initialized, it is lazily created (same as AddMCPClient).
func (koutaku *Koutaku) VerifyPerUserOAuthConnection(ctx context.Context, config *schemas.MCPClientConfig, accessToken string) (map[string]schemas.ChatTool, map[string]string, error) {
	// Ensure MCP manager is initialized (lazy init, same pattern as AddMCPClient)
	if koutaku.MCPManager == nil {
		koutaku.mcpInitOnce.Do(func() {
			mcpConfig := schemas.MCPConfig{
				ClientConfigs: []*schemas.MCPClientConfig{},
			}
			mcpConfig.PluginPipelineProvider = func() interface{} {
				return koutaku.getPluginPipeline()
			}
			mcpConfig.ReleasePluginPipeline = func(pipeline interface{}) {
				if pp, ok := pipeline.(*PluginPipeline); ok {
					koutaku.releasePluginPipeline(pp)
				}
			}
			codeMode := starlark.NewStarlarkCodeMode(nil, koutaku.logger)
			koutaku.MCPManager = mcp.NewMCPManager(koutaku.ctx, mcpConfig, koutaku.oauth2Provider, koutaku.logger, codeMode)
		})
	}
	if koutaku.MCPManager == nil {
		return nil, nil, fmt.Errorf("MCP manager is not initialized")
	}
	return koutaku.MCPManager.VerifyPerUserOAuthConnection(ctx, config, accessToken)
}

// SetClientTools delegates to the MCP manager to update the tool map for an
// existing MCP client.
func (koutaku *Koutaku) SetClientTools(clientID string, tools map[string]schemas.ChatTool, toolNameMapping map[string]string) {
	if koutaku.MCPManager != nil {
		koutaku.MCPManager.SetClientTools(clientID, tools, toolNameMapping)
	}
}

// UpdateToolManagerConfig updates the tool manager config for the MCP manager.
// This allows for hot-reloading of the tool manager config at runtime.
// Pass the current value of disableAutoToolInject whenever only other fields
// change so the flag is never silently reset to its zero value.
func (koutaku *Koutaku) UpdateToolManagerConfig(maxAgentDepth int, toolExecutionTimeoutInSeconds int, codeModeBindingLevel string, disableAutoToolInject bool) error {
	if koutaku.MCPManager == nil {
		return fmt.Errorf("mcp is not configured in this koutaku instance")
	}

	koutaku.MCPManager.UpdateToolManagerConfig(&schemas.MCPToolManagerConfig{
		MaxAgentDepth:         maxAgentDepth,
		ToolExecutionTimeout:  time.Duration(toolExecutionTimeoutInSeconds) * time.Second,
		CodeModeBindingLevel:  schemas.CodeModeBindingLevel(codeModeBindingLevel),
		DisableAutoToolInject: disableAutoToolInject,
	})
	return nil
}

// PROVIDER MANAGEMENT

// createBaseProvider creates a provider based on the base provider type
func (koutaku *Koutaku) createBaseProvider(providerKey schemas.ModelProvider, config *schemas.ProviderConfig) (schemas.Provider, error) {
	// Determine which provider type to create
	targetProviderKey := providerKey

	if config.CustomProviderConfig != nil {
		// Validate custom provider config
		if config.CustomProviderConfig.BaseProviderType == "" {
			return nil, fmt.Errorf("custom provider config missing base provider type")
		}

		// Validate that base provider type is supported
		if !IsSupportedBaseProvider(config.CustomProviderConfig.BaseProviderType) {
			return nil, fmt.Errorf("unsupported base provider type: %s", config.CustomProviderConfig.BaseProviderType)
		}

		// Automatically set the custom provider key to the provider name
		config.CustomProviderConfig.CustomProviderKey = string(providerKey)

		targetProviderKey = config.CustomProviderConfig.BaseProviderType
	}

	switch targetProviderKey {
	case schemas.OpenAI:
		return openai.NewOpenAIProvider(config, koutaku.logger), nil
	case schemas.Anthropic:
		return anthropic.NewAnthropicProvider(config, koutaku.logger), nil
	case schemas.Bedrock:
		return bedrock.NewBedrockProvider(config, koutaku.logger)
	case schemas.Cohere:
		return cohere.NewCohereProvider(config, koutaku.logger)
	case schemas.Azure:
		return azure.NewAzureProvider(config, koutaku.logger)
	case schemas.Vertex:
		return vertex.NewVertexProvider(config, koutaku.logger)
	case schemas.Mistral:
		return mistral.NewMistralProvider(config, koutaku.logger), nil
	case schemas.Ollama:
		return ollama.NewOllamaProvider(config, koutaku.logger)
	case schemas.Groq:
		return groq.NewGroqProvider(config, koutaku.logger)
	case schemas.SGL:
		return sgl.NewSGLProvider(config, koutaku.logger)
	case schemas.Parasail:
		return parasail.NewParasailProvider(config, koutaku.logger)
	case schemas.Perplexity:
		return perplexity.NewPerplexityProvider(config, koutaku.logger)
	case schemas.Cerebras:
		return cerebras.NewCerebrasProvider(config, koutaku.logger)
	case schemas.Gemini:
		return gemini.NewGeminiProvider(config, koutaku.logger), nil
	case schemas.OpenRouter:
		return openrouter.NewOpenRouterProvider(config, koutaku.logger), nil
	case schemas.Elevenlabs:
		return elevenlabs.NewElevenlabsProvider(config, koutaku.logger), nil
	case schemas.Nebius:
		return nebius.NewNebiusProvider(config, koutaku.logger)
	case schemas.HuggingFace:
		return huggingface.NewHuggingFaceProvider(config, koutaku.logger), nil
	case schemas.XAI:
		return xai.NewXAIProvider(config, koutaku.logger)
	case schemas.Replicate:
		return replicate.NewReplicateProvider(config, koutaku.logger)
	case schemas.VLLM:
		return vllm.NewVLLMProvider(config, koutaku.logger)
	case schemas.Runway:
		return runway.NewRunwayProvider(config, koutaku.logger)
	case schemas.Fireworks:
		return fireworks.NewFireworksProvider(config, koutaku.logger)
	default:
		return nil, fmt.Errorf("unsupported provider: %s", targetProviderKey)
	}
}

// prepareProvider sets up a provider with its configuration, keys, and worker channels.
// It initializes the request queue and starts worker goroutines for processing requests.
// Note: This function assumes the caller has already acquired the appropriate mutex for the provider.
func (koutaku *Koutaku) prepareProvider(providerKey schemas.ModelProvider, config *schemas.ProviderConfig) error {
	// Create ProviderQueue with lifecycle management
	pq := &ProviderQueue{
		queue:      make(chan *ChannelMessage, config.ConcurrencyAndBufferSize.BufferSize),
		done:       make(chan struct{}),
		signalOnce: sync.Once{},
	}

	koutaku.requestQueues.Store(providerKey, pq)

	// Start specified number of workers
	koutaku.waitGroups.Store(providerKey, &sync.WaitGroup{})

	provider, err := koutaku.createBaseProvider(providerKey, config)
	if err != nil {
		return fmt.Errorf("failed to create provider for the given key: %v", err)
	}

	waitGroupValue, _ := koutaku.waitGroups.Load(providerKey)
	currentWaitGroup := waitGroupValue.(*sync.WaitGroup)

	// Atomically append provider to the providers slice
	for {
		oldPtr := koutaku.providers.Load()
		var oldSlice []schemas.Provider
		if oldPtr != nil {
			oldSlice = *oldPtr
		}
		newSlice := make([]schemas.Provider, len(oldSlice)+1)
		copy(newSlice, oldSlice)
		newSlice[len(oldSlice)] = provider
		if koutaku.providers.CompareAndSwap(oldPtr, &newSlice) {
			break
		}
	}

	schemas.RegisterKnownProvider(providerKey)

	for range config.ConcurrencyAndBufferSize.Concurrency {
		currentWaitGroup.Add(1)
		go koutaku.requestWorker(provider, config, pq)
	}

	return nil
}

// getProviderQueue returns the ProviderQueue for a given provider key.
// If the queue doesn't exist, it creates one at runtime and initializes the provider,
// given the provider config is provided in the account interface implementation.
// This function uses read locks to prevent race conditions during provider updates.
// Callers must check the closing flag or select on the done channel before sending.
func (koutaku *Koutaku) getProviderQueue(providerKey schemas.ModelProvider) (*ProviderQueue, error) {
	// Use read lock to allow concurrent reads but prevent concurrent updates
	providerMutex := koutaku.getProviderMutex(providerKey)
	providerMutex.RLock()

	if pqValue, exists := koutaku.requestQueues.Load(providerKey); exists {
		pq := pqValue.(*ProviderQueue)
		providerMutex.RUnlock()
		return pq, nil
	}

	// Provider doesn't exist, need to create it
	// Upgrade to write lock for creation
	providerMutex.RUnlock()
	providerMutex.Lock()
	defer providerMutex.Unlock()

	// Double-check after acquiring write lock (another goroutine might have created it)
	if pqValue, exists := koutaku.requestQueues.Load(providerKey); exists {
		pq := pqValue.(*ProviderQueue)
		return pq, nil
	}
	koutaku.logger.Debug(fmt.Sprintf("Creating new request queue for provider %s at runtime", providerKey))
	config, err := koutaku.account.GetConfigForProvider(providerKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get config for provider: %v", err)
	}
	if config == nil {
		return nil, fmt.Errorf("config is nil for provider %s", providerKey)
	}
	if err := koutaku.prepareProvider(providerKey, config); err != nil {
		return nil, err
	}
	pqValue, ok := koutaku.requestQueues.Load(providerKey)
	if !ok {
		return nil, fmt.Errorf("request queue not found for provider %s", providerKey)
	}
	pq := pqValue.(*ProviderQueue)
	return pq, nil
}

// GetProviderByKey returns the provider instance for the given provider key.
// Returns nil if no provider with the given key exists.
func (koutaku *Koutaku) GetProviderByKey(providerKey schemas.ModelProvider) schemas.Provider {
	return koutaku.getProviderByKey(providerKey)
}

// SelectKeyForProviderRequestType selects an API key for the given provider, request type, and model.
// Used by WebSocket handlers that need a key for upstream connections while honoring request-specific
// AllowedRequests gates such as realtime-only support.
func (koutaku *Koutaku) SelectKeyForProviderRequestType(ctx *schemas.KoutakuContext, requestType schemas.RequestType, providerKey schemas.ModelProvider, model string) (schemas.Key, error) {
	if ctx == nil {
		ctx = koutaku.ctx
	}
	baseProvider := providerKey
	if config, err := koutaku.account.GetConfigForProvider(providerKey); err == nil && config != nil &&
		config.CustomProviderConfig != nil && config.CustomProviderConfig.BaseProviderType != "" {
		baseProvider = config.CustomProviderConfig.BaseProviderType
	}
	supportedKeys, _, err := koutaku.selectKeyFromProviderForModelWithPool(ctx, requestType, providerKey, model, baseProvider)
	if err != nil {
		return schemas.Key{}, err
	}
	if len(supportedKeys) == 0 {
		return schemas.Key{}, nil
	}
	if len(supportedKeys) == 1 {
		return supportedKeys[0], nil
	}
	return koutaku.keySelector(ctx, supportedKeys, providerKey, model)
}

// WSStreamHooks holds the post-hook runner and cleanup function returned by RunStreamPreHooks.
// Call PostHookRunner for each streaming chunk, setting StreamEndIndicator on the final chunk.
// Call Cleanup when done to release the pipeline back to the pool.
// If ShortCircuitResponse is non-nil, a plugin short-circuited with a cached response —
// the caller should write this response to the client and skip the upstream call.
type WSStreamHooks struct {
	PostHookRunner       schemas.PostHookRunner
	Cleanup              func()
	ShortCircuitResponse *schemas.KoutakuResponse
}

// RealtimeTurnHooks mirrors RunStreamPreHooks but is explicitly scoped to a
// single realtime turn rather than one long-lived transport connection.
type RealtimeTurnHooks struct {
	PostHookRunner schemas.PostHookRunner
	Cleanup        func()
}

// RunStreamPreHooks acquires a plugin pipeline, sets up tracing context, runs PreLLMHooks,
// and returns a PostHookRunner for per-chunk post-processing.
// Used by WebSocket handlers that bypass the normal inference path but still need plugin hooks.
func (koutaku *Koutaku) RunStreamPreHooks(ctx *schemas.KoutakuContext, req *schemas.KoutakuRequest) (*WSStreamHooks, *schemas.KoutakuError) {
	if ctx == nil {
		ctx = koutaku.ctx
	}

	if _, ok := ctx.Value(schemas.KoutakuContextKeyRequestID).(string); !ok {
		ctx.SetValue(schemas.KoutakuContextKeyRequestID, uuid.New().String())
	}

	tracer := koutaku.getTracer()
	ctx.SetValue(schemas.KoutakuContextKeyTracer, tracer)

	// Create a trace so the logging plugin can accumulate streaming chunks.
	// The traceID is used as the accumulator key in ProcessStreamingChunk.
	if _, ok := ctx.Value(schemas.KoutakuContextKeyTraceID).(string); !ok {
		traceID := tracer.CreateTrace("")
		if traceID != "" {
			ctx.SetValue(schemas.KoutakuContextKeyTraceID, traceID)
		}
	}

	// Mark as streaming context so RunPostLLMHooks uses accumulated timing
	ctx.SetValue(schemas.KoutakuContextKeyStreamStartTime, time.Now())

	pipeline := koutaku.getPluginPipeline()

	cleanup := func() {
		if traceID, ok := ctx.Value(schemas.KoutakuContextKeyTraceID).(string); ok && traceID != "" {
			tracer.CleanupStreamAccumulator(traceID)
		}
		koutaku.releasePluginPipeline(pipeline)
	}

	preReq, shortCircuit, preCount := pipeline.RunLLMPreHooks(ctx, req)
	if preReq == nil && shortCircuit == nil {
		koutakuErr := newKoutakuErrorFromMsg("koutaku request after plugin hooks cannot be nil")
		_, koutakuErr = pipeline.RunPostLLMHooks(ctx, nil, koutakuErr, preCount)
		drainAndAttachPluginLogs(ctx)
		if traceID, ok := ctx.Value(schemas.KoutakuContextKeyTraceID).(string); ok && strings.TrimSpace(traceID) != "" {
			tracer.CompleteAndFlushTrace(strings.TrimSpace(traceID))
		}
		cleanup()
		return nil, koutakuErr
	}
	if shortCircuit != nil {
		if shortCircuit.Error != nil {
			_, koutakuErr := pipeline.RunPostLLMHooks(ctx, nil, shortCircuit.Error, preCount)
			drainAndAttachPluginLogs(ctx)
			if traceID, ok := ctx.Value(schemas.KoutakuContextKeyTraceID).(string); ok && strings.TrimSpace(traceID) != "" {
				tracer.CompleteAndFlushTrace(strings.TrimSpace(traceID))
			}
			cleanup()
			if koutakuErr != nil {
				return nil, koutakuErr
			}
			return nil, shortCircuit.Error
		}
		if shortCircuit.Response != nil {
			resp, koutakuErr := pipeline.RunPostLLMHooks(ctx, shortCircuit.Response, nil, preCount)
			drainAndAttachPluginLogs(ctx)
			if traceID, ok := ctx.Value(schemas.KoutakuContextKeyTraceID).(string); ok && strings.TrimSpace(traceID) != "" {
				tracer.CompleteAndFlushTrace(strings.TrimSpace(traceID))
			}
			cleanup()
			if koutakuErr != nil {
				return nil, koutakuErr
			}
			return &WSStreamHooks{
				Cleanup:              func() {},
				ShortCircuitResponse: resp,
			}, nil
		}
	}

	wsProvider, wsModel, _ := preReq.GetRequestFields()
	postHookRunner := func(ctx *schemas.KoutakuContext, result *schemas.KoutakuResponse, err *schemas.KoutakuError) (*schemas.KoutakuResponse, *schemas.KoutakuError) {
		// Populate extra fields before RunPostLLMHooks so plugins (e.g. logging)
		// can read requestType/provider/model from the chunk or error.
		if result != nil {
			result.PopulateExtraFields(req.RequestType, wsProvider, wsModel, wsModel)
		}
		if err != nil {
			err.PopulateExtraFields(req.RequestType, wsProvider, wsModel, wsModel)
		}
		resp, koutakuErr := pipeline.RunPostLLMHooks(ctx, result, err, preCount)
		if IsFinalChunk(ctx) {
			drainAndAttachPluginLogs(ctx)
		}
		return resp, koutakuErr
	}

	return &WSStreamHooks{
		PostHookRunner: postHookRunner,
		Cleanup:        cleanup,
	}, nil
}

// RunRealtimeTurnPreHooks acquires a plugin pipeline and runs LLM pre-hooks for
// a single realtime turn. Unlike generic stream hooks, realtime turns do not
// support short-circuit responses in v1 because the transports cannot yet emit a
// fully synthetic assistant turn without an upstream generation.
func (koutaku *Koutaku) RunRealtimeTurnPreHooks(ctx *schemas.KoutakuContext, req *schemas.KoutakuRequest) (*RealtimeTurnHooks, *schemas.KoutakuError) {
	if req == nil {
		koutakuErr := newKoutakuErrorFromMsg("realtime turn request is nil")
		koutakuErr.ExtraFields.RequestType = schemas.RealtimeRequest
		return nil, koutakuErr
	}
	if ctx == nil {
		ctx = koutaku.ctx
	}

	if _, ok := ctx.Value(schemas.KoutakuContextKeyRequestID).(string); !ok {
		ctx.SetValue(schemas.KoutakuContextKeyRequestID, uuid.New().String())
	}

	tracer := koutaku.getTracer()
	ctx.SetValue(schemas.KoutakuContextKeyTracer, tracer)

	if _, ok := ctx.Value(schemas.KoutakuContextKeyTraceID).(string); !ok {
		traceID := tracer.CreateTrace("")
		if traceID != "" {
			ctx.SetValue(schemas.KoutakuContextKeyTraceID, traceID)
		}
	}

	pipeline := koutaku.getPluginPipeline()
	cleanup := func() {
		if traceID, ok := ctx.Value(schemas.KoutakuContextKeyTraceID).(string); ok && traceID != "" {
			tracer.CleanupStreamAccumulator(traceID)
		}
		koutaku.releasePluginPipeline(pipeline)
	}
	provider, model, _ := req.GetRequestFields()

	preReq, shortCircuit, preCount := pipeline.RunLLMPreHooks(ctx, req)
	if preReq == nil && shortCircuit == nil {
		koutakuErr := newKoutakuErrorFromMsg("koutaku request after plugin hooks cannot be nil")
		koutakuErr.PopulateExtraFields(schemas.RealtimeRequest, provider, model, model)
		_, koutakuErr = pipeline.RunPostLLMHooks(ctx, nil, koutakuErr, preCount)
		drainAndAttachPluginLogs(ctx)
		if traceID, ok := ctx.Value(schemas.KoutakuContextKeyTraceID).(string); ok && strings.TrimSpace(traceID) != "" {
			tracer.CompleteAndFlushTrace(strings.TrimSpace(traceID))
		}
		cleanup()
		return nil, koutakuErr
	}
	if shortCircuit != nil {
		if shortCircuit.Error != nil {
			shortCircuit.Error.PopulateExtraFields(schemas.RealtimeRequest, provider, model, model)
			_, koutakuErr := pipeline.RunPostLLMHooks(ctx, nil, shortCircuit.Error, preCount)
			drainAndAttachPluginLogs(ctx)
			if traceID, ok := ctx.Value(schemas.KoutakuContextKeyTraceID).(string); ok && strings.TrimSpace(traceID) != "" {
				tracer.CompleteAndFlushTrace(strings.TrimSpace(traceID))
			}
			cleanup()
			if koutakuErr != nil {
				return nil, koutakuErr
			}
			return nil, shortCircuit.Error
		}
		if shortCircuit.Response != nil {
			// Short-circuit responses are not supported for realtime turns (v1).
			// Treat this like an error turn so plugins can close pending state cleanly.
			koutakuErr := newKoutakuErrorFromMsg("realtime turn short-circuit responses are not supported")
			koutakuErr.PopulateExtraFields(schemas.RealtimeRequest, provider, model, model)
			_, koutakuErr = pipeline.RunPostLLMHooks(ctx, nil, koutakuErr, preCount)
			drainAndAttachPluginLogs(ctx)
			if traceID, ok := ctx.Value(schemas.KoutakuContextKeyTraceID).(string); ok && strings.TrimSpace(traceID) != "" {
				tracer.CompleteAndFlushTrace(strings.TrimSpace(traceID))
			}
			cleanup()
			return nil, koutakuErr
		}
	}

	return &RealtimeTurnHooks{
		PostHookRunner: func(ctx *schemas.KoutakuContext, result *schemas.KoutakuResponse, err *schemas.KoutakuError) (*schemas.KoutakuResponse, *schemas.KoutakuError) {
			resp, koutakuErr := pipeline.RunPostLLMHooks(ctx, result, err, preCount)
			drainAndAttachPluginLogs(ctx)
			return resp, koutakuErr
		},
		Cleanup: cleanup,
	}, nil
}

// getProviderByKey retrieves a provider instance from the providers array by its provider key.
// Returns the provider if found, or nil if no provider with the given key exists.
func (koutaku *Koutaku) getProviderByKey(providerKey schemas.ModelProvider) schemas.Provider {
	providers := koutaku.providers.Load()
	if providers == nil {
		return nil
	}
	// Checking if provider is in the memory
	for _, provider := range *providers {
		if provider.GetProviderKey() == providerKey {
			return provider
		}
	}
	// Could happen when provider is not initialized yet, check if provider config exists in account and if so, initialize it
	config, err := koutaku.account.GetConfigForProvider(providerKey)
	if err != nil || config == nil {
		if slices.Contains(dynamicallyConfigurableProviders, providerKey) {
			koutaku.logger.Info(fmt.Sprintf("initializing provider %s with default config", providerKey))
			// If no config found, use default config
			config = &schemas.ProviderConfig{
				NetworkConfig:            schemas.DefaultNetworkConfig,
				ConcurrencyAndBufferSize: schemas.DefaultConcurrencyAndBufferSize,
			}
		} else {
			return nil
		}
	}
	// Lock the provider mutex to avoid races
	providerMutex := koutaku.getProviderMutex(providerKey)
	providerMutex.Lock()
	defer providerMutex.Unlock()
	// Double-check after acquiring the lock
	providers = koutaku.providers.Load()
	if providers != nil {
		for _, p := range *providers {
			if p.GetProviderKey() == providerKey {
				return p
			}
		}
	}
	// Preparing provider
	if err := koutaku.prepareProvider(providerKey, config); err != nil {
		return nil
	}
	// Return newly prepared provider without recursion
	providers = koutaku.providers.Load()
	if providers != nil {
		for _, p := range *providers {
			if p.GetProviderKey() == providerKey {
				return p
			}
		}
	}
	return nil
}

// CORE INTERNAL LOGIC

// shouldTryFallbacks handles the primary error and returns true if we should proceed with fallbacks, false if we should return immediately
func (koutaku *Koutaku) shouldTryFallbacks(req *schemas.KoutakuRequest, primaryErr *schemas.KoutakuError) bool {
	// If no primary error, we succeeded
	if primaryErr == nil {
		koutaku.logger.Debug("no primary error, we should not try fallbacks")
		return false
	}

	// Handle request cancellation
	if primaryErr.Error != nil && primaryErr.Error.Type != nil && *primaryErr.Error.Type == schemas.RequestCancelled {
		koutaku.logger.Debug("request cancelled, we should not try fallbacks")
		return false
	}

	// Check if this is a short-circuit error that doesn't allow fallbacks
	// Note: AllowFallbacks = nil is treated as true (allow fallbacks by default)
	if primaryErr.AllowFallbacks != nil && !*primaryErr.AllowFallbacks {
		koutaku.logger.Debug("allowFallbacks is false, we should not try fallbacks")
		return false
	}

	// If no fallbacks configured, return primary error
	_, _, fallbacks := req.GetRequestFields()
	if len(fallbacks) == 0 {
		koutaku.logger.Debug("no fallbacks configured, we should not try fallbacks")
		return false
	}

	// Should proceed with fallbacks
	return true
}

// prepareFallbackRequest creates a fallback request and validates the provider config
// Returns the fallback request or nil if this fallback should be skipped
func (koutaku *Koutaku) prepareFallbackRequest(req *schemas.KoutakuRequest, fallback schemas.Fallback) *schemas.KoutakuRequest {
	// Check if we have config for this fallback provider
	_, err := koutaku.account.GetConfigForProvider(fallback.Provider)
	if err != nil {
		koutaku.logger.Warn("config not found for provider %s, skipping fallback: %v", fallback.Provider, err)
		return nil
	}

	// Create a new request with the fallback provider and model
	fallbackReq := *req

	if req.TextCompletionRequest != nil {
		tmp := *req.TextCompletionRequest
		tmp.Provider = fallback.Provider
		tmp.Model = fallback.Model
		fallbackReq.TextCompletionRequest = &tmp
	}

	if req.ChatRequest != nil {
		tmp := *req.ChatRequest
		tmp.Provider = fallback.Provider
		tmp.Model = fallback.Model
		fallbackReq.ChatRequest = &tmp
	}

	if req.ResponsesRequest != nil {
		tmp := *req.ResponsesRequest
		tmp.Provider = fallback.Provider
		tmp.Model = fallback.Model
		fallbackReq.ResponsesRequest = &tmp
	}

	if req.CountTokensRequest != nil {
		tmp := *req.CountTokensRequest
		tmp.Provider = fallback.Provider
		tmp.Model = fallback.Model
		fallbackReq.CountTokensRequest = &tmp
	}

	if req.EmbeddingRequest != nil {
		tmp := *req.EmbeddingRequest
		tmp.Provider = fallback.Provider
		tmp.Model = fallback.Model
		fallbackReq.EmbeddingRequest = &tmp
	}
	if req.RerankRequest != nil {
		tmp := *req.RerankRequest
		tmp.Provider = fallback.Provider
		tmp.Model = fallback.Model
		fallbackReq.RerankRequest = &tmp
	}
	if req.OCRRequest != nil {
		tmp := *req.OCRRequest
		tmp.Provider = fallback.Provider
		tmp.Model = fallback.Model
		fallbackReq.OCRRequest = &tmp
	}

	if req.SpeechRequest != nil {
		tmp := *req.SpeechRequest
		tmp.Provider = fallback.Provider
		tmp.Model = fallback.Model
		fallbackReq.SpeechRequest = &tmp
	}

	if req.TranscriptionRequest != nil {
		tmp := *req.TranscriptionRequest
		tmp.Provider = fallback.Provider
		tmp.Model = fallback.Model
		fallbackReq.TranscriptionRequest = &tmp
	}
	if req.ImageGenerationRequest != nil {
		tmp := *req.ImageGenerationRequest
		tmp.Provider = fallback.Provider
		tmp.Model = fallback.Model
		fallbackReq.ImageGenerationRequest = &tmp
	}
	if req.VideoGenerationRequest != nil {
		tmp := *req.VideoGenerationRequest
		tmp.Provider = fallback.Provider
		tmp.Model = fallback.Model
		fallbackReq.VideoGenerationRequest = &tmp
	}
	return &fallbackReq
}

// shouldContinueWithFallbacks processes errors from fallback attempts
// Returns true if we should continue with more fallbacks, false if we should stop
func (koutaku *Koutaku) shouldContinueWithFallbacks(fallback schemas.Fallback, fallbackErr *schemas.KoutakuError) bool {
	if fallbackErr.Error.Type != nil && *fallbackErr.Error.Type == schemas.RequestCancelled {
		return false
	}

	// Check if it was a short-circuit error that doesn't allow fallbacks
	if fallbackErr.AllowFallbacks != nil && !*fallbackErr.AllowFallbacks {
		return false
	}

	koutaku.logger.Debug(fmt.Sprintf("Fallback provider %s failed: %s", fallback.Provider, fallbackErr.Error.Message))
	return true
}

// handleRequest handles the request to the provider based on the request type
// It handles plugin hooks, request validation, response processing, and fallback providers.
// If the primary provider fails, it will try each fallback provider in order until one succeeds.
// It is the wrapper for all non-streaming public API methods.
func (koutaku *Koutaku) handleRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuRequest) (*schemas.KoutakuResponse, *schemas.KoutakuError) {
	defer koutaku.releaseKoutakuRequest(req)
	provider, model, fallbacks := req.GetRequestFields()
	if err := validateRequest(req); err != nil {
		err.PopulateExtraFields(req.RequestType, provider, model, model)
		return nil, err
	}

	// Handle nil context early to prevent blocking
	if ctx == nil {
		ctx = koutaku.ctx
	}

	koutaku.logger.Debug(fmt.Sprintf("primary provider %s with model %s and %d fallbacks", provider, model, len(fallbacks)))

	// Try the primary provider first
	ctx.SetValue(schemas.KoutakuContextKeyFallbackIndex, 0)
	// Ensure request ID is set in context before PreHooks
	if _, ok := ctx.Value(schemas.KoutakuContextKeyRequestID).(string); !ok {
		requestID := uuid.New().String()
		ctx.SetValue(schemas.KoutakuContextKeyRequestID, requestID)
	}
	primaryResult, primaryErr := koutaku.tryRequest(ctx, req)
	if primaryErr != nil {
		if primaryErr.Error != nil {
			koutaku.logger.Debug(fmt.Sprintf("primary provider %s with model %s returned error: %s", provider, model, primaryErr.Error.Message))
		} else {
			koutaku.logger.Debug(fmt.Sprintf("primary provider %s with model %s returned error: %v", provider, model, primaryErr))
		}
		if len(fallbacks) > 0 {
			koutaku.logger.Debug(fmt.Sprintf("check if we should try %d fallbacks", len(fallbacks)))
		}
	}

	// Check if we should proceed with fallbacks
	shouldTryFallbacks := koutaku.shouldTryFallbacks(req, primaryErr)
	if !shouldTryFallbacks {
		return primaryResult, primaryErr
	}

	// Try fallbacks in order
	for i, fallback := range fallbacks {
		ctx.SetValue(schemas.KoutakuContextKeyFallbackIndex, i+1)
		koutaku.logger.Debug(fmt.Sprintf("trying fallback provider %s with model %s", fallback.Provider, fallback.Model))
		ctx.SetValue(schemas.KoutakuContextKeyFallbackRequestID, uuid.New().String())
		clearCtxForFallback(ctx)

		// Start span for fallback attempt
		tracer := koutaku.getTracer()
		spanCtx, handle := tracer.StartSpan(ctx, fmt.Sprintf("fallback.%s.%s", fallback.Provider, fallback.Model), schemas.SpanKindFallback)
		tracer.SetAttribute(handle, schemas.AttrProviderName, string(fallback.Provider))
		tracer.SetAttribute(handle, schemas.AttrRequestModel, fallback.Model)
		tracer.SetAttribute(handle, "fallback.index", i+1)
		ctx.SetValue(schemas.KoutakuContextKeySpanID, spanCtx.Value(schemas.KoutakuContextKeySpanID))

		fallbackReq := koutaku.prepareFallbackRequest(req, fallback)
		if fallbackReq == nil {
			koutaku.logger.Debug(fmt.Sprintf("fallback provider %s with model %s is nil", fallback.Provider, fallback.Model))
			tracer.SetAttribute(handle, "error", "fallback request preparation failed")
			tracer.EndSpan(handle, schemas.SpanStatusError, "fallback request preparation failed")
			continue
		}

		// Try the fallback provider
		result, fallbackErr := koutaku.tryRequest(ctx, fallbackReq)
		if fallbackErr == nil {
			koutaku.logger.Debug(fmt.Sprintf("successfully used fallback provider %s with model %s", fallback.Provider, fallback.Model))
			tracer.EndSpan(handle, schemas.SpanStatusOk, "")
			return result, nil
		}

		// End span with error status
		if fallbackErr.Error != nil {
			tracer.SetAttribute(handle, "error", fallbackErr.Error.Message)
		}
		tracer.EndSpan(handle, schemas.SpanStatusError, "fallback failed")

		// Check if we should continue with more fallbacks
		if !koutaku.shouldContinueWithFallbacks(fallback, fallbackErr) {
			return nil, fallbackErr
		}
	}

	// All providers failed, return the original error
	return nil, primaryErr
}

// handleStreamRequest handles the stream request to the provider based on the request type
// It handles plugin hooks, request validation, response processing, and fallback providers.
// If the primary provider fails, it will try each fallback provider in order until one succeeds.
// It is the wrapper for all streaming public API methods.
func (koutaku *Koutaku) handleStreamRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	defer koutaku.releaseKoutakuRequest(req)

	provider, model, fallbacks := req.GetRequestFields()

	if err := validateRequest(req); err != nil {
		err.PopulateExtraFields(req.RequestType, provider, model, model)
		err.StatusCode = schemas.Ptr(fasthttp.StatusBadRequest)
		return nil, err
	}

	// Handle nil context early to prevent blocking
	if ctx == nil {
		ctx = koutaku.ctx
	}

	// Try the primary provider first
	ctx.SetValue(schemas.KoutakuContextKeyFallbackIndex, 0)
	// Ensure request ID is set in context before PreHooks
	if _, ok := ctx.Value(schemas.KoutakuContextKeyRequestID).(string); !ok {
		requestID := uuid.New().String()
		ctx.SetValue(schemas.KoutakuContextKeyRequestID, requestID)
	}
	primaryResult, primaryErr := koutaku.tryStreamRequest(ctx, req)

	// Check if we should proceed with fallbacks
	shouldTryFallbacks := koutaku.shouldTryFallbacks(req, primaryErr)
	if !shouldTryFallbacks {
		return primaryResult, primaryErr
	}

	// Try fallbacks in order
	for i, fallback := range fallbacks {
		ctx.SetValue(schemas.KoutakuContextKeyFallbackIndex, i+1)
		ctx.SetValue(schemas.KoutakuContextKeyFallbackRequestID, uuid.New().String())
		clearCtxForFallback(ctx)

		// Start span for fallback attempt
		tracer := koutaku.getTracer()
		spanCtx, handle := tracer.StartSpan(ctx, fmt.Sprintf("fallback.%s.%s", fallback.Provider, fallback.Model), schemas.SpanKindFallback)
		tracer.SetAttribute(handle, schemas.AttrProviderName, string(fallback.Provider))
		tracer.SetAttribute(handle, schemas.AttrRequestModel, fallback.Model)
		tracer.SetAttribute(handle, "fallback.index", i+1)
		ctx.SetValue(schemas.KoutakuContextKeySpanID, spanCtx.Value(schemas.KoutakuContextKeySpanID))

		fallbackReq := koutaku.prepareFallbackRequest(req, fallback)
		if fallbackReq == nil {
			tracer.SetAttribute(handle, "error", "fallback request preparation failed")
			tracer.EndSpan(handle, schemas.SpanStatusError, "fallback request preparation failed")
			continue
		}

		// Try the fallback provider
		result, fallbackErr := koutaku.tryStreamRequest(ctx, fallbackReq)
		if fallbackErr == nil {
			koutaku.logger.Debug(fmt.Sprintf("successfully used fallback provider %s with model %s", fallback.Provider, fallback.Model))
			tracer.EndSpan(handle, schemas.SpanStatusOk, "")
			return result, nil
		}

		// End span with error status
		if fallbackErr.Error != nil {
			tracer.SetAttribute(handle, "error", fallbackErr.Error.Message)
		}
		tracer.EndSpan(handle, schemas.SpanStatusError, "fallback failed")

		// Check if we should continue with more fallbacks
		if !koutaku.shouldContinueWithFallbacks(fallback, fallbackErr) {
			return nil, fallbackErr
		}
	}

	// All providers failed, return the original error
	return nil, primaryErr
}

// tryRequest is a generic function that handles common request processing logic
// It consolidates queue setup, plugin pipeline execution, enqueue logic, and response handling
func (koutaku *Koutaku) tryRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuRequest) (*schemas.KoutakuResponse, *schemas.KoutakuError) {
	provider, model, _ := req.GetRequestFields()
	pq, err := koutaku.getProviderQueue(provider)
	if err != nil {
		koutakuErr := newKoutakuError(err)
		koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
		return nil, koutakuErr
	}

	// Add MCP tools to request if MCP is configured and requested
	if koutaku.MCPManager != nil {
		req = koutaku.MCPManager.AddToolsToRequest(ctx, req)
	}

	tracer := koutaku.getTracer()
	if tracer == nil {
		koutakuErr := newKoutakuErrorFromMsg("tracer not found in context")
		koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
		return nil, koutakuErr
	}

	// Store tracer in context BEFORE calling requestHandler, so streaming goroutines
	// have access to it for completing deferred spans when the stream ends.
	// The streaming goroutine captures the context when it starts, so these values
	// must be set before requestHandler() is called.
	ctx.SetValue(schemas.KoutakuContextKeyTracer, tracer)

	pipeline := koutaku.getPluginPipeline()
	defer koutaku.releasePluginPipeline(pipeline)

	preReq, shortCircuit, preCount := pipeline.RunLLMPreHooks(ctx, req)
	if shortCircuit != nil {
		// Handle short-circuit with response (success case)
		if shortCircuit.Response != nil {
			resp, koutakuErr := pipeline.RunPostLLMHooks(ctx, shortCircuit.Response, nil, preCount)
			drainAndAttachPluginLogs(ctx)
			if koutakuErr != nil {
				koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
				return nil, koutakuErr
			}
			return resp, nil
		}
		// Handle short-circuit with error
		if shortCircuit.Error != nil {
			resp, koutakuErr := pipeline.RunPostLLMHooks(ctx, nil, shortCircuit.Error, preCount)
			drainAndAttachPluginLogs(ctx)
			if koutakuErr != nil {
				koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
				return nil, koutakuErr
			}
			return resp, nil
		}
	}
	if preReq == nil {
		koutakuErr := newKoutakuErrorFromMsg("koutaku request after plugin hooks cannot be nil")
		koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
		return nil, koutakuErr
	}

	msg := koutaku.getChannelMessage(*preReq)
	msg.Context = ctx

	// If the queue is closing, check whether the provider was updated (new queue
	// available) or removed. On update, transparently re-route to the new queue
	// so in-flight producers don't get spurious errors. On removal, error out.
	//
	// Use a direct sync.Map lookup instead of getProviderQueue to avoid the
	// lazy-creation path: getProviderQueue can resurrect a provider that was
	// just removed by RemoveProvider if the account config still exists.
	if pq.isClosing() {
		var reroutedPq *ProviderQueue
		if val, ok := koutaku.requestQueues.Load(provider); ok {
			if candidate := val.(*ProviderQueue); candidate != pq && !candidate.isClosing() {
				reroutedPq = candidate
			}
		}
		if reroutedPq == nil {
			koutaku.releaseChannelMessage(msg)
			koutakuErr := newKoutakuErrorFromMsg("provider is shutting down")
			koutakuErr.ExtraFields = schemas.KoutakuErrorExtraFields{
				RequestType:            req.RequestType,
				Provider:               provider,
				OriginalModelRequested: model,
			}
			return nil, koutakuErr
		}
		pq = reroutedPq
	}

	// Use select with done channel to detect shutdown during send
	select {
	case pq.queue <- msg:
		// Message was sent successfully
	case <-pq.done:
		koutaku.releaseChannelMessage(msg)
		koutakuErr := newKoutakuErrorFromMsg("provider is shutting down")
		koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
		return nil, koutakuErr
	case <-ctx.Done():
		koutaku.releaseChannelMessage(msg)
		koutakuErr := newKoutakuCtxDoneError(ctx, "while waiting for queue space")
		koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
		return nil, koutakuErr
	default:
		if koutaku.dropExcessRequests.Load() {
			koutaku.releaseChannelMessage(msg)
			koutaku.logger.Warn("request dropped: queue is full, please increase the queue size or set dropExcessRequests to false")
			koutakuErr := newKoutakuErrorFromMsg("request dropped: queue is full")
			koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
			return nil, koutakuErr
		}
		// Re-check closing flag before blocking send (lock-free atomic check)
		if pq.isClosing() {
			koutaku.releaseChannelMessage(msg)
			koutakuErr := newKoutakuErrorFromMsg("provider is shutting down")
			koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
			return nil, koutakuErr
		}
		select {
		case pq.queue <- msg:
			// Message was sent successfully
		case <-pq.done:
			koutaku.releaseChannelMessage(msg)
			koutakuErr := newKoutakuErrorFromMsg("provider is shutting down")
			koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
			return nil, koutakuErr
		case <-ctx.Done():
			koutaku.releaseChannelMessage(msg)
			koutakuErr := newKoutakuCtxDoneError(ctx, "while waiting for queue space")
			koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
			return nil, koutakuErr
		}
	}

	var result *schemas.KoutakuResponse
	var resp *schemas.KoutakuResponse
	pluginCount := len(*koutaku.llmPlugins.Load())
	select {
	case result = <-msg.Response:
		resp, koutakuErr := pipeline.RunPostLLMHooks(msg.Context, result, nil, pluginCount)
		drainAndAttachPluginLogs(msg.Context)
		if koutakuErr != nil {
			koutaku.releaseChannelMessage(msg)
			return nil, koutakuErr
		}
		koutaku.releaseChannelMessage(msg)
		// Strip raw fields that were captured for logging but should not reach the client.
		if resp != nil {
			dropReq, _ := ctx.Value(schemas.KoutakuContextKeyDropRawRequestFromClient).(bool)
			dropResp, _ := ctx.Value(schemas.KoutakuContextKeyDropRawResponseFromClient).(bool)
			if dropReq || dropResp {
				extraField := resp.GetExtraFields()
				if dropReq {
					extraField.RawRequest = nil
				}
				if dropResp {
					extraField.RawResponse = nil
				}
			}
		}
		return resp, nil
	case koutakuErrVal := <-msg.Err:
		koutakuErrPtr := &koutakuErrVal
		resp, koutakuErrPtr = pipeline.RunPostLLMHooks(msg.Context, nil, koutakuErrPtr, pluginCount)
		drainAndAttachPluginLogs(msg.Context)
		koutaku.releaseChannelMessage(msg)
		// Strip raw fields on error path too.
		dropReq, _ := ctx.Value(schemas.KoutakuContextKeyDropRawRequestFromClient).(bool)
		dropResp, _ := ctx.Value(schemas.KoutakuContextKeyDropRawResponseFromClient).(bool)
		if dropReq || dropResp {
			if koutakuErrPtr != nil {
				if dropReq {
					koutakuErrPtr.ExtraFields.RawRequest = nil
				}
				if dropResp {
					koutakuErrPtr.ExtraFields.RawResponse = nil
				}
			}
			if resp != nil {
				extraField := resp.GetExtraFields()
				if dropReq {
					extraField.RawRequest = nil
				}
				if dropResp {
					extraField.RawResponse = nil
				}
			}
		}
		if koutakuErrPtr != nil {
			return nil, koutakuErrPtr
		}
		return resp, nil
	case <-ctx.Done():
		// Do NOT releaseChannelMessage here. The message is already enqueued and
		// the worker still holds a reference to msg.Response and msg.Err. Returning
		// those channels to the pool now would let the next request reuse them while
		// the worker is still writing to them — stale data corruption. The worker
		// never calls releaseChannelMessage itself, so this message leaks from the
		// pool and is GC'd. That is intentional: a small pool leak on cancellation
		// is far safer than corrupting another request's channels.
		provider, model, _ := req.GetRequestFields()
		koutakuErr := newKoutakuCtxDoneError(ctx, "waiting for provider response")
		koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
		return nil, koutakuErr
	}
}

// tryStreamRequest is a generic function that handles common request processing logic
// It consolidates queue setup, plugin pipeline execution, enqueue logic, and response handling
func (koutaku *Koutaku) tryStreamRequest(ctx *schemas.KoutakuContext, req *schemas.KoutakuRequest) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	provider, model, _ := req.GetRequestFields()
	pq, err := koutaku.getProviderQueue(provider)
	if err != nil {
		koutakuErr := newKoutakuError(err)
		koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
		return nil, koutakuErr
	}

	// Add MCP tools to request if MCP is configured and requested
	if req.RequestType != schemas.SpeechStreamRequest && req.RequestType != schemas.TranscriptionStreamRequest && koutaku.MCPManager != nil {
		req = koutaku.MCPManager.AddToolsToRequest(ctx, req)
	}

	tracer := koutaku.getTracer()
	if tracer == nil {
		koutakuErr := newKoutakuErrorFromMsg("tracer not found in context")
		koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
		return nil, koutakuErr
	}

	// Store tracer in context BEFORE calling RunLLMPreHooks, so plugins and streaming goroutines
	// have access to it for completing deferred spans when the stream ends.
	// The streaming goroutine captures the context when it starts, so these values
	// must be set before requestHandler() is called.
	ctx.SetValue(schemas.KoutakuContextKeyTracer, tracer)

	// Ensure traceID exists so the logging plugin can create a stream accumulator
	// in PreLLMHook and accumulate chunks in PostLLMHook. For HTTP handler requests the
	// tracing middleware already sets this; for WebSocket bridge and Go SDK callers it
	// may be absent.
	if _, ok := ctx.Value(schemas.KoutakuContextKeyTraceID).(string); !ok {
		traceID := tracer.CreateTrace("")
		if traceID != "" {
			ctx.SetValue(schemas.KoutakuContextKeyTraceID, traceID)
		}
	}

	pipeline := koutaku.getPluginPipeline()
	releasePipeline := true
	defer func() {
		if releasePipeline {
			koutaku.releasePluginPipeline(pipeline)
		}
	}()

	preReq, shortCircuit, preCount := pipeline.RunLLMPreHooks(ctx, req)
	if shortCircuit != nil {
		// Handle short-circuit with response (success case)
		if shortCircuit.Response != nil {
			resp, koutakuErr := pipeline.RunPostLLMHooks(ctx, shortCircuit.Response, nil, preCount)
			drainAndAttachPluginLogs(ctx)
			if koutakuErr != nil {
				koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
				return nil, koutakuErr
			}
			return newKoutakuMessageChan(resp), nil
		}
		// Handle short-circuit with stream
		if shortCircuit.Stream != nil {
			outputStream := make(chan *schemas.KoutakuStreamChunk)
			releasePipeline = false // pipeline is released inside the goroutine after stream drains

			// Create a post hook runner cause pipeline object is put back in the pool on defer
			pipelinePostHookRunner := func(ctx *schemas.KoutakuContext, result *schemas.KoutakuResponse, err *schemas.KoutakuError) (*schemas.KoutakuResponse, *schemas.KoutakuError) {
				resp, koutakuErr := pipeline.RunPostLLMHooks(ctx, result, err, preCount)
				if IsFinalChunk(ctx) {
					drainAndAttachPluginLogs(ctx)
				}
				return resp, koutakuErr
			}

			go func() {
				defer func() {
					drainAndAttachPluginLogs(ctx) // ensure logs are drained even if stream closes without a final chunk
					pipeline.FinalizeStreamingPostHookSpans(ctx)
					koutaku.releasePluginPipeline(pipeline)
				}()
				defer close(outputStream)

				for streamMsg := range shortCircuit.Stream {
					if streamMsg == nil {
						continue
					}

					koutakuResponse := &schemas.KoutakuResponse{}
					if streamMsg.KoutakuTextCompletionResponse != nil {
						koutakuResponse.TextCompletionResponse = streamMsg.KoutakuTextCompletionResponse
					}
					if streamMsg.KoutakuChatResponse != nil {
						koutakuResponse.ChatResponse = streamMsg.KoutakuChatResponse
					}
					if streamMsg.KoutakuResponsesStreamResponse != nil {
						koutakuResponse.ResponsesStreamResponse = streamMsg.KoutakuResponsesStreamResponse
					}
					if streamMsg.KoutakuSpeechStreamResponse != nil {
						koutakuResponse.SpeechStreamResponse = streamMsg.KoutakuSpeechStreamResponse
					}
					if streamMsg.KoutakuTranscriptionStreamResponse != nil {
						koutakuResponse.TranscriptionStreamResponse = streamMsg.KoutakuTranscriptionStreamResponse
					}
					if streamMsg.KoutakuImageGenerationStreamResponse != nil {
						koutakuResponse.ImageGenerationStreamResponse = streamMsg.KoutakuImageGenerationStreamResponse
					}

					// Run post hooks on the stream message
					processedResponse, processedError := pipelinePostHookRunner(ctx, koutakuResponse, streamMsg.KoutakuError)

					// Build the client-facing chunk via the shared helper, which strips raw
					// request/response fields when in logging-only mode without mutating the
					// shared processedResponse or processedError objects.
					streamResponse := providerUtils.BuildClientStreamChunk(ctx, processedResponse, processedError)

					// Guarded send: if the consumer abandons outputStream (client
					// disconnect, ctx cancel), drain the upstream shortCircuit.Stream
					// so its producer can exit cleanly instead of blocking on its send.
					select {
					case outputStream <- streamResponse:
					case <-ctx.Done():
						for range shortCircuit.Stream {
						}
						return
					}

					// TODO: Release the processed response immediately after use
				}
			}()

			return outputStream, nil
		}
		// Handle short-circuit with error
		if shortCircuit.Error != nil {
			resp, koutakuErr := pipeline.RunPostLLMHooks(ctx, nil, shortCircuit.Error, preCount)
			drainAndAttachPluginLogs(ctx)
			if koutakuErr != nil {
				koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
				return nil, koutakuErr
			}
			return newKoutakuMessageChan(resp), nil
		}
	}
	if preReq == nil {
		koutakuErr := newKoutakuErrorFromMsg("koutaku request after plugin hooks cannot be nil")
		koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
		return nil, koutakuErr
	}

	msg := koutaku.getChannelMessage(*preReq)
	msg.Context = ctx

	// If the queue is closing, check whether the provider was updated (new queue
	// available) or removed. On update, transparently re-route to the new queue
	// so in-flight producers don't get spurious errors. On removal, error out.
	//
	// Use a direct sync.Map lookup instead of getProviderQueue to avoid the
	// lazy-creation path: getProviderQueue can resurrect a provider that was
	// just removed by RemoveProvider if the account config still exists.
	if pq.isClosing() {
		var reroutedPq *ProviderQueue
		if val, ok := koutaku.requestQueues.Load(provider); ok {
			if candidate := val.(*ProviderQueue); candidate != pq && !candidate.isClosing() {
				reroutedPq = candidate
			}
		}
		if reroutedPq == nil {
			koutaku.releaseChannelMessage(msg)
			koutakuErr := newKoutakuErrorFromMsg("provider is shutting down")
			koutakuErr.ExtraFields = schemas.KoutakuErrorExtraFields{
				RequestType:            req.RequestType,
				Provider:               provider,
				OriginalModelRequested: model,
			}
			return nil, koutakuErr
		}
		pq = reroutedPq
	}

	// Use select with done channel to detect shutdown during send
	select {
	case pq.queue <- msg:
		// Message was sent successfully
	case <-pq.done:
		koutaku.releaseChannelMessage(msg)
		koutakuErr := newKoutakuErrorFromMsg("provider is shutting down")
		koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
		return nil, koutakuErr
	case <-ctx.Done():
		koutaku.releaseChannelMessage(msg)
		koutakuErr := newKoutakuCtxDoneError(ctx, "while waiting for queue space")
		koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
		return nil, koutakuErr
	default:
		if koutaku.dropExcessRequests.Load() {
			koutaku.releaseChannelMessage(msg)
			koutaku.logger.Warn("request dropped: queue is full, please increase the queue size or set dropExcessRequests to false")
			koutakuErr := newKoutakuErrorFromMsg("request dropped: queue is full")
			koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
			return nil, koutakuErr
		}
		// Re-check closing flag before blocking send (lock-free atomic check)
		if pq.isClosing() {
			koutaku.releaseChannelMessage(msg)
			koutakuErr := newKoutakuErrorFromMsg("provider is shutting down")
			koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
			return nil, koutakuErr
		}
		select {
		case pq.queue <- msg:
			// Message was sent successfully
		case <-pq.done:
			koutaku.releaseChannelMessage(msg)
			koutakuErr := newKoutakuErrorFromMsg("provider is shutting down")
			koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
			return nil, koutakuErr
		case <-ctx.Done():
			koutaku.releaseChannelMessage(msg)
			koutakuErr := newKoutakuCtxDoneError(ctx, "while waiting for queue space")
			koutakuErr.PopulateExtraFields(req.RequestType, provider, model, model)
			return nil, koutakuErr
		}
	}

	select {
	case stream := <-msg.ResponseStream:
		koutaku.releaseChannelMessage(msg)
		return stream, nil
	case koutakuErrVal := <-msg.Err:
		if koutakuErrVal.Error != nil {
			koutaku.logger.Debug("error while executing stream request: %s", koutakuErrVal.Error.Message)
		} else {
			koutaku.logger.Debug("error while executing stream request: %+v", koutakuErrVal)
		}
		// Marking final chunk
		ctx.SetValue(schemas.KoutakuContextKeyStreamEndIndicator, true)
		// On error we will complete post-hooks
		recoveredResp, recoveredErr := pipeline.RunPostLLMHooks(ctx, nil, &koutakuErrVal, len(*koutaku.llmPlugins.Load()))
		drainAndAttachPluginLogs(ctx)
		koutaku.releaseChannelMessage(msg)
		if recoveredErr != nil {
			return nil, recoveredErr
		}
		if recoveredResp != nil {
			return newKoutakuMessageChan(recoveredResp), nil
		}
		return nil, &koutakuErrVal
	case <-ctx.Done():
		// Do NOT releaseChannelMessage here — see the identical note in tryRequest.
		// Worker still holds msg.ResponseStream/msg.Err; releasing now corrupts the
		// next request that reuses those pooled channels.
		return nil, newKoutakuCtxDoneError(ctx, "while waiting for stream response")
	}
}

// executeRequestWithRetries is a generic function that handles common request processing logic.
// It consolidates retry logic, backoff calculation, error handling, and key rotation.
// It is not a koutaku method because interface methods in go cannot be generic.
//
// keyProvider, when non-nil, is called on the first attempt and again whenever a rate-limit error
// triggers a key rotation. It receives the set of key IDs already used in the current rotation
// cycle so it can exclude them; when the pool is exhausted the provider resets the set and starts
// a fresh weighted round. Network errors (5xx) reuse the same key since they are transient server
// issues rather than per-key capacity problems.
func executeRequestWithRetries[T any](
	ctx *schemas.KoutakuContext,
	config *schemas.ProviderConfig,
	requestHandler func(key schemas.Key) (T, *schemas.KoutakuError),
	keyProvider func(usedKeyIDs map[string]bool) (schemas.Key, error),
	requestType schemas.RequestType,
	providerKey schemas.ModelProvider,
	model string,
	req *schemas.KoutakuRequest,
	logger schemas.Logger,
) (T, *schemas.KoutakuError) {
	var result T
	var koutakuError *schemas.KoutakuError
	var attempts int

	var currentKey schemas.Key
	var usedKeyIDs map[string]bool
	lastWasRateLimit := false

	for attempts = 0; attempts <= config.NetworkConfig.MaxRetries; attempts++ {
		ctx.SetValue(schemas.KoutakuContextKeyNumberOfRetries, attempts)

		// Reset the trail on the first attempt so a reused or shared context (koutaku.ctx)
		// doesn't carry over records from a previous request.
		if keyProvider != nil && attempts == 0 {
			ctx.SetValue(schemas.KoutakuContextKeyAttemptTrail, []schemas.KeyAttemptRecord{})
		}

		// Select / rotate key: always on attempt 0, and again when the previous failure was a
		// rate-limit (different key may have remaining capacity). Network errors keep the same key.
		if keyProvider != nil && (attempts == 0 || lastWasRateLimit) {
			if usedKeyIDs == nil {
				usedKeyIDs = make(map[string]bool)
			}

			// Wrap key selection in a dedicated span so traces show which key was chosen
			// (and when rotation happened). The span is opened before keyProvider is called
			// so selection errors are captured too.
			keyTracer, _ := ctx.Value(schemas.KoutakuContextKeyTracer).(schemas.Tracer)
			var keySpanCtx context.Context
			var keyHandle schemas.SpanHandle
			if keyTracer != nil {
				keySpanCtx, keyHandle = keyTracer.StartSpan(ctx, "key.selection", schemas.SpanKindInternal)
				keyTracer.SetAttribute(keyHandle, schemas.AttrProviderName, string(providerKey))
				keyTracer.SetAttribute(keyHandle, schemas.AttrRequestModel, model)
				if attempts > 0 {
					keyTracer.SetAttribute(keyHandle, "retry.count", attempts)
				}
			}

			selectedKey, err := keyProvider(usedKeyIDs)

			if keyTracer != nil {
				if err != nil {
					keyTracer.SetAttribute(keyHandle, "error", err.Error())
					keyTracer.EndSpan(keyHandle, schemas.SpanStatusError, err.Error())
				} else {
					keyTracer.SetAttribute(keyHandle, "key.id", selectedKey.ID)
					keyTracer.SetAttribute(keyHandle, "key.name", selectedKey.Name)
					keyTracer.EndSpan(keyHandle, schemas.SpanStatusOk, "")
					// Propagate the span context so subsequent spans (llm.call / retry.attempt.N)
					// are correctly linked in the trace hierarchy.
					ctx.SetValue(schemas.KoutakuContextKeySpanID, keySpanCtx.Value(schemas.KoutakuContextKeySpanID))
				}
			}

			if err != nil {
				var zero T
				return zero, newKoutakuErrorFromMsg(err.Error())
			}
			currentKey = selectedKey
			ctx.SetValue(schemas.KoutakuContextKeySelectedKeyID, currentKey.ID)
			ctx.SetValue(schemas.KoutakuContextKeySelectedKeyName, currentKey.Name)
		}

		// Append a trail record for every attempt (key rotation and same-key retries alike).
		// Skipped when keyProvider is nil (keyless providers have no key to track).
		// FailReason is populated below once the attempt outcome is known.
		if keyProvider != nil {
			schemas.AppendToContextList(ctx, schemas.KoutakuContextKeyAttemptTrail, schemas.KeyAttemptRecord{
				Attempt: attempts,
				KeyID:   currentKey.ID,
				KeyName: currentKey.Name,
			})
		}

		if attempts > 0 {
			// Log retry attempt
			var retryMsg string
			if koutakuError != nil && koutakuError.Error != nil {
				retryMsg = koutakuError.Error.Message
			} else if koutakuError != nil && koutakuError.StatusCode != nil {
				retryMsg = fmt.Sprintf("status=%d", *koutakuError.StatusCode)
				if koutakuError.Type != nil {
					retryMsg += ", type=" + *koutakuError.Type
				}
			}
			logger.Debug("retrying request (attempt %d/%d) for model %s: %s", attempts, config.NetworkConfig.MaxRetries, model, retryMsg)

			// Calculate and apply backoff
			backoff := calculateBackoff(attempts-1, config)
			logger.Debug("sleeping for %s before retry", backoff)

			time.Sleep(backoff)
		}

		logger.Debug("attempting %s request for provider %s", requestType, providerKey)

		// Start span for LLM call (or retry attempt)
		tracer, ok := ctx.Value(schemas.KoutakuContextKeyTracer).(schemas.Tracer)
		if !ok || tracer == nil {
			logger.Error("tracer not found in context of executeRequestWithRetries")
			return result, newKoutakuErrorFromMsg("tracer not found in context")
		}
		var spanName string
		var spanKind schemas.SpanKind
		if attempts > 0 {
			spanName = fmt.Sprintf("retry.attempt.%d", attempts)
			spanKind = schemas.SpanKindRetry
		} else {
			spanName = "llm.call"
			spanKind = schemas.SpanKindLLMCall
		}
		spanCtx, handle := tracer.StartSpan(ctx, spanName, spanKind)
		tracer.SetAttribute(handle, schemas.AttrProviderName, string(providerKey))
		tracer.SetAttribute(handle, schemas.AttrRequestModel, model)
		tracer.SetAttribute(handle, "request.type", string(requestType))
		if attempts > 0 {
			tracer.SetAttribute(handle, "retry.count", attempts)
		}

		// Add context-related attributes (selected key, virtual key, team, customer, etc.)
		if selectedKeyID, ok := ctx.Value(schemas.KoutakuContextKeySelectedKeyID).(string); ok && selectedKeyID != "" {
			tracer.SetAttribute(handle, schemas.AttrSelectedKeyID, selectedKeyID)
		}
		if selectedKeyName, ok := ctx.Value(schemas.KoutakuContextKeySelectedKeyName).(string); ok && selectedKeyName != "" {
			tracer.SetAttribute(handle, schemas.AttrSelectedKeyName, selectedKeyName)
		}
		if virtualKeyID, ok := ctx.Value(schemas.KoutakuContextKeyGovernanceVirtualKeyID).(string); ok && virtualKeyID != "" {
			tracer.SetAttribute(handle, schemas.AttrVirtualKeyID, virtualKeyID)
		}
		if virtualKeyName, ok := ctx.Value(schemas.KoutakuContextKeyGovernanceVirtualKeyName).(string); ok && virtualKeyName != "" {
			tracer.SetAttribute(handle, schemas.AttrVirtualKeyName, virtualKeyName)
		}
		if teamID, ok := ctx.Value(schemas.KoutakuContextKeyGovernanceTeamID).(string); ok && teamID != "" {
			tracer.SetAttribute(handle, schemas.AttrTeamID, teamID)
		}
		if teamName, ok := ctx.Value(schemas.KoutakuContextKeyGovernanceTeamName).(string); ok && teamName != "" {
			tracer.SetAttribute(handle, schemas.AttrTeamName, teamName)
		}
		if customerID, ok := ctx.Value(schemas.KoutakuContextKeyGovernanceCustomerID).(string); ok && customerID != "" {
			tracer.SetAttribute(handle, schemas.AttrCustomerID, customerID)
		}
		if customerName, ok := ctx.Value(schemas.KoutakuContextKeyGovernanceCustomerName).(string); ok && customerName != "" {
			tracer.SetAttribute(handle, schemas.AttrCustomerName, customerName)
		}
		if fallbackIndex, ok := ctx.Value(schemas.KoutakuContextKeyFallbackIndex).(int); ok {
			tracer.SetAttribute(handle, schemas.AttrFallbackIndex, fallbackIndex)
		}
		tracer.SetAttribute(handle, schemas.AttrNumberOfRetries, attempts)

		// Populate LLM request attributes (messages, parameters, etc.)
		if req != nil {
			tracer.PopulateLLMRequestAttributes(handle, req)
		}

		// Update context with span ID
		ctx.SetValue(schemas.KoutakuContextKeySpanID, spanCtx.Value(schemas.KoutakuContextKeySpanID))

		// Record stream start time for TTFT calculation (only for streaming requests)
		// This is also used by RunPostLLMHooks to detect streaming mode
		if IsStreamRequestType(requestType) {
			streamStartTime := time.Now()
			ctx.SetValue(schemas.KoutakuContextKeyStreamStartTime, streamStartTime)
		}

		// Attempt the request
		result, koutakuError = requestHandler(currentKey)

		// For streaming requests that returned success, check if the first chunk
		// is actually an error (e.g., rate limits sent as SSE events in HTTP 200).
		// This enables retries and fallbacks for providers that embed errors in
		// the SSE stream instead of returning proper HTTP error status codes.
		if koutakuError == nil {
			if streamChan, ok := any(result).(chan *schemas.KoutakuStreamChunk); ok {
				checkedStream, drainDone, firstChunkErr := providerUtils.CheckFirstStreamChunkForError(ctx, streamChan)
				if firstChunkErr != nil {
					<-drainDone
					koutakuError = firstChunkErr
				} else {
					result = any(checkedStream).(T)
				}
			}
		}

		// Check if result is a streaming channel - if so, defer span completion
		// Only defer for successful stream setup; error paths must end the span synchronously
		isStreamChan := false
		if koutakuError == nil {
			if ch, ok := any(result).(chan *schemas.KoutakuStreamChunk); ok && ch != nil {
				isStreamChan = true
			}
		}
		if isStreamChan {
			// For streaming requests, store the span handle in TraceStore keyed by trace ID
			// This allows the provider's streaming goroutine to retrieve it later
			if traceID, ok := ctx.Value(schemas.KoutakuContextKeyTraceID).(string); ok && traceID != "" {
				tracer.StoreDeferredSpan(traceID, handle)
			}
			// Don't end the span here - it will be ended when streaming completes
		} else {
			// Populate LLM response attributes for non-streaming responses
			if resp, ok := any(result).(*schemas.KoutakuResponse); ok {
				tracer.PopulateLLMResponseAttributes(ctx, handle, resp, koutakuError)
			}

			// End span with appropriate status
			if koutakuError != nil {
				if koutakuError.Error != nil {
					tracer.SetAttribute(handle, "error", koutakuError.Error.Message)
				}
				if koutakuError.StatusCode != nil {
					tracer.SetAttribute(handle, "status_code", *koutakuError.StatusCode)
				}
				tracer.EndSpan(handle, schemas.SpanStatusError, "request failed")
			} else {
				tracer.EndSpan(handle, schemas.SpanStatusOk, "")
			}
		}

		logger.Debug("request %s for provider %s completed", requestType, providerKey)

		// Check if successful or if we should retry
		if koutakuError == nil ||
			koutakuError.IsKoutakuError ||
			(koutakuError.Error != nil && koutakuError.Error.Type != nil && *koutakuError.Error.Type == schemas.RequestCancelled) {
			break
		}

		// Check if we should retry based on status code or error message
		shouldRetry := false
		isRateLimit := (koutakuError.StatusCode != nil && *koutakuError.StatusCode == 429) ||
			(koutakuError.Error != nil &&
				(IsRateLimitErrorMessage(koutakuError.Error.Message) ||
					(koutakuError.Error.Type != nil && IsRateLimitErrorMessage(*koutakuError.Error.Type)) ||
					(koutakuError.Error.Code != nil && IsRateLimitErrorMessage(*koutakuError.Error.Code))))

		errMessage := GetErrorMessage(koutakuError)

		if koutakuError.Error != nil &&
			(koutakuError.Error.Message == schemas.ErrProviderDoRequest ||
				koutakuError.Error.Message == schemas.ErrProviderNetworkError) {
			shouldRetry = true
			logger.Debug("detected request HTTP/network error, will retry: %s", errMessage)
		} else if (koutakuError.StatusCode != nil && retryableStatusCodes[*koutakuError.StatusCode]) || isRateLimit {
			shouldRetry = true
			logger.Debug("encountered error that should be retried: %s", errMessage)
		}

		// Fill FailReason on any failed attempt (retryable or terminal).
		// Use the provider error type when present; fall back to "unknown".
		if trail, ok := ctx.Value(schemas.KoutakuContextKeyAttemptTrail).([]schemas.KeyAttemptRecord); ok && len(trail) > 0 {
			reason := "unknown"
			if koutakuError.Error != nil && koutakuError.Error.Type != nil && *koutakuError.Error.Type != "" {
				reason = *koutakuError.Error.Type
			} else if isRateLimit {
				reason = "rate_limit_error"
			}
			trail[len(trail)-1].FailReason = &reason
			ctx.SetValue(schemas.KoutakuContextKeyAttemptTrail, trail)
		}

		if !shouldRetry {
			break
		}

		// Mark current key as used so the next selection excludes it (rate-limit only).
		// Network errors keep the same key — they are transient server issues, not per-key.
		if isRateLimit && keyProvider != nil {
			if usedKeyIDs == nil {
				usedKeyIDs = make(map[string]bool)
			}
			usedKeyIDs[currentKey.ID] = true
		}
		lastWasRateLimit = isRateLimit
	}

	// Add retry information to error
	if attempts > 0 {
		logger.Debug("request failed after %d %s", attempts, map[bool]string{true: "attempts", false: "attempt"}[attempts > 1])
	}

	// On final error, clear selected_key so it only reflects a key that actually served a successful response.
	// The attempt trail is the authoritative record of which keys were tried.
	if koutakuError != nil && keyProvider != nil {
		ctx.SetValue(schemas.KoutakuContextKeySelectedKeyID, "")
		ctx.SetValue(schemas.KoutakuContextKeySelectedKeyName, "")
	}

	return result, koutakuError
}

// requestWorker handles incoming requests from the queue for a specific provider.
// It manages retries, error handling, and response processing.
func (koutaku *Koutaku) requestWorker(provider schemas.Provider, config *schemas.ProviderConfig, pq *ProviderQueue) {
	defer func() {
		if waitGroupValue, ok := koutaku.waitGroups.Load(provider.GetProviderKey()); ok {
			waitGroup := waitGroupValue.(*sync.WaitGroup)
			waitGroup.Done()
		}
	}()

	for {
		var req *ChannelMessage
		select {
		case r := <-pq.queue:
			req = r
		case <-pq.done:
			// Provider is shutting down. Drain any buffered requests and send
			// back errors so callers are not left blocked on their response channel.
			for {
				select {
				case r := <-pq.queue:
					provKey, mod, _ := r.GetRequestFields()
					select {
					case r.Err <- schemas.KoutakuError{
						IsKoutakuError: false,
						Error: &schemas.ErrorField{
							Message: "provider is shutting down",
						},
						ExtraFields: schemas.KoutakuErrorExtraFields{
							RequestType:            r.RequestType,
							Provider:               provKey,
							OriginalModelRequested: mod,
						},
					}:
					case <-r.Context.Done():
					}
				default:
					return
				}
			}
		}

		_, model, _ := req.KoutakuRequest.GetRequestFields()

		var result *schemas.KoutakuResponse
		var stream chan *schemas.KoutakuStreamChunk
		var koutakuError *schemas.KoutakuError
		var err error

		// Determine the base provider type for key requirement checks
		baseProvider := provider.GetProviderKey()
		if cfg := config.CustomProviderConfig; cfg != nil && cfg.BaseProviderType != "" {
			baseProvider = cfg.BaseProviderType
		}
		req.Context.SetValue(schemas.KoutakuContextKeyIsCustomProvider, !IsStandardProvider(baseProvider))

		// Determine whether this provider attempt should capture raw payloads.
		//
		// Effective values are computed by merging provider config with any per-request
		// context overrides (KoutakuContextKeySendBackRawRequest/Response and
		// KoutakuContextKeyStoreRawRequestResponse). A context value set to either true
		// or false fully overrides the provider config for that flag.
		//
		// Each flag is independent:
		//   send_back_raw_request  — include raw request bytes in the client response.
		//   send_back_raw_response — include raw response bytes in the client response.
		//   store_raw_request_response — persist raw bytes in log records (logging plugin only).
		//
		// Capture is enabled per-side whenever send-back OR store is requested for that side.
		// Strip flags tell the response path to remove that side's bytes before the payload
		// reaches the caller (used when store=true but send-back=false for that side).
		//
		// All internal signals are always written explicitly on every attempt so stale values
		// from a previous provider attempt (e.g. different fallback provider config) cannot
		// leak into the new attempt on a reused context. The user override keys
		// (KoutakuContextKeySendBackRaw*, KoutakuContextKeyStoreRawRequestResponse) are
		// never overwritten — they are read-only from koutaku.go's perspective.

		// Step 1: compute effective value for each flag (provider config ← per-request override).
		effectiveSendBackReq := config.SendBackRawRequest
		if override, ok := req.Context.Value(schemas.KoutakuContextKeySendBackRawRequest).(bool); ok {
			effectiveSendBackReq = override
		}
		effectiveSendBackResp := config.SendBackRawResponse
		if override, ok := req.Context.Value(schemas.KoutakuContextKeySendBackRawResponse).(bool); ok {
			effectiveSendBackResp = override
		}
		effectiveStore := config.StoreRawRequestResponse
		if override, ok := req.Context.Value(schemas.KoutakuContextKeyStoreRawRequestResponse).(bool); ok {
			effectiveStore = override
		}

		// Step 2: derive per-side capture and strip flags.
		// Capture if we need to send the data back OR store it — independent per side.
		captureReq := effectiveSendBackReq || effectiveStore
		captureResp := effectiveSendBackResp || effectiveStore
		// Strip from client response if we captured for storage but not for send-back.
		dropReq := effectiveStore && !effectiveSendBackReq
		dropResp := effectiveStore && !effectiveSendBackResp

		// Step 3: write all internal signals explicitly (never touch the user override keys).
		req.Context.SetValue(schemas.KoutakuContextKeyCaptureRawRequest, captureReq)
		req.Context.SetValue(schemas.KoutakuContextKeyCaptureRawResponse, captureResp)
		req.Context.SetValue(schemas.KoutakuContextKeyDropRawRequestFromClient, dropReq)
		req.Context.SetValue(schemas.KoutakuContextKeyDropRawResponseFromClient, dropResp)
		// Tells the logging plugin whether to persist raw bytes in log records.
		req.Context.SetValue(schemas.KoutakuContextKeyShouldStoreRawInLogs, effectiveStore)

		var keys []schemas.Key
		// keyProvider is passed to executeRequestWithRetries to manage key selection and rotation.
		// It is nil when no key is required (e.g. providerRequiresKey=false) or for multi-key
		// batch/file/container operations that manage their own key lists.
		var keyProvider func(usedKeyIDs map[string]bool) (schemas.Key, error)

		if providerRequiresKey(config.CustomProviderConfig) {
			// ListModels needs all enabled/supported keys so providers can aggregate
			// and report per-key statuses (KeyStatuses).
			if req.RequestType == schemas.ListModelsRequest {
				keys, err = koutaku.getAllSupportedKeys(req.Context, provider.GetProviderKey(), baseProvider)
				if err != nil {
					koutaku.logger.Debug("error getting supported keys for list models: %v", err)
					req.Err <- schemas.KoutakuError{
						IsKoutakuError: false,
						Error: &schemas.ErrorField{
							Message: err.Error(),
							Error:   err,
						},
						ExtraFields: schemas.KoutakuErrorExtraFields{
							Provider:               provider.GetProviderKey(),
							RequestType:            req.RequestType,
							OriginalModelRequested: model,
							ResolvedModelUsed:      model,
						},
					}
					continue
				}
			} else {
				// Determine if this is a multi-key batch/file/container operation
				// BatchCreate, FileUpload, ContainerCreate, ContainerFileCreate use single key; other batch/file/container ops use multiple keys
				isMultiKeyBatchOp := isBatchRequestType(req.RequestType) && req.RequestType != schemas.BatchCreateRequest
				isMultiKeyFileOp := isFileRequestType(req.RequestType) && req.RequestType != schemas.FileUploadRequest
				isMultiKeyContainerOp := isContainerRequestType(req.RequestType) && req.RequestType != schemas.ContainerCreateRequest && req.RequestType != schemas.ContainerFileCreateRequest

				if isMultiKeyBatchOp || isMultiKeyFileOp || isMultiKeyContainerOp {
					var modelPtr *string
					if model != "" {
						modelPtr = &model
					}
					keys, err = koutaku.getKeysForBatchAndFileOps(req.Context, provider.GetProviderKey(), baseProvider, modelPtr, isMultiKeyBatchOp)
					if err != nil {
						koutaku.logger.Debug("error getting keys for batch/file operation: %v", err)
						req.Err <- schemas.KoutakuError{
							IsKoutakuError: false,
							Error: &schemas.ErrorField{
								Message: err.Error(),
								Error:   err,
							},
							ExtraFields: schemas.KoutakuErrorExtraFields{
								Provider:               provider.GetProviderKey(),
								RequestType:            req.RequestType,
								OriginalModelRequested: model,
								ResolvedModelUsed:      model,
							},
						}
						continue
					}
				} else {
					// Build the key pool for this request. Selection and rotation are deferred to
					// executeRequestWithRetries via keyProvider so that each retry attempt can use
					// a different key (on rate-limit errors) without re-running the full filtering.
					supportedKeys, canRotate, keyPoolErr := koutaku.selectKeyFromProviderForModelWithPool(req.Context, req.RequestType, provider.GetProviderKey(), model, baseProvider)
					if keyPoolErr != nil {
						koutaku.logger.Debug("error building key pool for model %s: %v", model, keyPoolErr)
						req.Err <- schemas.KoutakuError{
							IsKoutakuError: false,
							Error: &schemas.ErrorField{
								Message: keyPoolErr.Error(),
								Error:   keyPoolErr,
							},
							ExtraFields: schemas.KoutakuErrorExtraFields{
								Provider:               provider.GetProviderKey(),
								RequestType:            req.RequestType,
								OriginalModelRequested: model,
								ResolvedModelUsed:      model,
							},
						}
						continue
					}

					if len(supportedKeys) == 0 {
						// SkipKeySelection path — keyProvider stays nil, zero Key is used.
					} else if !canRotate {
						// Fixed key (DirectKey, explicit ID/name, session stickiness): always
						// return the same key regardless of usedKeyIDs.
						fixedKey := supportedKeys[0]
						keyProvider = func(_ map[string]bool) (schemas.Key, error) {
							return fixedKey, nil
						}
					} else {
						// Rotating pool: weighted selection with per-cycle exclusion.
						// Captures supportedKeys, koutaku.keySelector, provider/model by value.
						pool := supportedKeys
						provKey := provider.GetProviderKey()
						mdl := model
						keyProvider = func(usedKeyIDs map[string]bool) (schemas.Key, error) {
							available := make([]schemas.Key, 0, len(pool))
							for _, k := range pool {
								if !usedKeyIDs[k.ID] {
									available = append(available, k)
								}
							}
							if len(available) == 0 {
								// All keys exhausted — start a fresh weighted round.
								for id := range usedKeyIDs {
									delete(usedKeyIDs, id)
								}
								available = pool
							}
							return koutaku.keySelector(req.Context, available, provKey, mdl)
						}
					}
				}
			}
		}

		originalModelRequested := model
		// resolvedModel is set inside the handler closures below on every attempt so that each
		// key's own alias mapping is applied. The outer var holds the LAST attempt's value and is
		// read single-threaded by the worker after retries finish (e.g. the error-fallback at
		// line 5653). Streaming postHookRunner must NOT capture this var by reference — it
		// snapshots its own attemptResolvedModel inside the per-attempt closure.
		var resolvedModel string
		// lastAttemptFinalizer captures the LAST attempt's postHookSpanFinalizer for the
		// worker-level error fallback below. Single-threaded write (assigned by the retry
		// loop's per-attempt closure) and single-threaded read (after retries finish), so
		// no synchronization needed. Earlier attempts' finalizers fire via their provider
		// goroutines' defers — passed via the postHookSpanFinalizer parameter directly to
		// handleProviderStreamRequest, never via the shared req.Context.
		var lastAttemptFinalizer func(context.Context)

		// Execute request with retries. For streaming, the plugin pipeline,
		// postHookRunner, and finalizer are allocated per-attempt inside the
		// request handler closure. If they were request-scoped, a retry
		// triggered by CheckFirstStreamChunkForError could run against a
		// pipeline the previous attempt's provider goroutine has already
		// returned to the pool via its deferred finalizer.
		if IsStreamRequestType(req.RequestType) {
			stream, koutakuError = executeRequestWithRetries(req.Context, config, func(k schemas.Key) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
				resolvedModel = k.Aliases.Resolve(originalModelRequested)
				req.SetModel(resolvedModel)
				// Snapshot per-attempt so postHookRunner doesn't observe a later retry's
				// alias while this attempt's provider goroutine is still emitting chunks.
				attemptResolvedModel := resolvedModel
				pipeline := koutaku.getPluginPipeline()
				postHookRunner := func(ctx *schemas.KoutakuContext, result *schemas.KoutakuResponse, err *schemas.KoutakuError) (*schemas.KoutakuResponse, *schemas.KoutakuError) {
					// Populate extra fields before RunPostLLMHooks so plugins (e.g. logging)
					// can read requestType/provider/model from the chunk or error.
					// Uses the per-attempt snapshot — capturing the outer resolvedModel by
					// reference would let a later retry's alias bleed into this attempt's chunks.
					if result != nil {
						result.PopulateExtraFields(req.RequestType, provider.GetProviderKey(), originalModelRequested, attemptResolvedModel)
					}
					if err != nil {
						err.PopulateExtraFields(req.RequestType, provider.GetProviderKey(), originalModelRequested, attemptResolvedModel)
					}
					resp, koutakuErr := pipeline.RunPostLLMHooks(ctx, result, err, len(*koutaku.llmPlugins.Load()))
					if IsFinalChunk(ctx) {
						drainAndAttachPluginLogs(ctx)
					}
					if koutakuErr != nil {
						koutakuErr.PopulateExtraFields(req.RequestType, provider.GetProviderKey(), originalModelRequested, attemptResolvedModel)
						return nil, koutakuErr
					} else if resp != nil {
						resp.PopulateExtraFields(req.RequestType, provider.GetProviderKey(), originalModelRequested, attemptResolvedModel)
					}
					return resp, nil
				}
				// Store a finalizer callback to create aggregated post-hook spans at stream end.
				// Wrapped in sync.Once so the normal end-of-stream invocation and a deferred
				// safety-net invocation (e.g. from a provider goroutine's panic path) cannot
				// double-release the pipeline.
				var finalizerOnce sync.Once
				postHookSpanFinalizer := func(ctx context.Context) {
					finalizerOnce.Do(func() {
						pipeline.FinalizeStreamingPostHookSpans(ctx)
						koutaku.releasePluginPipeline(pipeline)
					})
				}
				lastAttemptFinalizer = postHookSpanFinalizer
				streamCh, streamErr := koutaku.handleProviderStreamRequest(provider, req, k, postHookRunner, postHookSpanFinalizer)
				// If stream setup failed before any provider goroutine started,
				// no deferred finalizer will run — release the pipeline directly
				// so a retry doesn't inherit a leaked pool entry.
				if streamErr != nil && streamCh == nil {
					finalizerOnce.Do(func() {
						koutaku.releasePluginPipeline(pipeline)
					})
				}
				return streamCh, streamErr
			}, keyProvider, req.RequestType, provider.GetProviderKey(), model, &req.KoutakuRequest, koutaku.logger)
		} else {
			result, koutakuError = executeRequestWithRetries(req.Context, config, func(k schemas.Key) (*schemas.KoutakuResponse, *schemas.KoutakuError) {
				resolvedModel = k.Aliases.Resolve(originalModelRequested)
				req.SetModel(resolvedModel)
				return koutaku.handleProviderRequest(provider, config, req, k, keys)
			}, keyProvider, req.RequestType, provider.GetProviderKey(), model, &req.KoutakuRequest, koutaku.logger)
		}

		// For streaming with an error, route release through the LAST attempt's
		// finalizer (wrapped in sync.Once) so we don't double-Put into the pool
		// or race the provider goroutine's deferred FinalizeStreamingPostHookSpans
		// call. lastAttemptFinalizer is set inside the per-attempt closure on every
		// iteration; after retries finish, it holds the LAST attempt's finalizer.
		// Earlier attempts' finalizers have already fired via their provider
		// goroutines' defers (passed via the postHookSpanFinalizer parameter
		// directly to handleProviderStreamRequest). For streaming without error,
		// the finalizer is invoked by completeDeferredSpan / the provider
		// goroutine's defer.
		if IsStreamRequestType(req.RequestType) && koutakuError != nil {
			if lastAttemptFinalizer != nil {
				lastAttemptFinalizer(req.Context)
			}
		}

		if koutakuError != nil {
			koutakuError.PopulateExtraFields(req.RequestType, provider.GetProviderKey(), originalModelRequested, resolvedModel)

			// Send error with context awareness to prevent deadlock
			select {
			case req.Err <- *koutakuError:
				// Error sent successfully
			case <-req.Context.Done():
				// Client no longer listening, log and continue
				koutaku.logger.Debug("Client context cancelled while sending error response")
			case <-time.After(5 * time.Second):
				// Timeout to prevent indefinite blocking
				koutaku.logger.Warn("Timeout while sending error response, client may have disconnected")
			}
		} else {
			if result != nil {
				result.PopulateExtraFields(req.RequestType, provider.GetProviderKey(), originalModelRequested, resolvedModel)
			}
			if IsStreamRequestType(req.RequestType) {
				// Send stream with context awareness to prevent deadlock
				select {
				case req.ResponseStream <- stream:
					// Stream sent successfully
				case <-req.Context.Done():
					// Client no longer listening, log and continue
					koutaku.logger.Debug("Client context cancelled while sending stream response")
				case <-time.After(5 * time.Second):
					// Timeout to prevent indefinite blocking
					koutaku.logger.Warn("Timeout while sending stream response, client may have disconnected")
				}
			} else {
				// Send response with context awareness to prevent deadlock
				select {
				case req.Response <- result:
					// Response sent successfully
				case <-req.Context.Done():
					// Client no longer listening, log and continue
					koutaku.logger.Debug("Client context cancelled while sending response")
				case <-time.After(5 * time.Second):
					// Timeout to prevent indefinite blocking
					koutaku.logger.Warn("Timeout while sending response, client may have disconnected")
				}
			}
		}
	}

	// koutaku.logger.Debug("worker for provider %s exiting...", provider.GetProviderKey())
}

// handleProviderRequest handles the request to the provider based on the request type
// key is used for single-key operations, keys is used for batch/file operations that need multiple keys
func (koutaku *Koutaku) handleProviderRequest(provider schemas.Provider, config *schemas.ProviderConfig, req *ChannelMessage, key schemas.Key, keys []schemas.Key) (*schemas.KoutakuResponse, *schemas.KoutakuError) {
	response := &schemas.KoutakuResponse{}
	switch req.RequestType {
	case schemas.ListModelsRequest:
		listModelsResponse, koutakuError := provider.ListModels(req.Context, keys, req.KoutakuRequest.ListModelsRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.ListModelsResponse = listModelsResponse
	case schemas.TextCompletionRequest:
		if changeType, ok := req.Context.Value(schemas.KoutakuContextKeyChangeRequestType).(schemas.RequestType); ok && changeType == schemas.ChatCompletionRequest {
			chatRequest := req.KoutakuRequest.TextCompletionRequest.ToKoutakuChatRequest()
			if chatRequest != nil {
				chatCompletionResponse, koutakuError := provider.ChatCompletion(req.Context, key, chatRequest)
				if koutakuError != nil {
					return nil, koutakuError
				}
				response.TextCompletionResponse = chatCompletionResponse.ToKoutakuTextCompletionResponse()
				break
			}
		}
		textCompletionResponse, koutakuError := provider.TextCompletion(req.Context, key, req.KoutakuRequest.TextCompletionRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.TextCompletionResponse = textCompletionResponse
	case schemas.ChatCompletionRequest:
		if changeType, ok := req.Context.Value(schemas.KoutakuContextKeyChangeRequestType).(schemas.RequestType); ok && changeType == schemas.ResponsesRequest {
			responsesRequest := req.KoutakuRequest.ChatRequest.ToResponsesRequest()
			if responsesRequest != nil {
				responsesResponse, koutakuError := provider.Responses(req.Context, key, responsesRequest)
				if koutakuError != nil {
					return nil, koutakuError
				}
				response.ChatResponse = responsesResponse.ToKoutakuChatResponse()
				break
			}
		}
		chatCompletionResponse, koutakuError := provider.ChatCompletion(req.Context, key, req.KoutakuRequest.ChatRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		chatCompletionResponse.BackfillParams(req.KoutakuRequest.ChatRequest)
		response.ChatResponse = chatCompletionResponse
	case schemas.ResponsesRequest:
		responsesResponse, koutakuError := provider.Responses(req.Context, key, req.KoutakuRequest.ResponsesRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		responsesResponse.BackfillParams(req.KoutakuRequest.ResponsesRequest)
		response.ResponsesResponse = responsesResponse
	case schemas.CountTokensRequest:
		countTokensResponse, koutakuError := provider.CountTokens(req.Context, key, req.KoutakuRequest.CountTokensRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.CountTokensResponse = countTokensResponse
	case schemas.EmbeddingRequest:
		embeddingResponse, koutakuError := provider.Embedding(req.Context, key, req.KoutakuRequest.EmbeddingRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.EmbeddingResponse = embeddingResponse
	case schemas.RerankRequest:
		rerankResponse, koutakuError := provider.Rerank(req.Context, key, req.KoutakuRequest.RerankRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.RerankResponse = rerankResponse
	case schemas.OCRRequest:
		var customProviderConfig *schemas.CustomProviderConfig
		if config != nil {
			customProviderConfig = config.CustomProviderConfig
		}
		if koutakuError := providerUtils.CheckOperationAllowed(provider.GetProviderKey(), customProviderConfig, schemas.OCRRequest); koutakuError != nil {
			if req.KoutakuRequest.OCRRequest != nil {
				koutakuError.ExtraFields.OriginalModelRequested = req.KoutakuRequest.OCRRequest.Model
			}
			return nil, koutakuError
		}
		ocrResponse, koutakuError := provider.OCR(req.Context, key, req.KoutakuRequest.OCRRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.OCRResponse = ocrResponse
	case schemas.SpeechRequest:
		speechResponse, koutakuError := provider.Speech(req.Context, key, req.KoutakuRequest.SpeechRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		speechResponse.BackfillParams(req.KoutakuRequest.SpeechRequest)
		response.SpeechResponse = speechResponse
	case schemas.TranscriptionRequest:
		transcriptionResponse, koutakuError := provider.Transcription(req.Context, key, req.KoutakuRequest.TranscriptionRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		transcriptionResponse.BackfillParams(req.KoutakuRequest.TranscriptionRequest)
		response.TranscriptionResponse = transcriptionResponse
	case schemas.ImageGenerationRequest:
		imageResponse, koutakuError := provider.ImageGeneration(req.Context, key, req.KoutakuRequest.ImageGenerationRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		imageResponse.BackfillParams(&req.KoutakuRequest)
		response.ImageGenerationResponse = imageResponse
	case schemas.ImageEditRequest:
		imageEditResponse, koutakuError := provider.ImageEdit(req.Context, key, req.KoutakuRequest.ImageEditRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		imageEditResponse.BackfillParams(&req.KoutakuRequest)
		response.ImageGenerationResponse = imageEditResponse
	case schemas.ImageVariationRequest:
		imageVariationResponse, koutakuError := provider.ImageVariation(req.Context, key, req.KoutakuRequest.ImageVariationRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		imageVariationResponse.BackfillParams(&req.KoutakuRequest)
		response.ImageGenerationResponse = imageVariationResponse
	case schemas.VideoGenerationRequest:
		videoGenerationResponse, koutakuError := provider.VideoGeneration(req.Context, key, req.KoutakuRequest.VideoGenerationRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		videoGenerationResponse.BackfillParams(&req.KoutakuRequest)
		response.VideoGenerationResponse = videoGenerationResponse
	case schemas.VideoRetrieveRequest:
		videoRetrieveResponse, koutakuError := provider.VideoRetrieve(req.Context, key, req.KoutakuRequest.VideoRetrieveRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.VideoGenerationResponse = videoRetrieveResponse
	case schemas.VideoDownloadRequest:
		videoDownloadResponse, koutakuError := provider.VideoDownload(req.Context, key, req.KoutakuRequest.VideoDownloadRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.VideoDownloadResponse = videoDownloadResponse
	case schemas.VideoListRequest:
		videoListResponse, koutakuError := provider.VideoList(req.Context, key, req.KoutakuRequest.VideoListRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.VideoListResponse = videoListResponse
	case schemas.VideoDeleteRequest:
		videoDeleteResponse, koutakuError := provider.VideoDelete(req.Context, key, req.KoutakuRequest.VideoDeleteRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.VideoDeleteResponse = videoDeleteResponse
	case schemas.VideoRemixRequest:
		videoRemixResponse, koutakuError := provider.VideoRemix(req.Context, key, req.KoutakuRequest.VideoRemixRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.VideoGenerationResponse = videoRemixResponse
	case schemas.FileUploadRequest:
		fileUploadResponse, koutakuError := provider.FileUpload(req.Context, key, req.KoutakuRequest.FileUploadRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.FileUploadResponse = fileUploadResponse
	case schemas.FileListRequest:
		fileListResponse, koutakuError := provider.FileList(req.Context, keys, req.KoutakuRequest.FileListRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.FileListResponse = fileListResponse
	case schemas.FileRetrieveRequest:
		fileRetrieveResponse, koutakuError := provider.FileRetrieve(req.Context, keys, req.KoutakuRequest.FileRetrieveRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.FileRetrieveResponse = fileRetrieveResponse
	case schemas.FileDeleteRequest:
		fileDeleteResponse, koutakuError := provider.FileDelete(req.Context, keys, req.KoutakuRequest.FileDeleteRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.FileDeleteResponse = fileDeleteResponse
	case schemas.FileContentRequest:
		fileContentResponse, koutakuError := provider.FileContent(req.Context, keys, req.KoutakuRequest.FileContentRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.FileContentResponse = fileContentResponse
	case schemas.BatchCreateRequest:
		batchCreateResponse, koutakuError := provider.BatchCreate(req.Context, key, req.KoutakuRequest.BatchCreateRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.BatchCreateResponse = batchCreateResponse
	case schemas.BatchListRequest:
		batchListResponse, koutakuError := provider.BatchList(req.Context, keys, req.KoutakuRequest.BatchListRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.BatchListResponse = batchListResponse
	case schemas.BatchRetrieveRequest:
		batchRetrieveResponse, koutakuError := provider.BatchRetrieve(req.Context, keys, req.KoutakuRequest.BatchRetrieveRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.BatchRetrieveResponse = batchRetrieveResponse
	case schemas.BatchCancelRequest:
		batchCancelResponse, koutakuError := provider.BatchCancel(req.Context, keys, req.KoutakuRequest.BatchCancelRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.BatchCancelResponse = batchCancelResponse
	case schemas.BatchDeleteRequest:
		batchDeleteResponse, koutakuError := provider.BatchDelete(req.Context, keys, req.KoutakuRequest.BatchDeleteRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.BatchDeleteResponse = batchDeleteResponse
	case schemas.BatchResultsRequest:
		batchResultsResponse, koutakuError := provider.BatchResults(req.Context, keys, req.KoutakuRequest.BatchResultsRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.BatchResultsResponse = batchResultsResponse
	case schemas.ContainerCreateRequest:
		containerCreateResponse, koutakuError := provider.ContainerCreate(req.Context, key, req.KoutakuRequest.ContainerCreateRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.ContainerCreateResponse = containerCreateResponse
	case schemas.ContainerListRequest:
		containerListResponse, koutakuError := provider.ContainerList(req.Context, keys, req.KoutakuRequest.ContainerListRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.ContainerListResponse = containerListResponse
	case schemas.ContainerRetrieveRequest:
		containerRetrieveResponse, koutakuError := provider.ContainerRetrieve(req.Context, keys, req.KoutakuRequest.ContainerRetrieveRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.ContainerRetrieveResponse = containerRetrieveResponse
	case schemas.ContainerDeleteRequest:
		containerDeleteResponse, koutakuError := provider.ContainerDelete(req.Context, keys, req.KoutakuRequest.ContainerDeleteRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.ContainerDeleteResponse = containerDeleteResponse
	case schemas.ContainerFileCreateRequest:
		containerFileCreateResponse, koutakuError := provider.ContainerFileCreate(req.Context, key, req.KoutakuRequest.ContainerFileCreateRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.ContainerFileCreateResponse = containerFileCreateResponse
	case schemas.ContainerFileListRequest:
		containerFileListResponse, koutakuError := provider.ContainerFileList(req.Context, keys, req.KoutakuRequest.ContainerFileListRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.ContainerFileListResponse = containerFileListResponse
	case schemas.ContainerFileRetrieveRequest:
		containerFileRetrieveResponse, koutakuError := provider.ContainerFileRetrieve(req.Context, keys, req.KoutakuRequest.ContainerFileRetrieveRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.ContainerFileRetrieveResponse = containerFileRetrieveResponse
	case schemas.ContainerFileContentRequest:
		containerFileContentResponse, koutakuError := provider.ContainerFileContent(req.Context, keys, req.KoutakuRequest.ContainerFileContentRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.ContainerFileContentResponse = containerFileContentResponse
	case schemas.ContainerFileDeleteRequest:
		containerFileDeleteResponse, koutakuError := provider.ContainerFileDelete(req.Context, keys, req.KoutakuRequest.ContainerFileDeleteRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.ContainerFileDeleteResponse = containerFileDeleteResponse
	case schemas.PassthroughRequest:
		passthroughResponse, koutakuError := provider.Passthrough(req.Context, key, req.KoutakuRequest.PassthroughRequest)
		if koutakuError != nil {
			return nil, koutakuError
		}
		response.PassthroughResponse = passthroughResponse
	default:
		_, model, _ := req.KoutakuRequest.GetRequestFields()
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: fmt.Sprintf("unsupported request type: %s", req.RequestType),
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            req.RequestType,
				Provider:               provider.GetProviderKey(),
				OriginalModelRequested: model,
				ResolvedModelUsed:      model,
			},
		}
	}
	return response, nil
}

// handleProviderStreamRequest handles the stream request to the provider based on the request type
func (koutaku *Koutaku) handleProviderStreamRequest(provider schemas.Provider, req *ChannelMessage, key schemas.Key, postHookRunner schemas.PostHookRunner, postHookSpanFinalizer func(context.Context)) (chan *schemas.KoutakuStreamChunk, *schemas.KoutakuError) {
	switch req.RequestType {
	case schemas.TextCompletionStreamRequest:
		if changeType, ok := req.Context.Value(schemas.KoutakuContextKeyChangeRequestType).(schemas.RequestType); ok && changeType == schemas.ChatCompletionRequest {
			chatRequest := req.KoutakuRequest.TextCompletionRequest.ToKoutakuChatRequest()
			if chatRequest != nil {
				return provider.ChatCompletionStream(req.Context, wrapConvertedStreamPostHookRunner(postHookRunner, schemas.ChatCompletionRequest), postHookSpanFinalizer, key, chatRequest)
			}
		}
		return provider.TextCompletionStream(req.Context, postHookRunner, postHookSpanFinalizer, key, req.KoutakuRequest.TextCompletionRequest)
	case schemas.ChatCompletionStreamRequest:
		if changeType, ok := req.Context.Value(schemas.KoutakuContextKeyChangeRequestType).(schemas.RequestType); ok && changeType == schemas.ResponsesRequest {
			responsesRequest := req.KoutakuRequest.ChatRequest.ToResponsesRequest()
			if responsesRequest != nil {
				return provider.ResponsesStream(req.Context, wrapConvertedStreamPostHookRunner(postHookRunner, schemas.ResponsesRequest), postHookSpanFinalizer, key, responsesRequest)
			}
		}
		return provider.ChatCompletionStream(req.Context, postHookRunner, postHookSpanFinalizer, key, req.KoutakuRequest.ChatRequest)
	case schemas.ResponsesStreamRequest:
		return provider.ResponsesStream(req.Context, postHookRunner, postHookSpanFinalizer, key, req.KoutakuRequest.ResponsesRequest)
	case schemas.SpeechStreamRequest:
		return provider.SpeechStream(req.Context, postHookRunner, postHookSpanFinalizer, key, req.KoutakuRequest.SpeechRequest)
	case schemas.TranscriptionStreamRequest:
		return provider.TranscriptionStream(req.Context, postHookRunner, postHookSpanFinalizer, key, req.KoutakuRequest.TranscriptionRequest)
	case schemas.ImageGenerationStreamRequest:
		return provider.ImageGenerationStream(req.Context, postHookRunner, postHookSpanFinalizer, key, req.KoutakuRequest.ImageGenerationRequest)
	case schemas.ImageEditStreamRequest:
		return provider.ImageEditStream(req.Context, postHookRunner, postHookSpanFinalizer, key, req.KoutakuRequest.ImageEditRequest)
	case schemas.PassthroughStreamRequest:
		return provider.PassthroughStream(req.Context, postHookRunner, postHookSpanFinalizer, key, req.KoutakuRequest.PassthroughRequest)
	default:
		_, model, _ := req.KoutakuRequest.GetRequestFields()
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: fmt.Sprintf("unsupported request type: %s", req.RequestType),
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType:            req.RequestType,
				Provider:               provider.GetProviderKey(),
				OriginalModelRequested: model,
				ResolvedModelUsed:      model,
			},
		}
	}
}

// handleMCPToolExecution is the common handler for MCP tool execution with plugin pipeline support.
// It handles pre-hooks, execution, post-hooks, and error handling for both Chat and Responses formats.
//
// Parameters:
//   - ctx: Execution context
//   - mcpRequest: The MCP request to execute (already populated with tool call)
//   - requestType: The request type for error reporting (ChatCompletionRequest or ResponsesRequest)
//
// Returns:
//   - *schemas.KoutakuMCPResponse: The MCP response after all hooks
//   - *schemas.KoutakuError: Any execution error
func (koutaku *Koutaku) handleMCPToolExecution(ctx *schemas.KoutakuContext, mcpRequest *schemas.KoutakuMCPRequest, requestType schemas.RequestType) (*schemas.KoutakuMCPResponse, *schemas.KoutakuError) {
	if koutaku.MCPManager == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "mcp is not configured in this koutaku instance",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: requestType,
			},
		}
	}

	// Ensure request ID exists for hooks/tracing consistency
	if _, ok := ctx.Value(schemas.KoutakuContextKeyRequestID).(string); !ok {
		ctx.SetValue(schemas.KoutakuContextKeyRequestID, uuid.New().String())
	}

	// Get plugin pipeline for MCP hooks
	pipeline := koutaku.getPluginPipeline()
	defer koutaku.releasePluginPipeline(pipeline)

	// Run pre-hooks
	preReq, shortCircuit, preCount := pipeline.RunMCPPreHooks(ctx, mcpRequest)

	// Handle short-circuit cases
	if shortCircuit != nil {
		// Handle short-circuit with response (success case)
		if shortCircuit.Response != nil {
			finalMcpResp, koutakuErr := pipeline.RunMCPPostHooks(ctx, shortCircuit.Response, nil, preCount)
			drainAndAttachPluginLogs(ctx)
			if koutakuErr != nil {
				return nil, koutakuErr
			}
			return finalMcpResp, nil
		}
		// Handle short-circuit with error
		if shortCircuit.Error != nil {
			// Capture post-hook results to respect transformations or recovery
			finalResp, finalErr := pipeline.RunMCPPostHooks(ctx, nil, shortCircuit.Error, preCount)
			drainAndAttachPluginLogs(ctx)
			// Return post-hook error if present (post-hook may have transformed the error)
			if finalErr != nil {
				return nil, finalErr
			}
			// Return post-hook response if present (post-hook may have recovered from error)
			if finalResp != nil {
				return finalResp, nil
			}
			// Fall back to original short-circuit error if post-hooks returned nil/nil
			return nil, shortCircuit.Error
		}
	}

	if preReq == nil {
		return nil, &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "MCP request after plugin hooks cannot be nil",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: requestType,
			},
		}
	}

	// Execute tool with modified request
	result, err := koutaku.MCPManager.ExecuteToolCall(ctx, preReq)

	// Prepare MCP response and error for post-hooks
	var mcpResp *schemas.KoutakuMCPResponse
	var koutakuErr *schemas.KoutakuError

	if err != nil {
		koutakuErr = &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: err.Error(),
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: requestType,
			},
		}
		// Preserve MCPUserOAuthRequiredError for downstream detection in agent mode
		var oauthErr *schemas.MCPUserOAuthRequiredError
		if errors.As(err, &oauthErr) {
			koutakuErr.ExtraFields.MCPAuthRequired = oauthErr
		}
	} else if result == nil {
		koutakuErr = &schemas.KoutakuError{
			IsKoutakuError: false,
			Error: &schemas.ErrorField{
				Message: "tool execution returned nil result",
			},
			ExtraFields: schemas.KoutakuErrorExtraFields{
				RequestType: requestType,
			},
		}
	} else {
		// Use the MCP response directly
		mcpResp = result
	}

	// Run post-hooks
	finalResp, finalErr := pipeline.RunMCPPostHooks(ctx, mcpResp, koutakuErr, preCount)
	drainAndAttachPluginLogs(ctx)

	if finalErr != nil {
		return nil, finalErr
	}

	return finalResp, nil
}

// executeMCPToolWithHooks is a wrapper around handleMCPToolExecution that matches the signature
// expected by the agent's executeToolFunc parameter. It runs MCP plugin hooks before and after
// tool execution to enable logging, telemetry, and other plugin functionality.
func (koutaku *Koutaku) executeMCPToolWithHooks(ctx *schemas.KoutakuContext, request *schemas.KoutakuMCPRequest) (*schemas.KoutakuMCPResponse, error) {
	// Defensive check: context must be non-nil to prevent panics in plugin hooks
	if ctx == nil {
		return nil, fmt.Errorf("context cannot be nil")
	}

	if request == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}

	// Determine request type from the MCP request - explicitly handle all known types
	var requestType schemas.RequestType
	switch request.RequestType {
	case schemas.MCPRequestTypeChatToolCall:
		requestType = schemas.ChatCompletionRequest
	case schemas.MCPRequestTypeResponsesToolCall:
		requestType = schemas.ResponsesRequest
	default:
		// Return error for unknown/unsupported request types instead of silently defaulting
		return nil, fmt.Errorf("unsupported MCP request type: %s", request.RequestType)
	}

	resp, koutakuErr := koutaku.handleMCPToolExecution(ctx, request, requestType)
	if koutakuErr != nil {
		if koutakuErr.ExtraFields.MCPAuthRequired != nil {
			return nil, koutakuErr.ExtraFields.MCPAuthRequired
		}
		return nil, fmt.Errorf("%s", GetErrorMessage(koutakuErr))
	}
	return resp, nil
}

// PLUGIN MANAGEMENT

// RunLLMPreHooks executes PreHooks in order, tracks how many ran, and returns the final request, any short-circuit decision, and the count.
func (p *PluginPipeline) RunLLMPreHooks(ctx *schemas.KoutakuContext, req *schemas.KoutakuRequest) (*schemas.KoutakuRequest, *schemas.LLMPluginShortCircuit, int) {
	// If the skip plugin pipeline flag is set, skip the plugin pipeline
	if skipPluginPipeline, ok := ctx.Value(schemas.KoutakuContextKeySkipPluginPipeline).(bool); ok && skipPluginPipeline {
		return req, nil, 0
	}
	var shortCircuit *schemas.LLMPluginShortCircuit
	var err error
	ctx.BlockRestrictedWrites()
	defer ctx.UnblockRestrictedWrites()
	for i, plugin := range p.llmPlugins {
		pluginName := plugin.GetName()
		p.logger.Debug("running pre-hook for plugin %s", pluginName)
		// Start span for this plugin's PreLLMHook
		spanCtx, handle := p.tracer.StartSpan(ctx, fmt.Sprintf("plugin.%s.prehook", sanitizeSpanName(pluginName)), schemas.SpanKindPlugin)
		// Update pluginCtx with span context for nested operations
		if spanCtx != nil {
			if spanID, ok := spanCtx.Value(schemas.KoutakuContextKeySpanID).(string); ok {
				ctx.SetValue(schemas.KoutakuContextKeySpanID, spanID)
			}
		}

		pluginCtx := ctx.WithPluginScope(&pluginName)
		req, shortCircuit, err = plugin.PreLLMHook(pluginCtx, req)
		pluginCtx.ReleasePluginScope()

		// End span with appropriate status
		if err != nil {
			p.tracer.SetAttribute(handle, "error", err.Error())
			p.tracer.EndSpan(handle, schemas.SpanStatusError, err.Error())
			p.preHookErrors = append(p.preHookErrors, err)
			p.logger.Warn("error in PreLLMHook for plugin %s: %s", pluginName, err.Error())
		} else if shortCircuit != nil {
			p.tracer.SetAttribute(handle, "short_circuit", true)
			p.tracer.EndSpan(handle, schemas.SpanStatusOk, "short-circuit")
		} else {
			p.tracer.EndSpan(handle, schemas.SpanStatusOk, "")
		}

		p.executedPreHooks = i + 1
		if shortCircuit != nil {
			return req, shortCircuit, p.executedPreHooks // short-circuit: only plugins up to and including i ran
		}
	}
	return req, nil, p.executedPreHooks
}

// RunPostLLMHooks executes PostHooks in reverse order for the plugins whose PreLLMHook ran.
// Accepts the response and error, and allows plugins to transform either (e.g., recover from error, or invalidate a response).
// Returns the final response and error after all hooks. If both are set, error takes precedence unless error is nil.
// runFrom is the count of plugins whose PreHooks ran; PostHooks will run in reverse from index (runFrom - 1) down to 0
// For streaming requests, it accumulates timing per plugin instead of creating individual spans per chunk.
func (p *PluginPipeline) RunPostLLMHooks(ctx *schemas.KoutakuContext, resp *schemas.KoutakuResponse, koutakuErr *schemas.KoutakuError, runFrom int) (*schemas.KoutakuResponse, *schemas.KoutakuError) {
	// If the skip plugin pipeline flag is set, skip the plugin pipeline
	if skipPluginPipeline, ok := ctx.Value(schemas.KoutakuContextKeySkipPluginPipeline).(bool); ok && skipPluginPipeline {
		return resp, koutakuErr
	}
	// Defensive: ensure count is within valid bounds
	if runFrom < 0 {
		runFrom = 0
	}
	if runFrom > len(p.llmPlugins) {
		runFrom = len(p.llmPlugins)
	}
	requestType, _, _, _ := GetResponseFields(resp, koutakuErr)
	// Realtime turns carry StreamStartTime for plugin latency/final-chunk context,
	// but they are finalized as one completed turn, not chunk-by-chunk stream output.
	isStreaming := ctx.Value(schemas.KoutakuContextKeyStreamStartTime) != nil && requestType != schemas.RealtimeRequest
	ctx.BlockRestrictedWrites()
	defer ctx.UnblockRestrictedWrites()
	var err error
	for i := runFrom - 1; i >= 0; i-- {
		plugin := p.llmPlugins[i]
		pluginName := plugin.GetName()
		p.logger.Debug("running post-hook for plugin %s", pluginName)
		if isStreaming {
			// For streaming: accumulate timing, don't create individual spans per chunk
			// Lazily create cached scoped contexts on first chunk (reused across all chunks)
			if p.streamScopedCtxs == nil {
				p.streamScopedCtxs = make(map[string]*schemas.KoutakuContext, len(p.llmPlugins))
				for _, pl := range p.llmPlugins {
					name := pl.GetName()
					p.streamScopedCtxs[name] = ctx.WithPluginScope(&name)
				}
			}
			pluginCtx := p.streamScopedCtxs[pluginName]
			start := time.Now()
			resp, koutakuErr, err = plugin.PostLLMHook(pluginCtx, resp, koutakuErr)
			duration := time.Since(start)

			p.accumulatePluginTiming(pluginName, duration, err != nil)
			if err != nil {
				p.postHookErrors = append(p.postHookErrors, err)
				p.logger.Warn("error in PostLLMHook for plugin %s: %v", pluginName, err)
			}
		} else {
			// For non-streaming: create span per plugin (existing behavior)
			spanCtx, handle := p.tracer.StartSpan(ctx, fmt.Sprintf("plugin.%s.posthook", sanitizeSpanName(pluginName)), schemas.SpanKindPlugin)
			// Update pluginCtx with span context for nested operations
			if spanCtx != nil {
				if spanID, ok := spanCtx.Value(schemas.KoutakuContextKeySpanID).(string); ok {
					ctx.SetValue(schemas.KoutakuContextKeySpanID, spanID)
				}
			}
			pluginCtx := ctx.WithPluginScope(&pluginName)
			resp, koutakuErr, err = plugin.PostLLMHook(pluginCtx, resp, koutakuErr)
			pluginCtx.ReleasePluginScope()
			// End span with appropriate status
			if err != nil {
				p.tracer.SetAttribute(handle, "error", err.Error())
				p.tracer.EndSpan(handle, schemas.SpanStatusError, err.Error())
				p.postHookErrors = append(p.postHookErrors, err)
				p.logger.Warn("error in PostLLMHook for plugin %s: %v", pluginName, err)
			} else {
				p.tracer.EndSpan(handle, schemas.SpanStatusOk, "")
			}
		}
		// If a plugin recovers from an error (sets koutakuErr to nil and sets resp), allow that
		// If a plugin invalidates a response (sets resp to nil and sets koutakuErr), allow that
	}
	// Increment chunk count for streaming
	if isStreaming {
		p.streamingMu.Lock()
		p.chunkCount++
		p.streamingMu.Unlock()
	}
	// Final logic: if both are set, error takes precedence, unless error is nil
	if koutakuErr != nil {
		if resp != nil && koutakuErr.StatusCode == nil && koutakuErr.Error != nil && koutakuErr.Error.Type == nil &&
			koutakuErr.Error.Message == "" && koutakuErr.Error.Error == nil {
			// Defensive: treat as recovery if error is empty
			return resp, nil
		}
		return resp, koutakuErr
	}
	return resp, nil
}

// RunMCPPreHooks executes MCP PreHooks in order for all registered MCP plugins.
// Returns the modified request, any short-circuit decision, and the count of hooks that ran.
// If a plugin short-circuits, only PostHooks for plugins up to and including that plugin will run.
func (p *PluginPipeline) RunMCPPreHooks(ctx *schemas.KoutakuContext, req *schemas.KoutakuMCPRequest) (*schemas.KoutakuMCPRequest, *schemas.MCPPluginShortCircuit, int) {
	// If the skip plugin pipeline flag is set, skip the plugin pipeline
	if skipPluginPipeline, ok := ctx.Value(schemas.KoutakuContextKeySkipPluginPipeline).(bool); ok && skipPluginPipeline {
		return req, nil, 0
	}
	var shortCircuit *schemas.MCPPluginShortCircuit
	var err error
	ctx.BlockRestrictedWrites()
	defer ctx.UnblockRestrictedWrites()
	for i, plugin := range p.mcpPlugins {
		pluginName := plugin.GetName()
		p.logger.Debug("running MCP pre-hook for plugin %s", pluginName)
		// Start span for this plugin's PreMCPHook
		spanCtx, handle := p.tracer.StartSpan(ctx, fmt.Sprintf("plugin.%s.mcp_prehook", sanitizeSpanName(pluginName)), schemas.SpanKindPlugin)
		// Update pluginCtx with span context for nested operations
		if spanCtx != nil {
			if spanID, ok := spanCtx.Value(schemas.KoutakuContextKeySpanID).(string); ok {
				ctx.SetValue(schemas.KoutakuContextKeySpanID, spanID)
			}
		}

		pluginCtx := ctx.WithPluginScope(&pluginName)
		req, shortCircuit, err = plugin.PreMCPHook(pluginCtx, req)
		pluginCtx.ReleasePluginScope()

		// End span with appropriate status
		if err != nil {
			p.tracer.SetAttribute(handle, "error", err.Error())
			p.tracer.EndSpan(handle, schemas.SpanStatusError, err.Error())
			p.preHookErrors = append(p.preHookErrors, err)
			p.logger.Warn("error in PreMCPHook for plugin %s: %s", pluginName, err.Error())
		} else if shortCircuit != nil {
			p.tracer.SetAttribute(handle, "short_circuit", true)
			p.tracer.EndSpan(handle, schemas.SpanStatusOk, "short-circuit")
		} else {
			p.tracer.EndSpan(handle, schemas.SpanStatusOk, "")
		}

		p.executedPreHooks = i + 1
		if shortCircuit != nil {
			return req, shortCircuit, p.executedPreHooks // short-circuit: only plugins up to and including i ran
		}
	}
	return req, nil, p.executedPreHooks
}

// RunMCPPostHooks executes MCP PostHooks in reverse order for the plugins whose PreMCPHook ran.
// Accepts the MCP response and error, and allows plugins to transform either (e.g., recover from error, or invalidate a response).
// Returns the final MCP response and error after all hooks. If both are set, error takes precedence unless error is nil.
// runFrom is the count of plugins whose PreHooks ran; PostHooks will run in reverse from index (runFrom - 1) down to 0
func (p *PluginPipeline) RunMCPPostHooks(ctx *schemas.KoutakuContext, mcpResp *schemas.KoutakuMCPResponse, koutakuErr *schemas.KoutakuError, runFrom int) (*schemas.KoutakuMCPResponse, *schemas.KoutakuError) {
	// If the skip plugin pipeline flag is set, skip the plugin pipeline
	if skipPluginPipeline, ok := ctx.Value(schemas.KoutakuContextKeySkipPluginPipeline).(bool); ok && skipPluginPipeline {
		return mcpResp, koutakuErr
	}
	// Defensive: ensure count is within valid bounds
	if runFrom < 0 {
		runFrom = 0
	}
	if runFrom > len(p.mcpPlugins) {
		runFrom = len(p.mcpPlugins)
	}
	ctx.BlockRestrictedWrites()
	defer ctx.UnblockRestrictedWrites()
	var err error
	for i := runFrom - 1; i >= 0; i-- {
		plugin := p.mcpPlugins[i]
		pluginName := plugin.GetName()
		p.logger.Debug("running MCP post-hook for plugin %s", pluginName)
		// Create span per plugin
		spanCtx, handle := p.tracer.StartSpan(ctx, fmt.Sprintf("plugin.%s.mcp_posthook", sanitizeSpanName(pluginName)), schemas.SpanKindPlugin)
		// Update pluginCtx with span context for nested operations
		if spanCtx != nil {
			if spanID, ok := spanCtx.Value(schemas.KoutakuContextKeySpanID).(string); ok {
				ctx.SetValue(schemas.KoutakuContextKeySpanID, spanID)
			}
		}

		pluginCtx := ctx.WithPluginScope(&pluginName)
		mcpResp, koutakuErr, err = plugin.PostMCPHook(pluginCtx, mcpResp, koutakuErr)
		pluginCtx.ReleasePluginScope()

		// End span with appropriate status
		if err != nil {
			p.tracer.SetAttribute(handle, "error", err.Error())
			p.tracer.EndSpan(handle, schemas.SpanStatusError, err.Error())
			p.postHookErrors = append(p.postHookErrors, err)
			p.logger.Warn("error in PostMCPHook for plugin %s: %v", pluginName, err)
		} else {
			p.tracer.EndSpan(handle, schemas.SpanStatusOk, "")
		}
		// If a plugin recovers from an error (sets koutakuErr to nil and sets mcpResp), allow that
		// If a plugin invalidates a response (sets mcpResp to nil and sets koutakuErr), allow that
	}
	// Final logic: if both are set, error takes precedence, unless error is nil
	if koutakuErr != nil {
		if mcpResp != nil && koutakuErr.StatusCode == nil && koutakuErr.Error != nil && koutakuErr.Error.Type == nil &&
			koutakuErr.Error.Message == "" && koutakuErr.Error.Error == nil {
			// Defensive: treat as recovery if error is empty
			return mcpResp, nil
		}
		return mcpResp, koutakuErr
	}
	return mcpResp, nil
}

// resetPluginPipeline resets a PluginPipeline instance for reuse.
// IMPORTANT: drainAndAttachPluginLogs must be called on the root KoutakuContext
// BEFORE this method, because it calls ReleasePluginScope on cached scoped contexts
// which nils out their pluginLogs pointer. The drain reads from the shared store
// on the root context, so it must happen while the store is still referenced.
func (p *PluginPipeline) resetPluginPipeline() {
	// Drop cross-request references while the object sits in the pool.
	// getPluginPipeline rebinds all four on acquisition, so nil'ing here
	// only affects GC hygiene — important when plugins are hot-swapped.
	p.llmPlugins = nil
	p.mcpPlugins = nil
	p.executedPreHooks = 0
	clear(p.preHookErrors)
	p.preHookErrors = p.preHookErrors[:0]
	clear(p.postHookErrors)
	p.postHookErrors = p.postHookErrors[:0]
	// Reset streaming timing accumulation under lock — the provider goroutine's
	// deferred finalizer may still be iterating these fields when the pipeline
	// is returned to the pool. logger/tracer are nilled here too so the write
	// is synchronized with the finalizer's read under the same mutex.
	p.streamingMu.Lock()
	p.logger = nil
	p.tracer = nil
	p.chunkCount = 0
	if p.postHookTimings != nil {
		// clear() drops *pluginTimingAccumulator values (freeing them for GC)
		// while retaining the map's backing hash table for reuse.
		clear(p.postHookTimings)
	}
	// clear() zeros elements in [0, len) — scrub before [:0] so the backing
	// array doesn't retain live string references once the slice is truncated.
	clear(p.postHookPluginOrder)
	p.postHookPluginOrder = p.postHookPluginOrder[:0]
	// Release cached scoped contexts for streaming
	for _, scopedCtx := range p.streamScopedCtxs {
		scopedCtx.ReleasePluginScope()
	}
	p.streamScopedCtxs = nil
	p.streamingMu.Unlock()
}

// drainAndAttachPluginLogs drains accumulated plugin logs from the KoutakuContext
// and attaches them to the trace for later retrieval by observability plugins.
func drainAndAttachPluginLogs(ctx *schemas.KoutakuContext) {
	tracer, traceID, err := GetTracerFromContext(ctx)
	if err != nil || tracer == nil || traceID == "" {
		return
	}
	logs := ctx.DrainPluginLogs()
	if len(logs) == 0 {
		return
	}
	tracer.AttachPluginLogs(traceID, logs)
}

// accumulatePluginTiming accumulates timing for a plugin during streaming
func (p *PluginPipeline) accumulatePluginTiming(pluginName string, duration time.Duration, hasError bool) {
	p.streamingMu.Lock()
	defer p.streamingMu.Unlock()
	if p.postHookTimings == nil {
		p.postHookTimings = make(map[string]*pluginTimingAccumulator)
	}
	timing, ok := p.postHookTimings[pluginName]
	if !ok {
		timing = &pluginTimingAccumulator{}
		p.postHookTimings[pluginName] = timing
		// Track order on first occurrence (first chunk)
		p.postHookPluginOrder = append(p.postHookPluginOrder, pluginName)
	}
	timing.totalDuration += duration
	timing.invocations++
	if hasError {
		timing.errors++
	}
}

// FinalizeStreamingPostHookSpans creates aggregated spans for each plugin after streaming completes.
// This should be called once at the end of streaming to create one span per plugin with average timing.
// Spans are nested to mirror the pre-hook hierarchy (each post-hook is a child of the previous one).
func (p *PluginPipeline) FinalizeStreamingPostHookSpans(ctx context.Context) {
	// Snapshot the accumulators under lock so per-chunk writers in the
	// provider goroutine can't race with the finalizer. Tracer calls below
	// run unlocked — we don't want to stall chunk writers on span I/O.
	type snapshotEntry struct {
		pluginName    string
		totalDuration time.Duration
		invocations   int
		errors        int
	}
	p.streamingMu.Lock()
	// Capture tracer under the same lock that guards resetPluginPipeline's
	// writes so the read/write pair on p.tracer is synchronized and the
	// unlocked tracer calls below use a stable local.
	tracer := p.tracer
	if tracer == nil || p.postHookTimings == nil || len(p.postHookPluginOrder) == 0 {
		p.streamingMu.Unlock()
		return
	}
	snapshot := make([]snapshotEntry, 0, len(p.postHookPluginOrder))
	for _, pluginName := range p.postHookPluginOrder {
		timing, ok := p.postHookTimings[pluginName]
		if !ok || timing.invocations == 0 {
			continue
		}
		snapshot = append(snapshot, snapshotEntry{
			pluginName:    pluginName,
			totalDuration: timing.totalDuration,
			invocations:   timing.invocations,
			errors:        timing.errors,
		})
	}
	p.streamingMu.Unlock()

	if len(snapshot) == 0 {
		return
	}

	// Collect handles and timing info to end spans in reverse order
	type spanInfo struct {
		handle    schemas.SpanHandle
		hasErrors bool
	}
	spans := make([]spanInfo, 0, len(snapshot))
	currentCtx := ctx

	// Start spans in execution order (nested: each is a child of the previous)
	for _, entry := range snapshot {
		// Create span as child of the previous span (nested hierarchy)
		newCtx, handle := tracer.StartSpan(currentCtx, fmt.Sprintf("plugin.%s.posthook", sanitizeSpanName(entry.pluginName)), schemas.SpanKindPlugin)
		if handle == nil {
			continue
		}

		// Calculate average duration in milliseconds
		avgMs := float64(entry.totalDuration.Milliseconds()) / float64(entry.invocations)

		// Set aggregated attributes
		tracer.SetAttribute(handle, schemas.AttrPluginInvocations, entry.invocations)
		tracer.SetAttribute(handle, schemas.AttrPluginAvgDurationMs, avgMs)
		tracer.SetAttribute(handle, schemas.AttrPluginTotalDurationMs, entry.totalDuration.Milliseconds())

		if entry.errors > 0 {
			tracer.SetAttribute(handle, schemas.AttrPluginErrorCount, entry.errors)
		}

		spans = append(spans, spanInfo{handle: handle, hasErrors: entry.errors > 0})
		currentCtx = newCtx
	}

	// End spans in reverse order (innermost first, like unwinding a call stack)
	for i := len(spans) - 1; i >= 0; i-- {
		if spans[i].hasErrors {
			tracer.EndSpan(spans[i].handle, schemas.SpanStatusError, "some invocations failed")
		} else {
			tracer.EndSpan(spans[i].handle, schemas.SpanStatusOk, "")
		}
	}
}

// GetChunkCount returns the number of chunks processed during streaming
func (p *PluginPipeline) GetChunkCount() int {
	p.streamingMu.Lock()
	defer p.streamingMu.Unlock()
	return p.chunkCount
}

// getPluginPipeline gets a PluginPipeline from the pool and configures it
func (koutaku *Koutaku) getPluginPipeline() *PluginPipeline {
	pipeline := koutaku.pluginPipelinePool.Get().(*PluginPipeline)
	pipeline.llmPlugins = *koutaku.llmPlugins.Load()
	pipeline.mcpPlugins = *koutaku.mcpPlugins.Load()
	pipeline.logger = koutaku.logger
	pipeline.tracer = koutaku.getTracer()
	return pipeline
}

// releasePluginPipeline returns a PluginPipeline to the pool.
// Caller must ensure drainAndAttachPluginLogs has already been called on the
// associated KoutakuContext before calling this method.
func (koutaku *Koutaku) releasePluginPipeline(pipeline *PluginPipeline) {
	pipeline.resetPluginPipeline()
	koutaku.pluginPipelinePool.Put(pipeline)
}

// POOL & RESOURCE MANAGEMENT

// getChannelMessage gets a ChannelMessage from the pool and configures it with the request.
// It also gets response and error channels from their respective pools.
func (koutaku *Koutaku) getChannelMessage(req schemas.KoutakuRequest) *ChannelMessage {
	// Get channels from pool
	responseChan := koutaku.responseChannelPool.Get().(chan *schemas.KoutakuResponse)
	errorChan := koutaku.errorChannelPool.Get().(chan schemas.KoutakuError)

	// Clear any previous values to avoid leaking between requests
	select {
	case <-responseChan:
	default:
	}
	select {
	case <-errorChan:
	default:
	}

	// Get message from pool and configure it
	msg := koutaku.channelMessagePool.Get().(*ChannelMessage)
	msg.KoutakuRequest = req
	msg.Response = responseChan
	msg.Err = errorChan

	// Conditionally allocate ResponseStream for streaming requests only
	if IsStreamRequestType(req.RequestType) {
		responseStreamChan := koutaku.responseStreamPool.Get().(chan chan *schemas.KoutakuStreamChunk)
		// Clear any previous values to avoid leaking between requests
		select {
		case <-responseStreamChan:
		default:
		}
		msg.ResponseStream = responseStreamChan
	}

	return msg
}

// drainQueueWithErrors drains all buffered messages from pq and sends each a
// "provider is shutting down" error. It must be called after all workers for
// the queue have exited (i.e. after wg.Wait()) to cover the TOCTOU window:
// a producer that passed isClosing() just before signalClosing fired can still
// win the `case pq.queue <- msg` branch in tryRequest, landing a message in
// the queue after the last worker's drain loop already exited via `default:`.
// Without this sweep, those callers block forever on <-msg.Response / <-msg.Err.
//
// Residual TOCTOU window (known limitation): this sweep runs exactly once via
// a non-blocking `select { default: }`. A producer that deposits a message
// after the sweep's `default:` branch exits has no worker and no sweep to drain
// it — the caller will block until its own context is cancelled. Fully closing
// this window requires a sender-side reference count (so the last producer can
// signal "queue is fully idle"), which is intentionally not implemented because
// it would add per-send atomic overhead on the hot path.
func (koutaku *Koutaku) drainQueueWithErrors(pq *ProviderQueue) {
	for {
		select {
		case r := <-pq.queue:
			provKey, mod, _ := r.GetRequestFields()
			select {
			case r.Err <- schemas.KoutakuError{
				IsKoutakuError: false,
				Error:          &schemas.ErrorField{Message: "provider is shutting down"},
				ExtraFields: schemas.KoutakuErrorExtraFields{
					RequestType:            r.RequestType,
					Provider:               provKey,
					OriginalModelRequested: mod,
				},
			}:
			case <-r.Context.Done():
				// No time.After needed: r.Err is a buffered channel of size 1 freshly
				// allocated per request, so the send always completes immediately unless
				// the caller already cancelled. ctx.Done() is the only valid escape.
			}
		default:
			return
		}
	}
}

// releaseChannelMessage returns a ChannelMessage and its channels to their respective pools.
func (koutaku *Koutaku) releaseChannelMessage(msg *ChannelMessage) {
	// Put channels back in pools
	koutaku.responseChannelPool.Put(msg.Response)
	koutaku.errorChannelPool.Put(msg.Err)

	// Return ResponseStream to pool if it was used
	if msg.ResponseStream != nil {
		// Drain any remaining channels to prevent memory leaks
		select {
		case <-msg.ResponseStream:
		default:
		}
		koutaku.responseStreamPool.Put(msg.ResponseStream)
	}

	// Release of Koutaku Request is handled in handle methods as they are required for fallbacks

	// Clear references and return to pool
	msg.Response = nil
	msg.ResponseStream = nil
	msg.Err = nil
	koutaku.channelMessagePool.Put(msg)
}

// resetKoutakuRequest resets a KoutakuRequest instance for reuse
func resetKoutakuRequest(req *schemas.KoutakuRequest) {
	req.RequestType = ""
	req.ListModelsRequest = nil
	req.TextCompletionRequest = nil
	req.ChatRequest = nil
	req.ResponsesRequest = nil
	req.CountTokensRequest = nil
	req.EmbeddingRequest = nil
	req.RerankRequest = nil
	req.OCRRequest = nil
	req.SpeechRequest = nil
	req.TranscriptionRequest = nil
	req.ImageGenerationRequest = nil
	req.ImageEditRequest = nil
	req.ImageVariationRequest = nil
	req.VideoGenerationRequest = nil
	req.VideoRetrieveRequest = nil
	req.VideoDownloadRequest = nil
	req.VideoListRequest = nil
	req.VideoRemixRequest = nil
	req.VideoDeleteRequest = nil
	req.FileUploadRequest = nil
	req.FileListRequest = nil
	req.FileRetrieveRequest = nil
	req.FileDeleteRequest = nil
	req.FileContentRequest = nil
	req.BatchCreateRequest = nil
	req.BatchListRequest = nil
	req.BatchRetrieveRequest = nil
	req.BatchCancelRequest = nil
	req.BatchDeleteRequest = nil
	req.BatchResultsRequest = nil
	req.ContainerCreateRequest = nil
	req.ContainerListRequest = nil
	req.ContainerRetrieveRequest = nil
	req.ContainerDeleteRequest = nil
	req.ContainerFileCreateRequest = nil
	req.ContainerFileListRequest = nil
	req.ContainerFileRetrieveRequest = nil
	req.ContainerFileContentRequest = nil
	req.ContainerFileDeleteRequest = nil
	req.PassthroughRequest = nil
}

// getKoutakuRequest gets a KoutakuRequest from the pool
func (koutaku *Koutaku) getKoutakuRequest() *schemas.KoutakuRequest {
	req := koutaku.koutakuRequestPool.Get().(*schemas.KoutakuRequest)
	return req
}

// releaseKoutakuRequest returns a KoutakuRequest to the pool
func (koutaku *Koutaku) releaseKoutakuRequest(req *schemas.KoutakuRequest) {
	resetKoutakuRequest(req)
	koutaku.koutakuRequestPool.Put(req)
}

// resetMCPRequest resets a KoutakuMCPRequest instance for reuse
func resetMCPRequest(req *schemas.KoutakuMCPRequest) {
	req.RequestType = ""
	req.ChatAssistantMessageToolCall = nil
	req.ResponsesToolMessage = nil
}

// getMCPRequest gets a KoutakuMCPRequest from the pool
func (koutaku *Koutaku) getMCPRequest() *schemas.KoutakuMCPRequest {
	req := koutaku.mcpRequestPool.Get().(*schemas.KoutakuMCPRequest)
	return req
}

// releaseMCPRequest returns a KoutakuMCPRequest to the pool
func (koutaku *Koutaku) releaseMCPRequest(req *schemas.KoutakuMCPRequest) {
	resetMCPRequest(req)
	koutaku.mcpRequestPool.Put(req)
}

// getAllSupportedKeys retrieves all valid keys for a ListModels request.
// allowing the provider to aggregate results from multiple keys.
func (koutaku *Koutaku) getAllSupportedKeys(ctx *schemas.KoutakuContext, providerKey schemas.ModelProvider, baseProviderType schemas.ModelProvider) ([]schemas.Key, error) {
	// Check if key has been set in the context explicitly
	if ctx != nil {
		key, ok := ctx.Value(schemas.KoutakuContextKeyDirectKey).(schemas.Key)
		if ok {
			if err := validateKey(baseProviderType, &key); err != nil {
				return nil, fmt.Errorf("invalid direct key for provider %v: %w", baseProviderType, err)
			}
			// If a direct key is specified, return it as a single-element slice
			return []schemas.Key{key}, nil
		}
	}

	keys, err := koutaku.account.GetKeysForProvider(ctx, providerKey)
	if err != nil {
		return nil, err
	}

	if len(keys) == 0 {
		return nil, fmt.Errorf("no keys found for provider: %v", providerKey)
	}

	// Filter keys for ListModels - only check if key has a value
	var supportedKeys []schemas.Key
	for _, key := range keys {
		// Skip disabled keys (default enabled when nil)
		if key.Enabled != nil && !*key.Enabled {
			continue
		}
		if err := validateKey(baseProviderType, &key); err != nil {
			koutaku.logger.Warn("error validating key %s (%s) for provider %s: %s, skipping key", key.Name, key.ID, providerKey, err.Error())
			continue
		}
		if strings.TrimSpace(key.Value.GetValue()) != "" || CanProviderKeyValueBeEmpty(baseProviderType) {
			supportedKeys = append(supportedKeys, key)
		}
	}

	koutaku.logger.Debug("[Koutaku] Provider %s: %d valid keys found", providerKey, len(supportedKeys))

	if len(supportedKeys) == 0 {
		return nil, fmt.Errorf("no valid keys found for provider: %v", providerKey)
	}

	return supportedKeys, nil
}

// getKeysForBatchAndFileOps retrieves keys for batch and file operations with model filtering.
// For batch operations, only keys with UseForBatchAPI enabled are included.
// Model filtering: if model is specified and key has model restrictions, only include if model is in list.
func (koutaku *Koutaku) getKeysForBatchAndFileOps(ctx *schemas.KoutakuContext, providerKey schemas.ModelProvider, baseProviderType schemas.ModelProvider, model *string, isBatchOp bool) ([]schemas.Key, error) {
	// Check if key has been set in the context explicitly
	if ctx != nil {
		key, ok := ctx.Value(schemas.KoutakuContextKeyDirectKey).(schemas.Key)
		if ok {
			if err := validateKey(baseProviderType, &key); err != nil {
				return nil, fmt.Errorf("invalid direct key for provider %v: %w", baseProviderType, err)
			}
			// If a direct key is specified, return it as a single-element slice
			return []schemas.Key{key}, nil
		}
	}

	keys, err := koutaku.account.GetKeysForProvider(ctx, providerKey)
	if err != nil {
		return nil, err
	}

	if len(keys) == 0 {
		return nil, fmt.Errorf("no keys found for provider: %v", providerKey)
	}

	var filteredKeys []schemas.Key
	for _, k := range keys {
		// Skip disabled keys
		if k.Enabled != nil && !*k.Enabled {
			continue
		}

		// For batch operations, only include keys with UseForBatchAPI enabled
		if isBatchOp && (k.UseForBatchAPI == nil || !*k.UseForBatchAPI) {
			continue
		}

		if err := validateKey(baseProviderType, &k); err != nil {
			koutaku.logger.Warn("error validating key %s (%s) for provider %s: %s, skipping key", k.Name, k.ID, providerKey, err.Error())
			continue
		}

		// Model filtering logic:
		// - If model is nil or empty → include all keys (no model filter)
		// - If model is specified:
		//   - If model is in key.BlacklistedModels → exclude (wins over Models allow list)
		//   - If key.Models is ["*"] → include key (supports all non-blacklisted models)
		//   - If key.Models is empty → exclude key (deny-by-default)
		//   - If key.Models is non-empty → only include if model is in list
		// Blacklist wins over allowlist
		if model != nil && *model != "" {
			if k.BlacklistedModels.IsBlocked(*model) || !k.Models.IsAllowed(*model) {
				continue
			}
		}

		// Check key value (or if provider allows empty keys or has Azure Entra ID credentials)
		if strings.TrimSpace(k.Value.GetValue()) != "" || CanProviderKeyValueBeEmpty(baseProviderType) {
			filteredKeys = append(filteredKeys, k)
		}
	}

	if len(filteredKeys) == 0 {
		modelStr := ""
		if model != nil {
			modelStr = *model
		}
		if isBatchOp {
			return nil, fmt.Errorf("no batch-enabled keys found for provider: %v and model: %s", providerKey, modelStr)
		}
		return nil, fmt.Errorf("no keys found for provider: %v and model: %s", providerKey, modelStr)
	}

	// Sort keys by ID for deterministic pagination order across requests
	sort.Slice(filteredKeys, func(i, j int) bool {
		return filteredKeys[i].ID < filteredKeys[j].ID
	})

	return filteredKeys, nil
}

// selectKeyFromProviderForModelWithPool returns the filtered pool of eligible keys for the given
// provider/model, along with a canRotate flag indicating whether key rotation across retries is
// permitted. Key selection (choosing which key to use) is deferred to executeRequestWithRetries
// via the keyProvider closure built by the caller.
//
// canRotate=false is returned for cases where the caller must always use the same key:
//   - DirectKey (caller-supplied key bypasses all selection)
//   - SkipKeySelection (provider allows keyless requests; empty slice returned)
//   - Explicit KoutakuContextKeyAPIKeyID / APIKeyName (user pinned a specific key)
//   - Session stickiness (key persisted in KV store for the session lifetime)
//   - Single-key pool (only one eligible key — rotation is a no-op, KV write skipped)
//
// canRotate=true is returned when there are two or more eligible keys and no pinning
// or stickiness constraint is in effect.
func (koutaku *Koutaku) selectKeyFromProviderForModelWithPool(ctx *schemas.KoutakuContext, requestType schemas.RequestType, providerKey schemas.ModelProvider, model string, baseProviderType schemas.ModelProvider) ([]schemas.Key, bool, error) {
	// DirectKey: caller supplied a key directly — no pool, no rotation.
	if ctx != nil {
		if key, ok := ctx.Value(schemas.KoutakuContextKeyDirectKey).(schemas.Key); ok {
			if err := validateKey(baseProviderType, &key); err != nil {
				return nil, false, fmt.Errorf("invalid direct key for provider %v: %w", baseProviderType, err)
			}
			return []schemas.Key{key}, false, nil
		}
	}
	// SkipKeySelection: provider allows keyless requests — return empty pool, no rotation.
	if skipKeySelection, ok := ctx.Value(schemas.KoutakuContextKeySkipKeySelection).(bool); ok && skipKeySelection && isKeySkippingAllowed(providerKey) {
		return []schemas.Key{}, false, nil
	}

	// Get keys for provider
	keys, err := koutaku.account.GetKeysForProvider(ctx, providerKey)
	if err != nil {
		return nil, false, err
	}
	if len(keys) == 0 {
		return nil, false, fmt.Errorf("no keys found for provider: %v and model: %s", providerKey, model)
	}

	// For batch API operations, filter keys to only include those with UseForBatchAPI enabled
	if isBatchRequestType(requestType) || isFileRequestType(requestType) {
		var batchEnabledKeys []schemas.Key
		for _, k := range keys {
			if k.UseForBatchAPI != nil && *k.UseForBatchAPI {
				batchEnabledKeys = append(batchEnabledKeys, k)
			}
		}
		if len(batchEnabledKeys) == 0 {
			return nil, false, fmt.Errorf("no config found for batch apis; enable 'Use for Batch APIs' on at least one key for provider: %v", providerKey)
		}
		keys = batchEnabledKeys
	}

	// Filter out keys that don't support the model: blacklisted_models wins over models allow list;
	// if the key has no models list, it supports all models except those blacklisted.
	var supportedKeys []schemas.Key

	// Skip model check conditions
	// We can improve these conditions in the future
	skipModelCheck := (model == "" && (isFileRequestType(requestType) || isBatchRequestType(requestType) || isContainerRequestType(requestType) || isModellessVideoRequestType(requestType) || isPassthroughRequestType(requestType))) || requestType == schemas.ListModelsRequest
	if skipModelCheck {
		// When skipping model check: just verify keys are enabled and have values
		for _, key := range keys {
			// Skip disabled keys
			if key.Enabled != nil && !*key.Enabled {
				continue
			}
			if err := validateKey(baseProviderType, &key); err != nil {
				koutaku.logger.Warn("error validating key %s (%s) for provider %s: %s, skipping key", key.Name, key.ID, providerKey, err.Error())
				continue
			}
			if strings.TrimSpace(key.Value.GetValue()) != "" || CanProviderKeyValueBeEmpty(baseProviderType) {
				supportedKeys = append(supportedKeys, key)
			}
		}
	} else {
		// When NOT skipping model check: do full model filtering
		for _, key := range keys {
			// Skip disabled keys
			if key.Enabled != nil && !*key.Enabled {
				continue
			}
			if err := validateKey(baseProviderType, &key); err != nil {
				koutaku.logger.Warn("error validating key %s (%s) for provider %s: %s, skipping key", key.Name, key.ID, providerKey, err.Error())
				continue
			}
			hasValue := strings.TrimSpace(key.Value.GetValue()) != "" || CanProviderKeyValueBeEmpty(baseProviderType)
			// ["*"] = allow all models; [] = deny all; specific list = allow only listed
			// NOTE: Model filtering uses the original requested model (which may be an alias).
			// key.Models and key.BlacklistedModels must therefore be expressed in alias keys.
			// The provider-specific identifier is resolved later in the handler closure via key.Aliases.Resolve(model).
			modelSupported := hasValue && key.Models.IsAllowed(model) && !key.BlacklistedModels.IsBlocked(model)
			if baseProviderType == schemas.VLLM && key.VLLMKeyConfig != nil {
				if key.VLLMKeyConfig.ModelName != "" {
					modelSupported = modelSupported && (key.VLLMKeyConfig.ModelName == model)
				}
			}
			if modelSupported {
				supportedKeys = append(supportedKeys, key)
			}
		}
	}
	if len(supportedKeys) == 0 {
		return nil, false, fmt.Errorf("no keys found that support model: %s", model)
	}

	// Explicit key ID takes priority over key name — pin to that key, no rotation.
	if ctx != nil {
		if keyID, ok := ctx.Value(schemas.KoutakuContextKeyAPIKeyID).(string); ok {
			if keyID = strings.TrimSpace(keyID); keyID != "" {
				for _, key := range supportedKeys {
					if key.ID == keyID {
						return []schemas.Key{key}, false, nil
					}
				}
				return nil, false, fmt.Errorf("no supported key found with id %q for provider: %v and model: %s", keyID, providerKey, model)
			}
		}
		if keyName, ok := ctx.Value(schemas.KoutakuContextKeyAPIKeyName).(string); ok {
			if keyName = strings.TrimSpace(keyName); keyName != "" {
				for _, key := range supportedKeys {
					if key.Name == keyName {
						return []schemas.Key{key}, false, nil
					}
				}
				return nil, false, fmt.Errorf("no supported key found with name %q for provider: %v and model: %s", keyName, providerKey, model)
			}
		}
	}

	// Single key: no rotation possible, skip session stickiness (no KV write needed).
	if len(supportedKeys) == 1 {
		return []schemas.Key{supportedKeys[0]}, false, nil
	}

	// Session stickiness: on the first request for a session ID, the randomly selected key is
	// persisted in the KV store. Subsequent requests reuse it for the session lifetime. The sticky
	// key is intentionally kept fixed across all retry attempts — return it as a single-element
	// pool with canRotate=false so rate-limit retries also stay on the same key.
	sessionID := ""
	if ctx != nil {
		if id, ok := ctx.Value(schemas.KoutakuContextKeySessionID).(string); ok && id != "" {
			sessionID = id
		}
	}
	fallbackIndex := 0
	if ctx != nil {
		fallbackIndex, _ = ctx.Value(schemas.KoutakuContextKeyFallbackIndex).(int)
	}
	stickinessActive := sessionID != "" && koutaku.kvStore != nil && fallbackIndex == 0

	if stickinessActive {
		kvKey := buildSessionKey(providerKey, sessionID, model)
		ttl, _ := ctx.Value(schemas.KoutakuContextKeySessionTTL).(time.Duration)
		if ttl <= 0 {
			ttl = schemas.DefaultSessionStickyTTL
		}

		if cachedKey, found, stale := getCachedKeyFromStore(koutaku.kvStore, kvKey, supportedKeys); found {
			if err := koutaku.kvStore.SetWithTTL(kvKey, cachedKey.ID, ttl); err != nil {
				koutaku.logger.Warn("error setting session cache for provider=%s key_id=%s: %s", providerKey, cachedKey.ID, err.Error())
			}
			return []schemas.Key{cachedKey}, false, nil
		} else if stale {
			if _, err := koutaku.kvStore.Delete(kvKey); err != nil {
				koutaku.logger.Warn("error deleting stale session cache for provider=%s: %s", providerKey, err.Error())
			}
		}

		selectedKey, err := koutaku.keySelector(ctx, supportedKeys, providerKey, model)
		if err != nil {
			return nil, false, err
		}

		wasSet, err := koutaku.kvStore.SetNXWithTTL(kvKey, selectedKey.ID, ttl)
		if err != nil {
			koutaku.logger.Warn("error setting session cache for provider=%s key_id=%s: %s", providerKey, selectedKey.ID, err.Error())
			return []schemas.Key{selectedKey}, false, nil
		}
		if wasSet {
			return []schemas.Key{selectedKey}, false, nil
		}

		// Another concurrent request won the race — re-read the persisted key.
		if currentKey, found, stale := getCachedKeyFromStore(koutaku.kvStore, kvKey, supportedKeys); found {
			return []schemas.Key{currentKey}, false, nil
		} else if stale {
			if _, err := koutaku.kvStore.Delete(kvKey); err != nil {
				koutaku.logger.Warn("error deleting stale session cache for provider=%s: %s", providerKey, err.Error())
			}
			return []schemas.Key{selectedKey}, false, nil
		}

		return []schemas.Key{selectedKey}, false, nil
	}

	// Normal case: return the full filtered pool with rotation enabled.
	return supportedKeys, true, nil
}

// getCachedKeyFromStore retrieves a key ID from the KV store and looks it up in supportedKeys.
// Returns the matching Key, found (true if key exists in supportedKeys), and stale (true if
// KV contains an ID but it is not in supportedKeys—caller should delete before SetNXWithTTL).
func getCachedKeyFromStore(kvStore schemas.KVStore, kvKey string, supportedKeys []schemas.Key) (schemas.Key, bool, bool) {
	raw, err := kvStore.Get(kvKey)
	if err != nil {
		return schemas.Key{}, false, false
	}

	var cachedKeyID string
	switch v := raw.(type) {
	case string:
		cachedKeyID = v
	case []byte:
		var s string
		if err := sonic.Unmarshal(v, &s); err == nil {
			cachedKeyID = s
		} else {
			cachedKeyID = string(v)
		}
	}

	if cachedKeyID != "" {
		for _, k := range supportedKeys {
			if k.ID == cachedKeyID {
				return k, true, false
			}
		}
		return schemas.Key{}, false, true
	}

	return schemas.Key{}, false, false
}

// Shutdown gracefully stops all workers when triggered.
// It closes all request channels and waits for workers to exit.
func (koutaku *Koutaku) Shutdown() {
	koutaku.logger.Info("closing all request channels...")
	// Cancel the context if not already done
	if koutaku.ctx.Err() == nil && koutaku.cancel != nil {
		koutaku.cancel()
	}
	// Signal all provider queues to close. Workers exit via pq.done;
	// we never close pq.queue to avoid "send on closed channel" panics in
	// producers that are concurrently in tryRequest.
	koutaku.requestQueues.Range(func(key, value interface{}) bool {
		pq := value.(*ProviderQueue)
		pq.signalClosing()
		return true
	})

	// Wait for all workers to exit
	koutaku.waitGroups.Range(func(key, value interface{}) bool {
		waitGroup := value.(*sync.WaitGroup)
		waitGroup.Wait()
		return true
	})

	// Final drain sweep — same reasoning as RemoveProvider's Step 3b.
	koutaku.requestQueues.Range(func(key, value interface{}) bool {
		koutaku.drainQueueWithErrors(value.(*ProviderQueue))
		return true
	})

	// Cleanup MCP manager
	if koutaku.MCPManager != nil {
		err := koutaku.MCPManager.Cleanup()
		if err != nil {
			koutaku.logger.Warn("Error cleaning up MCP manager: %s", err.Error())
		}
	}

	// Stop the tracerWrapper to clean up background goroutines
	if tracerWrapper := koutaku.tracer.Load().(*tracerWrapper); tracerWrapper != nil && tracerWrapper.tracer != nil {
		tracerWrapper.tracer.Stop()
	}

	// Cleanup plugins
	if llmPlugins := koutaku.llmPlugins.Load(); llmPlugins != nil {
		for _, plugin := range *llmPlugins {
			err := plugin.Cleanup()
			if err != nil {
				koutaku.logger.Warn(fmt.Sprintf("Error cleaning up LLM plugin: %s", err.Error()))
			}
		}
	}
	if mcpPlugins := koutaku.mcpPlugins.Load(); mcpPlugins != nil {
		for _, plugin := range *mcpPlugins {
			err := plugin.Cleanup()
			if err != nil {
				koutaku.logger.Warn(fmt.Sprintf("Error cleaning up MCP plugin: %s", err.Error()))
			}
		}
	}
	koutaku.logger.Info("all request channels closed")
}
