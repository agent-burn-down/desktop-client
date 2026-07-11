package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/version"
)

const httpTimeout = 10 * time.Second

// retryDelays are the backoff sleeps between attempts. Length + 1 is the total
// number of attempts (4). Mirrors the Python forwarder's (1, 2, 4) schedule.
var retryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// AuthError is returned when the backend rejects the collector key with HTTP
// 401. It carries the server-provided detail string and is distinct from
// transport errors, so callers can prompt the user to re-authenticate rather
// than retry.
type AuthError struct {
	Detail string
}

// Error implements the error interface.
func (e *AuthError) Error() string {
	if e.Detail == "" {
		return "authentication failed (401)"
	}
	return fmt.Sprintf("authentication failed (401): %s", e.Detail)
}

// RegisterOut is the response from POST /ingest/v1/register.
type RegisterOut struct {
	CollectorID int64  `json:"collector_id"`
	Policy      Policy `json:"policy"`
}

// HeartbeatOut is the response from POST /ingest/v1/heartbeat.
type HeartbeatOut struct {
	OK     bool   `json:"ok"`
	Policy Policy `json:"policy"`
}

// Counts is the accepted/dropped tally returned by POST /ingest/v1/events.
type Counts struct {
	Accepted int `json:"accepted"`
	Dropped  int `json:"dropped"`
}

// Client is a typed HTTP client for the backend ingest API.
type Client struct {
	baseURL      string
	collectorKey string
	userAgent    string
	httpClient   *http.Client
	sleep        func(time.Duration)
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the underlying *http.Client (useful for tests).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// WithSleep overrides the backoff sleep between retry attempts. Tests inject a
// no-op to avoid real delays; production uses time.Sleep.
func WithSleep(f func(time.Duration)) Option {
	return func(c *Client) { c.sleep = f }
}

// NewClient returns a Client for the given base URL and collector key. The
// underlying HTTP client uses a 10s timeout unless overridden.
func NewClient(baseURL, collectorKey string, opts ...Option) *Client {
	c := &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		collectorKey: collectorKey,
		userAgent:    "burndown-cli/" + version.Version,
		httpClient:   &http.Client{Timeout: httpTimeout},
		sleep:        time.Sleep,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Register registers (or re-registers) this collector and resolves its
// reporting user, returning the assigned collector ID and current policy.
func (c *Client) Register(ctx context.Context, machine, userEmail string) (*RegisterOut, error) {
	body := map[string]any{
		"machine":    machine,
		"user_email": userEmail,
		"version":    version.Version,
	}
	var out RegisterOut
	if err := c.postJSON(ctx, "/ingest/v1/register", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Heartbeat reports liveness for the given collector and returns the current
// policy.
func (c *Client) Heartbeat(ctx context.Context, collectorID int64) (*HeartbeatOut, error) {
	body := map[string]any{"collector_id": collectorID}
	var out HeartbeatOut
	if err := c.postJSON(ctx, "/ingest/v1/heartbeat", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SendEvents posts a batch of normalized events. Batches larger than
// MaxIngestBatch are rejected client-side before any request is made.
func (c *Client) SendEvents(
	ctx context.Context, collectorID int64, events []NormalizedEvent,
) (*Counts, error) {
	if len(events) > MaxIngestBatch {
		return nil, fmt.Errorf(
			"batch of %d events exceeds max %d; split before sending",
			len(events), MaxIngestBatch)
	}
	body := map[string]any{"collector_id": collectorID, "events": events}
	var out Counts
	if err := c.postJSON(ctx, "/ingest/v1/events", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Health checks GET /api/health. It requires no auth and returns nil when the
// backend is reachable and healthy.
func (c *Client) Health(ctx context.Context) error {
	return c.doWithRetry(ctx, http.MethodGet, "/api/health", nil, false, nil)
}

func (c *Client) postJSON(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request for %s: %w", path, err)
	}
	return c.doWithRetry(ctx, http.MethodPost, path, payload, true, out)
}

// doWithRetry issues the request, retrying only on 5xx and network errors per
// retryDelays. 4xx (including 401) and context cancellation are never retried.
func (c *Client) doWithRetry(
	ctx context.Context, method, path string, payload []byte, auth bool, out any,
) error {
	var lastErr error
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		if attempt > 0 {
			c.sleep(retryDelays[attempt-1])
		}
		retryable, err := c.doOnce(ctx, method, path, payload, auth, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable || ctx.Err() != nil {
			return err
		}
	}
	return fmt.Errorf(
		"request to %s failed after %d attempts: %w",
		path, len(retryDelays)+1, lastErr)
}

// doOnce performs a single attempt. The bool reports whether the error (if any)
// is retryable.
func (c *Client) doOnce(
	ctx context.Context, method, path string, payload []byte, auth bool, out any,
) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader(payload))
	if err != nil {
		return false, fmt.Errorf("build request for %s: %w", path, err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", c.userAgent)
	if auth {
		req.Header.Set("X-Collector-Key", c.collectorKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return true, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return handleResponse(resp, path, out)
}

// handleResponse maps a response to (retryable, error). A 200 decodes the body
// into out (when non-nil); 401 yields *AuthError; 5xx is retryable; other
// statuses are terminal.
func handleResponse(resp *http.Response, path string, out any) (bool, error) {
	switch {
	case resp.StatusCode == http.StatusOK:
		return false, decodeJSON(resp.Body, out)
	case resp.StatusCode == http.StatusUnauthorized:
		return false, &AuthError{Detail: parseDetail(resp.Body)}
	case resp.StatusCode >= 500:
		return true, fmt.Errorf(
			"%s: server error %d: %s", path, resp.StatusCode, readSnippet(resp.Body))
	default:
		return false, fmt.Errorf(
			"%s: unexpected status %d: %s", path, resp.StatusCode, readSnippet(resp.Body))
	}
}

func bodyReader(payload []byte) io.Reader {
	if payload == nil {
		return nil
	}
	return bytes.NewReader(payload)
}

func decodeJSON(r io.Reader, out any) error {
	if out == nil {
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	if err := json.NewDecoder(r).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func parseDetail(r io.Reader) string {
	var body struct {
		Detail string `json:"detail"`
	}
	data, _ := io.ReadAll(io.LimitReader(r, 4096))
	_ = json.Unmarshal(data, &body)
	return body.Detail
}

func readSnippet(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(data))
}
