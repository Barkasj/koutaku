package integrations

import (
	"context"
	"errors"

	koutaku "github.com/koutaku/koutaku/core"
	"github.com/koutaku/koutaku/core/providers/cohere"
	"github.com/koutaku/koutaku/core/schemas"
	"github.com/koutaku/koutaku/transports/koutaku-http/lib"
	"github.com/valyala/fasthttp"
)

// hydrateCohereRequestFromLargePayloadMetadata populates model + stream from
// LargePayloadMetadata when body parsing is skipped under large payload mode.
func hydrateCohereRequestFromLargePayloadMetadata(koutakuCtx *schemas.KoutakuContext, req interface{}) {
	if koutakuCtx == nil {
		return
	}
	isLargePayload, _ := koutakuCtx.Value(schemas.KoutakuContextKeyLargePayloadMode).(bool)
	if !isLargePayload {
		return
	}
	metadata := resolveLargePayloadMetadata(koutakuCtx)
	if metadata == nil {
		return
	}

	switch r := req.(type) {
	case *cohere.CohereChatRequest:
		if r.Model == "" {
			r.Model = metadata.Model
		}
		if metadata.StreamRequested != nil && r.Stream == nil {
			r.Stream = schemas.Ptr(*metadata.StreamRequested)
		}
	case *cohere.CohereEmbeddingRequest:
		if r.Model == "" {
			r.Model = metadata.Model
		}
	case *cohere.CohereRerankRequest:
		if r.Model == "" {
			r.Model = metadata.Model
		}
	case *cohere.CohereCountTokensRequest:
		if r.Model == "" {
			r.Model = metadata.Model
		}
	}
}

// cohereLargePayloadPreHook populates model + stream from LargePayloadMetadata
// when body parsing is skipped under large payload mode.
func cohereLargePayloadPreHook(_ *fasthttp.RequestCtx, koutakuCtx *schemas.KoutakuContext, req interface{}) error {
	hydrateCohereRequestFromLargePayloadMetadata(koutakuCtx, req)
	return nil
}

// CohereRouter holds route registrations for Cohere endpoints.
// It supports Cohere's v2 chat, embeddings, and rerank APIs.
type CohereRouter struct {
	*GenericRouter
}

// NewCohereRouter creates a new CohereRouter with the given koutaku client.
func NewCohereRouter(client *koutaku.Koutaku, handlerStore lib.HandlerStore, logger schemas.Logger) *CohereRouter {
	return &CohereRouter{
		GenericRouter: NewGenericRouter(client, handlerStore, CreateCohereRouteConfigs("/cohere"), nil, logger),
	}
}

// CreateCohereRouteConfigs creates route configurations for Cohere API endpoints.
func CreateCohereRouteConfigs(pathPrefix string) []RouteConfig {
	var routes []RouteConfig

	// Chat completions endpoint (v2/chat)
	routes = append(routes, RouteConfig{
		Type:        RouteConfigTypeCohere,
		Path:        pathPrefix + "/v2/chat",
		Method:      "POST",
		PreCallback: cohereLargePayloadPreHook,
		GetHTTPRequestType: func(ctx *fasthttp.RequestCtx) schemas.RequestType {
			return schemas.ChatCompletionRequest
		},
		GetRequestTypeInstance: func(ctx context.Context) interface{} {
			return &cohere.CohereChatRequest{}
		},
		RequestConverter: func(ctx *schemas.KoutakuContext, req interface{}) (*schemas.KoutakuRequest, error) {
			if cohereReq, ok := req.(*cohere.CohereChatRequest); ok {
				return &schemas.KoutakuRequest{
					ChatRequest: cohereReq.ToKoutakuChatRequest(ctx),
				}, nil
			}
			return nil, errors.New("invalid request type")
		},
		ChatResponseConverter: func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuChatResponse) (interface{}, error) {
			if resp.ExtraFields.Provider == schemas.Cohere {
				if resp.ExtraFields.RawResponse != nil {
					return resp.ExtraFields.RawResponse, nil
				}
			}
			return resp, nil
		},
		ErrorConverter: func(ctx *schemas.KoutakuContext, err *schemas.KoutakuError) interface{} {
			return err
		},
		StreamConfig: &StreamConfig{
			ChatStreamResponseConverter: func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuChatResponse) (string, interface{}, error) {
				if resp.ExtraFields.Provider == schemas.Cohere {
					if resp.ExtraFields.RawResponse != nil {
						return "", resp.ExtraFields.RawResponse, nil
					}
				}
				return "", resp, nil
			},
			ErrorConverter: func(ctx *schemas.KoutakuContext, err *schemas.KoutakuError) interface{} {
				return err
			},
		},
	})

	// Embeddings endpoint (v2/embed)
	routes = append(routes, RouteConfig{
		Type:        RouteConfigTypeCohere,
		Path:        pathPrefix + "/v2/embed",
		Method:      "POST",
		PreCallback: cohereLargePayloadPreHook,
		GetHTTPRequestType: func(ctx *fasthttp.RequestCtx) schemas.RequestType {
			return schemas.EmbeddingRequest
		},
		GetRequestTypeInstance: func(ctx context.Context) interface{} {
			return &cohere.CohereEmbeddingRequest{}
		},
		RequestConverter: func(ctx *schemas.KoutakuContext, req interface{}) (*schemas.KoutakuRequest, error) {
			if cohereReq, ok := req.(*cohere.CohereEmbeddingRequest); ok {
				return &schemas.KoutakuRequest{
					EmbeddingRequest: cohereReq.ToKoutakuEmbeddingRequest(ctx),
				}, nil
			}
			return nil, errors.New("invalid embedding request type")
		},
		EmbeddingResponseConverter: func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuEmbeddingResponse) (interface{}, error) {
			if resp.ExtraFields.Provider == schemas.Cohere {
				if resp.ExtraFields.RawResponse != nil {
					return resp.ExtraFields.RawResponse, nil
				}
			}
			return resp, nil
		},
		ErrorConverter: func(ctx *schemas.KoutakuContext, err *schemas.KoutakuError) interface{} {
			return err
		},
	})

	// Rerank endpoint (v2/rerank)
	routes = append(routes, RouteConfig{
		Type:        RouteConfigTypeCohere,
		Path:        pathPrefix + "/v2/rerank",
		Method:      "POST",
		PreCallback: cohereLargePayloadPreHook,
		GetHTTPRequestType: func(ctx *fasthttp.RequestCtx) schemas.RequestType {
			return schemas.RerankRequest
		},
		GetRequestTypeInstance: func(ctx context.Context) interface{} {
			return &cohere.CohereRerankRequest{}
		},
		RequestConverter: func(ctx *schemas.KoutakuContext, req interface{}) (*schemas.KoutakuRequest, error) {
			if cohereReq, ok := req.(*cohere.CohereRerankRequest); ok {
				return &schemas.KoutakuRequest{
					RerankRequest: cohereReq.ToKoutakuRerankRequest(ctx),
				}, nil
			}
			return nil, errors.New("invalid rerank request type")
		},
		RerankResponseConverter: func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuRerankResponse) (interface{}, error) {
			if resp.ExtraFields.Provider == schemas.Cohere {
				if resp.ExtraFields.RawResponse != nil {
					return resp.ExtraFields.RawResponse, nil
				}
			}
			return resp, nil
		},
		ErrorConverter: func(ctx *schemas.KoutakuContext, err *schemas.KoutakuError) interface{} {
			return err
		},
	})

	// Tokenize endpoint (v1/tokenize)
	routes = append(routes, RouteConfig{
		Type:        RouteConfigTypeCohere,
		Path:        pathPrefix + "/v1/tokenize",
		Method:      "POST",
		PreCallback: cohereLargePayloadPreHook,
		GetHTTPRequestType: func(ctx *fasthttp.RequestCtx) schemas.RequestType {
			return schemas.CountTokensRequest
		},
		GetRequestTypeInstance: func(ctx context.Context) interface{} {
			return &cohere.CohereCountTokensRequest{}
		},
		RequestConverter: func(ctx *schemas.KoutakuContext, req interface{}) (*schemas.KoutakuRequest, error) {
			if cohereReq, ok := req.(*cohere.CohereCountTokensRequest); ok {
				return &schemas.KoutakuRequest{
					CountTokensRequest: cohereReq.ToKoutakuResponsesRequest(ctx),
				}, nil
			}
			return nil, errors.New("invalid count tokens request type")
		},
		CountTokensResponseConverter: func(ctx *schemas.KoutakuContext, resp *schemas.KoutakuCountTokensResponse) (interface{}, error) {
			if resp.ExtraFields.Provider == schemas.Cohere {
				if resp.ExtraFields.RawResponse != nil {
					return resp.ExtraFields.RawResponse, nil
				}
			}
			return resp, nil
		},
		ErrorConverter: func(ctx *schemas.KoutakuContext, err *schemas.KoutakuError) interface{} {
			return err
		},
	})

	return routes
}
