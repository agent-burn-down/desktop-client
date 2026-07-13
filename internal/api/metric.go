package api

// MetricPoint is the wire contract for a single metric datapoint posted to
// POST /ingest/v1/metrics, mirroring the backend's MetricPoint schema exactly.
// Unlike NormalizedEvent, MetricName and Value are required (not Optional) on
// the backend, so they are plain fields rather than pointers.
type MetricPoint struct {
	MetricName string  `json:"metric_name"`
	Value      float64 `json:"value"`
	Timestamp  *string `json:"timestamp"`
	Model      *string `json:"model"`
	Repo       *string `json:"repo"`
	SessionID  *string `json:"session_id"`
}
