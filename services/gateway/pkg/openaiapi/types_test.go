package openaiapi_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/parthkumarsingh/llmrouter/services/gateway/pkg/openaiapi"
)

func TestMessageContentString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{
			name: "plain string content",
			body: `{"role":"user","content":"hello"}`,
			want: "hello",
		},
		{
			name: "null content, as sent with tool calls",
			body: `{"role":"assistant","content":null}`,
			want: "",
		},
		{
			name: "multimodal parts are flattened to their text",
			body: `{"role":"user","content":[{"type":"text","text":"describe "},{"type":"text","text":"this"}]}`,
			want: "describe this",
		},
		{
			name: "non-text parts contribute nothing",
			body: `{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x"}},{"type":"text","text":"caption"}]}`,
			want: "caption",
		},
		{
			name:    "an unsupported shape is an error, not a silent empty string",
			body:    `{"role":"user","content":42}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var m openaiapi.Message
			if err := json.Unmarshal([]byte(tc.body), &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, err := m.ContentString()

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ContentString: %v", err)
			}
			if got != tc.want {
				t.Errorf("ContentString() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEmbeddingsInputStrings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		want    []string
		wantErr bool
	}{
		{name: "single string", body: `{"input":"one"}`, want: []string{"one"}},
		{name: "array of strings", body: `{"input":["a","b"]}`, want: []string{"a", "b"}},
		{name: "token arrays are rejected", body: `{"input":[[1,2,3]]}`, wantErr: true},
		{name: "a number is rejected", body: `{"input":7}`, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var req openaiapi.EmbeddingsRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, err := req.InputStrings()

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("InputStrings: %v", err)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("InputStrings() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnknownRequestFieldsAreTolerated(t *testing.T) {
	t.Parallel()

	// SDK versions run ahead of gateways. A request carrying a parameter this build has never
	// heard of must still be accepted, or every OpenAI release would break the gateway.
	body := `{
	  "model":"auto",
	  "messages":[{"role":"user","content":"hi"}],
	  "some_future_parameter":{"nested":true},
	  "reasoning_effort":"high"
	}`

	var req openaiapi.ChatCompletionRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("a request with unknown fields was rejected: %v", err)
	}
	if req.Model != "auto" || len(req.Messages) != 1 {
		t.Errorf("known fields were not decoded: %+v", req)
	}
}

func TestErrorEnvelopeShape(t *testing.T) {
	t.Parallel()

	t.Run("code and param are emitted when set", func(t *testing.T) {
		b, err := json.Marshal(openaiapi.NewError("bad model", openaiapi.ErrTypeInvalidRequest, "model_not_found", "model"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		s := string(b)
		for _, want := range []string{`"message":"bad model"`, `"type":"invalid_request_error"`, `"code":"model_not_found"`, `"param":"model"`} {
			if !strings.Contains(s, want) {
				t.Errorf("error envelope %s is missing %s", s, want)
			}
		}
	})

	t.Run("code and param are null when unset", func(t *testing.T) {
		// SDKs read error.param and error.code unconditionally; omitting the keys entirely
		// makes some clients raise a KeyError, so they must serialise as null.
		b, err := json.Marshal(openaiapi.NewError("boom", openaiapi.ErrTypeAPI, "", ""))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		s := string(b)
		if !strings.Contains(s, `"param":null`) || !strings.Contains(s, `"code":null`) {
			t.Errorf("expected null param and code in %s", s)
		}
	})
}

func TestChatCompletionResponseRoundTrip(t *testing.T) {
	t.Parallel()

	stop := openaiapi.FinishStop
	resp := openaiapi.ChatCompletionResponse{
		ID:      "chatcmpl-1",
		Object:  openaiapi.ObjectChatCompletion,
		Created: 1700000000,
		Model:   "gpt-4o-mini",
		Choices: []openaiapi.Choice{{
			Index:        0,
			Message:      &openaiapi.Message{Role: openaiapi.RoleAssistant, Content: "hi"},
			FinishReason: &stop,
		}},
		Usage:     &openaiapi.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
		LLMRouter: &openaiapi.RoutingMeta{Provider: "openai", ResolvedModel: "gpt-4o-mini", Cached: true},
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// The routing extension must be additive: a strict OpenAI decoder ignores unknown keys, so
	// its presence cannot break an SDK.
	var decoded openaiapi.ChatCompletionResponse
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Choices[0].Message.Content != "hi" {
		t.Errorf("content did not survive the round trip: %+v", decoded.Choices[0])
	}
	if decoded.LLMRouter == nil || decoded.LLMRouter.Provider != "openai" {
		t.Error("the llmrouter extension block did not survive the round trip")
	}
	if !strings.Contains(string(b), `"finish_reason":"stop"`) {
		t.Errorf("finish_reason must be present on the wire: %s", b)
	}
}

func TestStreamChunkOmitsUsageUnlessRequested(t *testing.T) {
	t.Parallel()

	chunk := openaiapi.ChatCompletionChunk{
		ID:      "chatcmpl-1",
		Object:  openaiapi.ObjectChatCompletionChunk,
		Model:   "gpt-4o-mini",
		Choices: []openaiapi.Choice{{Index: 0, Delta: &openaiapi.Message{Content: "to"}}},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"usage"`) {
		t.Errorf("usage must be omitted from ordinary deltas: %s", b)
	}
	// finish_reason has no omitempty: OpenAI sends it as null on every intermediate chunk, and
	// some clients depend on the key being present.
	if !strings.Contains(string(b), `"finish_reason":null`) {
		t.Errorf("finish_reason should serialise as null on intermediate chunks: %s", b)
	}
}
