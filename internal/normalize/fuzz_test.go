package normalize

import (
	"encoding/json"
	"testing"
)

// FuzzDecodeAttrValue asserts that attribute decoding never panics on arbitrary
// JSON-shaped input.
func FuzzDecodeAttrValue(f *testing.F) {
	seeds := []string{
		`{"stringValue":"hello"}`,
		`{"intValue":"42"}`,
		`{"intValue":"nope"}`,
		`{"doubleValue":"3.14"}`,
		`{"boolValue":true}`,
		`{"arrayValue":{"values":[{"stringValue":"a"},{"intValue":"1"}]}}`,
		`{"unknownKey":"x"}`,
		`"raw"`,
		`null`,
		`[1,2,3]`,
		`{"arrayValue":{"values":"not-a-list"}}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(_ *testing.T, data []byte) {
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return
		}
		_ = decodeAttrValue(v)
	})
}

// FuzzNormalizeLogBatch asserts the batch normalizer never panics on arbitrary
// JSON payloads.
func FuzzNormalizeLogBatch(f *testing.F) {
	seeds := []string{
		`{}`,
		`{"resourceLogs":null}`,
		`{"resourceLogs":[{}]}`,
		`{"resourceLogs":[{"scopeLogs":[{}]}]}`,
		`{"resourceLogs":[{"scopeLogs":[{"logRecords":["x",null]}]}]}`,
		`{"resourceLogs":[{"scopeLogs":[{"logRecords":[` +
			`{"attributes":[{"key":"event.name","value":{"stringValue":"api_request"}}]}]}]}]}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(_ *testing.T, data []byte) {
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return
		}
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		_, _ = NormalizeLogBatch(m, "fuzz-repo")
	})
}
