package live

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// Google adapts the Gemini generateContent API.
//
// Gemini has no notion of continuing a partial assistant turn, so it is declared
// prefix_continuation: none in the catalogue and a failover onto Gemini restarts the generation.
// That trade-off is the reason the failover machinery reports a restart in its metadata rather
// than pretending the seam was invisible.
type Google struct {
	name    string
	baseURL string
	apiKey  string
	models  []domain.ModelDescriptor
	client  *http.Client
}

// NewGoogle builds the Gemini adapter.
func NewGoogle(spec config.ProviderSpec, apiKey string, timeout time.Duration) *Google {
	models := make([]domain.ModelDescriptor, 0, len(spec.Models))
	for _, m := range spec.Models {
		models = append(models, m.Descriptor(spec.Name))
	}
	return &Google{
		name:    spec.Name,
		baseURL: strings.TrimSuffix(spec.BaseURL, "/"),
		apiKey:  apiKey,
		models:  models,
		client:  newHTTPClient(timeout),
	}
}

// Name is the provider's catalogue name.
func (p *Google) Name() string { return p.name }

// Models lists the models this provider offers.
func (p *Google) Models() []domain.ModelDescriptor {
	out := make([]domain.ModelDescriptor, len(p.models))
	copy(out, p.models)
	return out
}

// HealthCheck lists models, which is unauthenticated-cheap and validates the key.
func (p *Google) HealthCheck(ctx context.Context) error {
	u := p.baseURL + "/models?key=" + url.QueryEscape(p.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return fmt.Errorf("%s health check: %w", p.name, err)
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

// geminiResponse covers both the unary and streamed payload shapes.
type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
}

func (g geminiResponse) text() string {
	var b strings.Builder
	for _, c := range g.Candidates {
		for _, part := range c.Content.Parts {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// Chat performs a non-streaming generateContent call.
func (p *Google) Chat(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
	body, err := json.Marshal(p.buildBody(req))
	if err != nil {
		return domain.ChatResponse{}, fmt.Errorf("%s: encoding request: %w", p.name, err)
	}
	endpoint := fmt.Sprintf("%s/models/%s:generateContent?key=%s",
		p.baseURL, url.PathEscape(req.RequestedModel), url.QueryEscape(p.apiKey))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return domain.ChatResponse{}, fmt.Errorf("%s: %w", p.name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return domain.ChatResponse{}, fmt.Errorf("%s: %w", p.name, err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode >= 400 {
		return domain.ChatResponse{}, upstreamStatusError(p.name, resp)
	}

	var out geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return domain.ChatResponse{}, fmt.Errorf("%s: decoding response: %w", p.name, err)
	}
	finish := ""
	if len(out.Candidates) > 0 {
		finish = mapGeminiFinish(out.Candidates[0].FinishReason)
	}
	return domain.ChatResponse{
		ID:           "gemini-" + domain.HashText(req.UserPrompt())[:16],
		Model:        req.RequestedModel,
		Provider:     p.name,
		Created:      time.Now(),
		Content:      out.text(),
		FinishReason: finish,
		Usage: domain.Usage{
			PromptTokens:     out.UsageMetadata.PromptTokenCount,
			CompletionTokens: out.UsageMetadata.CandidatesTokenCount,
		},
	}, nil
}

// ChatStream uses streamGenerateContent with alt=sse.
func (p *Google) ChatStream(ctx context.Context, req domain.ChatRequest) (<-chan domain.StreamChunk, error) {
	body, err := json.Marshal(p.buildBody(req))
	if err != nil {
		return nil, fmt.Errorf("%s: encoding request: %w", p.name, err)
	}
	endpoint := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse&key=%s",
		p.baseURL, url.PathEscape(req.RequestedModel), url.QueryEscape(p.apiKey))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.name, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
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

		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		usage := domain.Usage{}
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" {
				continue
			}
			var ev geminiResponse
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				emit(ctx, out, domain.StreamChunk{Err: fmt.Errorf("%s: malformed SSE frame: %w", p.name, err)})
				return
			}
			if ev.UsageMetadata.PromptTokenCount > 0 {
				usage.PromptTokens = ev.UsageMetadata.PromptTokenCount
			}
			if ev.UsageMetadata.CandidatesTokenCount > 0 {
				usage.CompletionTokens = ev.UsageMetadata.CandidatesTokenCount
			}
			if text := ev.text(); text != "" {
				if !emit(ctx, out, domain.StreamChunk{Delta: text}) {
					return
				}
			}
			if len(ev.Candidates) > 0 && ev.Candidates[0].FinishReason != "" {
				reason := mapGeminiFinish(ev.Candidates[0].FinishReason)
				u := usage
				emit(ctx, out, domain.StreamChunk{FinishReason: &reason, Usage: &u})
				return
			}
		}
		if err := sc.Err(); err != nil {
			emit(ctx, out, domain.StreamChunk{Err: fmt.Errorf("%s: stream read: %w", p.name, err)})
		}
	}()
	return out, nil
}

// Embed is unsupported here: Gemini embeddings use a different endpoint shape and the gateway
// serves embeddings from the local sidecar in every supported configuration.
func (p *Google) Embed(_ context.Context, _ domain.EmbedRequest) (domain.EmbedResponse, error) {
	return domain.EmbedResponse{}, fmt.Errorf("%s: embeddings are not wired for this provider", p.name)
}

// buildBody converts the domain request into Gemini's contents/systemInstruction shape.
func (p *Google) buildBody(req domain.ChatRequest) map[string]any {
	contents := make([]map[string]any, 0, len(req.Messages))
	var system strings.Builder

	for _, m := range req.Messages {
		switch m.Role {
		case domain.RoleSystem:
			if system.Len() > 0 {
				system.WriteByte('\n')
			}
			system.WriteString(m.Content)
		case domain.RoleAssistant:
			contents = append(contents, map[string]any{
				"role":  "model", // Gemini names the assistant turn "model"
				"parts": []map[string]any{{"text": m.Content}},
			})
		default:
			contents = append(contents, map[string]any{
				"role":  "user",
				"parts": []map[string]any{{"text": m.Content}},
			})
		}
	}

	gen := map[string]any{}
	if req.MaxTokens != nil {
		gen["maxOutputTokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		gen["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		gen["topP"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		gen["stopSequences"] = req.Stop
	}

	body := map[string]any{"contents": contents}
	if system.Len() > 0 {
		body["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": system.String()}},
		}
	}
	if len(gen) > 0 {
		body["generationConfig"] = gen
	}
	return body
}

func mapGeminiFinish(r string) string {
	switch r {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "content_filter"
	case "":
		return ""
	default:
		return strings.ToLower(r)
	}
}
