package queue

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
)

const maxSessionRows = 10_000

type SessionItem struct {
	SessionID string
	Revision  int64
	Summary   api.SessionSummary
	Attempts  int
}

type SessionLease struct {
	SessionID string
	Revision  int64
}

func (q *Queue) initSessions() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS sessions_queue (
			session_id TEXT PRIMARY KEY,
			revision INTEGER NOT NULL DEFAULT 1,
			payload TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			created_at TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			state TEXT NOT NULL DEFAULT 'pending',
			leased_until TEXT,
			acked_at TEXT
		)`,
		"CREATE INDEX IF NOT EXISTS idx_sessions_queue_state ON sessions_queue(state, updated_at)",
	}
	for _, statement := range stmts {
		if _, err := q.db.Exec(statement); err != nil {
			return fmt.Errorf("init sessions_queue schema: %w", err)
		}
	}
	return nil
}

// UpsertSessionEvents folds only allowlisted normalized events into one durable
// summary per session. No raw prompt, completion, path, or tool payload is read.
func (q *Queue) UpsertSessionEvents(events []api.NormalizedEvent, now time.Time) (int, error) {
	groups := groupSessionEvents(events)
	if len(groups) == 0 {
		return 0, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	tx, err := q.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin session upsert: %w", err)
	}
	for sessionID, group := range groups {
		if err := upsertSessionGroup(tx, sessionID, group, now); err != nil {
			_ = tx.Rollback()
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit session upsert: %w", err)
	}
	_, _ = q.db.Exec(`DELETE FROM sessions_queue WHERE session_id IN (
		SELECT session_id FROM sessions_queue ORDER BY (state='acked') DESC, updated_at ASC
		LIMIT MAX(0, (SELECT COUNT(*) FROM sessions_queue)-?)
	)`, maxSessionRows)
	return len(groups), nil
}

func groupSessionEvents(events []api.NormalizedEvent) map[string][]api.NormalizedEvent {
	groups := make(map[string][]api.NormalizedEvent)
	for _, event := range events {
		if event.SessionID == nil || strings.TrimSpace(*event.SessionID) == "" {
			continue
		}
		groups[*event.SessionID] = append(groups[*event.SessionID], event)
	}
	return groups
}

func upsertSessionGroup(
	tx *sql.Tx,
	sessionID string,
	events []api.NormalizedEvent,
	now time.Time,
) error {
	summary, err := loadSessionSummary(tx, sessionID)
	if err != nil {
		return err
	}
	summary = mergeSessionEvents(summary, sessionID, events, now)
	payload, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("marshal session %s: %w", sessionID, err)
	}
	_, err = tx.Exec(`INSERT INTO sessions_queue(
		session_id, payload, updated_at, created_at
	) VALUES(?, ?, ?, ?)
	ON CONFLICT(session_id) DO UPDATE SET
		revision=sessions_queue.revision+1, payload=excluded.payload,
		updated_at=excluded.updated_at, state='pending', leased_until=NULL,
		acked_at=NULL`, sessionID, string(payload), summary.UpdatedAt,
		now.UTC().Format(timeLayout))
	if err != nil {
		return fmt.Errorf("upsert session %s: %w", sessionID, err)
	}
	return nil
}

func loadSessionSummary(tx *sql.Tx, sessionID string) (api.SessionSummary, error) {
	var payload string
	err := tx.QueryRow(
		"SELECT payload FROM sessions_queue WHERE session_id=?", sessionID,
	).Scan(&payload)
	if err == sql.ErrNoRows {
		return api.SessionSummary{}, nil
	}
	if err != nil {
		return api.SessionSummary{}, fmt.Errorf("load session %s: %w", sessionID, err)
	}
	var summary api.SessionSummary
	if err := json.Unmarshal([]byte(payload), &summary); err != nil {
		return api.SessionSummary{}, fmt.Errorf("decode session %s: %w", sessionID, err)
	}
	return summary, nil
}

func mergeSessionEvents(
	summary api.SessionSummary,
	sessionID string,
	events []api.NormalizedEvent,
	now time.Time,
) api.SessionSummary {
	if summary.SessionID == "" {
		summary.SessionID = sessionID
		summary.Outcome = "running"
		summary.Models = []api.SessionModelUsage{}
		summary.ModelBreakdownComplete = true
	}
	models := make(map[string]*api.SessionModelUsage)
	for i := range summary.Models {
		key := modelKey(summary.Models[i].Model)
		row := summary.Models[i]
		models[key] = &row
	}
	for _, event := range events {
		mergeSessionEvent(&summary, models, event, now)
	}
	finishSessionModels(&summary, models)
	setSessionUpdatedAt(&summary, now)
	return summary
}

func mergeSessionEvent(
	summary *api.SessionSummary,
	models map[string]*api.SessionModelUsage,
	event api.NormalizedEvent,
	now time.Time,
) {
	stamp := eventStamp(event.Timestamp, now)
	mergeSessionTimes(summary, stamp)
	mergeSessionLabels(summary, event)
	mergeSessionUsage(summary, models, event)
	mergeSessionOutcome(summary, event, stamp)
}

func mergeSessionTimes(summary *api.SessionSummary, stamp string) {
	if summary.StartedAt == "" || stamp < summary.StartedAt {
		summary.StartedAt = stamp
	}
	if stamp > summary.LastActivityAt {
		summary.LastActivityAt = stamp
	}
}

func mergeSessionLabels(summary *api.SessionSummary, event api.NormalizedEvent) {
	if summary.Repo == nil && event.Repo != nil {
		repo := sanitizeLabel(*event.Repo)
		if repo != "" {
			summary.Repo = &repo
		}
	}
	if summary.Source == nil && event.EventName != nil {
		source := strings.SplitN(*event.EventName, ".", 2)[0]
		if source != "" {
			summary.Source = &source
		}
	}
}

func mergeSessionUsage(
	summary *api.SessionSummary,
	models map[string]*api.SessionModelUsage,
	event api.NormalizedEvent,
) {
	model := analyticalModel(event.Model)
	key := modelKey(model)
	row := models[key]
	if row == nil {
		row = &api.SessionModelUsage{Model: model}
		models[key] = row
	}
	row.InputTokens += value(event.InputTokens)
	row.OutputTokens += value(event.OutputTokens)
	row.CacheReadTokens += value(event.CacheReadTokens)
	row.CacheCreateTokens += value(event.CacheCreateTokens)
	summary.InputTokens += value(event.InputTokens)
	summary.OutputTokens += value(event.OutputTokens)
	summary.CacheReadTokens += value(event.CacheReadTokens)
	summary.CacheCreateTokens += value(event.CacheCreateTokens)
	if event.CostUSD != nil {
		addSessionCost(summary, *event.CostUSD)
	}
	if event.ToolName != nil {
		summary.ToolCalls++
	}
}

func addSessionCost(summary *api.SessionSummary, cost float64) {
	if summary.CostUSD == nil {
		zero := 0.0
		summary.CostUSD = &zero
	}
	*summary.CostUSD += cost
}

func mergeSessionOutcome(
	summary *api.SessionSummary,
	event api.NormalizedEvent,
	stamp string,
) {
	if event.ErrorMessage != nil {
		summary.ErrorCount++
		summary.Outcome = "failed"
	}
	if terminalEvent(event.EventName) && summary.Outcome != "failed" {
		summary.Outcome = "succeeded"
		summary.EndedAt = &stamp
	}
}

func finishSessionModels(
	summary *api.SessionSummary,
	models map[string]*api.SessionModelUsage,
) {
	summary.Models = summary.Models[:0]
	keys := make([]string, 0, len(models))
	for key := range models {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		summary.Models = append(summary.Models, *models[key])
	}
	if len(summary.Models) == 1 {
		summary.Model = summary.Models[0].Model
	} else {
		summary.Model = nil
	}
}

func setSessionUpdatedAt(summary *api.SessionSummary, now time.Time) {
	updated := now.UTC().Truncate(time.Second)
	prior, err := time.Parse(time.RFC3339Nano, summary.UpdatedAt)
	if err == nil && !updated.After(prior) {
		updated = prior.Add(time.Second)
	}
	summary.UpdatedAt = updated.Format(time.RFC3339)
}

func (q *Queue) LeaseSessionsBatch(n int, leaseDur time.Duration) ([]SessionItem, error) {
	if n <= 0 {
		return nil, nil
	}
	if n > 500 {
		n = 500
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now().UTC()
	rows, err := q.db.Query(`SELECT session_id,revision,payload,attempts FROM sessions_queue
		WHERE state='pending' OR (state='leased' AND (leased_until IS NULL OR leased_until < ?))
		ORDER BY updated_at ASC LIMIT ?`, now.Format(timeLayout), n)
	if err != nil {
		return nil, fmt.Errorf("select sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items, err := scanSessionItems(rows)
	if err != nil {
		return nil, err
	}
	if err := q.leaseSessionItems(items, now.Add(leaseDur)); err != nil {
		return nil, err
	}
	return items, rows.Err()
}

func scanSessionItems(rows *sql.Rows) ([]SessionItem, error) {
	var items []SessionItem
	for rows.Next() {
		var item SessionItem
		var payload string
		if err := rows.Scan(
			&item.SessionID, &item.Revision, &payload, &item.Attempts,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &item.Summary); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (q *Queue) leaseSessionItems(items []SessionItem, until time.Time) error {
	for _, item := range items {
		_, err := q.db.Exec(
			`UPDATE sessions_queue SET state='leased', leased_until=?
			 WHERE session_id=? AND revision=?`,
			until.UTC().Format(timeLayout), item.SessionID, item.Revision,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func (q *Queue) AckSessions(keys []SessionLease) error  { return q.finishSessions(keys, true) }
func (q *Queue) NackSessions(keys []SessionLease) error { return q.finishSessions(keys, false) }
func (q *Queue) finishSessions(keys []SessionLease, ack bool) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now().UTC().Format(timeLayout)
	for _, key := range keys {
		var err error
		if ack {
			_, err = q.db.Exec(
				`UPDATE sessions_queue SET state='acked', acked_at=?, leased_until=NULL
				 WHERE session_id=? AND revision=?`,
				now, key.SessionID, key.Revision,
			)
		} else {
			_, err = q.db.Exec(
				`UPDATE sessions_queue SET state='pending', attempts=attempts+1,
				 leased_until=NULL WHERE session_id=? AND revision=?`,
				key.SessionID, key.Revision,
			)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (q *Queue) PruneAckedSessions(cutoff time.Time) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	res, err := q.db.Exec(
		"DELETE FROM sessions_queue WHERE state='acked' AND acked_at < ?",
		cutoff.UTC().Format(timeLayout),
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func eventStamp(raw *string, fallback time.Time) string {
	if raw != nil {
		if parsed, err := time.Parse(time.RFC3339Nano, *raw); err == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
	}
	return fallback.UTC().Format(time.RFC3339)
}
func value(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
func analyticalModel(v *string) *string {
	if v == nil {
		return nil
	}
	s := strings.TrimSpace(*v)
	if s == "" || strings.EqualFold(s, "mixed") ||
		strings.HasPrefix(strings.ToLower(s), "mixed:") ||
		strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") ||
		strings.Contains(s, "\\") {
		return nil
	}
	return &s
}
func modelKey(v *string) string {
	if v == nil {
		return "\x00unknown"
	}
	return *v
}
func sanitizeLabel(v string) string {
	s := strings.TrimSpace(v)
	if strings.Contains(s, "/") || strings.Contains(s, "\\") {
		return ""
	}
	return s
}
func terminalEvent(v *string) bool {
	if v == nil {
		return false
	}
	s := strings.ToLower(*v)
	return strings.Contains(s, "session.end") ||
		strings.Contains(s, "session_end") ||
		strings.Contains(s, "conversation.end") ||
		strings.Contains(s, "conversation_end")
}
