package normalize

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agent-burn-down/desktop-client/internal/api"
)

// NormalizeLogBatch normalizes an OTLP/HTTP log batch into NormalizedEvents.
//
// It iterates resourceLogs[].scopeLogs[].logRecords[]. Records that lack a
// usable event name (or are otherwise malformed) are counted in dropped and
// never abort the batch; a partial result is always returned. The repo
// argument, when non-empty, overrides any per-record repo attribute.
//
// Privacy: output is built only from an allowlist of metadata attributes. No
// free-text prompt, completion, or tool-payload value is ever copied out.
func NormalizeLogBatch(payload map[string]any, repo string) ([]api.NormalizedEvent, int) {
	var events []api.NormalizedEvent
	dropped := 0
	for _, rl := range asSlice(payload["resourceLogs"]) {
		rlm, ok := rl.(map[string]any)
		if !ok {
			continue
		}
		for _, sl := range asSlice(rlm["scopeLogs"]) {
			slm, ok := sl.(map[string]any)
			if !ok {
				continue
			}
			for _, rec := range asSlice(slm["logRecords"]) {
				if ev := flattenLogRecord(rec, repo); ev != nil {
					events = append(events, *ev)
				} else {
					dropped++
				}
			}
		}
	}
	return events, dropped
}

// flattenLogRecord flattens one OTLP log record into a NormalizedEvent, or nil
// when the record is not a map or carries no usable event name.
func flattenLogRecord(record any, repo string) *api.NormalizedEvent {
	m, ok := record.(map[string]any)
	if !ok {
		return nil
	}
	attrs := attrsToMap(m["attributes"])
	name := normalizeEventName(pyOr(attrs["event.name"], attrs["otel.name"], m["eventName"]))
	if name == nil {
		return nil
	}
	toolName := displayIdentity(
		pyOr(attrs["tool_name"], attrs["tool"], attrs["codex.op"]), maxToolNameLen)
	mcpServer := displayIdentity(pyOr(attrs["mcp.server.name"], attrs["mcp_server"]), maxMCPServerLen)
	mcpTool := displayIdentity(pyOr(attrs["mcp.tool.name"], attrs["mcp_tool"]), maxMCPToolLen)
	derivedServer, derivedTool := deriveMCPIdentity(toolName)
	if mcpServer == nil {
		mcpServer = derivedServer
	}
	if mcpTool == nil {
		mcpTool = derivedTool
	}
	return &api.NormalizedEvent{
		EventName: name,
		Timestamp: eventTimestamp(attrs, m),
		SessionID: asString(firstAttr(attrs, sessionAliases...)),
		Model:     asString(pyOr(attrs["model"], attrs["slug"])),
		ToolName:  toolName,
		MCPServer: mcpServer,
		MCPTool:   mcpTool,
		MCPServerToolCount: boundedInt(
			pyOr(attrs["mcp.server.tool_count"], attrs["mcp_server_tool_count"]),
			0, maxMCPServerToolCount),
		MCPSchemaTokens: boundedInt(
			pyOr(attrs["mcp.schema.tokens"], attrs["mcp_schema_tokens"]),
			0, maxMCPSchemaTokens),
		SkillName:         displayIdentity(attrs["skill_name"], maxSkillNameLen),
		ToolSuccess:       boolish(attrs["success"]),
		ToolDurationMs:    floatOrNil(attrs["duration_ms"]),
		CostUSD:           floatOrNil(attrs["cost_usd"]),
		InputTokens:       intOrNil(pyOr(attrs["input_tokens"], attrs["input_token_count"])),
		OutputTokens:      intOrNil(pyOr(attrs["output_tokens"], attrs["output_token_count"])),
		CacheReadTokens:   intOrNil(pyOr(cacheReadAliases(attrs)...)),
		CacheCreateTokens: intOrNil(attrs["cache_creation_tokens"]),
		Repo:              asString(pyOr(repo, attrs["repo"], attrs["repository"])),
		ErrorMessage:      truncated(asString(pyOr(attrs["error"], attrs["error.message"]))),
	}
}

const (
	maxMCPServerLen       = 120
	maxMCPToolLen         = 160
	maxToolNameLen        = 160
	maxSkillNameLen       = 160
	maxMCPServerToolCount = 10_000
	maxMCPSchemaTokens    = 10_000_000
)

// displayIdentity accepts only short, single-line labels. It deliberately
// rejects path-shaped and structured values so configuration bodies,
// environment assignments, URLs, and raw schemas cannot be retained under an
// attribution field. Whitespace is collapsed for stable grouping.
func displayIdentity(v any, maxLen int) *string {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	s = strings.Join(strings.Fields(s), " ")
	if s == "" || len(s) > maxLen || unsafeDisplayIdentity(s) {
		return nil
	}
	return &s
}

func unsafeDisplayIdentity(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	if strings.ContainsAny(s, "/\\\x00\r\n{}[]=<>") || strings.Contains(s, "://") {
		return true
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// deriveMCPIdentity splits Codex's unambiguous mcp__server__tool convention.
// The original generic tool_name remains untouched for compatibility.
func deriveMCPIdentity(toolName *string) (*string, *string) {
	if toolName == nil || !strings.HasPrefix(*toolName, "mcp__") {
		return nil, nil
	}
	parts := strings.SplitN(strings.TrimPrefix(*toolName, "mcp__"), "__", 2)
	if len(parts) != 2 {
		return nil, nil
	}
	server := displayIdentity(parts[0], maxMCPServerLen)
	tool := displayIdentity(parts[1], maxMCPToolLen)
	if server == nil || tool == nil {
		return nil, nil
	}
	return server, tool
}

func boundedInt(v any, min, max int64) *int64 {
	n := intOrNil(v)
	if n == nil || *n < min || *n > max {
		return nil
	}
	return n
}

var sessionAliases = []string{"session.id", "session_id", "conversation.id", "thread.id"}

func cacheReadAliases(attrs map[string]any) []any {
	return []any{
		attrs["cache_read_tokens"],
		attrs["cached_input_tokens"],
		attrs["cached_token_count"],
	}
}

// eventTimestamp derives the event timestamp, falling back through
// event.timestamp, timestamp, timeUnixNano, and finally the current time.
func eventTimestamp(attrs, record map[string]any) *string {
	v := pyOr(
		attrs["event.timestamp"],
		attrs["timestamp"],
		nanoToIso(record["timeUnixNano"]),
		nowISO(),
	)
	if s := asString(v); s != nil {
		return s
	}
	now := nowISO()
	return &now
}

// claudeNamespace prefixes Claude Code's bare log event names. The hosted
// rollup store derives an event's source from the namespace before the first
// ".", so a bare name ("api_request") must be namespaced to attribute usage to
// Claude Code.
const claudeNamespace = "claude_code."

// normalizeEventName preserves agent namespaces such as "codex." and adds the
// "claude_code." namespace to Claude Code's bare event names (which arrive
// unprefixed) so the hosted rollup store can derive the correct source. Empty
// or non-string names are dropped by returning nil.
func normalizeEventName(v any) *string {
	s, ok := v.(string)
	if !ok || s == "" || s == "codex." {
		return nil
	}
	if !strings.Contains(s, ".") {
		s = claudeNamespace + s
	}
	return &s
}

// maxErrorMessageLen bounds the only free-text passthrough field so a
// misbehaving agent cannot stuff megabytes of diagnostics (or accidental
// prompt fragments) into an uploaded event.
const maxErrorMessageLen = 2048

// truncated caps a string pointer at maxErrorMessageLen bytes on a rune
// boundary; nil passes through.
func truncated(s *string) *string {
	if s == nil || len(*s) <= maxErrorMessageLen {
		return s
	}
	cut := (*s)[:maxErrorMessageLen]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return &cut
}

// asString returns a pointer to v when it is a non-empty string, else nil.
func asString(v any) *string {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	return &s
}

// intOrNil coerces a decoded value to *int64, mirroring Python int(); a nil,
// empty, or unparseable value yields nil.
func intOrNil(v any) *int64 {
	switch r := v.(type) {
	case nil:
		return nil
	case int64:
		return &r
	case int:
		n := int64(r)
		return &n
	case float64:
		n := int64(r)
		return &n
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(r), 10, 64)
		if err != nil {
			return nil
		}
		return &n
	default:
		return nil
	}
}

// floatOrNil coerces a decoded value to *float64, mirroring Python float().
func floatOrNil(v any) *float64 {
	switch r := v.(type) {
	case nil:
		return nil
	case float64:
		return &r
	case int64:
		f := float64(r)
		return &f
	case int:
		f := float64(r)
		return &f
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(r), 64)
		if err != nil {
			return nil
		}
		return &f
	default:
		return nil
	}
}

// boolish coerces a decoded value to *bool using the reference truthy/falsy
// string sets; indeterminate input yields nil.
func boolish(v any) *bool {
	switch r := v.(type) {
	case bool:
		return &r
	case int64:
		b := r != 0
		return &b
	case int:
		b := r != 0
		return &b
	case float64:
		b := int64(r) != 0
		return &b
	case string:
		return boolishString(r)
	default:
		return nil
	}
}

func boolishString(s string) *bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "ok", "success":
		t := true
		return &t
	case "0", "false", "no", "error", "failure":
		f := false
		return &f
	default:
		return nil
	}
}

func asSlice(v any) []any {
	s, ok := v.([]any)
	if !ok {
		return nil
	}
	return s
}
