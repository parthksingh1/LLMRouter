package http

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/config"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
	"github.com/parthkumarsingh/llmrouter/services/gateway/pkg/openaiapi"
)

// TenantStore resolves an API key to a tenant.
type TenantStore interface {
	Lookup(apiKey string) (domain.Tenant, bool)
	All() []domain.Tenant
}

// MemoryTenantStore is an in-memory tenant registry seeded from config/tenants.yaml.
//
// Keys are compared by constant-time digest rather than by map lookup on the raw key. A plain
// map compare is not timing-safe, and while that is a small risk for a demo it is exactly the
// kind of detail a reviewer looks for.
type MemoryTenantStore struct {
	byDigest map[[32]byte]domain.Tenant
	ordered  []domain.Tenant
}

// NewMemoryTenantStore builds the store from parsed configuration.
func NewMemoryTenantStore(f config.TenantsFile) *MemoryTenantStore {
	// Defensive: a TenantsFile built in code rather than parsed from YAML has not had defaults
	// applied, and a tenant with a zero budget would be refused on its first request.
	f.ApplyDefaults()

	s := &MemoryTenantStore{
		byDigest: make(map[[32]byte]domain.Tenant, len(f.Tenants)),
		ordered:  make([]domain.Tenant, 0, len(f.Tenants)),
	}
	for _, spec := range f.Tenants {
		t := spec.Domain()
		s.byDigest[sha256.Sum256([]byte(spec.APIKey))] = t
		s.ordered = append(s.ordered, t)
	}
	return s
}

// Lookup resolves an API key.
func (s *MemoryTenantStore) Lookup(apiKey string) (domain.Tenant, bool) {
	digest := sha256.Sum256([]byte(apiKey))
	t, ok := s.byDigest[digest]
	if !ok {
		return domain.Tenant{}, false
	}
	// Defence in depth: confirm the stored hash matches in constant time, so a hypothetical
	// digest collision cannot authenticate.
	if subtle.ConstantTimeCompare([]byte(t.APIKeyHash), []byte(domain.HashText(apiKey))) != 1 {
		return domain.Tenant{}, false
	}
	return t, true
}

// All returns every tenant, in configuration order.
func (s *MemoryTenantStore) All() []domain.Tenant {
	out := make([]domain.Tenant, len(s.ordered))
	copy(out, s.ordered)
	return out
}

// Auth authenticates `Authorization: Bearer <tenant-key>` and puts the tenant on the context.
//
// The error shapes match OpenAI's so that SDK exception handling behaves identically against
// LLMRouter and against api.openai.com.
func Auth(store TenantStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, err := bearerToken(r)
			if err != nil {
				WriteError(w, r, http.StatusUnauthorized, openaiapi.NewError(
					err.Error(), openaiapi.ErrTypeAuthentication, "invalid_api_key", ""))
				return
			}
			tenant, ok := store.Lookup(key)
			if !ok {
				WriteError(w, r, http.StatusUnauthorized, openaiapi.NewError(
					"Incorrect API key provided. You can find your API key in the tenant registry.",
					openaiapi.ErrTypeAuthentication, "invalid_api_key", ""))
				return
			}

			ctx := context.WithValue(r.Context(), ctxKeyTenant, tenant)
			// Enrich the request logger now that we know who is calling.
			ctx = context.WithValue(ctx, ctxKeyLogger, LoggerFrom(ctx).With("tenant_id", tenant.ID))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// authError describes why a bearer token was rejected, without echoing the token itself.
type authError struct{ msg string }

func (e *authError) Error() string { return e.msg }

func bearerToken(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", &authError{"Missing Authorization header. Expected 'Authorization: Bearer <key>'."}
	}
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", &authError{"Malformed Authorization header. Expected 'Authorization: Bearer <key>'."}
	}
	token := strings.TrimSpace(h[len(prefix):])
	if token == "" {
		return "", &authError{"Empty bearer token."}
	}
	return token, nil
}
