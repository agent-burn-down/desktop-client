package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendMetricsHappyPath(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"accepted":2,"dropped":0}`)
	}))
	defer srv.Close()

	repo := "test-repo"
	points := []MetricPoint{
		{MetricName: "claude_code.commit.count", Value: 1, Repo: &repo},
		{MetricName: "claude_code.cost.usage", Value: 0.05},
	}
	out, err := newTestClient(t, srv).SendMetrics(context.Background(), 123, points)
	if err != nil {
		t.Fatalf("SendMetrics: %v", err)
	}
	if out.Accepted != 2 || out.Dropped != 0 {
		t.Errorf("counts = %+v, want accepted=2 dropped=0", out)
	}
	if gotBody["collector_id"] != float64(123) {
		t.Errorf("collector_id = %v, want 123", gotBody["collector_id"])
	}
	pts, ok := gotBody["points"].([]any)
	if !ok || len(pts) != 2 {
		t.Fatalf("points = %v, want 2 elements", gotBody["points"])
	}
	first := pts[0].(map[string]any)
	if first["metric_name"] != "claude_code.commit.count" || first["value"] != float64(1) {
		t.Errorf("points[0] = %v, want commit.count=1", first)
	}
	if first["repo"] != "test-repo" {
		t.Errorf("points[0].repo = %v, want test-repo", first["repo"])
	}
}

func TestSendMetricsRejectsOversizedBatch(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))
	defer srv.Close()

	points := make([]MetricPoint, MaxIngestBatch+1)
	_, err := newTestClient(t, srv).SendMetrics(context.Background(), 1, points)
	if err == nil {
		t.Fatal("expected error for oversized batch, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds max") {
		t.Errorf("error = %q, want mention of exceeding max", err)
	}
	if calls != 0 {
		t.Errorf("server called %d times, want 0 (rejected client-side)", calls)
	}
}
