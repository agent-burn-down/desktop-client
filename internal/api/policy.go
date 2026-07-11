package api

import "time"

const (
	defaultFlushIntervalSeconds = 30
	defaultMaxBatchSize         = 500
	// MaxIngestBatch is the backend hard cap on events per /ingest/v1/events
	// request (EventsRequest.events max_length). Batches must not exceed it.
	MaxIngestBatch = 2000
)

// Policy carries the server-provided collector tuning returned inside register
// and heartbeat responses.
//
// Parsing is lenient: unknown keys are ignored and missing or non-positive
// values fall back to documented defaults via the accessor methods.
type Policy struct {
	FlushIntervalSeconds int    `json:"flush_interval_seconds"`
	MaxBatchSize         int    `json:"max_batch_size"`
	RefreshCadence       string `json:"refresh_cadence"`
}

// FlushInterval returns the flush interval, defaulting to 30s when the policy
// value is unset or non-positive.
func (p Policy) FlushInterval() time.Duration {
	secs := p.FlushIntervalSeconds
	if secs <= 0 {
		secs = defaultFlushIntervalSeconds
	}
	return time.Duration(secs) * time.Second
}

// BatchSize returns the POST batch size to use, defaulting to 500 when unset
// and capped at the backend hard limit of MaxIngestBatch events per request.
func (p Policy) BatchSize() int {
	n := p.MaxBatchSize
	if n <= 0 {
		n = defaultMaxBatchSize
	}
	if n > MaxIngestBatch {
		n = MaxIngestBatch
	}
	return n
}
