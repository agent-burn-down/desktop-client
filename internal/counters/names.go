package counters

// Counter names shared across the pipeline. Centralised so the daemon,
// uploader, and /healthz consumers agree on spelling.
const (
	// Receiver → normalize → filter → queue path (owned by the daemon).
	Received   = "received"
	Normalized = "normalized"
	Filtered   = "filtered"
	Queued     = "queued"
	Rollups    = "rollups"
	Errors     = "errors"
	QueueDepth = "queue_depth"

	// Uploader path.
	Uploaded        = "uploaded"
	UploadDropped   = "upload_dropped"
	UploadErrors    = "upload_errors"
	HeartbeatErrors = "heartbeat_errors"
	LastUploadAt    = "last_upload_at"
	LastHeartbeatAt = "last_heartbeat_at"
	// AuthFailed is 1 while flushing is paused after a 401, else 0.
	AuthFailed = "auth_failed"
)
