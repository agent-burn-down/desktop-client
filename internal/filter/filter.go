// Package filter implements the upload noise policy: keep signal event
// families, drop bulk noise, and conserve token/cost totals from dropped
// events via periodic synthetic rollup events.
package filter

import (
	"strings"
	"sync"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
)

// RollupEventName is the synthetic event emitted to preserve token and cost
// totals harvested from dropped events.
const RollupEventName = "token_rollup"

// keepFamilies are the chart-relevant event names kept for upload, mirroring
// the yaah retention taxonomy CHART_EVENT_NAMES.
var keepFamilies = map[string]bool{
	"sse_event":               true,
	"tool_result":             true,
	"tool_decision":           true,
	"hook_execution_start":    true,
	"hook_execution_complete": true,
	"api_error":               true,
	"api_retries_exhausted":   true,
	"compaction":              true,
}

// dropFamilies are known high-volume noise event names dropped from upload.
var dropFamilies = map[string]bool{
	"websocket_event": true,
}

// Stats is a snapshot of filter activity for status and doctor output.
type Stats struct {
	Kept     int64
	Filtered int64
	Rollups  int64
}

// bucket accumulates token and cost totals for a single model.
type bucket struct {
	input       int64
	output      int64
	cacheRead   int64
	cacheCreate int64
	cost        float64
	hasCost     bool
}

// Filter applies the upload policy and conserves token/cost totals from
// dropped events. It is safe for concurrent use.
type Filter struct {
	keep map[string]bool
	drop map[string]bool

	mu      sync.Mutex
	pending map[string]*bucket
	stats   Stats
}

// New returns a Filter using the default keep/drop tables, extended with the
// given extra keep and drop event names (keep takes precedence on conflict).
func New(extraKeep, extraDrop []string) *Filter {
	keep := cloneSet(keepFamilies)
	drop := cloneSet(dropFamilies)
	for _, name := range extraKeep {
		keep[name] = true
	}
	for _, name := range extraDrop {
		drop[name] = true
	}
	return &Filter{keep: keep, drop: drop, pending: make(map[string]*bucket)}
}

// Apply returns the events to upload. Dropped events have their token and cost
// totals accumulated into pending rollups grouped by model.
func (f *Filter) Apply(events []api.NormalizedEvent) []api.NormalizedEvent {
	kept := make([]api.NormalizedEvent, 0, len(events))
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range events {
		if f.shouldKeep(name(events[i].EventName)) {
			kept = append(kept, events[i])
			f.stats.Kept++
			continue
		}
		f.stats.Filtered++
		f.accumulate(events[i])
	}
	return kept
}

// shouldKeep decides an event by name: keep wins over drop; unknown names are
// kept (conservative — only known noise is dropped).
func (f *Filter) shouldKeep(eventName string) bool {
	family := strings.TrimPrefix(eventName, "codex.")
	if f.keep[family] {
		return true
	}
	if f.drop[family] {
		return false
	}
	return true
}

// accumulate folds any token/cost values from a dropped event into the pending
// rollup for its model. Callers must hold f.mu.
func (f *Filter) accumulate(ev api.NormalizedEvent) {
	if !hasValue(ev) {
		return
	}
	key := name(ev.Model)
	b := f.pending[key]
	if b == nil {
		b = &bucket{}
		f.pending[key] = b
	}
	b.input += deref(ev.InputTokens)
	b.output += deref(ev.OutputTokens)
	b.cacheRead += deref(ev.CacheReadTokens)
	b.cacheCreate += deref(ev.CacheCreateTokens)
	if ev.CostUSD != nil {
		b.cost += *ev.CostUSD
		b.hasCost = true
	}
}

// Flush drains pending rollups into synthetic token_rollup events, one per
// model that accumulated any value.
func (f *Filter) Flush() []api.NormalizedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pending) == 0 {
		return nil
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	out := make([]api.NormalizedEvent, 0, len(f.pending))
	for model, b := range f.pending {
		out = append(out, rollupEvent(model, b, ts))
		f.stats.Rollups++
	}
	f.pending = make(map[string]*bucket)
	return out
}

// Stats returns a snapshot of kept, filtered, and rollup counters.
func (f *Filter) Stats() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}

func rollupEvent(model string, b *bucket, ts string) api.NormalizedEvent {
	ev := api.NormalizedEvent{
		EventName:         strPtr(RollupEventName),
		Timestamp:         strPtr(ts),
		InputTokens:       nonZero(b.input),
		OutputTokens:      nonZero(b.output),
		CacheReadTokens:   nonZero(b.cacheRead),
		CacheCreateTokens: nonZero(b.cacheCreate),
	}
	if model != "" {
		ev.Model = strPtr(model)
	}
	if b.hasCost {
		cost := b.cost
		ev.CostUSD = &cost
	}
	return ev
}

func hasValue(ev api.NormalizedEvent) bool {
	return ev.InputTokens != nil || ev.OutputTokens != nil ||
		ev.CacheReadTokens != nil || ev.CacheCreateTokens != nil || ev.CostUSD != nil
}

func name(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func nonZero(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func strPtr(s string) *string { return &s }

func cloneSet(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
