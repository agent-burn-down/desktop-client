package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendSessionsUsesStructuredMetadataContract(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ingest/v1/sessions" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"accepted":1,"dropped":0,"duplicates":0}`))
	}))
	defer srv.Close()

	model := "gpt-5.6-sol"
	summary := SessionSummary{
		SessionID: "session-1", UpdatedAt: "2026-07-15T12:00:00Z",
		StartedAt: "2026-07-15T11:59:00Z", LastActivityAt: "2026-07-15T12:00:00Z",
		Outcome: "running", InputTokens: 10, Models: []SessionModelUsage{{Model: &model, InputTokens: 10}},
		ModelBreakdownComplete: true,
	}
	out, err := newTestClient(t, srv).SendSessions(context.Background(), 5, []SessionSummary{summary})
	if err != nil {
		t.Fatal(err)
	}
	if out.Accepted != 1 || body["collector_id"] != float64(5) {
		t.Fatalf("response/body = %+v %#v", out, body)
	}
	sessions := body["sessions"].([]any)
	wire := sessions[0].(map[string]any)
	if wire["model_breakdown_complete"] != true || wire["title"] != nil || wire["summary"] != nil {
		t.Fatalf("unexpected wire summary: %#v", wire)
	}
}

func TestSendSessionsRejectsOversizedBatch(t *testing.T) {
	c := NewClient("http://127.0.0.1", "key")
	_, err := c.SendSessions(context.Background(), 1, make([]SessionSummary, 501))
	if err == nil {
		t.Fatal("expected oversized batch error")
	}
}
