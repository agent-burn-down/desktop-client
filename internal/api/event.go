package api

// NormalizedEvent is the wire contract for a single telemetry record posted to
// POST /ingest/v1/events.
//
// Every field is a pointer and every JSON tag omits omitempty so all 20 keys
// are always emitted, with null for unset values. This mirrors the backend
// NormalizedEvent schema, whose normalizers always emit the full key set.
type NormalizedEvent struct {
	// EventID is the client-minted UUIDv7 idempotency key (queue.Item.EventID),
	// attached when the queue hands a batch to the uploader — never set by the
	// normalizer. Identical event_ids within the backend's dedupe window are
	// counted once, so a crash-after-send-before-ack replay is never
	// double-counted.
	EventID            *string  `json:"event_id"`
	EventName          *string  `json:"event_name"`
	Timestamp          *string  `json:"timestamp"`
	SessionID          *string  `json:"session_id"`
	Model              *string  `json:"model"`
	ToolName           *string  `json:"tool_name"`
	MCPServer          *string  `json:"mcp_server"`
	MCPTool            *string  `json:"mcp_tool"`
	MCPServerToolCount *int64   `json:"mcp_server_tool_count"`
	MCPSchemaTokens    *int64   `json:"mcp_schema_tokens"`
	SkillName          *string  `json:"skill_name"`
	ToolSuccess        *bool    `json:"tool_success"`
	ToolDurationMs     *float64 `json:"tool_duration_ms"`
	CostUSD            *float64 `json:"cost_usd"`
	InputTokens        *int64   `json:"input_tokens"`
	OutputTokens       *int64   `json:"output_tokens"`
	CacheReadTokens    *int64   `json:"cache_read_tokens"`
	CacheCreateTokens  *int64   `json:"cache_create_tokens"`
	Repo               *string  `json:"repo"`
	ErrorMessage       *string  `json:"error_message"`
}
