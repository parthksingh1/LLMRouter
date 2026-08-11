package providers

import (
	"fmt"
	"log/slog"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/providers/live"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/providers/mock"
)

// Build constructs one adapter per configured provider.
//
// The mock and live branches produce values satisfying the same port, so nothing downstream --
// router, dispatcher, failover state machine -- can tell which mode it is running in. That is
// the property that makes the offline demo meaningful rather than a separate code path.
func Build(cfg *config.Config, log *slog.Logger) ([]app.Provider, error) {
	if cfg.Provider.IsMock() {
		return buildMock(cfg, log)
	}
	return buildLive(cfg)
}

func buildMock(cfg *config.Config, log *slog.Logger) ([]app.Provider, error) {
	fixtures, err := mock.LoadFixtures(cfg.Provider.FixturesPath)
	if err != nil {
		return nil, fmt.Errorf("loading mock fixtures: %w", err)
	}
	if log != nil {
		log.Info("mock providers armed",
			"fixtures", fixtures.Len(),
			"path", cfg.Provider.FixturesPath,
			"midstream_fail_rate", cfg.Provider.MockMidstreamFailRate,
		)
	}

	out := make([]app.Provider, 0, len(cfg.Providers.Providers))
	for _, spec := range cfg.Providers.Providers {
		out = append(out, mock.New(mock.Options{
			Spec:                     spec,
			Fixtures:                 fixtures,
			Seed:                     cfg.Provider.MockSeed,
			Speed:                    1,
			FailAfter:                cfg.Provider.MockFailAfter[spec.Name],
			FailMidstream:            cfg.Provider.MockFailMidstream[spec.Name],
			MidstreamFailRate:        cfg.Provider.MockMidstreamFailRate,
			MidstreamFailAfterTokens: cfg.Provider.MockMidstreamFailAfterTk,
			StallProbability:         cfg.Provider.MockStallProbability,
		}))
	}
	return out, nil
}

func buildLive(cfg *config.Config) ([]app.Provider, error) {
	timeout := cfg.Providers.Defaults.RequestTimeout()
	out := make([]app.Provider, 0, len(cfg.Providers.Providers))

	for _, spec := range cfg.Providers.Providers {
		key := cfg.Provider.APIKeys[spec.Name]
		if key == "" {
			return nil, fmt.Errorf("provider %q: %s is not set", spec.Name, spec.APIKeyEnv)
		}
		switch spec.Name {
		case "anthropic":
			out = append(out, live.NewAnthropic(spec, key, timeout))
		case "google":
			out = append(out, live.NewGoogle(spec, key, timeout))
		case "openai", "mistral", "together":
			out = append(out, live.NewOpenAICompatible(spec, key, timeout))
		default:
			// A new OpenAI-compatible vendor needs only a catalogue entry, not code. Anything
			// with a bespoke protocol has to be added here explicitly rather than silently
			// mistranslated.
			out = append(out, live.NewOpenAICompatible(spec, key, timeout))
		}
	}
	return out, nil
}
