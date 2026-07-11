// Package counters provides a small thread-safe registry of named int64
// pipeline counters. The daemon and uploader share one registry; its snapshot
// is served on the receiver's GET /healthz for the status and doctor commands.
//
// Timestamp-style counters (for example last_upload_at) are stored as Unix
// seconds so they fit the same int64 map exposed over /healthz.
package counters

import "sync"

// Registry is a concurrency-safe map of counter name to value.
type Registry struct {
	mu   sync.Mutex
	vals map[string]int64
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{vals: make(map[string]int64)}
}

// Add increments the named counter by delta (delta may be negative).
func (r *Registry) Add(name string, delta int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vals[name] += delta
}

// Set replaces the named counter's value (used for gauges and timestamps).
func (r *Registry) Set(name string, value int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vals[name] = value
}

// Get returns the named counter's value, or zero if unset.
func (r *Registry) Get(name string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.vals[name]
}

// Snapshot returns a copy of all counters safe for the caller to mutate.
func (r *Registry) Snapshot() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int64, len(r.vals))
	for k, v := range r.vals {
		out[k] = v
	}
	return out
}
