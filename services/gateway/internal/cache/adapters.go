package cache

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // point ids only, not a security boundary
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// --- Redis blob store --------------------------------------------------------

// RedisBlobs stores cached responses in Redis with a TTL.
type RedisBlobs struct {
	client redis.UniversalClient
}

// NewRedisBlobs connects to Redis from a URL such as redis://redis:6379/0.
func NewRedisBlobs(rawURL string) (*RedisBlobs, error) {
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing REDIS_URL: %w", err)
	}
	// Short dial and read timeouts: the cache is an optimisation, and a slow Redis must not
	// add more latency than the cache saves.
	opts.DialTimeout = 2 * time.Second
	opts.ReadTimeout = 500 * time.Millisecond
	opts.WriteTimeout = 500 * time.Millisecond
	opts.PoolSize = 32

	return &RedisBlobs{client: redis.NewClient(opts)}, nil
}

// Get reads a cached blob.
func (r *RedisBlobs) Get(ctx context.Context, key string) ([]byte, bool, error) {
	value, err := r.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("redis get: %w", err)
	}
	return value, true, nil
}

// Set writes a cached blob with a TTL.
func (r *RedisBlobs) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := r.client.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("redis set: %w", err)
	}
	return nil
}

// Ping checks connectivity, for readiness reporting.
func (r *RedisBlobs) Ping(ctx context.Context) error {
	if err := r.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}
	return nil
}

// Close releases the connection pool.
func (r *RedisBlobs) Close() error { return r.client.Close() }

// --- Qdrant vector store -----------------------------------------------------

// QdrantVectors is a minimal Qdrant client over its HTTP API.
//
// The official client is not used: this needs exactly two operations, and a hand-written client
// keeps the dependency surface (and the container image) small. The trade-off is documented in
// docs/adr/0002-cache-threshold-calibration.md.
type QdrantVectors struct {
	baseURL    string
	collection string
	dim        int
	client     *http.Client

	// ensureOnce guards lazy collection creation, so the gateway does not need a migration
	// step and a fresh Qdrant volume works on first request.
	ensureOnce sync.Once
	ensureErr  error
}

// NewQdrantVectors builds the vector store client.
func NewQdrantVectors(baseURL, collection string, dim int) *QdrantVectors {
	return &QdrantVectors{
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		collection: collection,
		dim:        dim,
		client: &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 1 * time.Second}).DialContext,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     60 * time.Second,
			},
		},
	}
}

// EnsureCollection creates the collection if it does not exist.
func (q *QdrantVectors) EnsureCollection(ctx context.Context) error {
	q.ensureOnce.Do(func() {
		body := map[string]any{
			"vectors": map[string]any{"size": q.dim, "distance": "Cosine"},
		}
		resp, err := q.do(ctx, http.MethodPut, "/collections/"+url.PathEscape(q.collection), body)
		if err != nil {
			q.ensureErr = err
			return
		}
		defer drain(resp)
		// 409 means it already exists, which is the normal case after the first start.
		if resp.StatusCode >= 400 && resp.StatusCode != http.StatusConflict {
			q.ensureErr = fmt.Errorf("creating qdrant collection: unexpected status %d", resp.StatusCode)
		}
	})
	return q.ensureErr
}

// Search returns the nearest neighbour inside a namespace.
func (q *QdrantVectors) Search(ctx context.Context, namespace string, vector []float32) (Neighbour, bool, error) {
	if err := q.EnsureCollection(ctx); err != nil {
		return Neighbour{}, false, err
	}

	// The namespace filter is applied server-side. Filtering client-side would mean the wrong
	// tenant's vectors are fetched before being discarded, which is both slower and a leak
	// waiting for a refactor to expose.
	body := map[string]any{
		"vector":       vector,
		"limit":        1,
		"with_payload": true,
		"filter": map[string]any{
			"must": []any{
				map[string]any{
					"key":   "namespace",
					"match": map[string]any{"value": namespace},
				},
			},
		},
	}

	resp, err := q.do(ctx, http.MethodPost, "/collections/"+url.PathEscape(q.collection)+"/points/search", body)
	if err != nil {
		return Neighbour{}, false, err
	}
	defer drain(resp)
	if resp.StatusCode >= 400 {
		return Neighbour{}, false, fmt.Errorf("qdrant search: unexpected status %d", resp.StatusCode)
	}

	var out struct {
		Result []struct {
			Score   float64        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Neighbour{}, false, fmt.Errorf("decoding qdrant search: %w", err)
	}
	if len(out.Result) == 0 {
		return Neighbour{}, false, nil
	}

	hit := out.Result[0]
	id, _ := hit.Payload["cache_key"].(string)
	if id == "" {
		return Neighbour{}, false, nil
	}
	return Neighbour{ID: id, Similarity: hit.Score, Payload: hit.Payload}, true, nil
}

// Upsert stores a vector under a namespace.
func (q *QdrantVectors) Upsert(ctx context.Context, namespace, id string, vector []float32, payload map[string]any) error {
	if err := q.EnsureCollection(ctx); err != nil {
		return err
	}

	full := map[string]any{"namespace": namespace, "cache_key": id}
	for k, v := range payload {
		full[k] = v
	}

	body := map[string]any{
		"points": []any{
			map[string]any{
				// Qdrant point ids must be a uint64 or a UUID, so the Redis key is hashed into
				// a stable UUID-shaped string. Storing the real key in the payload keeps the
				// mapping recoverable.
				"id":      pointID(id),
				"vector":  vector,
				"payload": full,
			},
		},
	}

	resp, err := q.do(ctx, http.MethodPut, "/collections/"+url.PathEscape(q.collection)+"/points?wait=false", body)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("qdrant upsert: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// Ping checks connectivity, for readiness reporting.
func (q *QdrantVectors) Ping(ctx context.Context) error {
	resp, err := q.do(ctx, http.MethodGet, "/readyz", nil)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("qdrant readiness: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (q *QdrantVectors) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding qdrant request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, q.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("building qdrant request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := q.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling qdrant: %w", err)
	}
	return resp, nil
}

// pointID hashes a cache key into a deterministic UUID-shaped point id.
func pointID(key string) string {
	sum := sha1.Sum([]byte(key)) //nolint:gosec // identity, not authentication
	h := hex.EncodeToString(sum[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// --- embedder client ---------------------------------------------------------

// HTTPEmbedder calls the embedder sidecar.
type HTTPEmbedder struct {
	baseURL string
	client  *http.Client
}

// NewHTTPEmbedder builds the embedder client.
//
// The timeout is deliberately tight. Embedding sits on the cache-lookup path, which exists to
// make requests faster; a slow embedder must degrade to a cache miss rather than adding latency
// to every request.
func NewHTTPEmbedder(baseURL string, timeout time.Duration) *HTTPEmbedder {
	if timeout <= 0 {
		timeout = 250 * time.Millisecond
	}
	return &HTTPEmbedder{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     60 * time.Second,
			},
		},
	}
}

// Embed returns the vector for one text.
func (e *HTTPEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(map[string]any{"texts": []string{text}})
	if err != nil {
		return nil, fmt.Errorf("encoding embed request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling embedder: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("embedder returned status %d", resp.StatusCode)
	}

	var out struct {
		Vectors [][]float32 `json:"vectors"`
		Mode    string      `json:"mode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding embed response: %w", err)
	}
	if len(out.Vectors) == 0 {
		return nil, fmt.Errorf("embedder returned no vectors")
	}
	return out.Vectors[0], nil
}

// Ping checks the sidecar is up and reports which mode it is running.
func (e *HTTPEmbedder) Ping(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+"/healthz", nil)
	if err != nil {
		return "", fmt.Errorf("building embedder health request: %w", err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling embedder health: %w", err)
	}
	defer drain(resp)

	var out struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding embedder health: %w", err)
	}
	return out.Mode, nil
}

func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 32*1024)
	_ = resp.Body.Close()
}
