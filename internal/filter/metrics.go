package filter

import (
	"sync"

	"github.com/agent-burn-down/desktop-client/internal/api"
)

// metricAllowlist are the Claude Code OTLP metrics exporter counters kept for
// upload (issue #21): token/cost usage and commit/lines-of-code counts. Unlike
// Filter's event-name policy, this is a true allowlist — every metric name not
// listed here is dropped, never kept.
var metricAllowlist = map[string]bool{
	"claude_code.token.usage":         true,
	"claude_code.cost.usage":          true,
	"claude_code.commit.count":        true,
	"claude_code.lines_of_code.count": true,
}

// MetricStats is a snapshot of metric filter activity.
type MetricStats struct {
	Kept     int64
	Filtered int64
}

// MetricFilter keeps only allowlisted metric points, counting and dropping
// everything else so an unrecognized metric name is never uploaded raw.
// It is safe for concurrent use.
type MetricFilter struct {
	keep map[string]bool

	mu    sync.Mutex
	stats MetricStats
}

// NewMetricFilter returns a MetricFilter using the default allowlist, extended
// with the given extra metric names to keep.
func NewMetricFilter(extraKeep []string) *MetricFilter {
	keep := cloneSet(metricAllowlist)
	for _, name := range extraKeep {
		keep[name] = true
	}
	return &MetricFilter{keep: keep}
}

// Apply returns only the allowlisted points; every other point is counted in
// Filtered and dropped.
func (f *MetricFilter) Apply(points []api.MetricPoint) []api.MetricPoint {
	kept := make([]api.MetricPoint, 0, len(points))
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range points {
		if f.keep[points[i].MetricName] {
			kept = append(kept, points[i])
			f.stats.Kept++
			continue
		}
		f.stats.Filtered++
	}
	return kept
}

// Stats returns a snapshot of kept/filtered counters.
func (f *MetricFilter) Stats() MetricStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}
