package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// healthzTimeout bounds a single /healthz probe so status and send-test fail
// fast when the daemon is down.
const healthzTimeout = 2 * time.Second

// healthz is the receiver's GET /healthz response body.
type healthz struct {
	OK       bool             `json:"ok"`
	Counters map[string]int64 `json:"counters"`
}

// probeHealthz issues GET http://127.0.0.1:<port>/healthz. A non-nil error
// (typically connection refused) means the daemon is not running.
func probeHealthz(ctx context.Context, port int) (*healthz, error) {
	ctx, cancel := context.WithTimeout(ctx, healthzTimeout)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
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
