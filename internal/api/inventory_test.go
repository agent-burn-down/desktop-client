package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendInventoryContract(t *testing.T) {
	var path string
	var body map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(InventoryOut{
			Accepted: 1, Replaced: true, ObservedAt: "2026-07-21T20:00:00Z",
		})
	}))
	defer srv.Close()
	present := true
	out, err := NewClient(srv.URL, "test", WithHTTPClient(srv.Client())).SendInventory(
		context.Background(), 7, "2026-07-21T20:00:00Z",
		[]InventoryItem{{Kind: "skill", Name: "deploy-check", Source: "codex", Present: &present}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/ingest/v1/inventory" || out.Accepted != 1 || !out.Replaced {
		t.Fatalf("path=%q out=%+v", path, out)
	}
	var collectorID int64
	if err := json.Unmarshal(body["collector_id"], &collectorID); err != nil || collectorID != 7 {
		t.Fatalf("collector_id = %s (%v)", body["collector_id"], err)
	}
	if string(body["items"]) == "" || string(body["observed_at"]) == "" {
		t.Fatalf("missing inventory envelope fields: %v", body)
	}
}

func TestSendInventoryRejectsOversizedSnapshot(t *testing.T) {
	items := make([]InventoryItem, MaxInventoryItems+1)
	_, err := NewClient("https://example.invalid", "test").SendInventory(
		context.Background(), 1, "2026-07-21T20:00:00Z", items)
	if err == nil {
		t.Fatal("oversized inventory should fail before HTTP")
	}
}
