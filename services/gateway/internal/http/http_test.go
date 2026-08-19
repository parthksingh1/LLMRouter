package http_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	gwhttp "github.com/parthkumarsingh/llmrouter/services/gateway/internal/http"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/providers"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/providers/mock"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/telemetry"
	"github.com/parthkumarsingh/llmrouter/services/gateway/pkg/openaiapi"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testProviders() config.ProvidersFile {
	return config.ProvidersFile{
		Defaults: config.ProviderDefaults{
			CircuitBreaker:        config.CircuitBreakerSpec{ConsecutiveFailures: 3, OpenDurationMS: 500},
			HealthCheckIntervalMS: 50,
		},
		Providers: []config.ProviderSpec{
			{
				Name:               "openai",
				PrefixContinuation: config.PrefixPrefill,
				Latency:            config.LatencySpec{TTFBP50MS: 5, TTFBP95MS: 10, TTFBP99MS: 20, TokensPerSec: 5000},
				Models: []config.ModelSpec{
					{ID: "gpt-4o", Family: "gpt-4", Tier: "frontier", Quality: 0.95, Context: 128000, PriceInPerM: 2.5, PriceOutPerM: 10},
					{ID: "gpt-4o-mini", Family: "gpt-4", Tier: "efficient", Quality: 0.86, Context: 128000, PriceInPerM: 0.15, PriceOutPerM: 0.6},
					{ID: "text-embedding-3-small", Family: "embedding", Tier: "embedding", Context: 8191, PriceInPerM: 0.02},
				},
			},
		},
	}
}

func testPolicies() config.PoliciesFile {
	return config.PoliciesFile{
		DefaultPolicy: "quality_tiered",
		VirtualModels: map[string]string{"auto": "quality_tiered", "auto:cheap": "cost_optimized"},
		Policies: []config.PolicySpec{
			{Name: "quality_tiered", Strategy: "quality_tiered"},
			{Name: "cost_optimized", Strategy: "cost_optimized"},
		},
	}
}

func testTenants() config.TenantsFile {
	return config.TenantsFile{
		Defaults: config.TenantDefaults{
			DailyTokenBudget: 1000, MonthlyUSDBudget: 10,
			DefaultPolicy: "quality_tiered", AllowedModels: []string{"*"}, WarnThreshold: 0.8,
		},
		Tenants: []config.TenantSpec{
			{ID: "tenant-a", Name: "Acme", APIKey: "demo-tenant-a"},
			{ID: "tenant-b", Name: "Borealis", APIKey: "demo-tenant-b", AllowedModels: []string{"gpt-4o-mini"}},
		},
	}
}

// newTestServer builds the real router over mock providers.
func newTestServer(t *testing.T) http.Handler {
	t.Helper()

	catalogue := testProviders()
	adapters := []app.Provider{mock.New(mock.Options{Spec: catalogue.Providers[0], Speed: 1000})}

	registry, err := providers.NewRegistry(catalogue, adapters, quietLogger(), time.Now)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	registry.Start(ctx)
	t.Cleanup(func() {
		cancel()
		registry.Stop()
	})

	tenantsFile := testTenants()

	metrics := telemetry.NewMetrics()
	return gwhttp.NewRouter(gwhttp.Deps{
		Log:            quietLogger(),
		Version:        "test",
		Tenants:        gwhttp.NewMemoryTenantStore(tenantsFile),
		Meta:           gwhttp.NewMetaHandler(catalogue, testPolicies(), registry, "test"),
		MetricsHandler: metrics.Handler(),
		Recorder:       metrics,
		RequestTimeout: 5 * time.Second,
	})
}

func do(t *testing.T, h http.Handler, method, path, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthentication(t *testing.T) {
	t.Parallel()
	h := newTestServer(t)

	tests := []struct {
		name       string
		header     string
		wantStatus int
		wantCode   string
	}{
		{name: "valid key", header: "Bearer demo-tenant-a", wantStatus: http.StatusOK},
		{name: "case-insensitive scheme", header: "bearer demo-tenant-a", wantStatus: http.StatusOK},
		{name: "missing header", header: "", wantStatus: http.StatusUnauthorized, wantCode: "invalid_api_key"},
		{name: "wrong scheme", header: "Basic demo-tenant-a", wantStatus: http.StatusUnauthorized, wantCode: "invalid_api_key"},
		{name: "empty token", header: "Bearer ", wantStatus: http.StatusUnauthorized, wantCode: "invalid_api_key"},
		{name: "unknown key", header: "Bearer nope", wantStatus: http.StatusUnauthorized, wantCode: "invalid_api_key"},
		{name: "key with trailing space is trimmed", header: "Bearer demo-tenant-a  ", wantStatus: http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantCode == "" {
				return
			}

			var body openaiapi.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding error body: %v", err)
			}
			if body.Error.Type != openaiapi.ErrTypeAuthentication {
				t.Errorf("error type = %q, want %q", body.Error.Type, openaiapi.ErrTypeAuthentication)
			}
			if body.Error.Code == nil || *body.Error.Code != tc.wantCode {
				t.Errorf("error code = %v, want %q", body.Error.Code, tc.wantCode)
			}
			// A rejected key must never be echoed back to the caller or into logs.
			if strings.Contains(rec.Body.String(), "nope") {
				t.Error("the rejected API key was echoed in the error body")
			}
		})
	}
}

func TestModelsListing(t *testing.T) {
	t.Parallel()
	h := newTestServer(t)

	rec := do(t, h, http.MethodGet, "/v1/models", "demo-tenant-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var list openaiapi.ModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if list.Object != openaiapi.ObjectList {
		t.Errorf("object = %q, want %q", list.Object, openaiapi.ObjectList)
	}

	byID := map[string]openaiapi.Model{}
	for _, m := range list.Data {
		byID[m.ID] = m
	}

	// Virtual models must be advertised so routing is discoverable from the API itself.
	auto, ok := byID["auto"]
	if !ok {
		t.Fatal(`"auto" is missing from /v1/models`)
	}
	if !auto.Virtual {
		t.Error(`"auto" should be flagged as virtual`)
	}

	concrete, ok := byID["gpt-4o"]
	if !ok {
		t.Fatal("gpt-4o is missing from /v1/models")
	}
	if concrete.Provider != "openai" {
		t.Errorf("gpt-4o provider = %q, want openai", concrete.Provider)
	}
	if concrete.PriceInPerM == 0 {
		t.Error("concrete models should advertise pricing")
	}
	if concrete.Object != openaiapi.ObjectModel {
		t.Errorf("object = %q, want %q", concrete.Object, openaiapi.ObjectModel)
	}
}

func TestModelsListingIsScopedToTheTenantAllowlist(t *testing.T) {
	t.Parallel()
	h := newTestServer(t)

	rec := do(t, h, http.MethodGet, "/v1/models", "demo-tenant-b")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var list openaiapi.ModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	for _, m := range list.Data {
		if m.ID == "gpt-4o" {
			t.Error("tenant-b may only use gpt-4o-mini, but gpt-4o was listed")
		}
	}
	found := false
	for _, m := range list.Data {
		if m.ID == "gpt-4o-mini" {
			found = true
		}
	}
	if !found {
		t.Error("gpt-4o-mini should be listed for tenant-b")
	}
}

func TestHealthEndpointsAreUnauthenticated(t *testing.T) {
	t.Parallel()
	h := newTestServer(t)

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := do(t, h, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200 without credentials (body: %s)",
				path, rec.Code, rec.Body.String())
		}
	}

	// /readyz must report per-provider detail so an operator can see which one is down.
	rec := do(t, h, http.MethodGet, "/readyz", "")
	var body struct {
		Status    string `json:"status"`
		Providers []struct {
			Name    string `json:"name"`
			Healthy bool   `json:"healthy"`
			Breaker string `json:"breaker"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding /readyz: %v", err)
	}
	if len(body.Providers) != 1 {
		t.Fatalf("/readyz reported %d providers, want 1", len(body.Providers))
	}
	if !body.Providers[0].Healthy {
		t.Error("the mock provider should be healthy")
	}
	if body.Providers[0].Breaker != "closed" {
		t.Errorf("breaker = %q, want closed", body.Providers[0].Breaker)
	}
}

func TestMetricsEndpointExposesGatewaySeries(t *testing.T) {
	t.Parallel()
	h := newTestServer(t)

	// Generate a little traffic first so the counters exist.
	do(t, h, http.MethodGet, "/v1/models", "demo-tenant-a")

	rec := do(t, h, http.MethodGet, "/metrics", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Counters with dynamic label sets only appear once they have been observed, so this
	// asserts on the always-present series plus the request counter the traffic above created.
	for _, want := range []string{
		"llmrouter_requests_total",
		"llmrouter_request_duration_seconds",
		"llmrouter_requests_in_flight",
		"llmrouter_cache_hit_ratio",
		"llmrouter_guardrail_duration_seconds",
		"llmrouter_events_dropped_total",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("/metrics does not expose %s", want)
		}
	}
}

func TestRequestIDIsEchoedAndSanitised(t *testing.T) {
	t.Parallel()
	h := newTestServer(t)

	t.Run("generated when absent", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/healthz", "")
		if rec.Header().Get("X-Request-Id") == "" {
			t.Error("no request id was assigned")
		}
	})

	t.Run("adopted when supplied", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("X-Request-Id", "caller-supplied-123")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if got := rec.Header().Get("X-Request-Id"); got != "caller-supplied-123" {
			t.Errorf("request id = %q, want the caller's value", got)
		}
	})

	t.Run("control characters are stripped", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("X-Request-Id", "abc\ndef")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		got := rec.Header().Get("X-Request-Id")
		if strings.ContainsAny(got, "\r\n") {
			t.Errorf("request id %q still contains control characters; log injection is possible", got)
		}
	})

	t.Run("over-long ids are truncated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("X-Request-Id", strings.Repeat("x", 500))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if got := len(rec.Header().Get("X-Request-Id")); got > 128 {
			t.Errorf("request id length = %d, want it capped at 128", got)
		}
	})
}

func TestUnknownRoutesReturnOpenAIShapedErrors(t *testing.T) {
	t.Parallel()
	h := newTestServer(t)

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{name: "unknown path", method: http.MethodGet, path: "/v1/nope", wantStatus: http.StatusNotFound},
		{name: "wrong method", method: http.MethodDelete, path: "/healthz", wantStatus: http.StatusMethodNotAllowed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := do(t, h, tc.method, tc.path, "demo-tenant-a")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			var body openaiapi.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body is not OpenAI-shaped: %v (%s)", err, rec.Body.String())
			}
			if body.Error.Message == "" || body.Error.Type == "" {
				t.Errorf("error body is missing message or type: %+v", body.Error)
			}
		})
	}
}

func TestTenantStoreLookup(t *testing.T) {
	t.Parallel()

	store := gwhttp.NewMemoryTenantStore(testTenants())

	tn, ok := store.Lookup("demo-tenant-a")
	if !ok {
		t.Fatal("demo-tenant-a should resolve")
	}
	if tn.ID != "tenant-a" {
		t.Errorf("tenant id = %q, want tenant-a", tn.ID)
	}
	if tn.DailyTokenBudget != 1000 {
		t.Errorf("defaults were not applied: budget = %d", tn.DailyTokenBudget)
	}
	if _, ok := store.Lookup("wrong"); ok {
		t.Error("an unknown key must not resolve")
	}
	if got := len(store.All()); got != 2 {
		t.Errorf("All() returned %d tenants, want 2", got)
	}
}

func TestPanicsBecomeFiveHundreds(t *testing.T) {
	t.Parallel()

	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("synthetic panic for the recovery test")
	})
	h := gwhttp.RequestID(gwhttp.Recover(quietLogger())(boom))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body openaiapi.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("panic response is not OpenAI-shaped: %v", err)
	}
	// The panic message must not leak to the caller.
	if strings.Contains(rec.Body.String(), "synthetic panic") {
		t.Error("the panic message leaked into the response body")
	}
}
