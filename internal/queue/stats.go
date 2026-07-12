package queue

import (
	"database/sql"
	"fmt"
	"time"
)

// DailyStat is one day's aggregate usage over retained acked rows. Token and
// cost figures are summed from each row's stored payload JSON.
type DailyStat struct {
	Date              string  `json:"date"`
	Events            int64   `json:"events"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CacheReadTokens   int64   `json:"cache_read_tokens"`
	CacheCreateTokens int64   `json:"cache_create_tokens"`
	CostUSD           float64 `json:"cost_usd"`
}

// ToolStat is a tool_name and how many retained acked events referenced it.
type ToolStat struct {
	Tool  string `json:"tool"`
	Count int64  `json:"count"`
}

// StatsReport is the local usage summary over the retained acked window.
type StatsReport struct {
	Since time.Time   `json:"since"`
	Daily []DailyStat `json:"daily"`
	Tools []ToolStat  `json:"top_tools"`
}

// StatsSince aggregates retained acked rows whose acked_at is at or after since:
// per-day token/cost totals (grouped by the event timestamp's date, falling
// back to the acked date) and the topTools most-used tool_name values. Values
// are read from each row's payload JSON via SQLite's json_extract.
func (q *Queue) StatsSince(since time.Time, topTools int) (StatsReport, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	cutoff := since.UTC().Format(timeLayout)
	daily, err := q.dailyStats(cutoff)
	if err != nil {
		return StatsReport{}, err
	}
	tools, err := q.topTools(cutoff, topTools)
	if err != nil {
		return StatsReport{}, err
	}
	return StatsReport{Since: since, Daily: daily, Tools: tools}, nil
}

const dailyStatsQuery = `
SELECT
  COALESCE(substr(json_extract(payload,'$.timestamp'),1,10), substr(acked_at,1,10)) AS day,
  COUNT(*),
  COALESCE(SUM(json_extract(payload,'$.input_tokens')),0),
  COALESCE(SUM(json_extract(payload,'$.output_tokens')),0),
  COALESCE(SUM(json_extract(payload,'$.cache_read_tokens')),0),
  COALESCE(SUM(json_extract(payload,'$.cache_create_tokens')),0),
  COALESCE(SUM(json_extract(payload,'$.cost_usd')),0)
FROM queue
WHERE state='acked' AND acked_at IS NOT NULL AND acked_at >= ?
GROUP BY day
ORDER BY day`

func (q *Queue) dailyStats(cutoff string) ([]DailyStat, error) {
	rows, err := q.db.Query(dailyStatsQuery, cutoff)
	if err != nil {
		return nil, fmt.Errorf("daily stats query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DailyStat
	for rows.Next() {
		d, err := scanDaily(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanDaily(rows *sql.Rows) (DailyStat, error) {
	var d DailyStat
	if err := rows.Scan(
		&d.Date, &d.Events, &d.InputTokens, &d.OutputTokens,
		&d.CacheReadTokens, &d.CacheCreateTokens, &d.CostUSD,
	); err != nil {
		return DailyStat{}, fmt.Errorf("scan daily stat: %w", err)
	}
	return d, nil
}

const topToolsQuery = `
SELECT json_extract(payload,'$.tool_name') AS tool, COUNT(*) AS n
FROM queue
WHERE state='acked' AND acked_at IS NOT NULL AND acked_at >= ?
  AND json_extract(payload,'$.tool_name') IS NOT NULL
GROUP BY tool
ORDER BY n DESC, tool ASC
LIMIT ?`

func (q *Queue) topTools(cutoff string, limit int) ([]ToolStat, error) {
	if limit <= 0 {
		limit = DefaultTopTools
	}
	rows, err := q.db.Query(topToolsQuery, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("top tools query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ToolStat
	for rows.Next() {
		var t ToolStat
		if err := rows.Scan(&t.Tool, &t.Count); err != nil {
			return nil, fmt.Errorf("scan tool stat: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
