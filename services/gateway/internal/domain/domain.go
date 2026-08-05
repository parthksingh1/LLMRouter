// Package domain holds the entities and value objects of the gateway.
//
// It is the innermost layer of the hexagon: it imports nothing from the rest of the service and
// knows nothing about HTTP, providers, Redis or OpenTelemetry. Everything else may depend on it.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Role values used inside the gateway. They intentionally mirror the OpenAI wire roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message is a single conversation turn, already flattened to text.
type Message struct {
	Role    string
	Name    string
	Content string
}

// ChatRequest is a normalised chat completion request.
//
// RequestedModel is what the caller asked for and may be a virtual model such as "auto".
// The router resolves it to a concrete provider and model.
type ChatRequest struct {
	RequestID      string
	TenantID       string
	RequestedModel string
	Policy         string // explicit policy override, empty means "decide normally"
	Messages       []Message
	Stream         bool
	IncludeUsage   bool
	MaxTokens      *int
	Temperature    *float64
	TopP           *float64
	Stop           []string
	Seed           *int
	User           string

	// Raw carries the original wire body so provider adapters can pass through fields the
	// domain does not model (tools, response_format, logprobs) without losing fidelity.
	Raw map[string]any
}

// SystemPrompt returns the concatenated system messages, which form part of the cache namespace.
func (r ChatRequest) SystemPrompt() string {
	var b strings.Builder
	for _, m := range r.Messages {
		if m.Role == RoleSystem {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(m.Content)
		}
	}
	return b.String()
}

// UserPrompt returns the text of the last user message, which is what gets embedded for the
// semantic cache and classified for difficulty.
func (r ChatRequest) UserPrompt() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == RoleUser {
			return r.Messages[i].Content
		}
	}
	return ""
}

// TurnCount is the number of non-system turns, used by the difficulty classifier.
func (r ChatRequest) TurnCount() int {
	n := 0
	for _, m := range r.Messages {
		if m.Role != RoleSystem {
			n++
		}
	}
	return n
}

// PromptChars is the total character length across all messages.
func (r ChatRequest) PromptChars() int {
	n := 0
	for _, m := range r.Messages {
		n += len(m.Content)
	}
	return n
}

// ChatResponse is a completed, non-streamed generation.
type ChatResponse struct {
	ID           string
	Model        string
	Provider     string
	Created      time.Time
	Content      string
	FinishReason string
	Usage        Usage
	Raw          map[string]any
}

// Usage is token accounting for one logical request. When a request fails over mid-stream the
// usage of every attempt is summed into a single Usage so billing stays whole.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// TotalTokens is the sum of prompt and completion tokens.
func (u Usage) TotalTokens() int { return u.PromptTokens + u.CompletionTokens }

// Add accumulates another Usage into this one.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		PromptTokens:     u.PromptTokens + o.PromptTokens,
		CompletionTokens: u.CompletionTokens + o.CompletionTokens,
	}
}

// StreamChunk is one increment of a streamed generation.
//
// Exactly one of Delta, FinishReason, Usage or Err is meaningful per chunk. A chunk carrying a
// non-nil Err is terminal for that provider attempt and is what trips mid-stream failover.
type StreamChunk struct {
	Delta        string
	FinishReason *string
	Usage        *Usage
	Err          error
}

// EmbedRequest asks a provider for embeddings.
type EmbedRequest struct {
	RequestID string
	TenantID  string
	Model     string
	Inputs    []string
}

// EmbedResponse carries embedding vectors.
type EmbedResponse struct {
	Model    string
	Provider string
	Vectors  [][]float32
	Usage    Usage
}

// ModelDescriptor describes one concrete model offered by a provider.
type ModelDescriptor struct {
	ID            string
	Provider      string
	Family        string
	Tier          string
	Quality       float64 // measured pass rate on benchmarks/eval/dataset.jsonl
	ContextWindow int
	PriceInPerM   float64
	PriceOutPerM  float64
}

// CostUSD prices a Usage against this model.
func (m ModelDescriptor) CostUSD(u Usage) float64 {
	const perMillion = 1_000_000.0
	return float64(u.PromptTokens)/perMillion*m.PriceInPerM +
		float64(u.CompletionTokens)/perMillion*m.PriceOutPerM
}

// BlendedCostPerCall estimates the cost of a call with the given token shape. The router uses it
// to compare models without having to guess a single "price" for a model with split pricing.
func (m ModelDescriptor) BlendedCostPerCall(promptTokens, completionTokens int) float64 {
	return m.CostUSD(Usage{PromptTokens: promptTokens, CompletionTokens: completionTokens})
}

// Difficulty is the output of the prompt-difficulty classifier.
type Difficulty string

// Difficulty buckets.
const (
	DifficultyEasy   Difficulty = "easy"
	DifficultyMedium Difficulty = "medium"
	DifficultyHard   Difficulty = "hard"
)

// RouteDecision is the router's choice for one attempt.
type RouteDecision struct {
	Provider   string
	Model      ModelDescriptor
	Policy     string
	Difficulty Difficulty
	Reason     string  // human-readable, ends up on the span and in logs
	Score      float64 // strategy-specific ranking score, for debugging
	Attempt    int     // 0 for the first attempt, 1+ for fallbacks
}

// Tenant is an authenticated caller.
type Tenant struct {
	ID               string
	Name             string
	APIKeyHash       string
	DefaultPolicy    string
	DailyTokenBudget int64
	MonthlyUSDBudget float64
	AllowedModels    []string
	WarnThreshold    float64
}

// ModelAllowed reports whether a concrete model id is permitted for this tenant.
// The wildcard "*" allows everything.
func (t Tenant) ModelAllowed(modelID string) bool {
	if len(t.AllowedModels) == 0 {
		return true
	}
	for _, m := range t.AllowedModels {
		if m == "*" || m == modelID {
			return true
		}
	}
	return false
}

// CacheKey is the semantic-cache namespace tuple.
//
// Namespacing by tenant is what prevents one tenant's response being served to another. Model
// family is included because a cached answer from a small model should not satisfy a request
// that was routed to a frontier model. The system prompt is included because it changes the
// meaning of an identical user turn.
type CacheKey struct {
	TenantID         string
	ModelFamily      string
	SystemPromptHash string
}

// NewCacheKey builds a CacheKey, hashing the system prompt.
func NewCacheKey(tenantID, modelFamily, systemPrompt string) CacheKey {
	return CacheKey{
		TenantID:         tenantID,
		ModelFamily:      modelFamily,
		SystemPromptHash: HashText(systemPrompt),
	}
}

// Namespace renders the key as a stable string for use as a Redis key prefix and a Qdrant filter.
func (k CacheKey) Namespace() string {
	return k.TenantID + "|" + k.ModelFamily + "|" + k.SystemPromptHash
}

// CachedResponse is a semantic-cache hit.
type CachedResponse struct {
	Content    string
	Model      string
	Provider   string
	Usage      Usage
	Similarity float64
	StoredAt   time.Time
}

// ScreenResult is the verdict from the guardrails sidecar.
type ScreenResult struct {
	Allowed      bool
	RedactedText string
	Findings     []Finding
	LatencyMS    float64
	// FailedOpen is true when the sidecar was unreachable and policy allowed the request
	// through anyway. It is surfaced on the span and as a metric.
	FailedOpen bool
}

// Finding is one guardrail detection.
type Finding struct {
	Detector string
	Category string
	OWASP    string
	Severity string
	Start    int
	End      int
	Action   string
}

// BudgetVerdict is the outcome of a budget reservation.
type BudgetVerdict struct {
	Allowed       bool
	Warn          bool // crossed the warn threshold but is still allowed
	UsedTokens    int64
	LimitTokens   int64
	RetryAfterSec int
	Reason        string
}

// RequestEvent is the normalised analytics record emitted once per logical request.
// It is the contract with ClickHouse; see infra/clickhouse/init.sql.
type RequestEvent struct {
	TS                time.Time
	RequestID         string
	TenantID          string
	Policy            string
	Model             string
	Provider          string
	PromptTokens      int
	CompletionTokens  int
	Cached            bool
	CacheSimilarity   float64
	GuardrailBlocked  bool
	GuardrailFindings int
	FailoverCount     int
	StatusCode        int
	LatencyMS         int
	CostUSD           float64
}

// HashText returns the hex sha256 of s. Used for cache namespacing and prompt fingerprints.
func HashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// NormalizePrompt lowercases and collapses whitespace so that trivially different prompts share
// an exact hash. This is the "known negative" guard on top of embedding similarity: two prompts
// with the same normalised hash are certainly the same question.
func NormalizePrompt(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
