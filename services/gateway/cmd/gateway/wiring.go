package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/budget"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/cache"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/events"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/guardrails"
)

// optional holds the collaborators that the gateway can run without.
//
// Every one of these is a dependency on another process. Treating "not reachable" as fatal
// would mean the gateway cannot start until Redis, Qdrant, the embedder and the guardrails
// sidecar are all up -- which turns four independent services into one distributed single
// point of failure. Instead each is wired if it can be reached and left nil if it cannot, and
// the use case degrades that feature rather than the request.
type optional struct {
	cache      app.SemanticCache
	guardrails app.Guardrails
	budget     app.Budget
	events     app.EventSink

	closers []func() error
}

// close releases everything that was successfully wired.
func (o *optional) close() {
	for _, fn := range o.closers {
		if err := fn(); err != nil {
			slog.Default().Warn("closing a dependency failed", "error", err)
		}
	}
}

// wireOptional builds the optional collaborators, logging what came up and what did not.
//
// The probe is deliberately short: a slow dependency at start-up is treated as absent rather
// than delaying the listener, because a gateway that is not listening fails a readiness check
// and gets restarted, which does not help.
func wireOptional(ctx context.Context, cfg *config.Config, log *slog.Logger, dropped events.Dropped) *optional {
	out := &optional{}

	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	// --- semantic cache ------------------------------------------------------------------
	if cfg.Cache.Enabled {
		blobs, err := cache.NewRedisBlobs(cfg.Cache.RedisURL)
		switch {
		case err != nil:
			log.Warn("semantic cache disabled: redis url is invalid", "error", err)
		default:
			if err := blobs.Ping(probeCtx); err != nil {
				log.Warn("semantic cache disabled: redis is unreachable",
					"url", cfg.Cache.RedisURL, "error", err)
				_ = blobs.Close()
				break
			}

			// The vector store and the embedder are optional *within* the cache: without them
			// the exact tier still works, and the calibration shows the exact tier carries most
			// of the value anyway.
			var vectors cache.VectorStore
			var embedder cache.Embedder

			qdrant := cache.NewQdrantVectors(cfg.Cache.QdrantURL, cfg.Cache.QdrantCollection, cfg.Cache.EmbeddingDim)
			embedderClient := cache.NewHTTPEmbedder(cfg.Cache.EmbedderURL, 250*time.Millisecond)

			qdrantErr := qdrant.Ping(probeCtx)
			mode, embedderErr := embedderClient.Ping(probeCtx)

			if qdrantErr == nil && embedderErr == nil {
				vectors, embedder = qdrant, embedderClient
				log.Info("semantic cache tier enabled",
					"embedder_mode", mode, "threshold", cfg.Cache.SimilarityThreshold)
			} else {
				log.Warn("semantic cache running with the exact tier only",
					"qdrant_error", qdrantErr, "embedder_error", embedderErr)
			}

			store, err := cache.New(cache.Options{
				Blobs:     blobs,
				Vectors:   vectors,
				Embedder:  embedder,
				Threshold: cfg.Cache.SimilarityThreshold,
				TTL:       cfg.Cache.TTL,
				MaxChars:  cfg.Cache.MaxPromptChars,
				Log:       log,
			})
			if err != nil {
				log.Warn("semantic cache disabled", "error", err)
				_ = blobs.Close()
				break
			}
			out.cache = store
			out.closers = append(out.closers, blobs.Close)
		}
	} else {
		log.Info("semantic cache disabled by configuration")
	}

	// --- guardrails ----------------------------------------------------------------------
	if cfg.Guard.Enabled {
		client := guardrails.New(guardrails.Options{
			BaseURL:  cfg.Guard.URL,
			Timeout:  cfg.Guard.Timeout,
			FailOpen: cfg.Guard.FailOpen(),
			Log:      log,
		})
		// Wired even when the probe fails: the client's own fail-open/fail-closed policy is the
		// right place for that decision, and a sidecar that is down at start-up is usually up a
		// few seconds later.
		if err := client.Ping(probeCtx); err != nil {
			log.Warn("guardrails sidecar is not reachable yet",
				"url", cfg.Guard.URL, "fail_mode", cfg.Guard.FailMode, "error", err)
		} else {
			log.Info("guardrails enabled", "url", cfg.Guard.URL, "fail_mode", cfg.Guard.FailMode)
		}
		out.guardrails = client
	} else {
		log.Warn("guardrails disabled by configuration: prompts will not be screened")
	}

	// --- budgets -------------------------------------------------------------------------
	if cfg.Budget.Enabled {
		enforcer, err := budget.New(budget.Options{RedisURL: cfg.Budget.RedisURL, Log: log})
		switch {
		case err != nil:
			log.Warn("per-tenant budgets disabled: redis url is invalid", "error", err)
		default:
			if err := enforcer.Ping(probeCtx); err != nil {
				log.Warn("per-tenant budgets disabled: redis is unreachable",
					"url", cfg.Budget.RedisURL, "error", err)
				_ = enforcer.Close()
				break
			}
			log.Info("per-tenant budgets enabled")
			out.budget = enforcer
			out.closers = append(out.closers, enforcer.Close)
		}
	} else {
		log.Warn("per-tenant budgets disabled by configuration")
	}

	// --- analytics -----------------------------------------------------------------------
	sink := events.New(events.Options{
		URL:       cfg.Events.ClickHouseURL,
		Database:  cfg.Events.Database,
		User:      cfg.Events.User,
		Password:  cfg.Events.Password,
		BatchSize: cfg.Events.BatchSize,
		Interval:  cfg.Events.FlushInterval,
		Log:       log,
		Dropped:   dropped,
	})
	// Wired even if ClickHouse is down: the sink buffers and drops rather than blocking, so an
	// analytics outage costs analytics and nothing else. Losing the wiring entirely would mean
	// a restart is needed once ClickHouse comes back.
	if err := sink.Ping(probeCtx); err != nil {
		log.Warn("clickhouse is not reachable yet; analytics will be dropped until it is",
			"url", cfg.Events.ClickHouseURL, "error", err)
	} else {
		log.Info("analytics enabled", "url", cfg.Events.ClickHouseURL, "batch", cfg.Events.BatchSize)
	}
	out.events = sink
	out.closers = append(out.closers, sink.Close)

	return out
}
