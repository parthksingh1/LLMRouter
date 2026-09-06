package domain_test

import (
	"testing"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

func TestChatRequestPromptExtraction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		messages   []domain.Message
		wantSystem string
		wantUser   string
		wantTurns  int
	}{
		{
			name:     "empty conversation",
			messages: nil,
		},
		{
			name: "single user turn",
			messages: []domain.Message{
				{Role: domain.RoleUser, Content: "what is the capital of France"},
			},
			wantUser:  "what is the capital of France",
			wantTurns: 1,
		},
		{
			name: "system prompt is separated from the user turn",
			messages: []domain.Message{
				{Role: domain.RoleSystem, Content: "You are terse."},
				{Role: domain.RoleUser, Content: "hello"},
			},
			wantSystem: "You are terse.",
			wantUser:   "hello",
			wantTurns:  1,
		},
		{
			name: "multiple system messages are joined",
			messages: []domain.Message{
				{Role: domain.RoleSystem, Content: "You are terse."},
				{Role: domain.RoleSystem, Content: "Answer in English."},
				{Role: domain.RoleUser, Content: "hi"},
			},
			wantSystem: "You are terse.\nAnswer in English.",
			wantUser:   "hi",
			wantTurns:  1,
		},
		{
			name: "the last user turn wins in a multi-turn conversation",
			messages: []domain.Message{
				{Role: domain.RoleUser, Content: "first"},
				{Role: domain.RoleAssistant, Content: "ok"},
				{Role: domain.RoleUser, Content: "second"},
			},
			wantUser:  "second",
			wantTurns: 3,
		},
		{
			name: "a trailing assistant prefix does not become the user prompt",
			messages: []domain.Message{
				{Role: domain.RoleUser, Content: "explain quicksort"},
				{Role: domain.RoleAssistant, Content: "Quicksort partitions"},
			},
			wantUser:  "explain quicksort",
			wantTurns: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := domain.ChatRequest{Messages: tc.messages}

			if got := req.SystemPrompt(); got != tc.wantSystem {
				t.Errorf("SystemPrompt() = %q, want %q", got, tc.wantSystem)
			}
			if got := req.UserPrompt(); got != tc.wantUser {
				t.Errorf("UserPrompt() = %q, want %q", got, tc.wantUser)
			}
			if got := req.TurnCount(); got != tc.wantTurns {
				t.Errorf("TurnCount() = %d, want %d", got, tc.wantTurns)
			}
		})
	}
}

func TestUsageArithmetic(t *testing.T) {
	t.Parallel()

	a := domain.Usage{PromptTokens: 100, CompletionTokens: 50}
	b := domain.Usage{PromptTokens: 7, CompletionTokens: 3}

	if got := a.TotalTokens(); got != 150 {
		t.Errorf("TotalTokens() = %d, want 150", got)
	}
	sum := a.Add(b)
	if sum.PromptTokens != 107 || sum.CompletionTokens != 53 {
		t.Errorf("Add() = %+v, want {107 53}", sum)
	}
	// Add must not mutate its receiver: usage is summed across failover attempts, and an
	// in-place mutation there would double-count tokens for billing.
	if a.PromptTokens != 100 || a.CompletionTokens != 50 {
		t.Errorf("Add mutated the receiver: %+v", a)
	}
}

func TestModelDescriptorCostUSD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		model domain.ModelDescriptor
		usage domain.Usage
		want  float64
	}{
		{
			name:  "zero usage costs nothing",
			model: domain.ModelDescriptor{PriceInPerM: 2.5, PriceOutPerM: 10},
		},
		{
			name:  "split input and output pricing",
			model: domain.ModelDescriptor{PriceInPerM: 2.5, PriceOutPerM: 10},
			usage: domain.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000},
			want:  12.5,
		},
		{
			name:  "flat pricing",
			model: domain.ModelDescriptor{PriceInPerM: 0.88, PriceOutPerM: 0.88},
			usage: domain.Usage{PromptTokens: 500_000, CompletionTokens: 500_000},
			want:  0.88,
		},
		{
			name:  "small realistic call",
			model: domain.ModelDescriptor{PriceInPerM: 0.15, PriceOutPerM: 0.60},
			usage: domain.Usage{PromptTokens: 350, CompletionTokens: 280},
			want:  0.0000525 + 0.000168,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.model.CostUSD(tc.usage)
			if diff := got - tc.want; diff > 1e-12 || diff < -1e-12 {
				t.Errorf("CostUSD() = %.12f, want %.12f", got, tc.want)
			}
		})
	}
}

func TestTenantModelAllowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		allowed []string
		model   string
		want    bool
	}{
		{name: "empty allowlist permits everything", allowed: nil, model: "gpt-4o", want: true},
		{name: "wildcard permits everything", allowed: []string{"*"}, model: "gpt-4o", want: true},
		{name: "explicit match", allowed: []string{"gpt-4o-mini", "gpt-4o"}, model: "gpt-4o", want: true},
		{name: "no match is refused", allowed: []string{"gpt-4o-mini"}, model: "gpt-4o", want: false},
		{name: "matching is exact, not prefix", allowed: []string{"gpt-4o"}, model: "gpt-4o-mini", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tenant := domain.Tenant{AllowedModels: tc.allowed}
			if got := tenant.ModelAllowed(tc.model); got != tc.want {
				t.Errorf("ModelAllowed(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

func TestCacheKeyNamespaceIsolation(t *testing.T) {
	t.Parallel()

	base := domain.NewCacheKey("tenant-a", "gpt-4", "You are helpful.")

	tests := []struct {
		name         string
		key          domain.CacheKey
		wantDistinct bool
	}{
		{
			name:         "identical inputs share a namespace",
			key:          domain.NewCacheKey("tenant-a", "gpt-4", "You are helpful."),
			wantDistinct: false,
		},
		{
			name:         "a different tenant must not share a namespace",
			key:          domain.NewCacheKey("tenant-b", "gpt-4", "You are helpful."),
			wantDistinct: true,
		},
		{
			name:         "a different model family must not share a namespace",
			key:          domain.NewCacheKey("tenant-a", "claude-3", "You are helpful."),
			wantDistinct: true,
		},
		{
			name:         "a different system prompt must not share a namespace",
			key:          domain.NewCacheKey("tenant-a", "gpt-4", "You are terse."),
			wantDistinct: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			same := tc.key.Namespace() == base.Namespace()
			if same == tc.wantDistinct {
				t.Errorf("Namespace() collision check failed: got same=%v, wantDistinct=%v (%q vs %q)",
					same, tc.wantDistinct, tc.key.Namespace(), base.Namespace())
			}
		})
	}
}

func TestNormalizePrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "already normal", in: "hello world", want: "hello world"},
		{name: "case is folded", in: "Hello World", want: "hello world"},
		{name: "runs of whitespace collapse", in: "hello   \t\n world", want: "hello world"},
		{name: "leading and trailing space is dropped", in: "  hello  ", want: "hello"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := domain.NormalizePrompt(tc.in); got != tc.want {
				t.Errorf("NormalizePrompt(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHashTextIsStableAndDistinct(t *testing.T) {
	t.Parallel()

	// Stability across calls is what makes fixtures and cache keys reproducible. The two calls
	// are bound to variables first: comparing the expression with itself is indistinguishable
	// from a typo, to a reader and to staticcheck alike.
	first, second := domain.HashText("abc"), domain.HashText("abc")
	if first != second {
		t.Fatal("HashText is not stable across calls")
	}
	if domain.HashText("abc") == domain.HashText("abd") {
		t.Fatal("HashText collided on distinct inputs")
	}
	if len(domain.HashText("")) != 64 {
		t.Fatalf("HashText should return 64 hex characters, got %d", len(domain.HashText("")))
	}
}
