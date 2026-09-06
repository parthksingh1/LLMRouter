// Package live contains the real HTTP adapters, used when PROVIDER_MODE=live.
//
// Three of the five vendors (OpenAI, Mistral, Together) speak the OpenAI wire protocol, so they
// share one client and differ only in base URL and auth header. Anthropic and Google get their
// own translation layers in this package.
//
// None of this code runs on the demo path. It exists so the abstraction is honest: the same
// Provider port is satisfied by a real network client and by the mock harness, and swapping
// between them is a configuration change rather than a code change.
package live

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
	"github.com/parthkumarsingh/llmrouter/services/gateway/pkg/openaiapi"
)

// OpenAICompatible is an adapter for any vendor speaking the OpenAI Chat Completions protocol.
type OpenAICompatible struct {
	name    string
	baseURL string
	apiKey  string
	models  []domain.ModelDescriptor
	client  *http.Client

	// authHeader lets a vendor deviate on auth while keeping the same body format.
	authHeader func(r *http.Request, key string)
}

// NewOpenAICompatible builds an adapter for OpenAI, Mistral or Together.
func NewOpenAICompatible(spec config.ProviderSpec, apiKey string, timeout time.Duration) *OpenAICompatible {
	models := make([]domain.ModelDescriptor, 0, len(spec.Models))
	for _, m := range spec.Models {
		models = append(models, m.Descriptor(spec.Name))
	}
	return &OpenAICompatible{
		name:    spec.Name,
		baseURL: strings.TrimSuffix(spec.BaseURL, "/"),
		apiKey:  apiKey,
		models:  models,
		client:  newHTTPClient(timeout),
		authHeader: func(r *http.Request, key string) {
			r.Header.Set("Authorization", "Bearer "+key)
		},
	}
}

// Name is the provider's catalogue name.
func (p *OpenAICompatible) Name() string { return p.name }

// Models lists the models this provider offers.
func (p *OpenAICompatible) Models() []domain.ModelDescriptor {
	out := make([]domain.ModelDescriptor, len(p.models))
	copy(out, p.models)
	return out
}

// HealthCheck calls GET /models, the cheapest authenticated endpoint every vendor exposes.
func (p *OpenAICompatible) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/models", http.NoBody)
	if err != nil {
		return fmt.Errorf("%s health check: %w", p.name, err)
	}
	p.authHeader(req, p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s health check: %w", p.name, err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s health check: unexpected status %d", p.name, resp.StatusCode)
	}
	return nil
}

// Chat performs a non-streaming completion.
func (p *OpenAICompatible) Chat(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
	body, err := json.Marshal(p.buildBody(req, false))
	if err != nil {
		return domain.ChatResponse{}, fmt.Errorf("%s: encoding request: %w", p.name, err)
	}

	httpReq, err := p.newRequest(ctx, body)
	if err != nil {
		return domain.ChatResponse{}, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return domain.ChatResponse{}, fmt.Errorf("%s: %w", p.name, err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode >= 400 {
		return domain.ChatResponse{}, upstreamStatusError(p.name, resp)
	}

	var out openaiapi.ChatCompletionResponse
	if decodeErr := json.NewDecoder(resp.Body).Decode(&out); decodeErr != nil {
		return domain.ChatResponse{}, fmt.Errorf("%s: decoding response: %w", p.name, decodeErr)
	}
	if len(out.Choices) == 0 {
		return domain.ChatResponse{}, fmt.Errorf("%s: response contained no choices", p.name)
	}

	content := ""
	if out.Choices[0].Message != nil {
		content, err = out.Choices[0].Message.ContentString()
		if err != nil {
			return domain.ChatResponse{}, fmt.Errorf("%s: %w", p.name, err)
		}
	}
	finish := ""
	if out.Choices[0].FinishReason != nil {
		finish = *out.Choices[0].FinishReason
	}
	usage := domain.Usage{}
	if out.Usage != nil {
		usage = domain.Usage{PromptTokens: out.Usage.PromptTokens, CompletionTokens: out.Usage.CompletionTokens}
	}

	return domain.ChatResponse{
		ID:           out.ID,
		Model:        out.Model,
		Provider:     p.name,
		Created:      time.Unix(out.Created, 0),
		Content:      content,
		FinishReason: finish,
		Usage:        usage,
	}, nil
}

// ChatStream performs a streaming completion, translating SSE frames into domain chunks.
func (p *OpenAICompatible) ChatStream(ctx context.Context, req domain.ChatRequest) (<-chan domain.StreamChunk, error) {
	body, err := json.Marshal(p.buildBody(req, true))
	if err != nil {
		return nil, fmt.Errorf("%s: encoding request: %w", p.name, err)
	}

	httpReq, err := p.newRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	// The response body is closed by drainAndClose in the goroutine below. It has to
	// outlive this function: that is what streaming means, and bodyclose cannot see it.
	resp, err := p.client.Do(httpReq) //nolint:bodyclose // closed in the goroutine below
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.name, err)
	}
	if resp.StatusCode >= 400 {
		defer drainAndClose(resp)
		return nil, upstreamStatusError(p.name, resp)
	}

	out := make(chan domain.StreamChunk, 32)
	go func() {
		defer close(out)
		defer drainAndClose(resp)
		p.pumpSSE(ctx, resp.Body, out)
	}()
	return out, nil
}

// pumpSSE reads an OpenAI-style SSE body and emits domain chunks.
func (p *OpenAICompatible) pumpSSE(ctx context.Context, body io.Reader, out chan<- domain.StreamChunk) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return
		}

		var chunk openaiapi.ChatCompletionChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// A frame we cannot parse is a protocol break, not something to skip past: the
			// caller must be able to fail over rather than silently lose tokens.
			emit(ctx, out, domain.StreamChunk{Err: fmt.Errorf("%s: malformed SSE frame: %w", p.name, err)})
			return
		}
		if chunk.Usage != nil {
			u := domain.Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
			}
			if !emit(ctx, out, domain.StreamChunk{Usage: &u}) {
				return
			}
		}
		for _, ch := range chunk.Choices {
			if ch.Delta != nil {
				text, err := ch.Delta.ContentString()
				if err == nil && text != "" {
					if !emit(ctx, out, domain.StreamChunk{Delta: text}) {
						return
					}
				}
			}
			if ch.FinishReason != nil {
				reason := *ch.FinishReason
				if !emit(ctx, out, domain.StreamChunk{FinishReason: &reason}) {
					return
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		emit(ctx, out, domain.StreamChunk{Err: fmt.Errorf("%s: stream read: %w", p.name, err)})
	}
}

// Embed calls the vendor's embeddings endpoint.
func (p *OpenAICompatible) Embed(ctx context.Context, req domain.EmbedRequest) (domain.EmbedResponse, error) {
	body, err := json.Marshal(map[string]any{"model": req.Model, "input": req.Inputs})
	if err != nil {
		return domain.EmbedResponse{}, fmt.Errorf("%s: encoding request: %w", p.name, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return domain.EmbedResponse{}, fmt.Errorf("%s: %w", p.name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	p.authHeader(httpReq, p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return domain.EmbedResponse{}, fmt.Errorf("%s: %w", p.name, err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode >= 400 {
		return domain.EmbedResponse{}, upstreamStatusError(p.name, resp)
	}

	var out openaiapi.EmbeddingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return domain.EmbedResponse{}, fmt.Errorf("%s: decoding response: %w", p.name, err)
	}

	vectors := make([][]float32, len(out.Data))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vectors) {
			return domain.EmbedResponse{}, fmt.Errorf("%s: embedding index %d out of range", p.name, d.Index)
		}
		vectors[d.Index] = d.Embedding
	}
	usage := domain.Usage{}
	if out.Usage != nil {
		usage.PromptTokens = out.Usage.PromptTokens
	}
	return domain.EmbedResponse{Model: out.Model, Provider: p.name, Vectors: vectors, Usage: usage}, nil
}

func (p *OpenAICompatible) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.name, err)
	}
	r.Header.Set("Content-Type", "application/json")
	p.authHeader(r, p.apiKey)
	return r, nil
}

// buildBody renders a domain request back onto the OpenAI wire format.
//
// Fields the domain does not model are carried through from req.Raw, so tools, response_format
// and future parameters reach the vendor untouched.
func (p *OpenAICompatible) buildBody(req domain.ChatRequest, stream bool) map[string]any {
	body := map[string]any{}
	for k, v := range req.Raw {
		switch k {
		case "model", "messages", "stream", "stream_options":
			// Owned by the gateway: the router may have rewritten the model, and the failover
			// machinery may have appended an assistant prefix to the messages.
			continue
		default:
			body[k] = v
		}
	}

	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		msg := map[string]any{"role": m.Role, "content": m.Content}
		if m.Name != "" {
			msg["name"] = m.Name
		}
		msgs = append(msgs, msg)
	}

	body["model"] = req.RequestedModel
	body["messages"] = msgs
	body["stream"] = stream
	if stream && req.IncludeUsage {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	return body
}
