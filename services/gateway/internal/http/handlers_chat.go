package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
	"github.com/parthkumarsingh/llmrouter/services/gateway/pkg/openaiapi"
)

// maxRequestBytes caps a request body. Prompts are large but not unbounded, and without a cap a
// single client can exhaust the gateway's memory before any other limit applies.
const maxRequestBytes = 8 << 20 // 8 MiB

// Headers the gateway adds. They are additive, so an SDK that ignores them is unaffected.
const (
	// HeaderPolicy lets a caller override routing per request.
	HeaderPolicy = "X-LLMRouter-Policy"
	// HeaderProvider reports which vendor actually served the response.
	HeaderProvider = "X-LLMRouter-Provider"
	// HeaderModel reports the model the router resolved to.
	HeaderModel = "X-LLMRouter-Model"
	// HeaderCost reports the attributed cost of this request in USD.
	HeaderCost = "X-LLMRouter-Cost-USD"
	// HeaderBudgetWarning is set once a tenant crosses its warn threshold.
	HeaderBudgetWarning = "X-LLMRouter-Budget-Warning"
)

// Completer is the chat use case this handler drives.
type Completer interface {
	Complete(ctx context.Context, req domain.ChatRequest, tenant domain.Tenant) (app.Result, error)
}

// Embedder serves /v1/embeddings.
type Embedder interface {
	Embed(ctx context.Context, req domain.EmbedRequest, tenant domain.Tenant) (domain.EmbedResponse, error)
}

// ChatHandler adapts HTTP to the chat use case.
type ChatHandler struct {
	svc      Completer
	embedder Embedder
}

// NewChatHandler builds the handler. embedder may be nil, in which case /v1/embeddings reports
// that the capability is not configured rather than failing obscurely.
func NewChatHandler(svc Completer, embedder Embedder) *ChatHandler {
	return &ChatHandler{svc: svc, embedder: embedder}
}

// Completions serves POST /v1/chat/completions.
func (h *ChatHandler) Completions(w http.ResponseWriter, r *http.Request) {
	tenant, ok := TenantFrom(r.Context())
	if !ok {
		WriteError(w, r, http.StatusUnauthorized, openaiapi.NewError(
			"missing tenant context", openaiapi.ErrTypeAuthentication, "invalid_api_key", ""))
		return
	}

	var wire openaiapi.ChatCompletionRequest
	raw, err := decodeBody(r, &wire)
	if err != nil {
		WriteAppError(w, r, err)
		return
	}

	req, err := toDomainRequest(r, wire, raw, tenant)
	if err != nil {
		WriteAppError(w, r, err)
		return
	}

	if req.Stream {
		// Streaming, including mid-stream failover, is served by the streaming handler.
		h.completionsStream(w, r, req, tenant)
		return
	}

	result, err := h.svc.Complete(r.Context(), req, tenant)
	if err != nil {
		WriteAppError(w, r, err)
		return
	}

	setRoutingHeaders(w, result)
	WriteJSON(w, r, http.StatusOK, toWireResponse(req, result))
}

// Embeddings serves POST /v1/embeddings.
func (h *ChatHandler) Embeddings(w http.ResponseWriter, r *http.Request) {
	tenant, ok := TenantFrom(r.Context())
	if !ok {
		WriteError(w, r, http.StatusUnauthorized, openaiapi.NewError(
			"missing tenant context", openaiapi.ErrTypeAuthentication, "invalid_api_key", ""))
		return
	}
	if h.embedder == nil {
		WriteError(w, r, http.StatusNotImplemented, openaiapi.NewError(
			"embeddings are not configured on this gateway",
			openaiapi.ErrTypeInvalidRequest, "not_configured", ""))
		return
	}

	var wire openaiapi.EmbeddingsRequest
	if _, err := decodeBody(r, &wire); err != nil {
		WriteAppError(w, r, err)
		return
	}
	inputs, err := wire.InputStrings()
	if err != nil {
		WriteAppError(w, r, Invalid("input", "%s", err.Error()))
		return
	}
	if len(inputs) == 0 {
		WriteAppError(w, r, Invalid("input", "input must contain at least one string"))
		return
	}

	resp, err := h.embedder.Embed(r.Context(), domain.EmbedRequest{
		RequestID: RequestIDFrom(r.Context()),
		TenantID:  tenant.ID,
		Model:     wire.Model,
		Inputs:    inputs,
	}, tenant)
	if err != nil {
		WriteAppError(w, r, err)
		return
	}

	out := openaiapi.EmbeddingsResponse{
		Object: openaiapi.ObjectList,
		Model:  resp.Model,
		Data:   make([]openaiapi.EmbeddingData, 0, len(resp.Vectors)),
		Usage: &openaiapi.Usage{
			PromptTokens: resp.Usage.PromptTokens,
			TotalTokens:  resp.Usage.TotalTokens(),
		},
	}
	for i, v := range resp.Vectors {
		out.Data = append(out.Data, openaiapi.EmbeddingData{
			Object: openaiapi.ObjectEmbedding, Index: i, Embedding: v,
		})
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// --- wire translation ---------------------------------------------------------

// decodeBody reads and decodes a JSON body, returning the raw map alongside the typed value.
//
// The raw map is what lets provider adapters pass through fields the gateway does not model
// (tools, response_format, and whatever OpenAI ships next) without this build having to know
// about them.
func decodeBody(r *http.Request, out any) (map[string]any, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		return nil, Invalid("", "could not read the request body: %s", err.Error())
	}
	if len(body) > maxRequestBytes {
		return nil, Invalid("", "request body exceeds the %d byte limit", maxRequestBytes)
	}
	if len(body) == 0 {
		return nil, Invalid("", "request body is empty")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return nil, Invalid("", "could not parse the request body as JSON: %s", err.Error())
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, Invalid("", "request body must be a JSON object")
	}
	return raw, nil
}

// toDomainRequest validates and normalises a wire request.
func toDomainRequest(
	r *http.Request,
	wire openaiapi.ChatCompletionRequest,
	raw map[string]any,
	tenant domain.Tenant,
) (domain.ChatRequest, error) {
	if strings.TrimSpace(wire.Model) == "" {
		return domain.ChatRequest{}, Invalid("model", "model is required")
	}
	if len(wire.Messages) == 0 {
		return domain.ChatRequest{}, Invalid("messages", "messages must contain at least one message")
	}
	if wire.N != nil && *wire.N != 1 {
		// n > 1 would multiply cost and break the failover accounting, so it is refused
		// explicitly rather than silently ignored.
		return domain.ChatRequest{}, Invalid("n", "only n=1 is supported by this gateway")
	}

	msgs := make([]domain.Message, 0, len(wire.Messages))
	hasUser := false
	for i, m := range wire.Messages {
		content, err := m.ContentString()
		if err != nil {
			return domain.ChatRequest{}, Invalid(fmt.Sprintf("messages[%d].content", i), "%s", err.Error())
		}
		switch m.Role {
		case openaiapi.RoleSystem, openaiapi.RoleUser, openaiapi.RoleAssistant, openaiapi.RoleTool:
		default:
			return domain.ChatRequest{}, Invalid(fmt.Sprintf("messages[%d].role", i),
				"unknown role %q", m.Role)
		}
		if m.Role == openaiapi.RoleUser {
			hasUser = true
		}
		msgs = append(msgs, domain.Message{Role: m.Role, Name: m.Name, Content: content})
	}
	if !hasUser {
		return domain.ChatRequest{}, Invalid("messages", "at least one message must have the user role")
	}

	req := domain.ChatRequest{
		RequestID:      RequestIDFrom(r.Context()),
		TenantID:       tenant.ID,
		RequestedModel: wire.Model,
		Policy:         routingPolicy(r, wire),
		Messages:       msgs,
		Stream:         wire.Stream,
		MaxTokens:      wire.MaxTokens,
		Temperature:    wire.Temperature,
		TopP:           wire.TopP,
		Seed:           wire.Seed,
		User:           wire.User,
		Stop:           normaliseStop(wire.Stop),
		Raw:            raw,
	}
	if wire.StreamOptions != nil {
		req.IncludeUsage = wire.StreamOptions.IncludeUsage
	}
	return req, nil
}

// routingPolicy reads the policy override from the header, falling back to request metadata for
// SDKs that cannot set custom headers.
func routingPolicy(r *http.Request, wire openaiapi.ChatCompletionRequest) string {
	if p := strings.TrimSpace(r.Header.Get(HeaderPolicy)); p != "" {
		return p
	}
	if p, ok := wire.Metadata["llmrouter_policy"]; ok {
		return strings.TrimSpace(p)
	}
	return ""
}

// normaliseStop accepts OpenAI's string-or-array stop parameter.
func normaliseStop(v any) []string {
	switch s := v.(type) {
	case nil:
		return nil
	case string:
		return []string{s}
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	default:
		return nil
	}
}

func toWireResponse(req domain.ChatRequest, res app.Result) openaiapi.ChatCompletionResponse {
	stop := res.Response.FinishReason
	if stop == "" {
		stop = openaiapi.FinishStop
	}
	created := res.Response.Created
	if created.IsZero() {
		created = time.Now()
	}

	return openaiapi.ChatCompletionResponse{
		ID:      res.Response.ID,
		Object:  openaiapi.ObjectChatCompletion,
		Created: created.Unix(),
		Model:   res.Decision.Model.ID,
		Choices: []openaiapi.Choice{{
			Index: 0,
			Message: &openaiapi.Message{
				Role:    openaiapi.RoleAssistant,
				Content: res.Response.Content,
			},
			FinishReason: &stop,
		}},
		Usage: &openaiapi.Usage{
			PromptTokens:     res.Response.Usage.PromptTokens,
			CompletionTokens: res.Response.Usage.CompletionTokens,
			TotalTokens:      res.Response.Usage.TotalTokens(),
		},
		LLMRouter: &openaiapi.RoutingMeta{
			RequestID:     req.RequestID,
			Policy:        res.Decision.Policy,
			Provider:      res.Decision.Provider,
			ResolvedModel: res.Decision.Model.ID,
			Difficulty:    string(res.Decision.Difficulty),
			Cached:        res.Cached,
			CacheScore:    res.CacheScore,
			Failovers:     maxInt(0, len(res.FailoverPath)-1),
			FailoverPath:  res.FailoverPath,
			CostUSD:       res.CostUSD,
			GuardrailHits: len(res.Findings),
		},
	}
}

func setRoutingHeaders(w http.ResponseWriter, res app.Result) {
	w.Header().Set(HeaderProvider, res.Decision.Provider)
	w.Header().Set(HeaderModel, res.Decision.Model.ID)
	w.Header().Set(HeaderCost, strconv.FormatFloat(res.CostUSD, 'f', 8, 64))
	if res.Cached {
		w.Header().Set(HeaderCacheStatus, "hit")
	} else {
		w.Header().Set(HeaderCacheStatus, "miss")
	}
	if res.BudgetWarn {
		w.Header().Set(HeaderBudgetWarning, "tenant is above its warn threshold")
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
