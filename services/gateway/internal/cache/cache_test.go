package cache_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/cache"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// --- doubles -----------------------------------------------------------------

type memBlobs struct {
	mu   sync.Mutex
	data map[string][]byte
	err  error
}

func newMemBlobs() *memBlobs { return &memBlobs{data: map[string][]byte{}} }

func (m *memBlobs) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, false, m.err
	}
	v, ok := m.data[key]
	return v, ok, nil
}

func (m *memBlobs) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.data[key] = value
	return nil
}

func (m *memBlobs) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.data))
	for k := range m.data {
		out = append(out, k)
	}
	return out
}

// memVectors is an exhaustive nearest-neighbour index: correctness over speed, which is what a
// test needs.
type memVectors struct {
	mu     sync.Mutex
	points map[string][]point // namespace -> points
}

type point struct {
	id     string
	vector []float32
}

func newMemVectors() *memVectors { return &memVectors{points: map[string][]point{}} }

func (m *memVectors) Search(_ context.Context, namespace string, vector []float32) (cache.Neighbour, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	best := cache.Neighbour{}
	found := false
	for _, p := range m.points[namespace] {
		score := cosine(p.vector, vector)
		if !found || score > best.Similarity {
			best = cache.Neighbour{ID: p.id, Similarity: score}
			found = true
		}
	}
	return best, found, nil
}

func (m *memVectors) Upsert(_ context.Context, namespace, id string, vector []float32, _ map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.points[namespace] = append(m.points[namespace], point{id: id, vector: vector})
	return nil
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (sqrt(na) * sqrt(nb))
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 32; i++ {
		z -= (z*z - x) / (2 * z)
	}
	return z
}

// wordEmbedder is a bag-of-words embedder: close enough to a real one for cache tests, and
// entirely predictable.
type wordEmbedder struct {
	err error
}

func (w *wordEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if w.err != nil {
		return nil, w.err
	}
	const dim = 64
	vec := make([]float32, dim)
	for _, token := range strings.Fields(strings.ToLower(text)) {
		h := 0
		for _, r := range token {
			h = h*31 + int(r)
		}
		if h < 0 {
			h = -h
		}
		vec[h%dim] += 1
	}
	return vec, nil
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newStore(t *testing.T, blobs cache.BlobStore, vectors cache.VectorStore, embedder cache.Embedder, threshold float64) *cache.Store {
	t.Helper()
	s, err := cache.New(cache.Options{
		Blobs: blobs, Vectors: vectors, Embedder: embedder,
		Threshold: threshold, TTL: time.Hour, MaxChars: 8000, Log: quietLogger(),
	})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	return s
}

func response(content string) domain.ChatResponse {
	return domain.ChatResponse{
		Content: content, Model: "gpt-4o-mini", Provider: "openai",
		Usage: domain.Usage{PromptTokens: 20, CompletionTokens: 10},
	}
}

// --- normalisation ------------------------------------------------------------

func TestExactKeyNormalisation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		left  string
		right string
		same  bool
	}{
		{name: "identical", left: "list three metrics", right: "list three metrics", same: true},
		{name: "case and spacing", left: "List  Three   Metrics", right: "list three metrics", same: true},
		{name: "politeness prefix", left: "Quick question: list three metrics", right: "list three metrics", same: true},
		{name: "politeness suffix", left: "list three metrics. Thanks!", right: "list three metrics.", same: true},
		{name: "numeral spelling", left: "list 3 metrics", right: "list three metrics", same: true},
		{name: "british and american spelling", left: "summarise the log", right: "summarize the log", same: true},
		{name: "word order", left: "for the queue list metrics", right: "list metrics for the queue", same: true},

		// The cases the guard exists for.
		{name: "a changed number is a different question", left: "list three metrics", right: "list thirty metrics", same: false},
		{name: "an inverted verb is a different question", left: "increase the timeout", right: "decrease the timeout", same: false},
		{name: "a changed noun is a different question", left: "reset my password", right: "reset my router", same: false},
		{name: "instruction verbs are not interchangeable", left: "explain the design", right: "write the design", same: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := cache.SameQuestion(tc.left, tc.right); got != tc.same {
				t.Errorf("SameQuestion(%q, %q) = %v, want %v\n  left key:  %q\n  right key: %q",
					tc.left, tc.right, got, tc.same,
					cache.ExactKey(tc.left), cache.ExactKey(tc.right))
			}
		})
	}
}

// --- isolation ----------------------------------------------------------------

// TestCrossTenantIsolation is the test that matters most in this package.
//
// A semantic cache that leaks across tenants is worse than no cache: it silently serves one
// customer's data to another. The namespace is part of the storage key rather than a filter
// applied after retrieval, so this is a structural guarantee, and this test pins it.
func TestCrossTenantIsolation(t *testing.T) {
	t.Parallel()

	blobs := newMemBlobs()
	vectors := newMemVectors()
	store := newStore(t, blobs, vectors, &wordEmbedder{}, 0.9)
	ctx := context.Background()

	const prompt = "what is our internal deployment procedure"
	tenantA := domain.NewCacheKey("tenant-a", "gpt-4", "")
	tenantB := domain.NewCacheKey("tenant-b", "gpt-4", "")

	if err := store.Store(ctx, tenantA, prompt, response("tenant A confidential answer")); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Tenant A sees its own answer.
	got, hit, err := store.Lookup(ctx, tenantA, prompt)
	if err != nil || !hit {
		t.Fatalf("tenant A lookup: hit=%v err=%v", hit, err)
	}
	if got.Content != "tenant A confidential answer" {
		t.Errorf("tenant A got %q", got.Content)
	}

	// Tenant B must not, for the byte-identical prompt.
	_, hit, err = store.Lookup(ctx, tenantB, prompt)
	if err != nil {
		t.Fatalf("tenant B lookup: %v", err)
	}
	if hit {
		t.Fatal("tenant B was served tenant A's cached answer")
	}

	// And no stored key may be readable without naming the owning tenant.
	for _, key := range blobs.keys() {
		if !strings.Contains(key, "tenant-a") {
			t.Errorf("cache key %q does not carry its tenant; isolation is not structural", key)
		}
	}
}

func TestNamespaceSeparatesModelFamilyAndSystemPrompt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const prompt = "summarise this incident"

	tests := []struct {
		name  string
		write domain.CacheKey
		read  domain.CacheKey
		hit   bool
	}{
		{
			name:  "same namespace hits",
			write: domain.NewCacheKey("t", "gpt-4", "be terse"),
			read:  domain.NewCacheKey("t", "gpt-4", "be terse"),
			hit:   true,
		},
		{
			name: "a different model family does not hit",
			// An answer from a small model must not satisfy a request routed to a frontier one.
			write: domain.NewCacheKey("t", "gpt-4", "be terse"),
			read:  domain.NewCacheKey("t", "claude-3", "be terse"),
			hit:   false,
		},
		{
			name: "a different system prompt does not hit",
			// The system prompt changes what the same user turn means.
			write: domain.NewCacheKey("t", "gpt-4", "be terse"),
			read:  domain.NewCacheKey("t", "gpt-4", "answer as a pirate"),
			hit:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := newStore(t, newMemBlobs(), newMemVectors(), &wordEmbedder{}, 0.9)

			if err := store.Store(ctx, tc.write, prompt, response("cached")); err != nil {
				t.Fatalf("Store: %v", err)
			}
			_, hit, err := store.Lookup(ctx, tc.read, prompt)
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if hit != tc.hit {
				t.Errorf("hit = %v, want %v", hit, tc.hit)
			}
		})
	}
}

// --- tiers --------------------------------------------------------------------

func TestExactTierServesParaphrasesWithoutEmbedding(t *testing.T) {
	t.Parallel()

	embedder := &wordEmbedder{err: errors.New("the embedder must not be called")}
	// No vector store and a broken embedder: only the exact tier can answer.
	store := newStore(t, newMemBlobs(), nil, embedder, 0.9)
	ctx := context.Background()
	key := domain.NewCacheKey("t", "gpt-4", "")

	if err := store.Store(ctx, key, "List three metrics worth tracking.", response("the answer")); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Politeness, casing and numeral spelling must all resolve on the exact tier, which is
	// where most real cache value comes from and which costs nothing.
	for _, variant := range []string{
		"list three metrics worth tracking.",
		"Quick question: list 3 metrics worth tracking. Thanks!",
		"  LIST   THREE   METRICS   WORTH   TRACKING.  ",
	} {
		got, hit, err := store.Lookup(ctx, key, variant)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", variant, err)
		}
		if !hit {
			t.Errorf("expected an exact-tier hit for %q", variant)
			continue
		}
		if got.Similarity != 1.0 {
			t.Errorf("an exact-tier hit should report similarity 1.0, got %v", got.Similarity)
		}
	}
}

func TestSemanticTierIsGuardedByNormalisation(t *testing.T) {
	t.Parallel()

	blobs := newMemBlobs()
	vectors := newMemVectors()
	// A threshold low enough that the bag-of-words embedder will happily match anything
	// similar, so the guard is the only thing standing between us and a false hit.
	store := newStore(t, blobs, vectors, &wordEmbedder{}, 0.5)
	ctx := context.Background()
	key := domain.NewCacheKey("t", "gpt-4", "")

	if err := store.Store(ctx, key, "should I increase the timeout for the worker", response("yes, raise it")); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// A minimal pair: one word changed, meaning inverted. The embedding is extremely close.
	_, hit, err := store.Lookup(ctx, key, "should I decrease the timeout for the worker")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if hit {
		t.Fatal("a minimal pair was served from cache; this is the false-hit the guard exists to prevent")
	}
}

func TestSemanticTierDisabledWhenThresholdIsUnreachable(t *testing.T) {
	t.Parallel()

	// A threshold above 1.0 is how an operator turns the probabilistic tier off. The exact
	// tier must keep working.
	store := newStore(t, newMemBlobs(), newMemVectors(), &wordEmbedder{}, 1.5)
	ctx := context.Background()
	key := domain.NewCacheKey("t", "gpt-4", "")

	if err := store.Store(ctx, key, "list three metrics", response("cached")); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, hit, err := store.Lookup(ctx, key, "list 3 metrics"); err != nil || !hit {
		t.Errorf("the exact tier must still work with the semantic tier disabled: hit=%v err=%v", hit, err)
	}
}

func TestMissesAndDegradation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	key := domain.NewCacheKey("t", "gpt-4", "")

	t.Run("an empty cache misses", func(t *testing.T) {
		t.Parallel()
		store := newStore(t, newMemBlobs(), newMemVectors(), &wordEmbedder{}, 0.9)
		if _, hit, err := store.Lookup(ctx, key, "anything"); hit || err != nil {
			t.Errorf("hit=%v err=%v, want a clean miss", hit, err)
		}
	})

	t.Run("an oversized prompt is skipped rather than failing", func(t *testing.T) {
		t.Parallel()
		store := newStore(t, newMemBlobs(), newMemVectors(), &wordEmbedder{}, 0.9)
		huge := strings.Repeat("x", 9000)
		if _, hit, err := store.Lookup(ctx, key, huge); hit || err != nil {
			t.Errorf("hit=%v err=%v, want a clean miss", hit, err)
		}
		if err := store.Store(ctx, key, huge, response("x")); err != nil {
			t.Errorf("storing an oversized prompt should be a no-op, got %v", err)
		}
	})

	t.Run("an empty response is not stored", func(t *testing.T) {
		t.Parallel()
		blobs := newMemBlobs()
		store := newStore(t, blobs, newMemVectors(), &wordEmbedder{}, 0.9)
		if err := store.Store(ctx, key, "prompt", response("")); err != nil {
			t.Fatalf("Store: %v", err)
		}
		if len(blobs.keys()) != 0 {
			t.Error("an empty response should not be cached")
		}
	})

	t.Run("a broken blob store surfaces an error rather than a wrong answer", func(t *testing.T) {
		t.Parallel()
		blobs := newMemBlobs()
		blobs.err = errors.New("redis is down")
		store := newStore(t, blobs, newMemVectors(), &wordEmbedder{}, 0.9)

		_, hit, err := store.Lookup(ctx, key, "anything")
		if hit {
			t.Error("a broken cache must never report a hit")
		}
		if err == nil {
			t.Error("a broken cache should report the error so it can be counted as degraded")
		}
	})

	t.Run("a corrupt entry is treated as a miss", func(t *testing.T) {
		t.Parallel()
		blobs := newMemBlobs()
		store := newStore(t, blobs, newMemVectors(), &wordEmbedder{}, 0.9)

		if err := store.Store(ctx, key, "list three metrics", response("good")); err != nil {
			t.Fatalf("Store: %v", err)
		}
		for _, k := range blobs.keys() {
			_ = blobs.Set(ctx, k, []byte("{not json"), time.Hour)
		}
		if _, hit, err := store.Lookup(ctx, key, "list three metrics"); hit || err != nil {
			t.Errorf("hit=%v err=%v, want a clean miss on a corrupt entry", hit, err)
		}
	})
}

func TestNewRejectsMissingBlobStore(t *testing.T) {
	t.Parallel()

	if _, err := cache.New(cache.Options{Log: quietLogger()}); err == nil {
		t.Error("a cache with no blob store cannot function and must not be constructed")
	}
}

func TestRoundTripPreservesUsageForBilling(t *testing.T) {
	t.Parallel()

	store := newStore(t, newMemBlobs(), newMemVectors(), &wordEmbedder{}, 0.9)
	ctx := context.Background()
	key := domain.NewCacheKey("t", "gpt-4", "")

	original := domain.ChatResponse{
		Content: "answer", Model: "claude-3-5-haiku", Provider: "anthropic",
		Usage: domain.Usage{PromptTokens: 123, CompletionTokens: 45},
	}
	if err := store.Store(ctx, key, "a question", original); err != nil {
		t.Fatalf("Store: %v", err)
	}

	got, hit, err := store.Lookup(ctx, key, "a question")
	if err != nil || !hit {
		t.Fatalf("hit=%v err=%v", hit, err)
	}
	// Usage is carried so a cached response can still be reported to the caller and recorded in
	// analytics, even though it is billed at zero.
	if got.Usage != original.Usage {
		t.Errorf("usage = %+v, want %+v", got.Usage, original.Usage)
	}
	if got.Model != "claude-3-5-haiku" || got.Provider != "anthropic" {
		t.Errorf("the serving model must be preserved, got %s/%s", got.Provider, got.Model)
	}
	if got.StoredAt.IsZero() {
		t.Error("StoredAt should be set so cache age is observable")
	}
}
