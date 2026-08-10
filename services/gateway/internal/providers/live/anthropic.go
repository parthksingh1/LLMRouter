package live

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// anthropicVersion is the required API version header.
const anthropicVersion = "2023-06-01"

// defaultMaxTokens is sent when the caller did not specify one. Anthropic requires the field,
// unlike OpenAI, so the adapter has to supply a value rather than omit it.
const defaultMaxTokens = 4096

// Anthropic adapts the Messages API.
//
// It is the one vendor where mid-stream failover is fully faithful: a trailing assistant message
// is a first-class way to continue a partial answer, so a stream broken at token N resumes at
// token N+1 rather than restarting. See docs/adr/0003-failover-semantics.md.
type Anthropic struct {
	name    string
	baseURL string
	apiKey  string
	models  []domain.ModelDescriptor
	client  *http.Client
}

// NewAnthropic builds the Anthropic adapter.
func NewAnthropic(spec config.ProviderSpec, apiKey string, timeout time.Duration) *Anthropic {
	models := make([]domain.ModelDescriptor, 0, len(spec.Models))
	for _, m := range spec.Models {
		models = append(models, m.Descriptor(spec.Name))
	}
	return &Anthropic{
		name:    spec.Name,
		baseURL: strings.TrimSuffix(spec.BaseURL, "/"),
		apiKey:  apiKey,
		models:  models,
		client:  newHTTPClient(timeout),
	}
}

// Name is the provider's catalogue name.
func (p *Anthropic) Name() string { return p.name }

// Models lists the models this provider offers.
func (p *Anthropic) Models() []domain.ModelDescriptor {
	out := make([]domain.ModelDescriptor, len(p.models))
	copy(out, p.models)
	return out
}

// HealthCheck issues a one-token completion, the cheapest call that proves credentials work.
// Anthropic has no unauthenticated liveness endpoint.
func (p *Anthropic) HealthCheck(ctx context.Context) error {
	if len(p.models) == 0 {
		return fmt.Errorf("%s: no models configured", p.name)
	}
	body, err := json.Marshal(map[string]any{
		"model":      p.models[len(p.models)-1].ID, // cheapest model
		"max_tokens": 1,
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
	})
	if err != nil {
		return fmt.Errorf("%s health check: %w", p.name, err)
	}
	req, err := p.newRequest(ctx, body)
	if err != nil {
		return err
	}
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

// anthropicResponse is the non-streaming Messages response.
type anthropicResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// Chat performs a non-streaming completion.
func (p *Anthropic) Chat(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
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

	var out anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return domain.ChatResponse{}, fmt.Errorf("%s: decoding response: %w", p.name, err)
	}
	var text strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	return domain.ChatResponse{
		ID:           out.ID,
		Model:        out.Model,
		Provider:     p.name,
		Created:      time.Now(),
		Content:      text.String(),
		FinishReason: mapStopReason(out.StopReason),
		Usage: domain.Usage{
			PromptTokens:     out.Usage.InputTokens,
			CompletionTokens: out.Usage.OutputTokens,
		},
	}, nil
}

// ChatStream translates Anthropic's typed SSE events into domain chunks.
func (p *Anthropic) ChatStream(ctx context.Context, req domain.ChatRequest) (<-chan domain.StreamChunk, error) {
	body, err := json.Marshal(p.buildBody(req, true))
	if err != nil {
		return nil, fmt.Errorf("%s: encoding request: %w", p.name, err)
	}
	httpReq, err := p.newRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
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

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		usage := domain.Usage{}
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue // `event:` lines are redundant: the payload carries its own type
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" {
				continue
			}

			var ev struct {
				Type  string `json:"type"`
				Delta struct {
					Type       string `json:"type"`
					Text       string `json:"text"`
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Message struct {
					Usage struct {
						InputTokens  int `json:"input_tokens"`
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				} `json:"message"`
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				emit(ctx, out, domain.StreamChunk{Err: fmt.Errorf("%s: malformed SSE frame: %w", p.name, err)})
				return
			}

			switch ev.Type {
			case "message_start":
				usage.PromptTokens = ev.Message.Usage.InputTokens
			case "content_block_delta":
				if ev.Delta.Text != "" && !emit(ctx, out, domain.StreamChunk{Delta: ev.Delta.Text}) {
					return
				}
			case "message_delta":
				usage.CompletionTokens = ev.Usage.OutputTokens
				if ev.Delta.StopReason != "" {
					reason := mapStopReason(ev.Delta.StopReason)
					u := usage
					if !emit(ctx, out, domain.StreamChunk{FinishReason: &reason, Usage: &u}) {
						return
					}
				}
			case "error":
				emit(ctx, out, domain.StreamChunk{Err: fmt.Errorf("%s: upstream error event", p.name)})
				return
			case "message_stop":
				return
			}
		}
		if err := sc.Err(); err != nil {
			emit(ctx, out, domain.StreamChunk{Err: fmt.Errorf("%s: stream read: %w", p.name, err)})
		}
	}()
	return out, nil
}

// Embed is unsupported: Anthropic offers no embeddings endpoint.
func (p *Anthropic) Embed(_ context.Context, _ domain.EmbedRequest) (domain.EmbedResponse, error) {
	return domain.EmbedResponse{}, fmt.Errorf("%s: embeddings are not supported by this provider", p.name)
}

func (p *Anthropic) newRequest(ctx context.Context, body []byte) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.name, err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-api-key", p.apiKey)
	r.Header.Set("anthropic-version", anthropicVersion)
	return r, nil
}

// buildBody converts the domain request to the Messages format.
//
// Anthropic differs from OpenAI in three ways that matter: the system prompt is a top-level
// field rather than a message, max_tokens is mandatory, and a trailing assistant message is
// interpreted as text to continue.
func (p *Anthropic) buildBody(req domain.ChatRequest, stream bool) map[string]any {
	var system strings.Builder
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == domain.RoleSystem {
			if system.Len() > 0 {
				system.WriteByte('\n')
			}
			system.WriteString(m.Content)
			continue
		}
		msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
	}

	maxTokens := defaultMaxTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}

	body := map[string]any{
		"model":      req.RequestedModel,
		"messages":   msgs,
		"max_tokens": maxTokens,
		"stream":     stream,
	}
	if system.Len() > 0 {
		body["system"] = system.String()
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		body["stop_sequences"] = req.Stop
	}
	return body
}

// mapStopReason translates Anthropic stop reasons onto the OpenAI vocabulary, because clients
// branch on the OpenAI values.
func mapStopReason(r string) string {
	switch r {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "":
		return ""
	default:
		return r
	}
}
