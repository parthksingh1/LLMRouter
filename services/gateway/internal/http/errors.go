package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/pkg/openaiapi"
)

// WriteJSON writes a JSON body with the given status.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The client has gone or the socket broke; there is nothing useful left to send.
		LoggerFrom(r.Context()).Debug("writing response body failed", "error", err)
	}
}

// WriteError writes an OpenAI-shaped error envelope.
func WriteError(w http.ResponseWriter, r *http.Request, status int, body openaiapi.ErrorResponse) {
	WriteJSON(w, r, status, body)
}

// WriteAppError maps a domain or application error onto the correct status and OpenAI error
// shape. Keeping this mapping in one place is what stops handlers from inventing their own
// status codes and drifting away from SDK expectations.
func WriteAppError(w http.ResponseWriter, r *http.Request, err error) {
	status, body := mapError(err)

	// A budget rejection must tell the caller when to come back; SDKs honour Retry-After.
	var budgetErr *app.BudgetError
	if errors.As(err, &budgetErr) && budgetErr.Verdict.RetryAfterSec > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(budgetErr.Verdict.RetryAfterSec))
	}

	log := LoggerFrom(r.Context())
	if status >= 500 {
		log.Error("request failed", "error", err, "status", status)
	} else {
		log.Warn("request rejected", "error", err, "status", status)
	}
	WriteError(w, r, status, body)
}

func mapError(err error) (int, openaiapi.ErrorResponse) {
	switch {
	case errors.Is(err, context.Canceled):
		// The client hung up. 499 is nginx's convention and never reaches the wire, but it
		// keeps the access log honest about who abandoned the request.
		return 499, openaiapi.NewError("request cancelled by client",
			openaiapi.ErrTypeAPI, "request_cancelled", "")

	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, openaiapi.NewError("request timed out",
			openaiapi.ErrTypeAPI, "timeout", "")

	case errors.Is(err, app.ErrModelNotFound):
		return http.StatusNotFound, openaiapi.NewError(err.Error(),
			openaiapi.ErrTypeInvalidRequest, "model_not_found", "model")

	case errors.Is(err, app.ErrPolicyNotFound):
		return http.StatusBadRequest, openaiapi.NewError(err.Error(),
			openaiapi.ErrTypeInvalidRequest, "policy_not_found", "policy")

	case errors.Is(err, app.ErrModelNotAllowed):
		return http.StatusForbidden, openaiapi.NewError(err.Error(),
			openaiapi.ErrTypePermission, "model_not_allowed", "model")

	case errors.Is(err, app.ErrGuardrailBlocked):
		// 400 rather than 403: the request itself is the problem, and the caller can fix it by
		// changing the prompt. SDKs surface this as a BadRequestError, which is the right
		// mental model for a content policy rejection.
		return http.StatusBadRequest, openaiapi.NewError(err.Error(),
			openaiapi.ErrTypeInvalidRequest, "guardrail_blocked", "messages")

	case errors.Is(err, app.ErrBudgetExceeded):
		return http.StatusTooManyRequests, openaiapi.NewError(err.Error(),
			openaiapi.ErrTypeRateLimit, "budget_exceeded", "")

	case errors.Is(err, app.ErrNoProviderAvailable):
		return http.StatusServiceUnavailable, openaiapi.NewError(err.Error(),
			openaiapi.ErrTypeOverloaded, "no_provider_available", "")

	case errors.Is(err, app.ErrUpstream):
		return http.StatusBadGateway, openaiapi.NewError(err.Error(),
			openaiapi.ErrTypeAPI, "upstream_error", "")

	default:
		var invalid *InvalidRequestError
		if errors.As(err, &invalid) {
			return http.StatusBadRequest, openaiapi.NewError(invalid.Message,
				openaiapi.ErrTypeInvalidRequest, "invalid_request", invalid.Param)
		}
		return http.StatusInternalServerError, openaiapi.NewError("internal server error",
			openaiapi.ErrTypeAPI, "internal_error", "")
	}
}

// InvalidRequestError is a malformed request body or parameter.
type InvalidRequestError struct {
	Message string
	Param   string
}

func (e *InvalidRequestError) Error() string { return e.Message }

// Invalid builds an InvalidRequestError.
func Invalid(param, format string, args ...any) *InvalidRequestError {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	return &InvalidRequestError{Message: msg, Param: param}
}
