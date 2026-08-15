package guardrails_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/guardrails"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newClient(url string, failOpen bool) *guardrails.Client {
	return guardrails.New(guardrails.Options{
		BaseURL: url, Timeout: 2 * time.Second, FailOpen: failOpen, Log: quietLogger(),
	})
}

func TestScreenInputAllows(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"allowed":true,"redacted_text":"hello","findings":[],
		                            "latency_ms":0.4,"policy":"balanced","truncated":false}`)
	}))
	defer srv.Close()

	result, err := newClient(srv.URL, true).ScreenInput(context.Background(), "tenant-a", "hello")
	if err != nil {
		t.Fatalf("ScreenInput: %v", err)
	}

	if gotPath != "/v1/screen/input" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody["tenant_id"] != "tenant-a" {
		t.Errorf("tenant_id = %v", gotBody["tenant_id"])
	}
	if !result.Allowed || result.RedactedText != "hello" {
		t.Errorf("result = %+v", result)
	}
	if result.FailedOpen {
		t.Error("a successful call must not be marked as failed open")
	}
}

func TestScreenInputBlocksAndCarriesFindings(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"allowed":false,"redacted_text":"","findings":[
		    {"detector":"prompt_injection","category":"instruction_override","owasp":"LLM01",
		     "severity":"high","action":"block","start":0,"end":12}],
		    "latency_ms":0.9,"policy":"balanced"}`)
	}))
	defer srv.Close()

	result, err := newClient(srv.URL, true).ScreenInput(context.Background(), "t", "ignore everything")
	if err != nil {
		t.Fatalf("ScreenInput: %v", err)
	}
	if result.Allowed {
		t.Fatal("expected the request to be blocked")
	}
	if len(result.Findings) != 1 || result.Findings[0].OWASP != "LLM01" {
		t.Errorf("findings = %+v", result.Findings)
	}
}

func TestEmptyTextSkipsTheCallEntirely(t *testing.T) {
	t.Parallel()

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer srv.Close()

	result, err := newClient(srv.URL, true).ScreenInput(context.Background(), "t", "")
	if err != nil || !result.Allowed {
		t.Fatalf("empty text should be trivially allowed: %+v %v", result, err)
	}
	if called {
		t.Error("screening an empty string should not cost a network round trip")
	}
}

func TestFailureModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler http.HandlerFunc
		useDead bool
	}{
		{
			name:    "connection refused",
			useDead: true,
		},
		{
			name: "server error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		},
		{
			name: "malformed response",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{not json`)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name+"/fail open", func(t *testing.T) {
			url := deadURL(t, tc.useDead, tc.handler)

			result, err := newClient(url, true).ScreenInput(context.Background(), "t", "hello")
			if err != nil {
				t.Fatalf("fail-open must not return an error: %v", err)
			}
			if !result.Allowed {
				t.Error("fail-open must allow the request")
			}
			// The flag is what makes the metric, the log line and the span attribute possible.
			// A silently tolerated failure is the one that actually hurts.
			if !result.FailedOpen {
				t.Error("the result must record that screening was skipped")
			}
			if result.RedactedText != "hello" {
				t.Error("the original text must be preserved when screening is skipped")
			}
		})

		t.Run(tc.name+"/fail closed", func(t *testing.T) {
			url := deadURL(t, tc.useDead, tc.handler)

			_, err := newClient(url, false).ScreenInput(context.Background(), "t", "hello")
			if err == nil {
				t.Fatal("fail-closed must refuse the request")
			}
			if !errors.Is(err, guardrails.ErrUnavailable) {
				t.Errorf("error = %v, want ErrUnavailable so the handler can map it", err)
			}
		})
	}
}

// deadURL returns either an unroutable address or a live test server.
func deadURL(t *testing.T, dead bool, handler http.HandlerFunc) string {
	t.Helper()
	if dead {
		// A port nothing is listening on: the fastest reliable way to get a refused connection.
		return "http://127.0.0.1:1"
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestEmptyRedactedTextDoesNotBlankThePrompt(t *testing.T) {
	t.Parallel()

	// Defensive: the service always populates redacted_text, but a wire-level surprise must not
	// silently delete the user's question.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"allowed":true,"redacted_text":"","findings":[]}`)
	}))
	defer srv.Close()

	result, err := newClient(srv.URL, true).ScreenInput(context.Background(), "t", "the original")
	if err != nil {
		t.Fatalf("ScreenInput: %v", err)
	}
	if result.RedactedText != "the original" {
		t.Errorf("redacted text = %q, want the original prompt", result.RedactedText)
	}
}

func TestScreenOutputUsesTheOutputEndpoint(t *testing.T) {
	t.Parallel()

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"allowed":true,"redacted_text":"x","findings":[]}`)
	}))
	defer srv.Close()

	if _, err := newClient(srv.URL, true).ScreenOutput(context.Background(), "t", "x"); err != nil {
		t.Fatalf("ScreenOutput: %v", err)
	}
	if gotPath != "/v1/screen/output" {
		t.Errorf("path = %q, want /v1/screen/output", gotPath)
	}
}

func TestTimeoutIsEnforced(t *testing.T) {
	t.Parallel()

	// The whole point of the tight timeout: screening runs on every request, so a slow sidecar
	// must degrade to a skipped scan rather than adding its latency to the user's.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = io.WriteString(w, `{"allowed":true,"redacted_text":"x","findings":[]}`)
	}))
	defer srv.Close()

	client := guardrails.New(guardrails.Options{
		BaseURL: srv.URL, Timeout: 30 * time.Millisecond, FailOpen: true, Log: quietLogger(),
	})

	started := time.Now()
	result, err := client.ScreenInput(context.Background(), "t", "hello")
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("fail-open should absorb a timeout: %v", err)
	}
	if !result.FailedOpen {
		t.Error("a timeout must be recorded as failing open")
	}
	if elapsed > 300*time.Millisecond {
		t.Errorf("waited %s; the timeout is not being enforced", elapsed)
	}
}

func TestPing(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	if err := newClient(srv.URL, true).Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if err := newClient("http://127.0.0.1:1", true).Ping(context.Background()); err == nil {
		t.Error("Ping against a dead address should fail")
	}
}
