package api

// SessionModelUsage is one actual model's token contribution. A nil Model is
// explicit unknown attribution; composite display labels are never emitted.
type SessionModelUsage struct {
	Model             *string `json:"model"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CacheReadTokens   int64   `json:"cache_read_tokens"`
	CacheCreateTokens int64   `json:"cache_create_tokens"`
}

// SessionSummary is the strict metadata-only /ingest/v1/sessions contract.
type SessionSummary struct {
	SessionID              string              `json:"session_id"`
	UpdatedAt              string              `json:"updated_at"`
	StartedAt              string              `json:"started_at"`
	LastActivityAt         string              `json:"last_activity_at"`
	EndedAt                *string             `json:"ended_at"`
	Title                  *string             `json:"title"`
	Summary                *string             `json:"summary"`
	Repo                   *string             `json:"repo"`
	WorkspaceLabel         *string             `json:"workspace_label"`
	Model                  *string             `json:"model"`
	Models                 []SessionModelUsage `json:"models"`
	ModelBreakdownComplete bool                `json:"model_breakdown_complete"`
	Source                 *string             `json:"source"`
	Outcome                string              `json:"outcome"`
	StopReason             *string             `json:"stop_reason"`
	DurationMs             *int64              `json:"duration_ms"`
	InputTokens            int64               `json:"input_tokens"`
	OutputTokens           int64               `json:"output_tokens"`
	CacheReadTokens        int64               `json:"cache_read_tokens"`
	CacheCreateTokens      int64               `json:"cache_create_tokens"`
	CostUSD                *float64            `json:"cost_usd"`
	ToolCalls              int64               `json:"tool_calls"`
	ErrorCount             int64               `json:"error_count"`
}
