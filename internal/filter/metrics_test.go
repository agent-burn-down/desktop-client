package filter

import (
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/api"
)

func point(name string, value float64) api.MetricPoint {
	return api.MetricPoint{MetricName: name, Value: value}
}

// TestMetricFilterAllowlistedKept proves every default-allowlisted metric name
// survives Apply unchanged.
func TestMetricFilterAllowlistedKept(t *testing.T) {
	f := NewMetricFilter(nil)
	points := []api.MetricPoint{
		point("claude_code.token.usage", 100),
		point("claude_code.cost.usage", 0.05),
		point("claude_code.commit.count", 1),
		point("claude_code.lines_of_code.count", 42),
	}
	kept := f.Apply(points)
	if len(kept) != len(points) {
		t.Fatalf("kept %d points, want all %d allowlisted", len(kept), len(points))
	}
	stats := f.Stats()
	if stats.Kept != 4 || stats.Filtered != 0 {
		t.Fatalf("stats = %+v, want kept=4 filtered=0", stats)
	}
}

// TestMetricFilterUnknownNameDroppedNotUploaded is the acceptance-criteria
// case: an unrecognized metric name is counted and dropped, never returned
// from Apply (so it can never reach the uploader/backend raw).
func TestMetricFilterUnknownNameDroppedNotUploaded(t *testing.T) {
	f := NewMetricFilter(nil)
	points := []api.MetricPoint{
		point("claude_code.commit.count", 1),
		point("some.unrecognized.metric", 999),
		point("another.unknown.counter", 1),
	}
	kept := f.Apply(points)
	if len(kept) != 1 || kept[0].MetricName != "claude_code.commit.count" {
		t.Fatalf("kept = %+v, want only claude_code.commit.count", kept)
	}
	for _, p := range kept {
		if p.MetricName == "some.unrecognized.metric" || p.MetricName == "another.unknown.counter" {
			t.Fatalf("unknown metric name %q leaked through Apply", p.MetricName)
		}
	}
	stats := f.Stats()
	if stats.Kept != 1 || stats.Filtered != 2 {
		t.Fatalf("stats = %+v, want kept=1 filtered=2", stats)
	}
}

func TestMetricFilterExtraKeep(t *testing.T) {
	f := NewMetricFilter([]string{"custom.metric.name"})
	kept := f.Apply([]api.MetricPoint{point("custom.metric.name", 1), point("still.unknown", 1)})
	if len(kept) != 1 || kept[0].MetricName != "custom.metric.name" {
		t.Fatalf("kept = %+v, want only the extra-keep name", kept)
	}
}
