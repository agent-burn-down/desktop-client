package filter

import (
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/api"
)

func ev(name string) api.NormalizedEvent {
	n := name
	return api.NormalizedEvent{EventName: &n}
}

func TestApplyPerFamily(t *testing.T) {
	cases := []struct {
		name string
		keep bool
	}{
		{"sse_event", true},
		{"tool_result", true},
		{"tool_decision", true},
		{"hook_execution_start", true},
		{"hook_execution_complete", true},
		{"api_error", true},
		{"api_retries_exhausted", true},
		{"compaction", true},
		{"websocket_event", false},
		{"some_unknown_event", true}, // default keep (conservative)
		{"", true},                   // missing name kept by default
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := New(nil, nil)
			out := f.Apply([]api.NormalizedEvent{ev(tc.name)})
			if got := len(out) == 1; got != tc.keep {
				t.Fatalf("%q: kept=%v, want %v", tc.name, got, tc.keep)
			}
		})
	}
}

func TestConfigOverrides(t *testing.T) {
	f := New([]string{"custom_keep"}, []string{"chatty_event"})
	out := f.Apply([]api.NormalizedEvent{ev("custom_keep"), ev("chatty_event")})
	if len(out) != 1 || *out[0].EventName != "custom_keep" {
		t.Fatalf("override policy failed: %+v", out)
	}
}

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }

func TestTokenConservation(t *testing.T) {
	model := "claude-opus-4-8"
	events := []api.NormalizedEvent{
		{EventName: strPtr("sse_event"), Model: &model, InputTokens: i64(100), OutputTokens: i64(40)},
		{EventName: strPtr("websocket_event"), Model: &model, InputTokens: i64(7), OutputTokens: i64(3),
			CacheReadTokens: i64(5), CacheCreateTokens: i64(2), CostUSD: f64(0.01)},
		{EventName: strPtr("websocket_event"), Model: &model, InputTokens: i64(3), CostUSD: f64(0.02)},
		{EventName: strPtr("websocket_event"), InputTokens: i64(11)}, // nil model -> its own bucket
	}
	var inInput, inOutput, inCacheRead, inCacheCreate int64
	var inCost float64
	for _, e := range events {
		inInput += deref(e.InputTokens)
		inOutput += deref(e.OutputTokens)
		inCacheRead += deref(e.CacheReadTokens)
		inCacheCreate += deref(e.CacheCreateTokens)
		if e.CostUSD != nil {
			inCost += *e.CostUSD
		}
	}

	f := New(nil, nil)
	kept := f.Apply(events)
	rollups := f.Flush()

	var outInput, outOutput, outCacheRead, outCacheCreate int64
	var outCost float64
	for _, e := range append(kept, rollups...) {
		outInput += deref(e.InputTokens)
		outOutput += deref(e.OutputTokens)
		outCacheRead += deref(e.CacheReadTokens)
		outCacheCreate += deref(e.CacheCreateTokens)
		if e.CostUSD != nil {
			outCost += *e.CostUSD
		}
	}

	if outInput != inInput || outOutput != inOutput ||
		outCacheRead != inCacheRead || outCacheCreate != inCacheCreate {
		t.Fatalf("token conservation broken: in(%d,%d,%d,%d) out(%d,%d,%d,%d)",
			inInput, inOutput, inCacheRead, inCacheCreate,
			outInput, outOutput, outCacheRead, outCacheCreate)
	}
	if outCost != inCost {
		t.Fatalf("cost conservation broken: in %v out %v", inCost, outCost)
	}
	// One rollup per distinct model that accumulated (claude-opus-4-8 and nil).
	if len(rollups) != 2 {
		t.Fatalf("want 2 rollups, got %d", len(rollups))
	}
	for _, r := range rollups {
		if *r.EventName != RollupEventName {
			t.Fatalf("rollup event name = %q", *r.EventName)
		}
	}
}

func TestStatsCounters(t *testing.T) {
	f := New(nil, nil)
	f.Apply([]api.NormalizedEvent{
		ev("sse_event"),
		{EventName: strPtr("websocket_event"), InputTokens: i64(5)},
		ev("websocket_event"),
	})
	f.Flush()
	s := f.Stats()
	if s.Kept != 1 || s.Filtered != 2 || s.Rollups != 1 {
		t.Fatalf("stats = %+v; want kept=1 filtered=2 rollups=1", s)
	}
}

func TestFlushEmpty(t *testing.T) {
	f := New(nil, nil)
	if r := f.Flush(); r != nil {
		t.Fatalf("empty flush should be nil, got %v", r)
	}
}
