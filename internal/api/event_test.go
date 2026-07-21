package api

import (
	"encoding/json"
	"testing"
)

// allEventKeys is the exact set of keys the wire contract requires on every
// emitted event.
var allEventKeys = []string{
	"event_id", "event_name", "timestamp", "session_id", "model", "tool_name",
	"mcp_server", "mcp_tool", "mcp_server_tool_count", "mcp_schema_tokens", "skill_name",
	"tool_success", "tool_duration_ms", "cost_usd", "input_tokens",
	"output_tokens", "cache_read_tokens", "cache_create_tokens", "repo",
	"error_message",
}

func TestNormalizedEventEmitsAllKeysAsNull(t *testing.T) {
	data, err := json.Marshal(NormalizedEvent{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m) != len(allEventKeys) {
		t.Errorf("got %d keys, want %d: %s", len(m), len(allEventKeys), data)
	}
	for _, k := range allEventKeys {
		v, ok := m[k]
		if !ok {
			t.Errorf("missing key %q", k)
			continue
		}
		if string(v) != "null" {
			t.Errorf("key %q = %s, want null", k, v)
		}
	}
}

func TestNormalizedEventEmitsSetValues(t *testing.T) {
	name := "tool_use"
	success := true
	dur := 450.5
	in := int64(1200)
	mcpServer := "github"
	mcpTool := "get_issue"
	mcpToolCount := int64(12)
	mcpSchemaTokens := int64(3400)
	skillName := "issue tracker"
	ev := NormalizedEvent{
		EventName:          &name,
		MCPServer:          &mcpServer,
		MCPTool:            &mcpTool,
		MCPServerToolCount: &mcpToolCount,
		MCPSchemaTokens:    &mcpSchemaTokens,
		SkillName:          &skillName,
		ToolSuccess:        &success,
		ToolDurationMs:     &dur,
		InputTokens:        &in,
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(m) != len(allEventKeys) {
		t.Errorf("got %d keys, want %d", len(m), len(allEventKeys))
	}
	checks := map[string]string{
		"event_name":            `"tool_use"`,
		"tool_success":          "true",
		"tool_duration_ms":      "450.5",
		"input_tokens":          "1200",
		"mcp_server":            `"github"`,
		"mcp_tool":              `"get_issue"`,
		"mcp_server_tool_count": "12",
		"mcp_schema_tokens":     "3400",
		"skill_name":            `"issue tracker"`,
		"model":                 "null",
		"repo":                  "null",
	}
	for k, want := range checks {
		if got := string(m[k]); got != want {
			t.Errorf("key %q = %s, want %s", k, got, want)
		}
	}
}
