package live

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// newHTTPClient builds a client tuned for LLM traffic: long response deadlines (generations run
// for minutes) but short connect and TLS handshakes, so a dead endpoint is detected quickly
// instead of consuming the whole request budget.
func newHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
		// Streaming bodies must not be buffered: a response header timeout is the right knob,
		// not an overall client timeout, which would kill long generations.
		ResponseHeaderTimeout: 20 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// drainAndClose releases a response body back to the connection pool.
//
// Draining a bounded amount first lets keep-alive reuse the connection; without it every error
// path silently costs a new TLS handshake.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 64*1024)
	_ = resp.Body.Close()
}

// StatusError is an upstream non-2xx response, preserving the status so the router can decide
// whether the failure is retryable.
type StatusError struct {
	Provider string
	Status   int
	Body     string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: upstream returned %d: %s", e.Provider, e.Status, truncate(e.Body, 300))
}

// Retryable reports whether failing over to another provider is worthwhile. A 4xx that is not
// rate limiting is the caller's fault and will fail identically everywhere, so it is not.
func (e *StatusError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

func upstreamStatusError(provider string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	return &StatusError{Provider: provider, Status: resp.StatusCode, Body: string(body)}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// emit sends a chunk unless the context has ended. It reports whether the send succeeded, so
// callers can stop pumping a stream nobody is reading.
func emit(ctx context.Context, out chan<- domain.StreamChunk, c domain.StreamChunk) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- c:
		return true
	}
}
