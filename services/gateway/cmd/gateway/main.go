// Command gateway is the LLMRouter OpenAI-compatible gateway.
//
// This file is the composition root: it is the only place that constructs concrete adapters and
// wires them into the use cases. Everything below it depends on interfaces, which is what makes
// the offline mock stack and a live deployment the same program with different configuration.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	gwhttp "github.com/parthkumarsingh/llmrouter/services/gateway/internal/http"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/providers"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/router"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/stream"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/telemetry"
)

// version is overwritten at build time with -ldflags "-X main.version=...".
var version = "dev"

// defaultEmbeddingModel is used when a caller omits the model on /v1/embeddings. It matches the
// model the semantic cache uses, so an application can build an index with the same vectors the
// gateway caches against.
const defaultEmbeddingModel = "text-embedding-3-small"

func main() {
	healthcheck := flag.Bool("healthcheck", false,
		"probe the local gateway and exit 0 if healthy; used by the container HEALTHCHECK")
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck())
	}

	if err := run(); err != nil {
		// The logger may not exist yet if configuration failed, so fall back to stderr.
		fmt.Fprintf(os.Stderr, `{"level":"error","msg":"gateway failed to start","error":%q}`+"\n", err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	log := newLogger(cfg.Log)
	slog.SetDefault(log)

	log.Info("starting llmrouter gateway",
		"version", version,
		"env", cfg.Env,
		"provider_mode", cfg.Provider.Mode,
		"providers", len(cfg.Providers.Providers),
		"tenants", len(cfg.Tenants.Tenants),
		"default_policy", cfg.Policies.DefaultPolicy,
	)

	// Signals first, so a Ctrl-C during a slow start-up still exits cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	metrics := telemetry.NewMetrics()

	tracing, err := telemetry.NewTracing(ctx, telemetry.TracingOptions{
		Endpoint:    cfg.Telemetry.OTLPEndpoint,
		ServiceName: cfg.Telemetry.ServiceName,
		Version:     version,
		Environment: cfg.Env,
		SampleRatio: cfg.Telemetry.SampleRatio,
		Log:         log,
	})
	if err != nil {
		// Not fatal. A gateway that will not start because its trace collector is down has
		// turned an observability dependency into an availability dependency.
		log.Warn("tracing could not be initialised; continuing without it", "error", err)
		tracing = &telemetry.Tracing{}
	}
	defer func() {
		if err := tracing.Shutdown(context.Background()); err != nil {
			log.Warn("flushing traces failed", "error", err)
		}
	}()

	adapters, err := providers.Build(cfg, log)
	if err != nil {
		return fmt.Errorf("building provider adapters: %w", err)
	}

	registry, err := providers.NewRegistry(cfg.Providers, adapters, log, time.Now)
	if err != nil {
		return fmt.Errorf("building provider registry: %w", err)
	}
	registry.Start(ctx)
	defer registry.Stop()

	// Republish provider health as metrics on the same cadence as the health checks.
	go publishProviderHealth(ctx, registry, metrics, cfg.Providers.Defaults.HealthInterval())

	deps := wireOptional(ctx, cfg, log, metrics)
	defer deps.close()

	engine, err := router.New(cfg.Providers, cfg.Policies, registry)
	if err != nil {
		return fmt.Errorf("building the policy engine: %w", err)
	}
	log.Info("policy engine ready", "policies", engine.PolicyNames())

	streamRunner, err := stream.NewRunner(stream.Options{
		Registry:     registry,
		StallTimeout: cfg.Providers.Defaults.StallTimeout(),
		MaxAttempts:  cfg.Policies.Constraints.MaxAttempts,
		Recorder:     metrics,
		Log:          log,
	})
	if err != nil {
		return fmt.Errorf("building the stream runner: %w", err)
	}

	chatSvc, err := app.NewChatService(app.ChatServiceDeps{
		Router:      engine,
		Registry:    registry,
		Cache:       deps.cache,
		Guardrails:  deps.guardrails,
		Budget:      deps.budget,
		Stream:      streamRunner,
		Events:      deps.events,
		Recorder:    metrics,
		Log:         log,
		MaxAttempts: cfg.Policies.Constraints.MaxAttempts,
	})
	if err != nil {
		return fmt.Errorf("building the chat service: %w", err)
	}

	embedSvc, err := app.NewEmbedService(app.EmbedServiceDeps{
		Registry:     registry,
		Budget:       deps.budget,
		Events:       deps.events,
		Recorder:     metrics,
		Log:          log,
		ModelOwner:   cfg.Providers.FindModel,
		DefaultModel: defaultEmbeddingModel,
	})
	if err != nil {
		return fmt.Errorf("building the embeddings service: %w", err)
	}

	tenants := gwhttp.NewMemoryTenantStore(cfg.Tenants)
	meta := gwhttp.NewMetaHandler(cfg.Providers, cfg.Policies, registry, version)
	chat := gwhttp.NewChatHandler(chatSvc, embedSvc)

	handler := gwhttp.NewRouter(gwhttp.Deps{
		Log:            log,
		Version:        version,
		Tenants:        tenants,
		Meta:           meta,
		Chat:           chat,
		MetricsHandler: metrics.Handler(),
		Recorder:       metrics,
		TracingEnabled: tracing.Enabled(),
		RequestTimeout: cfg.Server.ReadTimeout,
	})

	srv := gwhttp.NewServer(
		cfg.Server.Addr,
		cfg.Server.MetricsAddr,
		handler,
		metrics.Handler(),
		cfg.Server.ReadTimeout,
		cfg.Server.WriteTimeout,
		cfg.Server.IdleTimeout,
		cfg.Server.ShutdownGrace,
		log,
	)

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("serving: %w", err)
	}
	log.Info("gateway stopped cleanly")
	return nil
}

// publishProviderHealth mirrors registry state into Prometheus gauges.
//
// It is a separate loop rather than a callback on the registry so that the registry stays free
// of any telemetry dependency and can be unit-tested without a metrics registry.
func publishProviderHealth(
	ctx context.Context,
	registry *providers.Registry,
	metrics *telemetry.Metrics,
	interval time.Duration,
) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	publish := func() {
		for _, s := range registry.Snapshot() {
			metrics.SetProviderHealth(s.Name, s.Healthy, string(s.Breaker))
		}
	}
	publish()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publish()
		}
	}
}

// newLogger builds the structured logger. JSON in every environment except an explicit
// console override, because logs are correlated with traces downstream and a human-formatted
// log line is not parseable by the collector.
func newLogger(cfg config.LogConfig) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			// Belt and braces: even if something tries to log a credential, drop it here.
			switch a.Key {
			case "authorization", "api_key", "apikey", "password", "secret", "token":
				return slog.Attr{Key: a.Key, Value: slog.StringValue("[redacted]")}
			}
			return a
		},
	}

	var h slog.Handler
	if cfg.Format == "console" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h).With("service", "llmrouter-gateway", "version", version)
}

// runHealthcheck probes the local readiness endpoint. Living in the same binary means the
// container image needs no curl, which keeps it distroless-small.
func runHealthcheck() int {
	addr := os.Getenv("GATEWAY_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if addr[0] == ':' {
		addr = "localhost" + addr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/readyz", nil)
	if err != nil {
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 1
		}
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
