package mock

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// Injection failures the mock can produce. They are distinct sentinels so tests and the
// failover benchmark can assert on which kind of break happened.
var (
	// ErrUpstreamRefused simulates a provider returning 5xx before any token is produced.
	ErrUpstreamRefused = errors.New("mock: upstream refused the request")
	// ErrStreamReset simulates the upstream connection dying mid-generation. This is the
	// condition mid-stream failover exists to survive.
	ErrStreamReset = errors.New("mock: upstream stream reset mid-generation")
	// ErrStreamStalled simulates an upstream that accepts the request and then goes quiet.
	ErrStreamStalled = errors.New("mock: upstream stalled")
)

// Options configures one mock provider instance.
type Options struct {
	Spec     config.ProviderSpec
	Fixtures *FixtureSet
	Seed     int64
	// Speed divides simulated durations. 1 means real time; tests use a large value.
	Speed float64

	// FailAfter makes the provider fail every request after N successes. Zero disables it.
	FailAfter int
	// FailMidstream breaks every stream after MidstreamFailAfterTokens tokens.
	FailMidstream bool
	// MidstreamFailRate breaks a stream with this probability, independent of FailMidstream.
	MidstreamFailRate float64
	// MidstreamFailAfterTokens is where in the generation the break happens.
	MidstreamFailAfterTokens int
	// StallProbability makes a stream go quiet instead of erroring, exercising the stall
	// timeout path rather than the reset path.
	StallProbability float64
}

// Provider is a deterministic offline stand-in for a real vendor. It satisfies app.Provider.
type Provider struct {
	name     string
	spec     config.ProviderSpec
	models   []domain.ModelDescriptor
	fixtures *FixtureSet
	latency  *LatencyModel
	opts     Options

	// served counts completed requests, for FailAfter.
	served atomic.Int64
	// unhealthy is set once FailAfter has tripped, so health checks agree with reality and the
	// registry marks the provider down rather than the router discovering it per request.
	unhealthy atomic.Bool
}

// New builds a mock provider from a catalogue entry.
func New(opts Options) *Provider {
	models := make([]domain.ModelDescriptor, 0, len(opts.Spec.Models))
	for _, m := range opts.Spec.Models {
		models = append(models, m.Descriptor(opts.Spec.Name))
	}
	if opts.MidstreamFailAfterTokens <= 0 {
		opts.MidstreamFailAfterTokens = 24
	}
	if opts.Fixtures == nil {
		opts.Fixtures = &FixtureSet{byKey: map[string]Fixture{}}
	}
	// Derive a per-provider seed so two providers with the same config still diverge.
	seed := opts.Seed + int64(len(opts.Spec.Name))*7919
	return &Provider{
		name:     opts.Spec.Name,
		spec:     opts.Spec,
		models:   models,
		fixtures: opts.Fixtures,
		latency:  NewLatencyModel(opts.Spec.Latency, seed, opts.Speed),
		opts:     opts,
	}
}

// Name is the provider's catalogue name.
func (p *Provider) Name() string { return p.name }

// Models lists the models this provider offers.
func (p *Provider) Models() []domain.ModelDescriptor {
	out := make([]domain.ModelDescriptor, len(p.models))
	copy(out, p.models)
	return out
}

// HealthCheck reports the injected health state. It never touches the network.
func (p *Provider) HealthCheck(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if p.unhealthy.Load() {
		return fmt.Errorf("%s: %w", p.name, ErrUpstreamRefused)
	}
	return nil
}

// Chat produces a complete response, sleeping for the simulated generation time.
func (p *Provider) Chat(ctx context.Context, req domain.ChatRequest) (domain.ChatResponse, error) {
	if err := p.admit(); err != nil {
		return domain.ChatResponse{}, err
	}

	variant, prompt, model := p.resolve(req)

	if !sleepCtx(ctx.Done(), p.latency.TTFB()) {
		return domain.ChatResponse{}, ctx.Err()
	}
	if !sleepCtx(ctx.Done(), p.latency.GenerationTime(variant.CompletionTokens)) {
		return domain.ChatResponse{}, ctx.Err()
	}

	return domain.ChatResponse{
		ID:           "chatcmpl-mock-" + domain.HashText(prompt)[:24],
		Model:        model.ID,
		Provider:     p.name,
		Created:      time.Now(),
		Content:      variant.Text,
		FinishReason: "stop",
		Usage: domain.Usage{
			PromptTokens:     p.promptTokens(req),
			CompletionTokens: variant.CompletionTokens,
		},
	}, nil
}

// ChatStream emits the response token by token at the provider's configured rate, honouring any
// injected mid-stream failure.
//
// The channel is always closed exactly once. A break is signalled as a final chunk carrying Err,
// never as a silent close, so the caller can tell "the model finished" from "the pipe died".
func (p *Provider) ChatStream(ctx context.Context, req domain.ChatRequest) (<-chan domain.StreamChunk, error) {
	if err := p.admit(); err != nil {
		return nil, err
	}

	variant, _, _ := p.resolve(req)

	// A prefix continuation asks the provider to carry on from text already generated by a
	// previous provider. We emit only the remainder, which is what makes token accounting
	// across a failover add up rather than double-count.
	remainder := variant.Text
	if prefix := assistantPrefix(req); prefix != "" {
		remainder = continuationOf(variant.Text, prefix)
	}

	tokens := tokenise(remainder)
	promptTokens := p.promptTokens(req)

	breakAt := -1
	if p.opts.FailMidstream || p.latency.Float() < p.opts.MidstreamFailRate {
		breakAt = p.opts.MidstreamFailAfterTokens
		if breakAt >= len(tokens) {
			// Break just before the end rather than not at all, so injection is reliable even
			// for short answers.
			breakAt = len(tokens) - 1
		}
	}
	stall := p.latency.Float() < p.opts.StallProbability

	out := make(chan domain.StreamChunk, 16)
	ttfb := p.latency.TTFB()
	gap := p.latency.InterTokenDelay()

	go func() {
		defer close(out)

		if !sleepCtx(ctx.Done(), ttfb) {
			out <- domain.StreamChunk{Err: ctx.Err()}
			return
		}

		for i, tok := range tokens {
			if breakAt >= 0 && i == breakAt {
				if stall {
					// Go quiet. The caller's stall timer is what must notice.
					<-ctx.Done()
					out <- domain.StreamChunk{Err: fmt.Errorf("%s: %w", p.name, ErrStreamStalled)}
					return
				}
				out <- domain.StreamChunk{Err: fmt.Errorf("%s: %w", p.name, ErrStreamReset)}
				return
			}
			select {
			case <-ctx.Done():
				out <- domain.StreamChunk{Err: ctx.Err()}
				return
			case out <- domain.StreamChunk{Delta: tok}:
			}
			if !sleepCtx(ctx.Done(), gap) {
				out <- domain.StreamChunk{Err: ctx.Err()}
				return
			}
		}

		finish := "stop"
		usage := domain.Usage{PromptTokens: promptTokens, CompletionTokens: EstimateTokens(remainder)}
		out <- domain.StreamChunk{FinishReason: &finish, Usage: &usage}
	}()

	return out, nil
}

// Embed returns deterministic pseudo-embeddings.
//
// Real embeddings come from the local embedder sidecar; this exists so that /v1/embeddings is
// answerable in mock mode without a second hop, and so its output is stable across runs.
func (p *Provider) Embed(ctx context.Context, req domain.EmbedRequest) (domain.EmbedResponse, error) {
	if err := p.admit(); err != nil {
		return domain.EmbedResponse{}, err
	}
	if !sleepCtx(ctx.Done(), p.latency.TTFB()/4) {
		return domain.EmbedResponse{}, ctx.Err()
	}

	const dim = 384
	vectors := make([][]float32, 0, len(req.Inputs))
	total := 0
	for _, in := range req.Inputs {
		vectors = append(vectors, HashEmbed(in, dim))
		total += EstimateTokens(in)
	}
	return domain.EmbedResponse{
		Model:    req.Model,
		Provider: p.name,
		Vectors:  vectors,
		Usage:    domain.Usage{PromptTokens: total},
	}, nil
}

// admit applies request-count failure injection and reports whether the call may proceed.
func (p *Provider) admit() error {
	if p.unhealthy.Load() {
		return fmt.Errorf("%s: %w", p.name, ErrUpstreamRefused)
	}
	n := p.served.Add(1)
	if p.opts.FailAfter > 0 && n > int64(p.opts.FailAfter) {
		p.unhealthy.Store(true)
		return fmt.Errorf("%s: %w (failing after %d requests)", p.name, ErrUpstreamRefused, p.opts.FailAfter)
	}
	return nil
}

// Reset clears injected failure state. Used between benchmark scenarios.
func (p *Provider) Reset() {
	p.served.Store(0)
	p.unhealthy.Store(false)
}

func (p *Provider) resolve(req domain.ChatRequest) (FixtureVariant, string, domain.ModelDescriptor) {
	model := p.modelFor(req.RequestedModel)
	prompt := req.UserPrompt()
	variant, _, _ := p.fixtures.Lookup(prompt, TierFor(model.Tier))
	return variant, prompt, model
}

func (p *Provider) modelFor(id string) domain.ModelDescriptor {
	for _, m := range p.models {
		if m.ID == id {
			return m
		}
	}
	// An unknown id falls back to the provider's first model, which keeps arbitrary curl
	// commands working in the demo instead of 404-ing on a typo.
	if len(p.models) > 0 {
		return p.models[0]
	}
	return domain.ModelDescriptor{ID: id, Provider: p.name}
}

func (p *Provider) promptTokens(req domain.ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += EstimateTokens(m.Content) + 3 // per-message role overhead, as OpenAI counts it
	}
	return n
}

// assistantPrefix returns the trailing assistant message, if the request ends with one. That is
// how the failover machinery asks a provider to continue a partial answer.
func assistantPrefix(req domain.ChatRequest) string {
	if len(req.Messages) == 0 {
		return ""
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != domain.RoleAssistant {
		return ""
	}
	return last.Content
}

// continuationOf returns the part of full that follows prefix.
//
// When the fallback provider's canned answer does not begin with what the first provider already
// emitted -- the normal case, since they are different models -- we cannot literally continue it.
// We instead drop a proportional amount of the fallback's own answer, which preserves the one
// property that matters for accounting: the client receives a complete answer of a sensible
// length, and tokens are counted once.
func continuationOf(full, prefix string) string {
	if prefix == "" {
		return full
	}
	if strings.HasPrefix(full, prefix) {
		return strings.TrimPrefix(full, prefix)
	}
	fullToks := tokenise(full)
	prefixToks := tokenise(prefix)
	if len(prefixToks) >= len(fullToks) {
		// The prefix already covers the whole answer; emit a short closing fragment so the
		// stream still terminates naturally.
		return " (continued)"
	}
	return strings.Join(fullToks[len(prefixToks):], "")
}

// tokenise splits text into streaming units, keeping the trailing space on each so that
// concatenating every delta reproduces the original text exactly.
func tokenise(s string) []string {
	if s == "" {
		return nil
	}
	fields := strings.SplitAfter(s, " ")
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// HashEmbed produces a deterministic unit-norm vector from text.
//
// It is a bag-of-character-trigrams hashed into dim buckets. Two paraphrases share many
// trigrams and therefore land close together, which is enough for the offline cache path to
// behave qualitatively like a real embedding model. The real model is used whenever the
// embedder sidecar is reachable; this is the EMBEDDER_MODE=hash fallback.
func HashEmbed(text string, dim int) []float32 {
	v := make([]float32, dim)
	norm := domain.NormalizePrompt(text)
	if len(norm) < 3 {
		norm = norm + "__"
	}
	for i := 0; i+3 <= len(norm); i++ {
		tri := norm[i : i+3]
		h := fnv1a(tri)
		idx := int(h % uint64(dim))
		sign := float32(1)
		if h&(1<<63) != 0 {
			sign = -1
		}
		v[idx] += sign
	}
	var mag float32
	for _, x := range v {
		mag += x * x
	}
	if mag == 0 {
		v[0] = 1
		return v
	}
	inv := float32(1.0 / math.Sqrt(float64(mag)))
	for i := range v {
		v[i] *= inv
	}
	return v
}

func fnv1a(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}
