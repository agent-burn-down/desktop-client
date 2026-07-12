package counters

// Telemetry is the collector self-telemetry snapshot reported as the optional
// "counters" object in the heartbeat request and mirrored under "telemetry" in
// `status --json`. Deriving both from Report keeps them a single source of
// truth: the values the daemon reports on /healthz and the values it sends to
// the backend are computed the same way. Field names are snake_case to match
// the wire contract.
type Telemetry struct {
	// Received is the count of events accepted by the local OTLP receiver
	// before filtering (normalized events plus normalize drops).
	Received int64 `json:"received"`
	// Filtered is the count of events dropped by the local privacy/keep filter.
	Filtered int64 `json:"filtered"`
	// Uploaded is the count of events the backend accepted on /ingest/v1/events.
	Uploaded int64 `json:"uploaded"`
	// Dropped is the count of events the backend reported as dropped.
	Dropped int64 `json:"dropped"`
	// Errors is the total of pipeline, upload, and heartbeat errors.
	Errors int64 `json:"errors"`
	// QueueDepth is the number of outstanding (not-yet-acked) queue rows.
	QueueDepth int64 `json:"queue_depth"`
	// Version is the collector build version string.
	Version string `json:"version"`
}

// Report derives the self-telemetry snapshot from a counters snapshot map (as
// returned by Registry.Snapshot merged with the live queue depth) and the
// collector version. Both the heartbeat sender and `status --json` call this so
// their reported values always agree.
func Report(snap map[string]int64, version string) Telemetry {
	return Telemetry{
		Received:   snap[Received],
		Filtered:   snap[Filtered],
		Uploaded:   snap[Uploaded],
		Dropped:    snap[UploadDropped],
		Errors:     snap[Errors] + snap[UploadErrors] + snap[HeartbeatErrors],
		QueueDepth: snap[QueueDepth],
		Version:    version,
	}
}
