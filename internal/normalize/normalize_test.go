package normalize

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agent-burn-down/desktop-client/internal/api"
)

func TestDecodeAttrValue(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want any
	}{
		{"string", map[string]any{"stringValue": "hello"}, "hello"},
		{"int", map[string]any{"intValue": "42"}, int64(42)},
		{"int bad", map[string]any{"intValue": "nope"}, nil},
		{"double", map[string]any{"doubleValue": "3.14"}, 3.14},
		{"bool", map[string]any{"boolValue": true}, true},
		{
			"array",
			map[string]any{"arrayValue": map[string]any{
				"values": []any{
					map[string]any{"stringValue": "a"},
					map[string]any{"intValue": "1"},
				},
			}},
			[]any{"a", int64(1)},
		},
		{"unknown shape", map[string]any{"unknownKey": "x"}, nil},
		{"non-dict passthrough", "raw", "raw"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeAttrValue(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("decodeAttrValue(%#v) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestAttrsToMap(t *testing.T) {
	attrs := []any{
		map[string]any{"key": "model", "value": map[string]any{"stringValue": "claude-opus-4-8"}},
		map[string]any{"key": "input_tokens", "value": map[string]any{"intValue": "1500"}},
		map[string]any{"value": map[string]any{"stringValue": "orphan"}}, // no key -> skipped
	}
	got := attrsToMap(attrs)
	want := map[string]any{"model": "claude-opus-4-8", "input_tokens": int64(1500)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attrsToMap = %#v, want %#v", got, want)
	}
	if len(attrsToMap(nil)) != 0 || len(attrsToMap([]any{})) != 0 {
		t.Fatal("empty attrs should yield empty map")
	}
}

func TestNanoToIso(t *testing.T) {
	if got := nanoToIso("1700000000000000000"); got != "2023-11-14T22:13:20Z" {
		t.Fatalf("nanoToIso = %v, want 2023-11-14T22:13:20Z", got)
	}
	if got := nanoToIso("not-a-number"); got != nil {
		t.Fatalf("nanoToIso(bad) = %v, want nil", got)
	}
	if got := nanoToIso(nil); got != nil {
		t.Fatalf("nanoToIso(nil) = %v, want nil", got)
	}
}

var realisticPayload = map[string]any{
	"resourceLogs": []any{
		map[string]any{
			"scopeLogs": []any{
				map[string]any{
					"logRecords": []any{
						map[string]any{
							"timeUnixNano": "1750000000000000000",
							"attributes": []any{
								attr("event.name", "stringValue", "api_request"),
								attr("session.id", "stringValue", "sess-abc123"),
								attr("model", "stringValue", "claude-opus-4-8"),
								attr("input_tokens", "intValue", "1200"),
								attr("output_tokens", "intValue", "340"),
								attr("cost_usd", "doubleValue", "0.0123"),
								// Free-text attrs that must never reach the output.
								attr("prompt", "stringValue", "SECRET-PROMPT-TEXT"),
								attr("completion", "stringValue", "SECRET-COMPLETION"),
								attr("tool_input", "stringValue", "SECRET-TOOL-INPUT"),
							},
						},
						map[string]any{
							"timeUnixNano": "1750000001000000000",
							"attributes": []any{
								attr("event.name", "stringValue", "tool_use"),
								attr("session.id", "stringValue", "sess-abc123"),
								attr("tool_name", "stringValue", "bash"),
								attrBool("success", true),
								attr("duration_ms", "doubleValue", "450.5"),
							},
						},
						// Bad record: no event name -> dropped.
						map[string]any{"timeUnixNano": "1750000002000000000", "attributes": []any{}},
					},
				},
			},
		},
	},
}

func attr(key, kind string, val any) map[string]any {
	return map[string]any{"key": key, "value": map[string]any{kind: val}}
}

func attrBool(key string, val bool) map[string]any {
	return map[string]any{"key": key, "value": map[string]any{"boolValue": val}}
}

func TestNormalizeLogBatchCounts(t *testing.T) {
	events, dropped := NormalizeLogBatch(realisticPayload, "yaah-hosted")
	if len(events) != 2 || dropped != 1 {
		t.Fatalf("got %d events, %d dropped; want 2, 1", len(events), dropped)
	}
}

func TestNormalizeLogBatchAPIRequestShape(t *testing.T) {
	events, _ := NormalizeLogBatch(realisticPayload, "yaah-hosted")
	ev := findEvent(t, events, "claude_code.api_request")
	assertStr(t, "session_id", ev.SessionID, "sess-abc123")
	assertStr(t, "model", ev.Model, "claude-opus-4-8")
	assertInt(t, "input_tokens", ev.InputTokens, 1200)
	assertInt(t, "output_tokens", ev.OutputTokens, 340)
	assertFloat(t, "cost_usd", ev.CostUSD, 0.0123)
	assertStr(t, "repo", ev.Repo, "yaah-hosted")
}

func TestNormalizeLogBatchToolUseShape(t *testing.T) {
	events, _ := NormalizeLogBatch(realisticPayload, "yaah-hosted")
	ev := findEvent(t, events, "claude_code.tool_use")
	assertStr(t, "tool_name", ev.ToolName, "bash")
	if ev.ToolSuccess == nil || !*ev.ToolSuccess {
		t.Fatalf("tool_success = %v, want true", ev.ToolSuccess)
	}
	assertFloat(t, "tool_duration_ms", ev.ToolDurationMs, 450.5)
}

func TestPrivacyNoFreeText(t *testing.T) {
	events, _ := NormalizeLogBatch(realisticPayload, "yaah-hosted")
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET-PROMPT-TEXT", "SECRET-COMPLETION", "SECRET-TOOL-INPUT"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("output leaked free-text attr %q: %s", secret, raw)
		}
	}
}

func TestNormalizeLogBatchRepoOverride(t *testing.T) {
	events, _ := NormalizeLogBatch(realisticPayload, "override-repo")
	for _, ev := range events {
		assertStr(t, "repo", ev.Repo, "override-repo")
	}
}

func TestNormalizeLogBatchEmptyPayload(t *testing.T) {
	events, dropped := NormalizeLogBatch(map[string]any{}, "")
	if len(events) != 0 || dropped != 0 {
		t.Fatalf("empty payload: got %d events, %d dropped; want 0, 0", len(events), dropped)
	}
}

func TestNormalizeLogBatchMalformedRecords(t *testing.T) {
	payload := map[string]any{
		"resourceLogs": []any{
			map[string]any{
				"scopeLogs": []any{
					map[string]any{"logRecords": []any{"not-a-dict", nil}},
				},
			},
		},
	}
	events, dropped := NormalizeLogBatch(payload, "")
	if len(events) != 0 || dropped != 2 {
		t.Fatalf("malformed: got %d events, %d dropped; want 0, 2", len(events), dropped)
	}
}

func TestNormalizeEventNameCodexPrefix(t *testing.T) {
	payload := map[string]any{
		"resourceLogs": []any{
			map[string]any{
				"scopeLogs": []any{
					map[string]any{
						"logRecords": []any{
							map[string]any{
								"timeUnixNano": "1750000000000000000",
								"attributes": []any{
									attr("event.name", "stringValue", "codex.api_request"),
									attr("model", "stringValue", "gpt-4o"),
								},
							},
						},
					},
				},
			},
		},
	}
	events, _ := NormalizeLogBatch(payload, "")
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	assertStr(t, "event_name", events[0].EventName, "codex.api_request")
}

func TestNormalizeEventNameClaudeBareGetsNamespaced(t *testing.T) {
	payload := map[string]any{
		"resourceLogs": []any{
			map[string]any{
				"scopeLogs": []any{
					map[string]any{
						"logRecords": []any{
							map[string]any{
								"timeUnixNano": "1750000000000000000",
								"attributes": []any{
									attr("event.name", "stringValue", "api_request"),
									attr("model", "stringValue", "claude-opus-4-8"),
								},
							},
						},
					},
				},
			},
		},
	}
	events, _ := NormalizeLogBatch(payload, "")
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	assertStr(t, "event_name", events[0].EventName, "claude_code.api_request")
}

func TestCodexErrorMessageAliasAndPrivacy(t *testing.T) {
	payload := map[string]any{
		"resourceLogs": []any{map[string]any{
			"scopeLogs": []any{map[string]any{
				"logRecords": []any{map[string]any{
					"attributes": []any{
						attr("event.name", "stringValue", "codex.api_error"),
						attr("error.message", "stringValue", "request failed"),
						attr("prompt", "stringValue", "SECRET-CODEX-PROMPT"),
						attr("completion", "stringValue", "SECRET-CODEX-COMPLETION"),
						attr("tool_input", "stringValue", "SECRET-CODEX-TOOL-PAYLOAD"),
					},
				}},
			}},
		}},
	}
	events, _ := NormalizeLogBatch(payload, "")
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	assertStr(t, "error_message", events[0].ErrorMessage, "request failed")
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET-CODEX-PROMPT", "SECRET-CODEX-COMPLETION", "SECRET-CODEX-TOOL-PAYLOAD"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("output leaked %q: %s", secret, raw)
		}
	}
}

func TestNormalizedEventHasAll15Keys(t *testing.T) {
	events, _ := NormalizeLogBatch(realisticPayload, "test-repo")
	raw, err := json.Marshal(events[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"event_id", "event_name", "timestamp", "session_id", "model", "tool_name",
		"tool_success", "tool_duration_ms", "cost_usd", "input_tokens",
		"output_tokens", "cache_read_tokens", "cache_create_tokens",
		"repo", "error_message",
	}
	if len(m) != len(want) {
		t.Fatalf("got %d keys, want %d: %v", len(m), len(want), m)
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing key %q", k)
		}
	}
}

// TestTokenAliasZeroFalls verifies the Python-`or` fallthrough on falsy 0.
func TestTokenAliasZeroFalls(t *testing.T) {
	attrs := map[string]any{"input_tokens": int64(0), "input_token_count": int64(99)}
	got := intOrNil(pyOr(attrs["input_tokens"], attrs["input_token_count"]))
	if got == nil || *got != 99 {
		t.Fatalf("zero primary should fall to alias: got %v, want 99", got)
	}
}

func findEvent(t *testing.T, events []api.NormalizedEvent, name string) api.NormalizedEvent {
	t.Helper()
	for _, ev := range events {
		if ev.EventName != nil && *ev.EventName == name {
			return ev
		}
	}
	t.Fatalf("event %q not found", name)
	return api.NormalizedEvent{}
}

func assertStr(t *testing.T, field string, got *string, want string) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %q", field, got, want)
	}
}

func assertInt(t *testing.T, field string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %d", field, got, want)
	}
}

func assertFloat(t *testing.T, field string, got *float64, want float64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %v", field, got, want)
	}
}

func TestErrorMessageTruncated(t *testing.T) {
	long := strings.Repeat("x", maxErrorMessageLen-1) + "ééé"
	payload := map[string]any{
		"resourceLogs": []any{map[string]any{
			"scopeLogs": []any{map[string]any{
				"logRecords": []any{map[string]any{
					"attributes": []any{
						attr("event.name", "stringValue", "api_error"),
						attr("error", "stringValue", long),
					},
				}},
			}},
		}},
	}
	events, _ := NormalizeLogBatch(payload, "")
	if len(events) != 1 || events[0].ErrorMessage == nil {
		t.Fatalf("want 1 event with error_message, got %+v", events)
	}
	got := *events[0].ErrorMessage
	if len(got) > maxErrorMessageLen {
		t.Fatalf("error_message length %d exceeds cap %d", len(got), maxErrorMessageLen)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation broke UTF-8 validity")
	}
}
