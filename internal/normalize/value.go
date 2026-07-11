package normalize

import (
	"strconv"
	"strings"
	"time"
)

// decodeAttrValue decodes a single OTLP attribute value into a Go scalar.
//
// It mirrors the reference collector: non-map values pass through unchanged;
// maps are inspected for stringValue, intValue, doubleValue, boolValue (in that
// order) or an arrayValue.values list. Unparseable numbers and unknown shapes
// decode to nil.
func decodeAttrValue(value any) any {
	m, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if raw, ok := m["stringValue"]; ok {
		return raw
	}
	if raw, ok := m["intValue"]; ok {
		return coerceIntRaw(raw)
	}
	if raw, ok := m["doubleValue"]; ok {
		return coerceFloatRaw(raw)
	}
	if raw, ok := m["boolValue"]; ok {
		return raw
	}
	return decodeArray(m)
}

// decodeArray decodes an OTLP arrayValue into a []any, recursing over elements.
func decodeArray(m map[string]any) any {
	av, ok := m["arrayValue"].(map[string]any)
	if !ok {
		return nil
	}
	values, ok := av["values"].([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, decodeAttrValue(v))
	}
	return out
}

// attrsToMap flattens an OTLP attributes list into a key->value map, skipping
// non-map items and items without a non-empty string key (last write wins).
func attrsToMap(attrs any) map[string]any {
	out := make(map[string]any)
	list, ok := attrs.([]any)
	if !ok {
		return out
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key, ok := m["key"].(string)
		if !ok || key == "" {
			continue
		}
		out[key] = decodeAttrValue(m["value"])
	}
	return out
}

// nanoToIso converts a nanosecond Unix timestamp to an ISO-8601 UTC string
// ending in "Z", truncated to whole seconds. Unparseable input yields nil.
func nanoToIso(nano any) any {
	n, ok := coerceIntRaw(nano).(int64)
	if !ok {
		return nil
	}
	return time.Unix(n/1_000_000_000, 0).UTC().Format(time.RFC3339)
}

// nowISO returns the current UTC time as an ISO-8601 seconds string ending "Z".
func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// firstAttr returns the first attribute value that is neither nil nor an empty
// string across the given name aliases, else nil. Unlike pyOr it treats zero
// and false as present (mirrors the reference _first_attr).
func firstAttr(attrs map[string]any, names ...string) any {
	for _, name := range names {
		v := attrs[name]
		if v != nil && v != "" {
			return v
		}
	}
	return nil
}

// pyOr mirrors Python's short-circuit `a or b or c`: it returns the first
// truthy value, or the last value if none is truthy.
func pyOr(vals ...any) any {
	var last any
	for _, v := range vals {
		last = v
		if truthy(v) {
			return v
		}
	}
	return last
}

// truthy reports whether a decoded attribute value is truthy in Python terms:
// nil, "", 0, false, and empty collections are falsy.
func truthy(v any) bool {
	switch r := v.(type) {
	case nil:
		return false
	case bool:
		return r
	case string:
		return r != ""
	case []any:
		return len(r) > 0
	case map[string]any:
		return len(r) > 0
	default:
		return numericTruthy(v)
	}
}

func numericTruthy(v any) bool {
	switch r := v.(type) {
	case int64:
		return r != 0
	case int:
		return r != 0
	case float64:
		return r != 0
	default:
		return true
	}
}

// coerceIntRaw converts a raw JSON scalar to an int64 (as any), mirroring
// Python int(): numeric strings and numbers convert, anything else yields nil.
func coerceIntRaw(raw any) any {
	switch r := raw.(type) {
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(r), 10, 64)
		if err != nil {
			return nil
		}
		return n
	case float64:
		return int64(r)
	case int64:
		return r
	case int:
		return int64(r)
	default:
		return nil
	}
}

// coerceFloatRaw converts a raw JSON scalar to a float64 (as any), mirroring
// Python float(): numeric strings and numbers convert, anything else is nil.
func coerceFloatRaw(raw any) any {
	switch r := raw.(type) {
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(r), 64)
		if err != nil {
			return nil
		}
		return f
	case float64:
		return r
	case int64:
		return float64(r)
	case int:
		return float64(r)
	default:
		return nil
	}
}
