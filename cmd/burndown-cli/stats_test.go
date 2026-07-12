package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/queue"
)

// seedQueue opens the queue at the config-derived path and enqueues then acks
// the given events, so the stats reader sees them as retained history.
func seedQueue(t *testing.T, events []api.NormalizedEvent) {
	t.Helper()
	path, err := queue.DefaultPath()
	if err != nil {
		t.Fatalf("default path: %v", err)
	}
	q, err := queue.Open(path, queue.Options{})
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	if err := q.Enqueue(events); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	items, err := q.LeaseBatch(len(events), time.Minute)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	ids := make([]int64, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	if err := q.Ack(ids); err != nil {
		t.Fatalf("ack: %v", err)
	}
	_ = q.Close()
}

func statEvent(in, out int64, cost float64, tool string) api.NormalizedEvent {
	ts := time.Now().UTC().Format("2006-01-02") + "T12:00:00Z"
	return api.NormalizedEvent{
		Timestamp: &ts, InputTokens: &in, OutputTokens: &out,
		CostUSD: &cost, ToolName: &tool,
	}
}

func TestStatsEmptyWhenNoQueue(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	out, err := runCmd(t, "stats", "--json")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	var report statsOutput
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if report.Days != config.DefaultRetentionDays {
		t.Errorf("days = %d, want default %d", report.Days, config.DefaultRetentionDays)
	}
	if len(report.Daily) != 0 || report.Totals.Events != 0 {
		t.Errorf("expected empty report, got %+v", report)
	}
}

func TestStatsReportsRetainedTotals(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	seedQueue(t, []api.NormalizedEvent{
		statEvent(100, 20, 0.50, "Bash"),
		statEvent(200, 40, 1.25, "Read"),
		statEvent(50, 10, 0.05, "Bash"),
	})

	out, err := runCmd(t, "stats", "--json")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	var report statsOutput
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if report.Totals.Events != 3 || report.Totals.InputTokens != 350 {
		t.Fatalf("totals wrong: %+v", report.Totals)
	}
	if report.Totals.OutputTokens != 70 {
		t.Fatalf("output tokens = %d, want 70", report.Totals.OutputTokens)
	}
	if len(report.TopTools) == 0 || report.TopTools[0].Tool != "Bash" || report.TopTools[0].Count != 2 {
		t.Fatalf("top tools wrong: %+v", report.TopTools)
	}
}

func TestStatsDaysBoundedByRetention(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	store, _ := config.NewFileStore()
	if err := store.Save(&config.Config{APIURL: "https://x", RetentionDays: 3}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	out, err := runCmd(t, "stats", "--json", "--days", "30")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	var report statsOutput
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if report.Days != 3 {
		t.Fatalf("days = %d, want clamped to retention 3", report.Days)
	}
}

func TestStatsTextOutput(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	seedQueue(t, []api.NormalizedEvent{statEvent(10, 5, 0.10, "Bash")})
	out, err := runCmd(t, "stats")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !strings.Contains(out, "TOTAL") || !strings.Contains(out, "top tools") {
		t.Fatalf("text output missing sections:\n%s", out)
	}
}
