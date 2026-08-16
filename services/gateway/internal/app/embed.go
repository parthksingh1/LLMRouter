package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// EmbedServiceDeps is the injected collaborator set for embeddings.
type EmbedServiceDeps struct {
	Registry ProviderRegistry
	Budget   Budget
	Events   EventSink
	Recorder Recorder
	Clock    Clock
	Log      *slog.Logger

	// ModelOwner resolves an embedding model id to the provider that serves it.
	ModelOwner func(modelID string) (domain.ModelDescriptor, bool)
	// DefaultModel is used when the caller does not name one.
	DefaultModel string
}

// EmbedService serves /v1/embeddings.
//
// Embeddings are deliberately simpler than chat: no routing policy, no cache, no guardrails.
// Routing an embedding across vendors would be actively wrong -- vectors from different models
// are not comparable, so a "failover" would silently corrupt whatever index they are stored in.
// The request therefore either goes to the model that was asked for, or it fails.
type EmbedService struct {
	deps EmbedServiceDeps
}

// NewEmbedService builds the use case.
func NewEmbedService(deps EmbedServiceDeps) (*EmbedService, error) {
	if deps.Registry == nil {
		return nil, fmt.Errorf("embed service requires a provider registry")
	}
	if deps.ModelOwner == nil {
		return nil, fmt.Errorf("embed service requires a model resolver")
	}
	if deps.Clock == nil {
		deps.Clock = SystemClock{}
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	return &EmbedService{deps: deps}, nil
}

// Embed produces embedding vectors.
func (s *EmbedService) Embed(ctx context.Context, req domain.EmbedRequest, tenant domain.Tenant) (domain.EmbedResponse, error) {
	if req.Model == "" {
		req.Model = s.deps.DefaultModel
	}

	model, ok := s.deps.ModelOwner(req.Model)
	if !ok {
		return domain.EmbedResponse{}, fmt.Errorf("%w: %q", ErrModelNotFound, req.Model)
	}
	if !tenant.ModelAllowed(model.ID) {
		return domain.EmbedResponse{}, fmt.Errorf("%w: tenant %s may not use %s",
			ErrModelNotAllowed, tenant.ID, model.ID)
	}

	provider, ok := s.deps.Registry.Get(model.Provider)
	if !ok {
		return domain.EmbedResponse{}, fmt.Errorf("%w: no adapter for provider %q",
			ErrNoProviderAvailable, model.Provider)
	}

	start := s.deps.Clock.Now()
	req.Model = model.ID
	resp, err := provider.Embed(ctx, req)
	elapsed := s.deps.Clock.Now().Sub(start)

	if s.deps.Recorder != nil {
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		s.deps.Recorder.ObserveUpstream(model.Provider, model.ID, outcome, elapsed)
	}
	if err != nil {
		return domain.EmbedResponse{}, fmt.Errorf("%w: %s: %s", ErrUpstream, model.Provider, err)
	}

	costUSD := model.CostUSD(resp.Usage)
	if s.deps.Recorder != nil {
		s.deps.Recorder.ObserveUsage(tenant.ID, model.Provider, model.ID,
			resp.Usage.PromptTokens, 0, costUSD)
	}
	if s.deps.Budget != nil {
		if err := s.deps.Budget.Commit(ctx, tenant, resp.Usage, costUSD); err != nil {
			s.deps.Log.Warn("budget commit failed for embeddings", "tenant_id", tenant.ID, "error", err)
		}
	}
	if s.deps.Events != nil {
		s.deps.Events.Emit(ctx, domain.RequestEvent{
			TS:           start,
			RequestID:    req.RequestID,
			TenantID:     tenant.ID,
			Policy:       "embeddings",
			Model:        model.ID,
			Provider:     model.Provider,
			PromptTokens: resp.Usage.PromptTokens,
			StatusCode:   200,
			LatencyMS:    int(elapsed.Milliseconds()),
			CostUSD:      costUSD,
		})
	}
	return resp, nil
}
