// Package http is the delivery layer: routing, middleware, wire translation and error mapping.
//
// It is the only package that knows about net/http, and nothing imports it except cmd/gateway.
package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
	"github.com/parthkumarsingh/llmrouter/services/gateway/pkg/openaiapi"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyTenant
	ctxKeyLogger
)

// HeaderRequestID is echoed on every response so a caller can quote it in a bug report.
const HeaderRequestID = "X-Request-Id"

// RequestIDFrom returns the request id carried on the context, or "" if there is none.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// TenantFrom returns the authenticated tenant carried on the context.
func TenantFrom(ctx context.Context) (domain.Tenant, bool) {
	t, ok := ctx.Value(ctxKeyTenant).(domain.Tenant)
	return t, ok
}

// LoggerFrom returns the request-scoped logger, falling back to the default logger.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// RequestID assigns or adopts a request id and puts it on the context and the response.
//
// An inbound X-Request-Id is honoured so that a caller's correlation id survives the hop, but it
// is length-capped and stripped of control characters: an unvalidated header ends up in logs and
// in trace attributes, which is a log-injection vector.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitiseRequestID(r.Header.Get(HeaderRequestID))
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set(HeaderRequestID, id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func sanitiseRequestID(s string) string {
	if len(s) > 128 {
		s = s[:128]
	}
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// Logging emits one structured line per request and attaches a request-scoped logger.
//
// It deliberately never logs headers or bodies: prompts routinely contain PII, and Authorization
// carries tenant keys. What is logged is metadata only.
func Logging(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			reqLog := log.With(
				"request_id", RequestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
			)
			ctx := context.WithValue(r.Context(), ctxKeyLogger, reqLog)

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))

			attrs := []any{
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"bytes", rec.written,
			}
			if t, ok := TenantFrom(ctx); ok {
				attrs = append(attrs, "tenant_id", t.ID)
			}

			switch {
			case rec.status >= 500:
				reqLog.Error("request failed", attrs...)
			case rec.status >= 400:
				reqLog.Warn("request rejected", attrs...)
			default:
				reqLog.Info("request completed", attrs...)
			}
		})
	}
}

// Recover turns a panic into a 500 rather than a dropped connection, and logs the stack once.
//
// A panic mid-SSE cannot produce a status code -- headers are long gone -- so in that case the
// stream is simply terminated and the panic recorded.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A panic handler runs after the request is already over: there is no live context
			// left to propagate, and the request id it needs is read back off the request.
			defer func() { //nolint:contextcheck // no live request context by the time this runs
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is the documented way to abort a response; it is not a bug.
				// Compared with errors.Is rather than ==: a handler is free to wrap it before
				// panicking, and a wrapped abort must not be logged as a crash.
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				log.Error("panic recovered",
					"request_id", RequestIDFrom(r.Context()),
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				if sr, ok := w.(*statusRecorder); ok && sr.wroteHeader {
					return
				}
				WriteError(w, r, http.StatusInternalServerError,
					openaiapi.NewError("internal server error", openaiapi.ErrTypeAPI, "internal_error", ""))
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds non-streaming requests.
//
// Streaming requests are exempt: an SSE response legitimately outlives any fixed deadline, and
// its liveness is enforced by the per-stream stall timeout instead.
func Timeout(d time.Duration, isStreaming func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d <= 0 || (isStreaming != nil && isStreaming(r)) {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// statusRecorder captures the status code and byte count for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	written     int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += n
	return n, err
}

// Flush forwards to the underlying writer so SSE still streams through the recorder.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
