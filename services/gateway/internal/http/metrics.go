package http

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// RequestRecorder is the slice of telemetry this package needs.
//
// Declaring it here rather than importing internal/telemetry keeps the delivery layer free of a
// metrics dependency and lets tests assert on recorded values with a fake.
type RequestRecorder interface {
	ObserveRequest(route, method string, status int, cached bool, d time.Duration)
	IncInFlight()
	DecInFlight()
}

// HeaderCacheStatus tells the caller whether the semantic cache served the response. It is also
// what the metrics middleware reads to label request duration, so the cached and uncached p50
// figures in the README come from the same signal a client can see.
const HeaderCacheStatus = "X-LLMRouter-Cache"

// Metrics records one observation per request.
//
// The route label is chi's registered pattern ("/v1/chat/completions"), never the raw URL. Using
// the raw path would make every request id its own time series and melt Prometheus.
func Metrics(rec RequestRecorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rec == nil {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			rec.IncInFlight()
			defer rec.DecInFlight()

			sr, ok := w.(*statusRecorder)
			if !ok {
				sr = &statusRecorder{ResponseWriter: w, status: http.StatusOK}
				w = sr
			}

			next.ServeHTTP(w, r)

			rec.ObserveRequest(
				routePattern(r),
				r.Method,
				sr.status,
				sr.Header().Get(HeaderCacheStatus) == "hit",
				time.Since(start),
			)
		})
	}
}

// routePattern returns the matched chi pattern, falling back to a constant for unmatched paths
// so that a scan for nonexistent URLs cannot create unbounded label cardinality.
func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil {
		if p := rc.RoutePattern(); p != "" {
			return p
		}
	}
	return "unmatched"
}
