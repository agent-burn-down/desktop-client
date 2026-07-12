package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/agent-burn-down/desktop-client/internal/counters"
	"github.com/agent-burn-down/desktop-client/internal/receiver"
)

// newSendTestCmd builds the `send-test` command: post a synthetic OTLP log to
// the local receiver and confirm it reached the durable queue.
func newSendTestCmd() *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "send-test",
		Short: "Post a synthetic OTLP log to the local receiver and confirm it queued",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSendTest(cmd, port)
		},
	}
	cmd.Flags().IntVar(&port, "port", receiver.DefaultPort, "loopback port of the OTLP receiver")
	return cmd
}

func runSendTest(cmd *cobra.Command, port int) error {
	before, err := probeHealthz(cmd.Context(), port)
	if err != nil {
		return fmt.Errorf(
			"daemon not reachable on 127.0.0.1:%d; run `burndown-cli serve` first: %w", port, err)
	}
	accepted, err := postSyntheticLog(cmd.Context(), port)
	if err != nil {
		return err
	}
	after, err := probeHealthz(cmd.Context(), port)
	if err != nil {
		return fmt.Errorf("re-probe after send failed: %w", err)
	}
	delta := after.Counters[counters.Queued] - before.Counters[counters.Queued]
	if delta <= 0 {
		return fmt.Errorf(
			"receiver accepted %d event(s) but the queued counter did not advance; "+
				"the event may have been filtered", accepted)
	}
	outf(cmd.OutOrStdout(),
		"send-test ok: receiver accepted %d, queued advanced by %d\n", accepted, delta)
	return nil
}

// postSyntheticLog posts one clearly-marked synthetic api_request event and
// returns the receiver's accepted count.
func postSyntheticLog(ctx context.Context, port int) (int, error) {
	payload, err := json.Marshal(syntheticLogPayload())
	if err != nil {
		return 0, err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/logs", port)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("post synthetic log: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Accepted int `json:"accepted"`
		Dropped  int `json:"dropped"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("decode receiver response: %w", err)
	}
	return body.Accepted, nil
}

// syntheticLogPayload builds a minimal OTLP/HTTP logs batch with one api_request
// event marked session_id "send-test" so it is recognisable downstream.
func syntheticLogPayload() map[string]any {
	attr := func(k, v string) map[string]any {
		return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
	}
	return map[string]any{
		"resourceLogs": []any{map[string]any{
			"scopeLogs": []any{map[string]any{
				"logRecords": []any{map[string]any{
					"attributes": []any{
						attr("event.name", "api_request"),
						attr("session.id", "send-test"),
						attr("model", "send-test"),
						map[string]any{
							"key":   "input_tokens",
							"value": map[string]any{"intValue": "1"},
						},
					},
				}},
			}},
		}},
	}
}
