package http_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
	gwhttp "github.com/parthkumarsingh/llmrouter/services/gateway/internal/http"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/telemetry"
	"github.com/parthkumarsingh/llmrouter/services/gateway/pkg/openaiapi"
)

// stubCompleter records what the handler passed down and returns a canned result.
type stubCompleter struct {
	result        app.Result
	err           error
	seen          domain.ChatRequest
	tenant        domain.Tenant
	failoverAfter bool
}

func (s *stubCompleter) Complete(_ context.Context, req domain.ChatRequest, tenant domain.Tenant) (app.Result, error) {
	s.seen = req
	s.tenant = tenant
	if s.err != nil {
		return app.Result{}, s.err
	}
	return s.result, nil
}

// CompleteStream makes the stub a StreamCompleter, so the streaming handler exercises the same
// SSE writer the real service uses. It replays the canned answer as a single delta, which is
// enough to assert on frame ordering, the role-first rule and the [DONE] terminator.
func (s *stubCompleter) CompleteStream(
	_ context.Context,
	req domain.ChatRequest,
	tenant domain.Tenant,
	sink app.StreamSink,
) (app.Result, error) {
	s.seen = req
	s.tenant = tenant
	if s.err != nil {
		return app.Result{}, s.err
	}
	if s.failoverAfter {
		if err := sink.Delta("partial "); err != nil {
			return app.Result{}, err
		}
		if err := sink.Failover(app.StreamFailover{
			Attempt: 1, From: "openai", To: "anthropic", Model: "claude-3-5-sonnet",
			Reason: "stream_reset", Mode: "continue", TokensPreserved: 8,
		}); err != nil {
			return app.Result{}, err
		}
	}
	if err := sink.Delta(s.result.Response.Content); err != nil {
		return app.Result{}, err
	}
	if err := sink.Done(app.StreamSummary{
		FinishReason: s.result.Response.FinishReason,
		Usage:        s.result.Response.Usage,
		Provider:     s.result.Decision.Provider,
		Model:        s.result.Decision.Model.ID,
		Policy:       s.result.Decision.Policy,
		Difficulty:   string(s.result.Decision.Difficulty),
		CostUSD:      s.result.CostUSD,
		Path:         s.result.FailoverPath,
	}); err != nil {
		return app.Result{}, err
	}
	return s.result, nil
}

type stubEmbedder struct {
	resp domain.EmbedResponse
	err  error
	seen domain.EmbedRequest
}

func (s *stubEmbedder) Embed(_ context.Context, req domain.EmbedRequest, _ domain.Tenant) (domain.EmbedResponse, error) {
	s.seen = req
	if s.err != nil {
		return domain.EmbedResponse{}, s.err
	}
	return s.resp, nil
}

func okResult() app.Result {
	return app.Result{
		Response: domain.ChatResponse{
			ID: "chatcmpl-1", Content: "Paris", FinishReason: "stop",
			Usage: domain.Usage{PromptTokens: 11, CompletionTokens: 1},
		},
		Decision: domain.RouteDecision{
			Provider: "openai", Policy: "quality_tiered", Difficulty: domain.DifficultyEasy,
			Model: domain.ModelDescriptor{ID: "gpt-4o-mini", Provider: "openai"},
		},
		CostUSD:      0.0000027,
		FailoverPath: []string{"openai/gpt-4o-mini"},
	}
}

// chatServer mounts the real router with stubbed use cases.
func chatServer(t *testing.T, completer gwhttp.Completer, embedder gwhttp.Embedder) http.Handler {
	t.Helper()

	catalogue := testProviders()
	metrics := telemetry.NewMetrics()

	return gwhttp.NewRouter(gwhttp.Deps{
		Log:            quietLogger(),
		Version:        "test",
		Tenants:        gwhttp.NewMemoryTenantStore(testTenants()),
		Meta:           gwhttp.NewMetaHandler(catalogue, testPolicies(), nil, "test"),
		Chat:           gwhttp.NewChatHandler(completer, embedder),
		MetricsHandler: metrics.Handler(),
		Recorder:       metrics,
	})
}

func post(t *testing.T, h http.Handler, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChatCompletionsHappyPath(t *testing.T) {
	t.Parallel()

	stub := &stubCompleter{result: okResult()}
	h := chatServer(t, stub, nil)

	rec := post(t, h, "/v1/chat/completions", "demo-tenant-a",
		`{"model":"auto","messages":[{"role":"user","content":"capital of France?"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var resp openaiapi.ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if resp.Object != openaiapi.ObjectChatCompletion {
		t.Errorf("object = %q", resp.Object)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message == nil {
		t.Fatalf("choices = %+v", resp.Choices)
	}
	content, _ := resp.Choices[0].Message.ContentString()
	if content != "Paris" {
		t.Errorf("content = %q", content)
	}
	// The response model must be the model that actually served, not the virtual "auto".
	if resp.Model != "gpt-4o-mini" {
		t.Errorf("model = %q, want the resolved model", resp.Model)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 12 {
		t.Errorf("usage = %+v, want a total of 12", resp.Usage)
	}
	if resp.LLMRouter == nil || resp.LLMRouter.Provider != "openai" {
		t.Errorf("routing metadata missing: %+v", resp.LLMRouter)
	}

	for header, want := range map[string]string{
		"X-LLMRouter-Provider": "openai",
		"X-LLMRouter-Model":    "gpt-4o-mini",
		"X-LLMRouter-Cache":    "miss",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	// The tenant from the auth middleware must reach the use case.
	if stub.tenant.ID != "tenant-a" {
		t.Errorf("tenant = %q, want tenant-a", stub.tenant.ID)
	}
	if stub.seen.RequestID == "" {
		t.Error("the request id must be propagated into the domain request")
	}
}

func TestChatCompletionsValidation(t *testing.T) {
	t.Parallel()

	h := chatServer(t, &stubCompleter{result: okResult()}, nil)

	tests := []struct {
		name      string
		body      string
		wantParam string
	}{
		{name: "no model", body: `{"messages":[{"role":"user","content":"hi"}]}`, wantParam: "model"},
		{name: "blank model", body: `{"model":"  ","messages":[{"role":"user","content":"hi"}]}`, wantParam: "model"},
		{name: "no messages", body: `{"model":"auto","messages":[]}`, wantParam: "messages"},
		{name: "no user turn", body: `{"model":"auto","messages":[{"role":"system","content":"be nice"}]}`, wantParam: "messages"},
		{name: "unknown role", body: `{"model":"auto","messages":[{"role":"wizard","content":"hi"}]}`, wantParam: "messages[0].role"},
		{name: "n greater than one", body: `{"model":"auto","n":2,"messages":[{"role":"user","content":"hi"}]}`, wantParam: "n"},
		{name: "malformed json", body: `{"model":`, wantParam: ""},
		{name: "empty body", body: ``, wantParam: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := post(t, h, "/v1/chat/completions", "demo-tenant-a", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}

			var body openaiapi.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body is not OpenAI-shaped: %v", err)
			}
			if body.Error.Type != openaiapi.ErrTypeInvalidRequest {
				t.Errorf("error type = %q", body.Error.Type)
			}
			if tc.wantParam != "" {
				if body.Error.Param == nil || *body.Error.Param != tc.wantParam {
					t.Errorf("param = %v, want %q", body.Error.Param, tc.wantParam)
				}
			}
		})
	}
}

func TestChatCompletionsErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantType   string
		wantHeader string
	}{
		{
			name: "guardrail block is a 400", err: &app.GuardrailError{Findings: []domain.Finding{{OWASP: "LLM01", Category: "injection"}}},
			wantStatus: http.StatusBadRequest, wantType: openaiapi.ErrTypeInvalidRequest,
		},
		{
			name:       "budget exhaustion is a 429 with Retry-After",
			err:        &app.BudgetError{Verdict: domain.BudgetVerdict{RetryAfterSec: 120, UsedTokens: 5, LimitTokens: 1}},
			wantStatus: http.StatusTooManyRequests, wantType: openaiapi.ErrTypeRateLimit, wantHeader: "Retry-After",
		},
		{
			name: "no provider available is a 503", err: app.ErrNoProviderAvailable,
			wantStatus: http.StatusServiceUnavailable, wantType: openaiapi.ErrTypeOverloaded,
		},
		{
			name: "upstream failure is a 502", err: &app.UpstreamError{},
			wantStatus: http.StatusBadGateway, wantType: openaiapi.ErrTypeAPI,
		},
		{
			name: "unknown model is a 404", err: app.ErrModelNotFound,
			wantStatus: http.StatusNotFound, wantType: openaiapi.ErrTypeInvalidRequest,
		},
		{
			name: "disallowed model is a 403", err: app.ErrModelNotAllowed,
			wantStatus: http.StatusForbidden, wantType: openaiapi.ErrTypePermission,
		},
		{
			name: "unknown policy is a 400", err: app.ErrPolicyNotFound,
			wantStatus: http.StatusBadRequest, wantType: openaiapi.ErrTypeInvalidRequest,
		},
		{
			name: "a timeout is a 504", err: context.DeadlineExceeded,
			wantStatus: http.StatusGatewayTimeout, wantType: openaiapi.ErrTypeAPI,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := chatServer(t, &stubCompleter{err: tc.err}, nil)
			rec := post(t, h, "/v1/chat/completions", "demo-tenant-a",
				`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var body openaiapi.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body is not OpenAI-shaped: %v", err)
			}
			if body.Error.Type != tc.wantType {
				t.Errorf("error type = %q, want %q", body.Error.Type, tc.wantType)
			}
			if tc.wantHeader != "" && rec.Header().Get(tc.wantHeader) == "" {
				t.Errorf("%s header is missing; SDK retry logic depends on it", tc.wantHeader)
			}
		})
	}
}

func TestPolicyOverrideIsReadFromHeaderAndMetadata(t *testing.T) {
	t.Parallel()

	t.Run("header", func(t *testing.T) {
		stub := &stubCompleter{result: okResult()}
		h := chatServer(t, stub, nil)

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer demo-tenant-a")
		req.Header.Set("X-LLMRouter-Policy", "cost_optimized")
		h.ServeHTTP(httptest.NewRecorder(), req)

		if stub.seen.Policy != "cost_optimized" {
			t.Errorf("policy = %q, want cost_optimized from the header", stub.seen.Policy)
		}
	})

	t.Run("metadata fallback for SDKs that cannot set headers", func(t *testing.T) {
		stub := &stubCompleter{result: okResult()}
		h := chatServer(t, stub, nil)

		post(t, h, "/v1/chat/completions", "demo-tenant-a",
			`{"model":"auto","metadata":{"llmrouter_policy":"latency_optimized"},
			  "messages":[{"role":"user","content":"hi"}]}`)

		if stub.seen.Policy != "latency_optimized" {
			t.Errorf("policy = %q, want latency_optimized from metadata", stub.seen.Policy)
		}
	})
}

func TestRawBodyIsPreservedForProviderPassthrough(t *testing.T) {
	t.Parallel()

	stub := &stubCompleter{result: okResult()}
	h := chatServer(t, stub, nil)

	post(t, h, "/v1/chat/completions", "demo-tenant-a",
		`{"model":"auto","messages":[{"role":"user","content":"hi"}],
		  "tools":[{"type":"function","function":{"name":"f"}}],"future_field":true}`)

	if stub.seen.Raw == nil {
		t.Fatal("the raw body must be carried so adapters can pass through unmodelled fields")
	}
	if _, ok := stub.seen.Raw["tools"]; !ok {
		t.Error("tools were lost")
	}
	if _, ok := stub.seen.Raw["future_field"]; !ok {
		t.Error("an unknown field was dropped instead of being passed through")
	}
}

func TestStopParameterAcceptsStringOrArray(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want []string
	}{
		{name: "string", body: `"STOP"`, want: []string{"STOP"}},
		{name: "array", body: `["A","B"]`, want: []string{"A", "B"}},
		{name: "absent", body: `null`, want: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &stubCompleter{result: okResult()}
			h := chatServer(t, stub, nil)

			post(t, h, "/v1/chat/completions", "demo-tenant-a",
				`{"model":"auto","stop":`+tc.body+`,"messages":[{"role":"user","content":"hi"}]}`)

			if strings.Join(stub.seen.Stop, "|") != strings.Join(tc.want, "|") {
				t.Errorf("stop = %v, want %v", stub.seen.Stop, tc.want)
			}
		})
	}
}

func TestStreamingProducesAValidSSEStream(t *testing.T) {
	t.Parallel()

	stub := &stubCompleter{result: okResult()}
	h := chatServer(t, stub, nil)

	rec := post(t, h, "/v1/chat/completions", "demo-tenant-a",
		`{"model":"auto","stream":true,"stream_options":{"include_usage":true},
		  "messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	// A reverse proxy that buffers defeats streaming entirely.
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Error("X-Accel-Buffering: no is required so nginx does not buffer the stream")
	}

	body := rec.Body.String()
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("the stream must terminate with data: [DONE]; got:\n%s", body)
	}

	var chunks []openaiapi.ChatCompletionChunk
	var sawUsage bool
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var c openaiapi.ChatCompletionChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatalf("a stream frame is not valid JSON: %v (%s)", err, payload)
		}
		if c.Object != openaiapi.ObjectChatCompletionChunk {
			t.Errorf("chunk object = %q", c.Object)
		}
		if c.Usage != nil {
			sawUsage = true
		}
		chunks = append(chunks, c)
	}

	if len(chunks) < 3 {
		t.Fatalf("expected at least role, content and terminator frames, got %d", len(chunks))
	}
	// OpenAI sends the role on the first delta and only there.
	if chunks[0].Choices[0].Delta == nil || chunks[0].Choices[0].Delta.Role != openaiapi.RoleAssistant {
		t.Error("the first frame must carry the assistant role")
	}
	last := chunks[len(chunks)-1]
	if last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "stop" {
		t.Error("the final frame must carry a finish reason")
	}
	if !sawUsage {
		t.Error("stream_options.include_usage was set, so usage must be reported")
	}
	if last.LLMRouter == nil || last.LLMRouter.Provider != "openai" {
		t.Error("the terminating frame should carry the routing metadata")
	}
}

func TestStreamingErrorsBeforeTheFirstByteAreOrdinaryHTTPErrors(t *testing.T) {
	t.Parallel()

	h := chatServer(t, &stubCompleter{err: app.ErrNoProviderAvailable}, nil)
	rec := post(t, h, "/v1/chat/completions", "demo-tenant-a",
		`{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	// Nothing has been written yet, so a real status code is still possible and is far more
	// useful to a client than a 200 containing an error frame.
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Error("a pre-stream failure must not be delivered as SSE")
	}
}

func TestEmbeddings(t *testing.T) {
	t.Parallel()

	t.Run("happy path", func(t *testing.T) {
		stub := &stubEmbedder{resp: domain.EmbedResponse{
			Model:   "text-embedding-3-small",
			Vectors: [][]float32{{0.1, 0.2}, {0.3, 0.4}},
			Usage:   domain.Usage{PromptTokens: 4},
		}}
		h := chatServer(t, &stubCompleter{result: okResult()}, stub)

		rec := post(t, h, "/v1/embeddings", "demo-tenant-a",
			`{"model":"text-embedding-3-small","input":["alpha","beta"]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}

		var resp openaiapi.EmbeddingsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if len(resp.Data) != 2 {
			t.Fatalf("got %d embeddings, want 2", len(resp.Data))
		}
		if resp.Data[1].Index != 1 {
			t.Errorf("embedding index = %d, want 1", resp.Data[1].Index)
		}
		if resp.Object != openaiapi.ObjectList {
			t.Errorf("object = %q, want list", resp.Object)
		}
		if len(stub.seen.Inputs) != 2 {
			t.Errorf("the handler passed %d inputs down", len(stub.seen.Inputs))
		}
	})

	t.Run("a single string input is accepted", func(t *testing.T) {
		stub := &stubEmbedder{resp: domain.EmbedResponse{Vectors: [][]float32{{1}}}}
		h := chatServer(t, &stubCompleter{result: okResult()}, stub)

		rec := post(t, h, "/v1/embeddings", "demo-tenant-a", `{"model":"m","input":"just one"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("token-array input is rejected clearly", func(t *testing.T) {
		h := chatServer(t, &stubCompleter{result: okResult()}, &stubEmbedder{})
		rec := post(t, h, "/v1/embeddings", "demo-tenant-a", `{"model":"m","input":[[1,2,3]]}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("not configured reports 501 rather than failing obscurely", func(t *testing.T) {
		h := chatServer(t, &stubCompleter{result: okResult()}, nil)
		rec := post(t, h, "/v1/embeddings", "demo-tenant-a", `{"model":"m","input":"x"}`)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501", rec.Code)
		}
	})
}

func TestOversizedBodyIsRejected(t *testing.T) {
	t.Parallel()

	h := chatServer(t, &stubCompleter{result: okResult()}, nil)
	huge := strings.Repeat("x", 9<<20)
	rec := post(t, h, "/v1/chat/completions", "demo-tenant-a",
		`{"model":"auto","messages":[{"role":"user","content":"`+huge+`"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an oversized body", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "limit") {
		t.Errorf("the error should explain the size limit: %s", rec.Body.String())
	}
}

func TestSSEWriterRequiresAFlusher(t *testing.T) {
	t.Parallel()

	// A ResponseWriter that cannot flush cannot stream; saying so beats silently buffering.
	w := nonFlushingWriter{httptest.NewRecorder()}
	if err := gwhttp.NewSSEWriter(w).Open(); err == nil {
		t.Error("expected ErrNoFlusher")
	}
}

type nonFlushingWriter struct{ rec *httptest.ResponseRecorder }

func (w nonFlushingWriter) Header() http.Header         { return w.rec.Header() }
func (w nonFlushingWriter) Write(b []byte) (int, error) { return w.rec.Write(b) }
func (w nonFlushingWriter) WriteHeader(code int)        { w.rec.WriteHeader(code) }

func TestFailoverEventDoesNotBreakAVanillaSDKStream(t *testing.T) {
	t.Parallel()

	stub := &stubCompleter{result: okResult(), failoverAfter: true}
	h := chatServer(t, stub, nil)

	rec := post(t, h, "/v1/chat/completions", "demo-tenant-a",
		`{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	body := rec.Body.String()
	if !strings.Contains(body, "event: failover") {
		t.Fatal("the failover event should be published for clients that want it")
	}

	// A vanilla OpenAI SDK reads only UNNAMED data frames. Every one of those must still be a
	// valid chunk, or adding failover metadata would break every existing client.
	var lastEvent string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "event: "):
			lastEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			payload := strings.TrimPrefix(line, "data: ")
			if lastEvent != "" {
				// Belongs to a named event; an OpenAI SDK skips it.
				lastEvent = ""
				continue
			}
			if payload == "[DONE]" {
				continue
			}
			var chunk openaiapi.ChatCompletionChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				t.Fatalf("an unnamed data frame is not a valid chunk: %v (%s)", err, payload)
			}
			if chunk.Object != openaiapi.ObjectChatCompletionChunk {
				t.Errorf("unnamed frame has object %q", chunk.Object)
			}
		case line == "":
			// Frame separator; the event name applies only to the frame that follows it.
		}
	}
}
