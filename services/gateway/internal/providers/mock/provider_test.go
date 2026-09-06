package mock_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/providers/mock"
)

// fastSpec builds a provider catalogue entry whose simulated latency is negligible, so unit
// tests do not pay the demo's realistic timings.
func fastSpec(name string) config.ProviderSpec {
	return config.ProviderSpec{
		Name:               name,
		PrefixContinuation: config.PrefixNative,
		Latency:            config.LatencySpec{TTFBP50MS: 10, TTFBP95MS: 20, TTFBP99MS: 40, TokensPerSec: 1000},
		Models: []config.ModelSpec{
			{ID: name + "-big", Family: name, Tier: "frontier", Quality: 0.95, PriceInPerM: 3, PriceOutPerM: 15},
			{ID: name + "-small", Family: name, Tier: "efficient", Quality: 0.86, PriceInPerM: 0.2, PriceOutPerM: 0.6},
		},
	}
}

func newProvider(t *testing.T, opts mock.Options) *mock.Provider {
	t.Helper()
	if opts.Spec.Name == "" {
		opts.Spec = fastSpec("openai")
	}
	if opts.Speed == 0 {
		opts.Speed = 500 // 500x faster than real time
	}
	return mock.New(opts)
}

func userRequest(model, prompt string) domain.ChatRequest {
	return domain.ChatRequest{
		RequestID:      "req-test",
		TenantID:       "tenant-a",
		RequestedModel: model,
		Messages:       []domain.Message{{Role: domain.RoleUser, Content: prompt}},
	}
}

func TestChatIsDeterministic(t *testing.T) {
	t.Parallel()

	p := newProvider(t, mock.Options{})
	ctx := context.Background()

	first, err := p.Chat(ctx, userRequest("openai-big", "explain consistent hashing"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	second, err := p.Chat(ctx, userRequest("openai-big", "explain consistent hashing"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if first.Content != second.Content {
		t.Error("the same prompt produced different text; benchmark numbers would not reproduce")
	}
	if first.Usage != second.Usage {
		t.Errorf("usage differed between identical calls: %+v vs %+v", first.Usage, second.Usage)
	}
	if first.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", first.Provider)
	}
	if first.Usage.CompletionTokens == 0 {
		t.Error("completion tokens must be counted, or cost attribution is meaningless")
	}
}

func TestTierChangesTheAnswer(t *testing.T) {
	t.Parallel()

	p := newProvider(t, mock.Options{})
	ctx := context.Background()
	const prompt = "derive the complexity of merge sort"

	big, err := p.Chat(ctx, userRequest("openai-big", prompt))
	if err != nil {
		t.Fatalf("Chat(frontier): %v", err)
	}
	small, err := p.Chat(ctx, userRequest("openai-small", prompt))
	if err != nil {
		t.Fatalf("Chat(efficient): %v", err)
	}

	if big.Content == small.Content {
		t.Error("frontier and efficient tiers must differ, or quality drift can never be measured")
	}
	if len(big.Content) <= len(small.Content) {
		t.Error("the frontier answer should be the longer one")
	}
}

func TestUnknownModelFallsBackRatherThanFailing(t *testing.T) {
	t.Parallel()

	p := newProvider(t, mock.Options{})
	// An arbitrary curl in the demo must not 500 because of a model typo.
	resp, err := p.Chat(context.Background(), userRequest("no-such-model", "hello"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content == "" {
		t.Error("expected a synthesised answer")
	}
}

func TestChatStreamConcatenatesToTheWholeAnswer(t *testing.T) {
	t.Parallel()

	p := newProvider(t, mock.Options{})
	ctx := context.Background()
	req := userRequest("openai-big", "what is a bloom filter")

	whole, err := p.Chat(ctx, req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	ch, err := p.ChatStream(ctx, req)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var sb strings.Builder
	var finish string
	var usage *domain.Usage
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("unexpected stream error: %v", c.Err)
		}
		sb.WriteString(c.Delta)
		if c.FinishReason != nil {
			finish = *c.FinishReason
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}

	if sb.String() != whole.Content {
		t.Errorf("streamed text does not reassemble the non-streamed answer:\n got %q\nwant %q",
			sb.String(), whole.Content)
	}
	if finish != "stop" {
		t.Errorf("finish reason = %q, want stop", finish)
	}
	if usage == nil {
		t.Fatal("a completed stream must report usage, or streamed requests cannot be billed")
	}
	if usage.CompletionTokens == 0 {
		t.Error("streamed usage reported zero completion tokens")
	}
}

func TestFailAfterTripsAndMarksTheProviderUnhealthy(t *testing.T) {
	t.Parallel()

	p := newProvider(t, mock.Options{FailAfter: 2})
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := p.Chat(ctx, userRequest("openai-big", "hello")); err != nil {
			t.Fatalf("request %d should have succeeded: %v", i+1, err)
		}
	}

	_, err := p.Chat(ctx, userRequest("openai-big", "hello"))
	if !errors.Is(err, mock.ErrUpstreamRefused) {
		t.Fatalf("request 3 error = %v, want ErrUpstreamRefused", err)
	}
	// Health must agree with behaviour, so the registry stops routing here rather than the
	// router rediscovering the outage on every request.
	if err := p.HealthCheck(ctx); !errors.Is(err, mock.ErrUpstreamRefused) {
		t.Errorf("HealthCheck = %v, want ErrUpstreamRefused", err)
	}

	p.Reset()
	if err := p.HealthCheck(ctx); err != nil {
		t.Errorf("after Reset, HealthCheck = %v, want nil", err)
	}
}

func TestMidstreamFailureIsSignalledNotSilent(t *testing.T) {
	t.Parallel()

	p := newProvider(t, mock.Options{FailMidstream: true, MidstreamFailAfterTokens: 5})

	ch, err := p.ChatStream(context.Background(), userRequest("openai-big", "explain raft consensus"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var got int
	var streamErr error
	for c := range ch {
		if c.Err != nil {
			streamErr = c.Err
			continue
		}
		if c.Delta != "" {
			got++
		}
	}

	if streamErr == nil {
		t.Fatal("a broken stream must deliver an error chunk, not just close the channel")
	}
	if !errors.Is(streamErr, mock.ErrStreamReset) {
		t.Errorf("stream error = %v, want ErrStreamReset", streamErr)
	}
	if got != 5 {
		t.Errorf("emitted %d tokens before the break, want 5", got)
	}
}

func TestStreamStopsWhenTheContextIsCancelled(t *testing.T) {
	t.Parallel()

	// Real-time speed so the stream is still running when we cancel it.
	p := newProvider(t, mock.Options{Speed: 1})

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.ChatStream(ctx, userRequest("openai-big", "write a long essay about databases"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	// Consume one chunk, then abandon the request the way a disconnecting client would.
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
		t.Fatal("the stream goroutine did not exit after cancellation; it leaks")
	}
}

func TestPrefixContinuationEmitsOnlyTheRemainder(t *testing.T) {
	t.Parallel()

	p := newProvider(t, mock.Options{})
	ctx := context.Background()
	const prompt = "summarise the CAP theorem"

	full, err := p.Chat(ctx, userRequest("openai-big", prompt))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// Take the first few words as though a previous provider had already streamed them.
	words := strings.SplitAfter(full.Content, " ")
	if len(words) < 5 {
		t.Fatalf("fixture answer too short to test continuation: %q", full.Content)
	}
	prefix := strings.Join(words[:4], "")

	req := userRequest("openai-big", prompt)
	req.Messages = append(req.Messages, domain.Message{Role: domain.RoleAssistant, Content: prefix})

	ch, err := p.ChatStream(ctx, req)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var sb strings.Builder
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("unexpected stream error: %v", c.Err)
		}
		sb.WriteString(c.Delta)
	}

	if strings.HasPrefix(sb.String(), prefix) {
		t.Error("the continuation repeated the prefix; tokens would be double-billed and the " +
			"client would see duplicated text")
	}
	if prefix+sb.String() != full.Content {
		t.Errorf("prefix + continuation does not reconstruct the answer:\n got %q\nwant %q",
			prefix+sb.String(), full.Content)
	}
}

func TestLoadFixtures(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "responses.jsonl")
	body := strings.Join([]string{
		`{"prompt":"what is 2+2","prompt_tokens":5,"difficulty":"easy","responses":{` +
			`"frontier":{"text":"4","completion_tokens":1,"correct":true},` +
			`"efficient":{"text":"4","completion_tokens":1,"correct":true}}}`,
		``,
		`{"prompt":"prove fermat","prompt_tokens":9,"difficulty":"hard","responses":{` +
			`"frontier":{"text":"a long proof","completion_tokens":3,"correct":true},` +
			`"efficient":{"text":"unsure","completion_tokens":2,"correct":false}}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing fixtures: %v", err)
	}

	fs, err := mock.LoadFixtures(path)
	if err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	if fs.Len() != 2 {
		t.Fatalf("loaded %d fixtures, want 2", fs.Len())
	}

	v, _, seeded := fs.Lookup("what is 2+2", mock.TierFrontier)
	if !seeded {
		t.Error("expected a seeded fixture")
	}
	if v.Text != "4" {
		t.Errorf("fixture text = %q, want 4", v.Text)
	}

	// Normalisation means capitalisation and spacing do not miss the fixture.
	if _, _, seeded := fs.Lookup("  What  Is  2+2 ", mock.TierFrontier); !seeded {
		t.Error("lookup should normalise whitespace and case before hashing")
	}

	// The efficient tier of a hard question is deliberately wrong; that is what the eval
	// harness measures as quality drift.
	hard, _, _ := fs.Lookup("prove fermat", mock.TierEfficient)
	if hard.Correct {
		t.Error("the efficient variant of the hard fixture should be marked incorrect")
	}

	if _, _, seeded := fs.Lookup("never seen this", mock.TierFrontier); seeded {
		t.Error("an unseeded prompt must be reported as synthesised")
	}
}

func TestLoadFixturesMissingFileIsNotFatal(t *testing.T) {
	t.Parallel()

	fs, err := mock.LoadFixtures(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("a missing fixture file should not be an error: %v", err)
	}
	if fs.Len() != 0 {
		t.Errorf("expected an empty fixture set, got %d", fs.Len())
	}
}

func TestLoadFixturesRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(path, []byte("{not json}\n"), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if _, err := mock.LoadFixtures(path); err == nil {
		t.Error("expected an error for malformed fixture JSON")
	}
}

func TestEstimateTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want int
	}{
		{in: "", want: 0},
		{in: "a", want: 1},
		{in: "abcd", want: 1},
		{in: "abcde", want: 2},
		{in: strings.Repeat("x", 400), want: 100},
	}
	for _, tc := range tests {
		if got := mock.EstimateTokens(tc.in); got != tc.want {
			t.Errorf("EstimateTokens(len %d) = %d, want %d", len(tc.in), got, tc.want)
		}
	}
}

func TestHashEmbedIsDeterministicAndNormalised(t *testing.T) {
	t.Parallel()

	const dim = 384
	a := mock.HashEmbed("how do I reset my password", dim)
	b := mock.HashEmbed("how do I reset my password", dim)

	if len(a) != dim {
		t.Fatalf("vector length = %d, want %d", len(a), dim)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("HashEmbed is not deterministic")
		}
	}

	var norm float64
	for _, x := range a {
		norm += float64(x) * float64(x)
	}
	if norm < 0.99 || norm > 1.01 {
		t.Errorf("vector is not unit-norm: |v|^2 = %v", norm)
	}

	// A paraphrase should be closer than an unrelated sentence, or the offline cache path
	// would behave nothing like the real embedding model.
	para := mock.HashEmbed("how can I reset my password", dim)
	other := mock.HashEmbed("what is the capital of Peru", dim)
	if cosine(a, para) <= cosine(a, other) {
		t.Errorf("paraphrase similarity %.3f should exceed unrelated similarity %.3f",
			cosine(a, para), cosine(a, other))
	}
}

func TestEmbedReturnsOneVectorPerInput(t *testing.T) {
	t.Parallel()

	p := newProvider(t, mock.Options{})
	resp, err := p.Embed(context.Background(), domain.EmbedRequest{
		Model:  "text-embedding-3-small",
		Inputs: []string{"alpha", "beta", "gamma"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Vectors) != 3 {
		t.Fatalf("got %d vectors, want 3", len(resp.Vectors))
	}
	if resp.Usage.PromptTokens == 0 {
		t.Error("embedding usage should be counted")
	}
}

func cosine(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}
