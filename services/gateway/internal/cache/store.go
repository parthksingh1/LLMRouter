package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// Embedder turns a prompt into a vector. Declared here rather than imported so the cache can be
// tested with a trivial fake instead of a running sidecar.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// VectorStore is the nearest-neighbour index behind the semantic tier.
type VectorStore interface {
	// Search returns the nearest neighbour within the namespace, and whether one was found.
	Search(ctx context.Context, namespace string, vector []float32) (Neighbour, bool, error)
	// Upsert stores a vector under an id, inside a namespace.
	Upsert(ctx context.Context, namespace, id string, vector []float32, payload map[string]any) error
}

// Neighbour is a nearest-neighbour result.
type Neighbour struct {
	ID         string
	Similarity float64
	Payload    map[string]any
}

// BlobStore holds the cached responses themselves, keyed by id.
type BlobStore interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// Entry is what gets stored against a cache id.
type Entry struct {
	Prompt           string    `json:"prompt"`
	Content          string    `json:"content"`
	Model            string    `json:"model"`
	Provider         string    `json:"provider"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	StoredAt         time.Time `json:"stored_at"`
}

// Store is the two-tier semantic cache. It satisfies app.SemanticCache.
type Store struct {
	blobs     BlobStore
	vectors   VectorStore
	embedder  Embedder
	threshold float64
	ttl       time.Duration
	maxChars  int
	log       *slog.Logger

	// semanticEnabled is false when the calibrated threshold is unreachable (> 1.0), which is
	// how an operator turns the probabilistic tier off without removing the wiring.
	semanticEnabled bool
}

var _ app.SemanticCache = (*Store)(nil)

// Options configures the cache.
type Options struct {
	Blobs     BlobStore
	Vectors   VectorStore
	Embedder  Embedder
	Threshold float64
	TTL       time.Duration
	MaxChars  int
	Log       *slog.Logger
}

// New builds the cache.
func New(opts Options) (*Store, error) {
	if opts.Blobs == nil {
		return nil, fmt.Errorf("cache requires a blob store")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.TTL <= 0 {
		opts.TTL = 24 * time.Hour
	}
	if opts.MaxChars <= 0 {
		opts.MaxChars = 8000
	}

	// The semantic tier needs both an embedder and a vector store. Missing either is a valid
	// configuration -- the exact tier alone is useful and safe -- so it degrades rather than
	// failing to start.
	semantic := opts.Vectors != nil && opts.Embedder != nil && opts.Threshold > 0 && opts.Threshold <= 1.0
	if !semantic {
		opts.Log.Info("semantic cache tier disabled",
			"reason", semanticDisabledReason(opts),
			"threshold", opts.Threshold)
	}

	return &Store{
		blobs:           opts.Blobs,
		vectors:         opts.Vectors,
		embedder:        opts.Embedder,
		threshold:       opts.Threshold,
		ttl:             opts.TTL,
		maxChars:        opts.MaxChars,
		log:             opts.Log,
		semanticEnabled: semantic,
	}, nil
}

func semanticDisabledReason(o Options) string {
	switch {
	case o.Vectors == nil:
		return "no vector store configured"
	case o.Embedder == nil:
		return "no embedder configured"
	case o.Threshold > 1.0:
		return "threshold above 1.0, which no cosine similarity can reach"
	default:
		return "threshold not positive"
	}
}

// Lookup tries the exact tier, then the semantic tier.
//
// The order matters for both cost and safety: the exact tier is a single hash lookup with no
// false-hit risk, so paying for an embedding call before trying it would be slower and no safer.
func (s *Store) Lookup(ctx context.Context, key domain.CacheKey, prompt string) (domain.CachedResponse, bool, error) {
	if len(prompt) > s.maxChars {
		// An enormous prompt is unlikely to repeat and expensive to embed. Skipping it is a
		// miss, not an error.
		return domain.CachedResponse{}, false, nil
	}

	// --- tier 1: exact -----------------------------------------------------------------
	entry, found, err := s.getEntry(ctx, s.exactRedisKey(key, prompt))
	if err != nil {
		return domain.CachedResponse{}, false, err
	}
	if found {
		return toCachedResponse(entry, 1.0), true, nil
	}

	if !s.semanticEnabled {
		return domain.CachedResponse{}, false, nil
	}

	// --- tier 2: semantic --------------------------------------------------------------
	vector, err := s.embedder.Embed(ctx, prompt)
	if err != nil {
		return domain.CachedResponse{}, false, fmt.Errorf("embedding prompt: %w", err)
	}

	neighbour, ok, err := s.vectors.Search(ctx, key.Namespace(), vector)
	if err != nil {
		return domain.CachedResponse{}, false, fmt.Errorf("vector search: %w", err)
	}
	if !ok {
		return domain.CachedResponse{}, false, nil
	}
	// Report the observed similarity even on a miss, so the metric histogram covers the whole
	// distribution rather than only the hits.
	if neighbour.Similarity < s.threshold {
		return domain.CachedResponse{Similarity: neighbour.Similarity}, false, nil
	}

	entry, found, err = s.getEntry(ctx, neighbour.ID)
	if err != nil {
		return domain.CachedResponse{}, false, err
	}
	if !found {
		// The vector outlived its response: Redis expired the blob but Qdrant still indexes it.
		// A miss, not an error -- and the write path will re-add both.
		s.log.Debug("cache vector without a stored response", "id", neighbour.ID)
		return domain.CachedResponse{Similarity: neighbour.Similarity}, false, nil
	}

	// Final guard: a neighbour that the exact tier would have rejected as a different question
	// is refused even when its similarity clears the threshold. This is what stops the
	// "increase the timeout" / "decrease the timeout" class of false hit, which the calibration
	// showed a bi-encoder cannot separate on similarity alone.
	if !SameQuestion(prompt, entry.Prompt) && neighbour.Similarity < 1.0 {
		s.log.Debug("semantic neighbour rejected by the exact-tier guard",
			"similarity", neighbour.Similarity)
		return domain.CachedResponse{Similarity: neighbour.Similarity}, false, nil
	}

	return toCachedResponse(entry, neighbour.Similarity), true, nil
}

// Store writes an answer into both tiers.
func (s *Store) Store(ctx context.Context, key domain.CacheKey, prompt string, resp domain.ChatResponse) error {
	if resp.Content == "" || len(prompt) > s.maxChars {
		return nil
	}

	entry := Entry{
		Prompt:           prompt,
		Content:          resp.Content,
		Model:            resp.Model,
		Provider:         resp.Provider,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		StoredAt:         time.Now().UTC(),
	}
	blob, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encoding cache entry: %w", err)
	}

	exactKey := s.exactRedisKey(key, prompt)
	if err := s.blobs.Set(ctx, exactKey, blob, s.ttl); err != nil {
		return fmt.Errorf("writing cache entry: %w", err)
	}

	if !s.semanticEnabled {
		return nil
	}

	vector, err := s.embedder.Embed(ctx, prompt)
	if err != nil {
		// The exact tier is already written, so the request still benefits. Losing the vector
		// costs future paraphrase hits, not correctness.
		return fmt.Errorf("embedding prompt for the vector index: %w", err)
	}
	if err := s.vectors.Upsert(ctx, key.Namespace(), exactKey, vector, map[string]any{
		"tenant_id":    key.TenantID,
		"model_family": key.ModelFamily,
		"model":        resp.Model,
	}); err != nil {
		return fmt.Errorf("indexing cache vector: %w", err)
	}
	return nil
}

func (s *Store) getEntry(ctx context.Context, redisKey string) (Entry, bool, error) {
	blob, found, err := s.blobs.Get(ctx, redisKey)
	if err != nil {
		return Entry{}, false, fmt.Errorf("reading cache entry: %w", err)
	}
	if !found {
		return Entry{}, false, nil
	}
	var entry Entry
	if err := json.Unmarshal(blob, &entry); err != nil {
		// A corrupt entry is a miss, not a failed request.
		s.log.Warn("discarding corrupt cache entry", "key", redisKey, "error", err)
		return Entry{}, false, nil
	}
	return entry, true, nil
}

// exactRedisKey is the storage key for a prompt inside a namespace.
//
// The namespace is part of the key, not a filter applied afterwards. That is what makes
// cross-tenant leakage impossible rather than merely unlikely: tenant B's key cannot name
// tenant A's entry, so there is no code path in which the wrong tenant's answer can be read.
func (s *Store) exactRedisKey(key domain.CacheKey, prompt string) string {
	return "llmrouter:cache:" + key.Namespace() + ":" + domain.HashText(ExactKey(prompt))
}

func toCachedResponse(e Entry, similarity float64) domain.CachedResponse {
	return domain.CachedResponse{
		Content:    e.Content,
		Model:      e.Model,
		Provider:   e.Provider,
		Similarity: similarity,
		StoredAt:   e.StoredAt,
		Usage: domain.Usage{
			PromptTokens:     e.PromptTokens,
			CompletionTokens: e.CompletionTokens,
		},
	}
}
