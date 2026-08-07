// Package openaiapi contains the OpenAI-compatible wire types the gateway speaks.
//
// These types exist so that vanilla OpenAI SDKs (Python, Node, Go) can point their base URL at
// LLMRouter and work unmodified. They are deliberately kept free of any internal concept: the
// translation between the wire and the domain lives in internal/http.
//
// Fields are modelled on the OpenAI Chat Completions API as of 2024-12. Unknown request fields
// are tolerated and ignored rather than rejected, because SDK versions run ahead of gateways.
package openaiapi

import (
	"encoding/json"
	"fmt"
)

// Object type discriminators used in responses.
const (
	ObjectChatCompletion      = "chat.completion"
	ObjectChatCompletionChunk = "chat.completion.chunk"
	ObjectList                = "list"
	ObjectModel               = "model"
	ObjectEmbedding           = "embedding"
)

// Roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Finish reasons.
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishContentFilter = "content_filter"
	FinishToolCalls     = "tool_calls"
)

// Message is one turn of a conversation.
//
// Content is `any` on the wire because the OpenAI API accepts either a plain string or an array
// of content parts (for multimodal input). Use ContentString to get the text.
type Message struct {
	// Role is omitted when empty because OpenAI sends it only on the first delta of a stream;
	// emitting "role":"" on every content delta breaks strict client parsers.
	Role       string     `json:"role,omitempty"`
	Content    any        `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ContentString flattens Content to text, concatenating the text parts of a multimodal message
// and ignoring non-text parts. Returns an error only when the shape is unrecognisable.
func (m Message) ContentString() (string, error) {
	switch v := m.Content.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case []any:
		var out string
		for _, part := range v {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := p["type"].(string); t != "text" {
				continue // images and audio contribute no text
			}
			if s, ok := p["text"].(string); ok {
				out += s
			}
		}
		return out, nil
	default:
		return "", fmt.Errorf("unsupported message content type %T", m.Content)
	}
}

// ToolCall is a function invocation requested by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Index    *int         `json:"index,omitempty"` // present in streaming deltas
	Function FunctionCall `json:"function"`
}

// FunctionCall is the name and JSON-encoded arguments of a tool call.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// Tool is a function the model may call.
type Tool struct {
	Type     string         `json:"type"`
	Function ToolDefinition `json:"function"`
}

// ToolDefinition describes one callable function.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ChatCompletionRequest is POST /v1/chat/completions.
type ChatCompletionRequest struct {
	Model            string         `json:"model"`
	Messages         []Message      `json:"messages"`
	Stream           bool           `json:"stream,omitempty"`
	StreamOptions    *StreamOptions `json:"stream_options,omitempty"`
	Temperature      *float64       `json:"temperature,omitempty"`
	TopP             *float64       `json:"top_p,omitempty"`
	N                *int           `json:"n,omitempty"`
	Stop             any            `json:"stop,omitempty"`
	MaxTokens        *int           `json:"max_tokens,omitempty"`
	PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
	Seed             *int           `json:"seed,omitempty"`
	User             string         `json:"user,omitempty"`
	Tools            []Tool         `json:"tools,omitempty"`
	ToolChoice       any            `json:"tool_choice,omitempty"`
	ResponseFormat   any            `json:"response_format,omitempty"`

	// Metadata is an OpenAI passthrough field. LLMRouter also reads routing hints from it
	// (metadata.llmrouter_policy) for SDKs that cannot set custom headers.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// StreamOptions controls streaming extras.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatCompletionResponse is the non-streaming response body.
type ChatCompletionResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage,omitempty"`

	// LLMRouter is a non-standard extension block. SDKs ignore unknown fields, so adding it
	// keeps the response wire-compatible while giving callers routing visibility.
	LLMRouter *RoutingMeta `json:"llmrouter,omitempty"`
}

// Choice is one completion alternative.
type Choice struct {
	Index        int      `json:"index"`
	Message      *Message `json:"message,omitempty"`
	Delta        *Message `json:"delta,omitempty"`
	FinishReason *string  `json:"finish_reason"`
	Logprobs     any      `json:"logprobs,omitempty"`
}

// ChatCompletionChunk is one SSE `data:` frame of a streamed completion.
type ChatCompletionChunk struct {
	ID                string       `json:"id"`
	Object            string       `json:"object"`
	Created           int64        `json:"created"`
	Model             string       `json:"model"`
	SystemFingerprint string       `json:"system_fingerprint,omitempty"`
	Choices           []Choice     `json:"choices"`
	Usage             *Usage       `json:"usage,omitempty"`
	LLMRouter         *RoutingMeta `json:"llmrouter,omitempty"`
}

// Usage is the token accounting block.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// RoutingMeta is the LLMRouter extension: which provider served the request and why.
type RoutingMeta struct {
	RequestID     string   `json:"request_id"`
	Policy        string   `json:"policy"`
	Provider      string   `json:"provider"`
	ResolvedModel string   `json:"resolved_model"`
	Difficulty    string   `json:"difficulty,omitempty"`
	Cached        bool     `json:"cached"`
	CacheScore    float64  `json:"cache_score,omitempty"`
	Failovers     int      `json:"failovers,omitempty"`
	FailoverPath  []string `json:"failover_path,omitempty"`
	CostUSD       float64  `json:"cost_usd"`
	GuardrailHits int      `json:"guardrail_findings,omitempty"`
}

// EmbeddingsRequest is POST /v1/embeddings. Input is a string or []string on the wire.
type EmbeddingsRequest struct {
	Model          string `json:"model"`
	Input          any    `json:"input"`
	EncodingFormat string `json:"encoding_format,omitempty"`
	Dimensions     *int   `json:"dimensions,omitempty"`
	User           string `json:"user,omitempty"`
}

// InputStrings normalises Input to a slice of strings.
func (r EmbeddingsRequest) InputStrings() ([]string, error) {
	switch v := r.Input.(type) {
	case string:
		return []string{v}, nil
	case []any:
		out := make([]string, 0, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				// Token-array input is valid OpenAI but not supported here.
				return nil, fmt.Errorf("input[%d]: expected string, got %T", i, item)
			}
			out = append(out, s)
		}
		return out, nil
	case []string:
		return v, nil
	default:
		return nil, fmt.Errorf("unsupported input type %T", r.Input)
	}
}

// EmbeddingsResponse is the embeddings response body.
type EmbeddingsResponse struct {
	Object string          `json:"object"`
	Data   []EmbeddingData `json:"data"`
	Model  string          `json:"model"`
	Usage  *Usage          `json:"usage,omitempty"`
}

// EmbeddingData is one embedding vector.
type EmbeddingData struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// Model is one entry of GET /v1/models.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`

	// LLMRouter extensions, useful for a caller choosing a model programmatically.
	Provider      string  `json:"provider,omitempty"`
	Family        string  `json:"family,omitempty"`
	Tier          string  `json:"tier,omitempty"`
	ContextWindow int     `json:"context_window,omitempty"`
	PriceInPerM   float64 `json:"price_in_per_million,omitempty"`
	PriceOutPerM  float64 `json:"price_out_per_million,omitempty"`
	Virtual       bool    `json:"virtual,omitempty"`
}

// ModelList is the GET /v1/models envelope.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// ErrorResponse is the OpenAI error envelope. SDKs parse this shape to build exceptions, so it
// must be exact.
type ErrorResponse struct {
	Error APIError `json:"error"`
}

// APIError is the error body.
type APIError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// Error type strings that SDKs branch on.
const (
	ErrTypeInvalidRequest = "invalid_request_error"
	ErrTypeAuthentication = "authentication_error"
	ErrTypePermission     = "permission_error"
	ErrTypeRateLimit      = "rate_limit_error"
	ErrTypeAPI            = "api_error"
	ErrTypeOverloaded     = "overloaded_error"
)

// NewError builds an ErrorResponse. code may be empty, in which case it is omitted as null.
func NewError(msg, typ, code, param string) ErrorResponse {
	e := APIError{Message: msg, Type: typ}
	if code != "" {
		e.Code = &code
	}
	if param != "" {
		e.Param = &param
	}
	return ErrorResponse{Error: e}
}
