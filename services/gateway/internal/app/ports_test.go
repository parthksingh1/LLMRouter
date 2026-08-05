package app_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// The HTTP layer maps errors onto status codes with errors.Is. If a rich error stops unwrapping
// to its sentinel, a guardrail block silently becomes a 500. These tests pin that contract.
func TestRichErrorsUnwrapToTheirSentinels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		sentinel error
		contains string
	}{
		{
			name: "guardrail error",
			err: &app.GuardrailError{Findings: []domain.Finding{
				{Detector: "prompt_injection", Category: "instruction_override", OWASP: "LLM01"},
			}},
			sentinel: app.ErrGuardrailBlocked,
			contains: "LLM01",
		},
		{
			name: "budget error",
			err: &app.BudgetError{Verdict: domain.BudgetVerdict{
				UsedTokens: 1200, LimitTokens: 1000, RetryAfterSec: 60,
			}},
			sentinel: app.ErrBudgetExceeded,
			contains: "1200/1000",
		},
		{
			name: "upstream error",
			err: &app.UpstreamError{Attempts: []app.AttemptFailure{
				{Provider: "openai", Model: "gpt-4o", Err: errors.New("connection reset")},
				{Provider: "anthropic", Model: "claude-3-5-sonnet", Err: errors.New("429")},
			}},
			sentinel: app.ErrUpstream,
			contains: "anthropic/claude-3-5-sonnet",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if !errors.Is(tc.err, tc.sentinel) {
				t.Errorf("errors.Is(%v, %v) = false; the HTTP layer would map this to a 500", tc.err, tc.sentinel)
			}
			if !strings.Contains(tc.err.Error(), tc.contains) {
				t.Errorf("error message %q should contain %q for debugging", tc.err, tc.contains)
			}
			// Wrapping must not break the mapping either -- use cases wrap with context.
			wrapped := fmt.Errorf("serving request: %w", tc.err)
			if !errors.Is(wrapped, tc.sentinel) {
				t.Error("the sentinel was lost when the error was wrapped with context")
			}
		})
	}
}

func TestUpstreamErrorWithNoAttempts(t *testing.T) {
	t.Parallel()

	e := &app.UpstreamError{}
	if e.Error() == "" {
		t.Error("an empty UpstreamError must still render a message")
	}
	if !errors.Is(e, app.ErrUpstream) {
		t.Error("an empty UpstreamError must still unwrap to its sentinel")
	}
}

func TestErrorsAsRecoversTheConcreteType(t *testing.T) {
	t.Parallel()

	// WriteAppError reads BudgetError.Verdict to set Retry-After, so errors.As must find it
	// through a wrap.
	original := &app.BudgetError{Verdict: domain.BudgetVerdict{RetryAfterSec: 42}}
	wrapped := fmt.Errorf("dispatching: %w", original)

	var recovered *app.BudgetError
	if !errors.As(wrapped, &recovered) {
		t.Fatal("errors.As failed to recover the budget error")
	}
	if recovered.Verdict.RetryAfterSec != 42 {
		t.Errorf("RetryAfterSec = %d, want 42", recovered.Verdict.RetryAfterSec)
	}
}

func TestSystemClockAdvances(t *testing.T) {
	t.Parallel()

	var c app.Clock = app.SystemClock{}
	if c.Now().IsZero() {
		t.Error("SystemClock returned the zero time")
	}
}
