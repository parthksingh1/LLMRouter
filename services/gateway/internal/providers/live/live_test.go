package live_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/providers/live"
)

func spec(name, baseURL string, prefix config.PrefixMode) config.ProviderSpec {
	return config.ProviderSpec{
		Name:               name,
		BaseURL:            baseURL,
		PrefixContinuation: prefix,
		Models: []config.ModelSpec{
			{ID: name + "-big", Family: name, Tier: "frontier", Quality: 0.95, PriceInPerM: 3, PriceOutPerM: 15},
			{ID: name + "-small", Family: name, Tier: "efficient", Quality: 0.86, PriceInPerM: 0.2, PriceOutPerM: 0.6},
		},
	}
}

func chatReq(model, prompt string) domain.ChatRequest {
	return domain.ChatRequest{
		RequestID:      "req-1",
		TenantID:       "tenant-a",
		RequestedModel: model,
		Messages: []domain.Message{
			{Role: domain.RoleSystem, Content: "Be terse."},
			{Role: domain.RoleUser, Content: prompt},
		},
	}
}

// sse writes SSE frames with the flushing a streaming client expects.
func sse(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, f := range frames {
		_, _ = io.WriteString(w, f+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// drain collects a stream into text, the finish reason, usage and any terminal error.
func drain(ch <-chan domain.StreamChunk) (text, finish string, usage *domain.Usage, err error) {
	var sb strings.Builder
	for c := range ch {
		switch {
		case c.Err != nil:
			err = c.Err
		case c.FinishReason != nil:
			finish = *c.FinishReason
			if c.Usage != nil {
				usage = c.Usage
			}
		case c.Usage != nil:
			usage = c.Usage
		default:
			sb.WriteString(c.Delta)
		}
	}
	return sb.String(), finish, usage, err
}

// --- OpenAI-compatible -------------------------------------------------------

func TestOpenAICompatibleChat(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	var gotAuth, gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
		  "id":"chatcmpl-abc","object":"chat.completion","created":1700000000,"model":"openai-big",
		  "choices":[{"index":0,"message":{"role":"assistant","content":"42"},"finish_reason":"stop"}],
		  "usage":{"prompt_tokens":11,"completion_tokens":1,"total_tokens":12}}`)
	}))
	defer srv.Close()

	p := live.NewOpenAICompatible(spec("openai", srv.URL, config.PrefixPrefill), "sk-test", 5*time.Second)

	resp, err := p.Chat(context.Background(), chatReq("openai-big", "what is six times seven"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want a bearer token", gotAuth)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotBody["stream"] != false {
		t.Errorf("stream = %v, want false for a unary call", gotBody["stream"])
	}
	if gotBody["model"] != "openai-big" {
		t.Errorf("model = %v, want the routed model", gotBody["model"])
	}
	// The system message stays inline for OpenAI-compatible vendors.
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages, want 2", len(msgs))
	}

	if resp.Content != "42" {
		t.Errorf("content = %q, want 42", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want stop", resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 1 {
		t.Errorf("usage = %+v, want {11 1}", resp.Usage)
	}
	if resp.Provider != "openai" {
		t.Errorf("provider = %q, want openai", resp.Provider)
	}
}

func TestOpenAICompatiblePassesThroughUnmodelledFields(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	p := live.NewOpenAICompatible(spec("openai", srv.URL, config.PrefixPrefill), "sk-test", 5*time.Second)

	req := chatReq("openai-big", "call a tool")
	req.Raw = map[string]any{
		"tools":           []any{map[string]any{"type": "function"}},
		"response_format": map[string]any{"type": "json_object"},
		// Gateway-owned keys must be overwritten, not passed through.
		"model":  "the-caller-asked-for-auto",
		"stream": true,
	}

	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if _, ok := gotBody["tools"]; !ok {
		t.Error("tools were dropped; a caller's tool definitions must reach the vendor")
	}
	if _, ok := gotBody["response_format"]; !ok {
		t.Error("response_format was dropped")
	}
	if gotBody["model"] != "openai-big" {
		t.Errorf("model = %v; the router's choice must win over the raw body", gotBody["model"])
	}
	if gotBody["stream"] != false {
		t.Errorf("stream = %v; the gateway owns this field", gotBody["stream"])
	}
}

func TestOpenAICompatibleStream(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sse(w,
			`data: {"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"}}]}`,
			`data: {"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":", world"}}]}`,
			`data: {"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: {"id":"1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
			`data: [DONE]`,
		)
	}))
	defer srv.Close()

	p := live.NewOpenAICompatible(spec("openai", srv.URL, config.PrefixPrefill), "sk-test", 5*time.Second)

	ch, err := p.ChatStream(context.Background(), chatReq("openai-big", "greet me"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	text, finish, usage, streamErr := drain(ch)

	if streamErr != nil {
		t.Fatalf("unexpected stream error: %v", streamErr)
	}
	if text != "Hello, world" {
		t.Errorf("text = %q, want %q", text, "Hello, world")
	}
	if finish != "stop" {
		t.Errorf("finish = %q, want stop", finish)
	}
	if usage == nil || usage.CompletionTokens != 3 {
		t.Errorf("usage = %+v, want completion_tokens 3", usage)
	}
}

func TestOpenAICompatibleStreamSurfacesMalformedFrames(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sse(w,
			`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}`,
			`data: {this is not json`,
		)
	}))
	defer srv.Close()

	p := live.NewOpenAICompatible(spec("openai", srv.URL, config.PrefixPrefill), "sk-test", 5*time.Second)

	ch, err := p.ChatStream(context.Background(), chatReq("openai-big", "hi"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	text, _, _, streamErr := drain(ch)

	// A frame we cannot parse must be reported, not skipped: the caller has to be able to fail
	// over rather than silently return a truncated answer.
	if streamErr == nil {
		t.Fatal("a malformed SSE frame must produce a terminal error chunk")
	}
	if text != "partial" {
		t.Errorf("tokens emitted before the break should still reach the caller, got %q", text)
	}
}

func TestOpenAICompatibleErrorStatuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        int
		wantRetryable bool
	}{
		{name: "rate limited is retryable elsewhere", status: http.StatusTooManyRequests, wantRetryable: true},
		{name: "server error is retryable elsewhere", status: http.StatusInternalServerError, wantRetryable: true},
		{name: "bad gateway is retryable elsewhere", status: http.StatusBadGateway, wantRetryable: true},
		{name: "bad request is the caller's fault", status: http.StatusBadRequest, wantRetryable: false},
		{name: "unauthorized will fail identically elsewhere", status: http.StatusUnauthorized, wantRetryable: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":{"message":"upstream said no"}}`)
			}))
			defer srv.Close()

			p := live.NewOpenAICompatible(spec("openai", srv.URL, config.PrefixPrefill), "sk-test", 5*time.Second)

			_, err := p.Chat(context.Background(), chatReq("openai-big", "hi"))
			if err == nil {
				t.Fatal("expected an error")
			}

			var se *live.StatusError
			if !errors.As(err, &se) {
				t.Fatalf("error %v is not a *StatusError; the router cannot classify it", err)
			}
			if se.Status != tc.status {
				t.Errorf("status = %d, want %d", se.Status, tc.status)
			}
			if se.Retryable() != tc.wantRetryable {
				t.Errorf("Retryable() = %v, want %v for status %d", se.Retryable(), tc.wantRetryable, tc.status)
			}
			if !strings.Contains(se.Error(), "upstream said no") {
				t.Errorf("the upstream message should be preserved for debugging: %v", se)
			}
		})
	}
}

func TestOpenAICompatibleHealthAndEmbed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
		case "/embeddings":
			// Returned out of order on purpose: the adapter must reorder by index.
			_, _ = io.WriteString(w, `{"object":"list","model":"text-embedding-3-small","data":[
			  {"object":"embedding","index":1,"embedding":[0.3,0.4]},
			  {"object":"embedding","index":0,"embedding":[0.1,0.2]}],
			  "usage":{"prompt_tokens":4,"total_tokens":4}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := live.NewOpenAICompatible(spec("openai", srv.URL, config.PrefixPrefill), "sk-test", 5*time.Second)

	if err := p.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck: %v", err)
	}

	resp, err := p.Embed(context.Background(), domain.EmbedRequest{
		Model:  "text-embedding-3-small",
		Inputs: []string{"first", "second"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Vectors) != 2 {
		t.Fatalf("got %d vectors, want 2", len(resp.Vectors))
	}
	if resp.Vectors[0][0] != 0.1 {
		t.Errorf("vectors were not reordered by index: %v", resp.Vectors)
	}
	if resp.Usage.PromptTokens != 4 {
		t.Errorf("usage = %+v, want 4 prompt tokens", resp.Usage)
	}
}

func TestOpenAICompatibleHealthCheckFailsOnBadStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := live.NewOpenAICompatible(spec("openai", srv.URL, config.PrefixPrefill), "bad-key", 5*time.Second)
	if err := p.HealthCheck(context.Background()); err == nil {
		t.Error("a 401 from the vendor must fail the health check")
	}
}

func TestProviderMetadata(t *testing.T) {
	t.Parallel()

	p := live.NewOpenAICompatible(spec("openai", "http://example.invalid", config.PrefixPrefill), "k", time.Second)
	if p.Name() != "openai" {
		t.Errorf("Name() = %q", p.Name())
	}
	models := p.Models()
	if len(models) != 2 {
		t.Fatalf("Models() returned %d entries, want 2", len(models))
	}
	// Models() must hand back a copy; a caller mutating it must not corrupt the adapter.
	models[0].ID = "mutated"
	if p.Models()[0].ID == "mutated" {
		t.Error("Models() exposed its backing array")
	}
}

// --- Anthropic ---------------------------------------------------------------

func TestAnthropicRequestTranslation(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	var gotVersion, gotKey, gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("anthropic-version")
		gotKey = r.Header.Get("x-api-key")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)

		_, _ = io.WriteString(w, `{"id":"msg_1","model":"anthropic-big","stop_reason":"end_turn",
		  "content":[{"type":"text","text":"terse answer"}],
		  "usage":{"input_tokens":9,"output_tokens":2}}`)
	}))
	defer srv.Close()

	p := live.NewAnthropic(spec("anthropic", srv.URL, config.PrefixNative), "sk-ant", 5*time.Second)

	resp, err := p.Chat(context.Background(), chatReq("anthropic-big", "answer briefly"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if gotPath != "/messages" {
		t.Errorf("path = %q, want /messages", gotPath)
	}
	if gotKey != "sk-ant" {
		t.Errorf("x-api-key = %q", gotKey)
	}
	if gotVersion == "" {
		t.Error("anthropic-version header is required and was not sent")
	}
	// The system prompt is a top-level field for Anthropic, not a message.
	if gotBody["system"] != "Be terse." {
		t.Errorf("system = %v, want the system prompt hoisted out of messages", gotBody["system"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("sent %d messages, want 1 (the system turn must be hoisted)", len(msgs))
	}
	// max_tokens is mandatory for this vendor, so the adapter must supply a default.
	if _, ok := gotBody["max_tokens"]; !ok {
		t.Error("max_tokens is required by the Messages API and was not sent")
	}

	if resp.Content != "terse answer" {
		t.Errorf("content = %q", resp.Content)
	}
	// stop_reason must be translated into the OpenAI vocabulary clients branch on.
	if resp.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want stop (translated from end_turn)", resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 9 || resp.Usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v, want {9 2}", resp.Usage)
	}
}

func TestAnthropicStreamTranslation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sse(w,
			`event: message_start`+"\n"+`data: {"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":0}}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Consistent "}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hashing"}}`,
			`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":7}}`,
			`event: message_stop`+"\n"+`data: {"type":"message_stop"}`,
		)
	}))
	defer srv.Close()

	p := live.NewAnthropic(spec("anthropic", srv.URL, config.PrefixNative), "sk-ant", 5*time.Second)

	ch, err := p.ChatStream(context.Background(), chatReq("anthropic-big", "explain"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	text, finish, usage, streamErr := drain(ch)

	if streamErr != nil {
		t.Fatalf("unexpected stream error: %v", streamErr)
	}
	if text != "Consistent hashing" {
		t.Errorf("text = %q", text)
	}
	if finish != "length" {
		t.Errorf("finish = %q, want length (translated from max_tokens)", finish)
	}
	// Usage arrives split across two event types and must be assembled into one record, or
	// prompt tokens would be lost for every streamed Anthropic call.
	if usage == nil || usage.PromptTokens != 12 || usage.CompletionTokens != 7 {
		t.Errorf("usage = %+v, want {12 7} assembled from message_start and message_delta", usage)
	}
}

func TestAnthropicStreamErrorEvent(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sse(w,
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"start"}}`,
			`data: {"type":"error","error":{"type":"overloaded_error"}}`,
		)
	}))
	defer srv.Close()

	p := live.NewAnthropic(spec("anthropic", srv.URL, config.PrefixNative), "sk-ant", 5*time.Second)
	ch, err := p.ChatStream(context.Background(), chatReq("anthropic-big", "hi"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if _, _, _, streamErr := drain(ch); streamErr == nil {
		t.Error("an upstream error event must terminate the stream with an error")
	}
}

func TestAnthropicEmbedIsUnsupported(t *testing.T) {
	t.Parallel()

	p := live.NewAnthropic(spec("anthropic", "http://example.invalid", config.PrefixNative), "k", time.Second)
	if _, err := p.Embed(context.Background(), domain.EmbedRequest{}); err == nil {
		t.Error("Anthropic has no embeddings endpoint; the adapter must say so rather than hang")
	}
}

// --- Google ------------------------------------------------------------------

func TestGoogleRequestTranslation(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	var gotPath, gotQuery string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"Paris"}]},"finishReason":"STOP"}],
		  "usageMetadata":{"promptTokenCount":6,"candidatesTokenCount":1}}`)
	}))
	defer srv.Close()

	p := live.NewGoogle(spec("google", srv.URL, config.PrefixNone), "goog-key", 5*time.Second)

	req := chatReq("google-big", "capital of France")
	req.Messages = append(req.Messages, domain.Message{Role: domain.RoleAssistant, Content: "The capital"})

	resp, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if !strings.Contains(gotPath, ":generateContent") {
		t.Errorf("path = %q, want a :generateContent call", gotPath)
	}
	if !strings.Contains(gotQuery, "key=goog-key") {
		t.Errorf("query = %q, want the API key", gotQuery)
	}
	if _, ok := gotBody["systemInstruction"]; !ok {
		t.Error("the system prompt must be sent as systemInstruction")
	}

	contents, _ := gotBody["contents"].([]any)
	if len(contents) != 2 {
		t.Fatalf("sent %d contents, want 2", len(contents))
	}
	// Gemini names the assistant role "model"; sending "assistant" is rejected upstream.
	last, _ := contents[1].(map[string]any)
	if last["role"] != "model" {
		t.Errorf("assistant role = %v, want model", last["role"])
	}

	if resp.Content != "Paris" {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want stop (translated from STOP)", resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 6 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestGoogleStreamTranslation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "alt=sse") {
			t.Errorf("streaming must request alt=sse, got %q", r.URL.RawQuery)
		}
		sse(w,
			`data: {"candidates":[{"content":{"parts":[{"text":"Once "}]}}],"usageMetadata":{"promptTokenCount":4}}`,
			`data: {"candidates":[{"content":{"parts":[{"text":"upon a time"}]}}]}`,
			`data: {"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":5}}`,
		)
	}))
	defer srv.Close()

	p := live.NewGoogle(spec("google", srv.URL, config.PrefixNone), "goog-key", 5*time.Second)

	ch, err := p.ChatStream(context.Background(), chatReq("google-big", "tell a story"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	text, finish, usage, streamErr := drain(ch)

	if streamErr != nil {
		t.Fatalf("unexpected stream error: %v", streamErr)
	}
	if text != "Once upon a time" {
		t.Errorf("text = %q", text)
	}
	if finish != "content_filter" {
		t.Errorf("finish = %q, want content_filter (translated from SAFETY)", finish)
	}
	if usage == nil || usage.CompletionTokens != 5 {
		t.Errorf("usage = %+v", usage)
	}
}

func TestGoogleHealthCheck(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"models":[]}`)
	}))
	defer srv.Close()

	p := live.NewGoogle(spec("google", srv.URL, config.PrefixNone), "goog-key", 5*time.Second)
	if err := p.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck: %v", err)
	}
}

func TestStreamsRespectContextCancellation(t *testing.T) {
	t.Parallel()

	// A server that never finishes: the adapter must stop when the caller gives up.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"x"}}]}`+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer srv.Close()

	p := live.NewOpenAICompatible(spec("openai", srv.URL, config.PrefixPrefill), "sk-test", 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.ChatStream(ctx, chatReq("openai-big", "stream forever"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	<-ch
	cancel()

	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the adapter goroutine outlived its context; it leaks")
	}
}
