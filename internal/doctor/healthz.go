package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// healthz is the daemon's GET /healthz response body.
type healthz struct {
	OK       bool             `json:"ok"`
	Counters map[string]int64 `json:"counters"`
}

// healthzURLForPort builds the loopback health URL for a receiver port.
func healthzURLForPort(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
}

// probeHealthz issues GET <url>. A non-nil error (typically connection refused)
// means the daemon is not running.
func probeHealthz(ctx context.Context, client *http.Client, url string) (*healthz, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("healthz returned status %d", resp.StatusCode)
	}
	var out healthz
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode healthz: %w", err)
	}
	return &out, nil
}
