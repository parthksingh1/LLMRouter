package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/parthkumarsingh/llmrouter/services/gateway/pkg/openaiapi"
)

// Deps is everything the HTTP layer needs, injected rather than reached for.
//
// Constructing this struct is the composition root's job (cmd/gateway). Handlers receive only
// what they use, which is what keeps this package testable without a live stack.
type Deps struct {
	Log     *slog.Logger
	Version string

	Tenants TenantStore
	Meta    *MetaHandler

	// Chat is nil until the routing use case is wired in; the chat endpoints are simply not
	// mounted in that case rather than 500-ing.
	Chat ChatEndpoints

	MetricsHandler http.Handler
	Recorder       RequestRecorder
	// TracingEnabled mounts the OpenTelemetry middleware. Off when no collector is configured.
	TracingEnabled bool

	// RequestTimeout bounds non-streaming requests. Streaming requests are exempt.
	RequestTimeout time.Duration
}

// NewRouter assembles the public API.
//
// Middleware order matters and is not arbitrary:
//
//	RealIP -> RequestID -> Logging -> Recover -> Timeout -> Tracing -> Metrics -> Auth -> handler
//
// RequestID first so every later log line and error carries it; Logging before Recover so a
// panic still produces an access log entry; Recover before Timeout so a panic inside the
// timeout-wrapped handler is still caught; Metrics inside Recover so a panicking request is
// still counted as a 500; Auth last so unauthenticated requests are cheap.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(chimw.RealIP)
	r.Use(RequestID)
	r.Use(Logging(d.Log))
	r.Use(Recover(d.Log))
	r.Use(Timeout(d.RequestTimeout, isStreamingRequest))
	r.Use(Tracing(d.TracingEnabled))
	r.Use(Metrics(d.Recorder))

	// Unauthenticated operational surface. Kept off /v1 so it is trivially excluded from an
	// ingress that requires auth.
	r.Get("/healthz", d.Meta.Healthz)
	r.Get("/readyz", d.Meta.Readyz)
	if d.MetricsHandler != nil {
		r.Handle("/metrics", d.MetricsHandler)
	}

	r.Route("/v1", func(v1 chi.Router) {
		v1.Use(Auth(d.Tenants))

		v1.Get("/models", d.Meta.Models)
		if d.Chat != nil {
			v1.Post("/chat/completions", d.Chat.Completions)
			v1.Post("/embeddings", d.Chat.Embeddings)
		}
	})

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		WriteError(w, req, http.StatusNotFound, notFoundBody(req.URL.Path))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		WriteError(w, req, http.StatusMethodNotAllowed, methodNotAllowedBody(req.Method))
	})

	return r
}

// isStreamingRequest reports whether a request is likely to produce an SSE response, so the
// blanket timeout can be skipped. Reading the body here is not an option -- the handler needs it
// -- so we use the cheap signals available on the request line and headers.
func isStreamingRequest(r *http.Request) bool {
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		return true
	}
	// The OpenAI SDKs do not set Accept for streaming, so fall back to the path: chat
	// completions may stream, and the handler applies its own per-stream stall timeout.
	return strings.HasSuffix(r.URL.Path, "/chat/completions")
}

// Server owns the listeners and their lifecycle.
type Server struct {
	log     *slog.Logger
	api     *http.Server
	metrics *http.Server
	grace   time.Duration
}

// NewServer builds the API server, and a second listener for metrics when a distinct address is
// configured. Serving metrics on its own port is what lets an ingress expose :8080 publicly
// while :9090 stays on the cluster network.
func NewServer(
	apiAddr, metricsAddr string,
	handler http.Handler,
	metricsHandler http.Handler,
	readTimeout, writeTimeout, idleTimeout, grace time.Duration,
	log *slog.Logger,
) *Server {
	s := &Server{
		log:   log,
		grace: grace,
		api: &http.Server{
			Addr:              apiAddr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       readTimeout,
			// WriteTimeout must exceed the longest expected generation, and is disabled
			// entirely when set to zero so that SSE streams are never cut off mid-answer.
			WriteTimeout: writeTimeout,
			IdleTimeout:  idleTimeout,
			ErrorLog:     slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		},
	}

	if metricsAddr != "" && metricsAddr != apiAddr && metricsHandler != nil {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metricsHandler)
		s.metrics = &http.Server{
			Addr:              metricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		}
	}
	return s
}

// Run serves until ctx is cancelled, then drains.
//
// On cancellation the server stops accepting new connections and gives in-flight requests up to
// the grace period to finish. That matters more here than in a typical service: cutting an SSE
// stream at token 300 of 400 is a visible failure for the user, so the grace period is measured
// in tens of seconds rather than the usual five.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 2)

	go func() {
		s.log.Info("api listening", "addr", s.api.Addr)
		if err := s.api.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("api server: %w", err)
			return
		}
		errCh <- nil
	}()

	if s.metrics != nil {
		go func() {
			s.log.Info("metrics listening", "addr", s.metrics.Addr)
			if err := s.metrics.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("metrics server: %w", err)
				return
			}
			errCh <- nil
		}()
	}

	select {
	case err := <-errCh:
		if err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		s.log.Info("shutdown signal received, draining", "grace", s.grace)
	}

	// Deliberately rooted at Background rather than derived from ctx: ctx is already
	// cancelled by the time we get here, and a derived context would give the drain a
	// grace period of zero, which is the opposite of graceful.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.grace)
	defer cancel()

	var firstErr error
	if s.metrics != nil {
		if err := s.metrics.Shutdown(shutdownCtx); err != nil { //nolint:contextcheck // see above
			firstErr = fmt.Errorf("metrics shutdown: %w", err)
		}
	}
	if err := s.api.Shutdown(shutdownCtx); err != nil { //nolint:contextcheck // see above
		if errors.Is(err, context.DeadlineExceeded) {
			s.log.Warn("grace period elapsed with requests still in flight; closing")
			_ = s.api.Close()
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("api shutdown: %w", err)
		}
	}
	s.log.Info("drained")
	return firstErr
}

// ChatEndpoints is the subset of the chat use case the router mounts. Declaring it here rather
// than importing a concrete handler keeps the composition root free to swap implementations and
// lets the router be tested with a stub.
type ChatEndpoints interface {
	Completions(w http.ResponseWriter, r *http.Request)
	Embeddings(w http.ResponseWriter, r *http.Request)
}

func notFoundBody(path string) openaiapi.ErrorResponse {
	return openaiapi.NewError(fmt.Sprintf("Unknown endpoint %q. This gateway implements the "+
		"OpenAI chat completions, embeddings and models endpoints under /v1.", path),
		openaiapi.ErrTypeInvalidRequest, "unknown_endpoint", "")
}

func methodNotAllowedBody(method string) openaiapi.ErrorResponse {
	return openaiapi.NewError(fmt.Sprintf("Method %s is not allowed on this endpoint.", method),
		openaiapi.ErrTypeInvalidRequest, "method_not_allowed", "")
}
