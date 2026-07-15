package queue

import (
	"testing"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
)

func TestSessionQueueBuildsMultiModelSummaryAndSurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/queue.db"
	q, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	session, repo := "session-1", "agent-burn-down-app"
	claude, codex := "claude-sonnet-4-6", "gpt-5.6-sol"
	start, later := int64(10), int64(20)
	stamp1, stamp2 := "2026-07-15T12:00:00Z", "2026-07-15T12:01:00Z"
	events := []api.NormalizedEvent{
		{SessionID: &session, Repo: &repo, Model: &claude, Timestamp: &stamp1, InputTokens: &start},
		{SessionID: &session, Repo: &repo, Model: &codex, Timestamp: &stamp2, OutputTokens: &later},
	}
	if n, err := q.UpsertSessionEvents(events, time.Date(2026, 7, 15, 12, 1, 0, 0, time.UTC)); err != nil || n != 1 {
		t.Fatalf("upsert = %d, %v", n, err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	q, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Close() }()
	items, err := q.LeaseSessionsBatch(10, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("lease = %d, %v", len(items), err)
	}
	summary := items[0].Summary
	if summary.Model != nil || len(summary.Models) != 2 || !summary.ModelBreakdownComplete {
		t.Fatalf("bad model breakdown: %+v", summary)
	}
	if summary.InputTokens != 10 || summary.OutputTokens != 20 || summary.Repo == nil || *summary.Repo != repo {
		t.Fatalf("bad summary: %+v", summary)
	}
}

func TestSessionQueueRevisionSafeAckAndPrivacySanitization(t *testing.T) {
	q := openTemp(t, Options{})
	session, model, unsafeRepo := "session-2", "mixed: a, b", "/tmp/private/repo"
	input := int64(5)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	event := api.NormalizedEvent{SessionID: &session, Model: &model, Repo: &unsafeRepo, InputTokens: &input}
	if _, err := q.UpsertSessionEvents([]api.NormalizedEvent{event}, now); err != nil {
		t.Fatal(err)
	}
	leased, _ := q.LeaseSessionsBatch(1, time.Minute)
	if leased[0].Summary.Repo != nil || leased[0].Summary.Models[0].Model != nil {
		t.Fatalf("unsafe attribution leaked: %+v", leased[0].Summary)
	}
	if _, err := q.UpsertSessionEvents([]api.NormalizedEvent{event}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := q.AckSessions([]SessionLease{{SessionID: session, Revision: leased[0].Revision}}); err != nil {
		t.Fatal(err)
	}
	newer, _ := q.LeaseSessionsBatch(1, time.Minute)
	if len(newer) != 1 || newer[0].Revision == leased[0].Revision || newer[0].Summary.InputTokens != 10 {
		t.Fatalf("new revision was lost: %+v", newer)
	}
}

func TestSessionQueueTracksCompletedAndFailedOutcomes(t *testing.T) {
	q := openTemp(t, Options{})
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	completedID, completedEvent := "completed", "codex.session.end"
	failedID, failedEvent, failure := "failed", "claude_code.tool_result", "timeout"
	if _, err := q.UpsertSessionEvents([]api.NormalizedEvent{
		{SessionID: &completedID, EventName: &completedEvent},
		{SessionID: &failedID, EventName: &failedEvent, ErrorMessage: &failure},
	}, now); err != nil {
		t.Fatal(err)
	}
	items, err := q.LeaseSessionsBatch(10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := make(map[string]string)
	for _, item := range items {
		outcomes[item.SessionID] = item.Summary.Outcome
	}
	if outcomes[completedID] != "succeeded" || outcomes[failedID] != "failed" {
		t.Fatalf("outcomes = %#v", outcomes)
	}
}
