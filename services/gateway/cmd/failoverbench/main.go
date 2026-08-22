// Command failoverbench measures stream completion under mid-stream provider failure.
//
//	go run ./cmd/failoverbench -streams 10000 -failure-rate 0.05 -out ../../benchmarks/results/failover.json
//
// This drives the real code: the actual stream.Runner, the actual mock providers, the actual
// prefix-continuation logic. It is not a model of failover, which is the point -- a simulation
// would prove only that the simulation works.
//
// A stream counts as COMPLETE when the client received a terminating finish reason and the
// concatenated deltas form a whole answer. A stream that was truncated, or that ended with an
// error after the client had already seen tokens, counts as failed. That is a deliberately
// strict definition: from the caller's point of view a half-answer is a failure even though the
// HTTP status said 200.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/providers"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/providers/mock"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/router"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/stream"
)

// collector is a Sink that records what a client would have received.
type collector struct {
	text      strings.Builder
	failovers []stream.FailoverEvent
	summary   stream.Summary
	done      bool
}

func (c *collector) Delta(text string) error {
	c.text.WriteString(text)
	return nil
}

func (c *collector) Failover(event stream.FailoverEvent) error {
	c.failovers = append(c.failovers, event)
	return nil
}

func (c *collector) Done(summary stream.Summary) error {
	c.summary = summary
	c.done = true
	return nil
}

type outcome struct {
	completed   bool
	failovers   int
	restarted   bool
	tokens      int
	usageTokens int
	durationMS  float64
	err         string
}

func main() {
	streams := flag.Int("streams", 10000, "number of streams to run")
	failureRate := flag.Float64("failure-rate", 0.05, "probability that a stream breaks mid-generation")
	stallRate := flag.Float64("stall-rate", 0.01, "probability that a stream goes quiet instead of erroring")
	seed := flag.Int64("seed", 1337, "rng seed")
	speed := flag.Float64("speed", 4000, "how much faster than real time the mock providers run")
	workers := flag.Int("workers", runtime.NumCPU(), "concurrent streams")
	stallTimeout := flag.Duration("stall-timeout", 250*time.Millisecond,
		"how long a stream may go quiet before it is declared dead; must stay well above "+
			"scheduler jitter or the benchmark measures the Go runtime rather than failover")
	out := flag.String("out", "", "write results JSON here")
	configDir := flag.String("config", "", "path to the config directory")
	flag.Parse()

	dir := *configDir
	if dir == "" {
		dir = filepath.Join("..", "..", "..", "..", "config")
	}

	catalogue, err := config.LoadProviders(filepath.Join(dir, "providers.yaml"))
	if err != nil {
		fail("loading providers: %v", err)
	}
	policies, err := config.LoadPolicies(filepath.Join(dir, "policies.yaml"))
	if err != nil {
		fail("loading policies: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Failure injection is applied to every provider, so a fallback can break too -- which is
	// the case that actually decides whether the chain holds up.
	adapters := make([]app.Provider, 0, len(catalogue.Providers))
	for _, spec := range catalogue.Providers {
		adapters = append(adapters, mock.New(mock.Options{
			Spec:                     spec,
			Seed:                     *seed,
			Speed:                    *speed,
			MidstreamFailRate:        *failureRate,
			StallProbability:         *stallRate,
			MidstreamFailAfterTokens: 12,
		}))
	}

	registry, err := providers.NewRegistry(catalogue, adapters, log, time.Now)
	if err != nil {
		fail("building registry: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry.Start(ctx)
	defer registry.Stop()

	engine, err := router.New(catalogue, policies, registry)
	if err != nil {
		fail("building router: %v", err)
	}

	// The stall timeout is NOT scaled by the accelerated clock, and that is deliberate.
	//
	// At speed=4000 the configured 4s timeout would become 1ms, while the gap between tokens is
	// a few microseconds -- and Go's scheduler jitter under load comfortably exceeds 1ms. The
	// first version of this benchmark did scale it, and reported a 67% completion rate that was
	// entirely scheduler noise being misread as stalled upstreams. The timeout has to stay far
	// above the runtime's own latency for the measurement to be about failover at all.
	runner, err := stream.NewRunner(stream.Options{
		Registry:     registry,
		StallTimeout: *stallTimeout,
		MaxAttempts:  policies.Constraints.MaxAttempts,
		Log:          log,
	})
	if err != nil {
		fail("building runner: %v", err)
	}

	tenant := domain.Tenant{ID: "bench", DefaultPolicy: policies.DefaultPolicy, AllowedModels: []string{"*"}}

	prompts := []string{
		"Explain how a write-ahead log works.",
		"Derive the complexity and analyse the trade-off in a distributed system.",
		"What is the capital of Peru?",
		"Write a runbook entry for the on-call engineer.",
		"Debug the race condition in the concurrent queue and explain the root cause.",
	}

	results := make([]outcome, *streams)
	var wg sync.WaitGroup
	jobs := make(chan int, *workers*2)

	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			// Each worker gets its own RNG so runs are reproducible for a fixed worker count.
			rng := rand.New(rand.NewSource(*seed + int64(worker)*7919)) //nolint:gosec // simulation
			for i := range jobs {
				results[i] = runOne(ctx, runner, engine, tenant, prompts[rng.Intn(len(prompts))])
			}
		}(w)
	}

	started := time.Now()
	for i := 0; i < *streams; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(started)

	report(results, elapsed, *streams, *failureRate, *stallRate, *seed, *out)
}

func runOne(
	ctx context.Context,
	runner *stream.Runner,
	engine *router.Engine,
	tenant domain.Tenant,
	prompt string,
) outcome {
	req := domain.ChatRequest{
		RequestID:      "bench",
		TenantID:       tenant.ID,
		RequestedModel: "auto",
		Stream:         true,
		Messages:       []domain.Message{{Role: domain.RoleUser, Content: prompt}},
	}

	decision, err := engine.Route(ctx, req, tenant)
	if err != nil {
		return outcome{err: "route: " + err.Error()}
	}
	fallbacks := engine.Fallbacks(ctx, req, tenant, decision)

	sink := &collector{}
	started := time.Now()
	result, err := runner.Run(ctx, req, decision, fallbacks, sink)
	duration := time.Since(started)

	o := outcome{
		failovers:   len(result.Failovers),
		restarted:   result.Restarted,
		tokens:      len(strings.Fields(sink.text.String())),
		usageTokens: result.Usage.TotalTokens(),
		durationMS:  float64(duration.Microseconds()) / 1000,
	}
	if err != nil {
		o.err = err.Error()
		return o
	}

	// Strict completion: the client saw a terminator AND received text.
	o.completed = sink.done && sink.text.Len() > 0 && result.FinishReason != ""
	return o
}

func report(results []outcome, elapsed time.Duration, streams int, failureRate, stallRate float64, seed int64, out string) {
	completed, failed, withFailover, restarted := 0, 0, 0, 0
	totalFailovers := 0
	durations := make([]float64, 0, len(results))
	errorCounts := map[string]int{}

	for _, r := range results {
		durations = append(durations, r.durationMS)
		if r.completed {
			completed++
		} else {
			failed++
			key := classify(r.err)
			errorCounts[key]++
		}
		if r.failovers > 0 {
			withFailover++
			totalFailovers += r.failovers
		}
		if r.restarted {
			restarted++
		}
	}

	completionRate := float64(completed) / float64(len(results)) * 100
	sort.Float64s(durations)

	payload := map[string]any{
		"provenance": map[string]any{
			"generated_by": "services/gateway/cmd/failoverbench",
			"generated_at": time.Now().UTC().Format(time.RFC3339),
			"mode":         "gateway",
			"seed":         seed,
			"git_sha":      gitSHA(),
			"platform":     runtime.GOOS + "-" + runtime.GOARCH,
			"go_version":   runtime.Version(),
		},
		"streams":                         streams,
		"injected_midstream_failure_rate": failureRate,
		"injected_stall_rate":             stallRate,
		"completed":                       completed,
		"failed":                          failed,
		"completion_rate_pct":             round(completionRate, 4),
		"streams_with_failover":           withFailover,
		"failover_rate_pct":               round(float64(withFailover)/float64(len(results))*100, 3),
		"total_failovers":                 totalFailovers,
		"restarted_streams":               restarted,
		"restart_rate_pct":                round(float64(restarted)/float64(len(results))*100, 3),
		"failure_reasons":                 errorCounts,
		"wall_clock_seconds":              round(elapsed.Seconds(), 2),
		"latency_ms": map[string]float64{
			"p50": round(quantile(durations, 0.50), 3),
			"p95": round(quantile(durations, 0.95), 3),
			"p99": round(quantile(durations, 0.99), 3),
			"max": round(durations[len(durations)-1], 3),
		},
		"completion_definition": "the client received a terminating finish reason AND at least " +
			"one token. A truncated stream counts as a failure even though its HTTP status was 200.",
		"note": "Mock providers run at an accelerated clock so 10,000 streams finish in seconds. " +
			"The failover logic, the stall detection and the prefix continuation are the real " +
			"implementations; only the wall time is compressed.",
	}

	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		fail("encoding results: %v", err)
	}

	if out != "" {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			fail("creating output directory: %v", err)
		}
		if err := os.WriteFile(out, append(encoded, '\n'), 0o644); err != nil { //nolint:gosec // results file
			fail("writing results: %v", err)
		}
	}

	fmt.Printf("failover bench: %d streams at a %.1f%% mid-stream failure rate\n", streams, failureRate*100)
	fmt.Printf("  completion rate  %.4f%%  (%d completed, %d failed)\n", completionRate, completed, failed)
	fmt.Printf("  failovers        %d streams recovered, %d total switches\n", withFailover, totalFailovers)
	fmt.Printf("  restarts         %d streams (%.2f%%) had to restart rather than continue\n",
		restarted, float64(restarted)/float64(len(results))*100)
	if len(errorCounts) > 0 {
		fmt.Printf("  failures         %v\n", errorCounts)
	}
	fmt.Printf("  wall clock       %.2fs\n", elapsed.Seconds())
	if out != "" {
		fmt.Printf("  wrote            %s\n", out)
	}
}

func classify(err string) string {
	switch {
	case err == "":
		return "no_terminator"
	case strings.Contains(err, "every provider attempt failed"):
		// Checked before "stalled": an exhausted chain embeds every attempt's error, and the
		// last one is often a stall. Labelling that "stalled" hides the fact that the chain
		// ran out, which is the failure that actually matters.
		return "chain_exhausted"
	case strings.Contains(err, "stalled"):
		return "stalled"
	default:
		return "other"
	}
}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(q * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func round(v float64, places int) float64 {
	factor := 1.0
	for i := 0; i < places; i++ {
		factor *= 10
	}
	return float64(int64(v*factor+0.5)) / factor
}

func gitSHA() string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
