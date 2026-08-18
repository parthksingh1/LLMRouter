package http

import (
	"net/http"
	"sort"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/providers"
	"github.com/parthkumarsingh/llmrouter/services/gateway/pkg/openaiapi"
)

// MetaHandler serves the endpoints that describe the gateway itself rather than proxying a
// generation: model discovery, liveness and readiness.
type MetaHandler struct {
	catalogue config.ProvidersFile
	policies  config.PoliciesFile
	registry  *providers.Registry
	version   string
	startedAt time.Time
}

// NewMetaHandler builds the meta handler.
func NewMetaHandler(
	catalogue config.ProvidersFile,
	policies config.PoliciesFile,
	registry *providers.Registry,
	version string,
) *MetaHandler {
	return &MetaHandler{
		catalogue: catalogue,
		policies:  policies,
		registry:  registry,
		version:   version,
		startedAt: time.Now(),
	}
}

// Models serves GET /v1/models.
//
// Alongside the concrete models it advertises the virtual ones ("auto", "auto:cheap", ...), so a
// caller listing models discovers routing without reading the documentation. Virtual entries are
// flagged, and carry no pricing, because their price depends on what the router picks.
func (h *MetaHandler) Models(w http.ResponseWriter, r *http.Request) {
	tenant, hasTenant := TenantFrom(r.Context())
	created := h.startedAt.Unix()

	list := openaiapi.ModelList{Object: openaiapi.ObjectList}

	for _, alias := range sortedKeys(h.policies.VirtualModels) {
		list.Data = append(list.Data, openaiapi.Model{
			ID:      alias,
			Object:  openaiapi.ObjectModel,
			Created: created,
			OwnedBy: "llmrouter",
			Virtual: true,
			Family:  h.policies.VirtualModels[alias],
		})
	}

	for _, m := range h.catalogue.Descriptors() {
		// A tenant only sees what it is allowed to call; listing models it would be refused
		// for is a confusing API.
		if hasTenant && !tenant.ModelAllowed(m.ID) {
			continue
		}
		list.Data = append(list.Data, openaiapi.Model{
			ID:            m.ID,
			Object:        openaiapi.ObjectModel,
			Created:       created,
			OwnedBy:       m.Provider,
			Provider:      m.Provider,
			Family:        m.Family,
			Tier:          m.Tier,
			ContextWindow: m.ContextWindow,
			PriceInPerM:   m.PriceInPerM,
			PriceOutPerM:  m.PriceOutPerM,
		})
	}

	WriteJSON(w, r, http.StatusOK, list)
}

// healthResponse is the body of /healthz and /readyz.
type healthResponse struct {
	Status    string                     `json:"status"`
	Version   string                     `json:"version"`
	UptimeSec int64                      `json:"uptime_sec"`
	Providers []providers.HealthSnapshot `json:"providers,omitempty"`
}

// Healthz is liveness: the process is up and can serve. It deliberately does not consult
// dependencies -- a liveness probe that fails on a downstream outage causes a restart loop that
// makes the outage worse.
func (h *MetaHandler) Healthz(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, r, http.StatusOK, healthResponse{
		Status:    "ok",
		Version:   h.version,
		UptimeSec: int64(time.Since(h.startedAt).Seconds()),
	})
}

// Readyz is readiness: can this instance usefully take traffic right now?
//
// The bar is "at least one provider is dispatchable". One degraded provider is not an outage --
// that is what routing and failover are for -- but zero means every request would 503, so the
// instance should be pulled from the load balancer.
func (h *MetaHandler) Readyz(w http.ResponseWriter, r *http.Request) {
	if h.registry == nil {
		// No registry means no upstreams are wired, which is a valid configuration only in
		// tests. Report it rather than panicking on a probe.
		WriteJSON(w, r, http.StatusServiceUnavailable, healthResponse{
			Status:    "no provider registry configured",
			Version:   h.version,
			UptimeSec: int64(time.Since(h.startedAt).Seconds()),
		})
		return
	}

	snapshot := h.registry.Snapshot()
	ready := h.registry.AnyHealthy()

	status := http.StatusOK
	label := "ready"
	if !ready {
		status = http.StatusServiceUnavailable
		label = "no provider available"
	}

	WriteJSON(w, r, status, healthResponse{
		Status:    label,
		Version:   h.version,
		UptimeSec: int64(time.Since(h.startedAt).Seconds()),
		Providers: snapshot,
	})
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
