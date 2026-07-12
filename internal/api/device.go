package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Device-authorization polling codes returned by POST /api/device/token while
// the grant has not yet resolved (HTTP 400 {"error": <code>}), plus the two
// terminal outcomes.
const (
	DeviceCodeAuthorizationPending = "authorization_pending"
	DeviceCodeSlowDown             = "slow_down"
	DeviceCodeAccessDenied         = "access_denied"
	DeviceCodeExpiredToken         = "expired_token"
)

// DeviceAuth is the response from POST /api/device/authorize.
type DeviceAuth struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

// DeviceToken is the response from POST /api/device/token once a grant has
// been approved. CollectorKey is returned exactly once by the backend. A
// device-issued key always carries a fresh expiry (never nullable, unlike the
// register/heartbeat key_expires_at, which can be null for legacy keys).
type DeviceToken struct {
	CollectorKey string `json:"collector_key"`
	KeyID        int64  `json:"key_id"`
	KeyExpiresAt string `json:"key_expires_at"`
}

// DeviceTokenError is returned while polling POST /api/device/token before it
// resolves, or on a terminal denial/expiry. Code is one of the DeviceCode*
// constants.
type DeviceTokenError struct {
	Code string
}

func (e *DeviceTokenError) Error() string {
	return fmt.Sprintf("device login: %s", e.Code)
}

// DeviceAuthorize starts a device-authorization grant (POST
// /api/device/authorize). Unauthenticated; deviceName becomes the CLI's
// suggested device label shown on the approval page.
func (c *Client) DeviceAuthorize(ctx context.Context, deviceName string) (*DeviceAuth, error) {
	body := map[string]any{"device_name": deviceName}
	var out DeviceAuth
	if err := c.postJSONNoAuth(ctx, "/api/device/authorize", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeviceToken polls POST /api/device/token once. It returns *DeviceTokenError
// for the documented 400 codes (authorization_pending, slow_down,
// access_denied, expired_token); other errors are transport/server failures.
// This call never retries — polling cadence is the caller's responsibility.
func (c *Client) DeviceToken(ctx context.Context, deviceCode string) (*DeviceToken, error) {
	payload, err := json.Marshal(map[string]any{"device_code": deviceCode})
	if err != nil {
		return nil, fmt.Errorf("marshal device token request: %w", err)
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.baseURL+"/api/device/token", bodyReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build device token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST /api/device/token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		var out DeviceToken
		if err := decodeJSON(resp.Body, &out); err != nil {
			return nil, err
		}
		return &out, nil
	}
	if resp.StatusCode == http.StatusBadRequest {
		return nil, &DeviceTokenError{Code: parseDeviceCode(resp.Body)}
	}
	return nil, fmt.Errorf(
		"/api/device/token: unexpected status %d: %s", resp.StatusCode, readSnippet(resp.Body))
}

func parseDeviceCode(r io.Reader) string {
	var body struct {
		Error string `json:"error"`
	}
	data, _ := io.ReadAll(io.LimitReader(r, 4096))
	_ = json.Unmarshal(data, &body)
	return body.Error
}
