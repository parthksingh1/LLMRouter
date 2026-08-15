// Package guardrails is the gateway's client for the screening sidecar.
//
// The interesting decision here is what to do when the sidecar is unreachable, and it is a
// genuine trade rather than an oversight:
//
//	fail open   the request proceeds unscreened. Availability is preserved; a screening gap
//	            opens. This is the default, because a guardrails outage taking down every LLM
//	            request is a worse failure than a few minutes of unscreened traffic -- and
//	            because the operator finds out either way, from the metric and the WARN log.
//
//	fail closed the request is refused. Correct for a regulated deployment where sending
//	            unscreened data is worse than sending nothing.
//
// GUARDRAILS_FAIL_MODE picks. Whichever is chosen, the outcome is recorded: a failure that is
// silently tolerated is the failure mode that actually hurts. See docs/adr/0005.
package guardrails

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/app"
	"github.com/parthkumarsingh/llmrouter/services/gateway/internal/domain"
)

// Client calls the guardrails sidecar. It satisfies app.Guardrails.
type Client struct {
	baseURL  string
	client   *http.Client
	failOpen bool
	log      *slog.Logger
}

var _ app.Guardrails = (*Client)(nil)

// Options configures the client.
type Options struct {
	BaseURL string
	Timeout time.Duration
	// FailOpen allows a request through when the sidecar cannot be reached.
	FailOpen bool
	Log      *slog.Logger
}

// New builds the client.
func New(opts Options) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = 50 * time.Millisecond
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Client{
		baseURL:  opts.BaseURL,
		failOpen: opts.FailOpen,
		log:      opts.Log,
		client: &http.Client{
			// A tight timeout is the point. Screening runs on every request, and the service's
			// own budget is a p99 under 12 ms; anything slower than this is broken, and waiting
			// longer only adds the sidecar's latency to the user's.
			Timeout: opts.Timeout,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext,
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 64,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// ScreenInput screens a prompt before dispatch.
func (c *Client) ScreenInput(ctx context.Context, tenantID, text string) (domain.ScreenResult, error) {
	return c.screen(ctx, "/v1/screen/input", tenantID, text)
}

// ScreenOutput screens a completion on the way back.
func (c *Client) ScreenOutput(ctx context.Context, tenantID, text string) (domain.ScreenResult, error) {
	return c.screen(ctx, "/v1/screen/output", tenantID, text)
}

// screenRequest is the wire request. It mirrors ScreenRequest in the Python service.
type screenRequest struct {
	TenantID  string `json:"tenant_id"`
	Text      string `json:"text"`
	RequestID string `json:"request_id,omitempty"`
}

// screenResponse mirrors ScreenResponse.
type screenResponse struct {
	Allowed      bool          `json:"allowed"`
	RedactedText string        `json:"redacted_text"`
	Findings     []wireFinding `json:"findings"`
	LatencyMS    float64       `json:"latency_ms"`
	Policy       string        `json:"policy"`
	Truncated    bool          `json:"truncated"`
}

type wireFinding struct {
	Detector string `json:"detector"`
	Category string `json:"category"`
	OWASP    string `json:"owasp"`
	Severity string `json:"severity"`
	Action   string `json:"action"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
}

func (c *Client) screen(ctx context.Context, path, tenantID, text string) (domain.ScreenResult, error) {
	if text == "" {
		return domain.ScreenResult{Allowed: true}, nil
	}

	body, err := json.Marshal(screenRequest{TenantID: tenantID, Text: text})
	if err != nil {
		return domain.ScreenResult{}, fmt.Errorf("encoding screen request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return domain.ScreenResult{}, fmt.Errorf("building screen request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return c.unavailable(tenantID, text, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return c.unavailable(tenantID, text, fmt.Errorf("guardrails returned status %d", resp.StatusCode))
	}

	var out screenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return c.unavailable(tenantID, text, fmt.Errorf("decoding screen response: %w", err))
	}

	findings := make([]domain.Finding, 0, len(out.Findings))
	for _, f := range out.Findings {
		findings = append(findings, domain.Finding{
			Detector: f.Detector,
			Category: f.Category,
			OWASP:    f.OWASP,
			Severity: f.Severity,
			Start:    f.Start,
			End:      f.End,
			Action:   f.Action,
		})
	}

	redacted := out.RedactedText
	if redacted == "" {
		// Defensive: an empty redacted_text would silently blank the prompt. The service always
		// populates it, but a wire-level surprise must not delete the user's question.
		redacted = text
	}

	return domain.ScreenResult{
		Allowed:      out.Allowed,
		RedactedText: redacted,
		Findings:     findings,
		LatencyMS:    out.LatencyMS,
	}, nil
}

// ErrUnavailable means the sidecar could not be reached and policy is to fail closed.
var ErrUnavailable = errors.New("guardrails service is unavailable")

// unavailable applies the configured failure mode.
func (c *Client) unavailable(tenantID, text string, cause error) (domain.ScreenResult, error) {
	if c.failOpen {
		// WARN, not DEBUG: an unscreened request is a security event even when it is the
		// correct trade-off, and it must be findable in the logs afterwards.
		c.log.Warn("guardrails unavailable, failing open",
			"tenant_id", tenantID, "error", cause)
		return domain.ScreenResult{
			Allowed:      true,
			RedactedText: text,
			FailedOpen:   true,
		}, nil
	}

	c.log.Error("guardrails unavailable, failing closed",
		"tenant_id", tenantID, "error", cause)
	return domain.ScreenResult{}, fmt.Errorf("%w: %w", ErrUnavailable, cause)
}

// Ping checks the sidecar is reachable, for readiness reporting.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("building guardrails health request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("guardrails health: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("guardrails health: status %d", resp.StatusCode)
	}
	return nil
}
