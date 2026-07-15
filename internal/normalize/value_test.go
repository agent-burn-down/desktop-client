package normalize

import "testing"

func TestBoolish(t *testing.T) {
	cases := []struct {
		in   any
		want *bool
	}{
		{true, boolPtr(true)},
		{false, boolPtr(false)},
		{int64(1), boolPtr(true)},
		{int64(0), boolPtr(false)},
		{0.5, boolPtr(false)}, // bool(int(0.5)) == false
		{1.9, boolPtr(true)},
		{"success", boolPtr(true)},
		{" YES ", boolPtr(true)},
		{"failure", boolPtr(false)},
		{"no", boolPtr(false)},
		{"maybe", nil},
		{nil, nil},
		{[]any{1}, nil},
	}
	for _, tc := range cases {
		got := boolish(tc.in)
		if !eqBool(got, tc.want) {
			t.Fatalf("boolish(%#v) = %v, want %v", tc.in, deBool(got), deBool(tc.want))
		}
	}
}

func TestIntOrNil(t *testing.T) {
	cases := []struct {
		in   any
		want *int64
	}{
		{int64(42), i64(42)},
		{42.9, i64(42)},
		{"1200", i64(1200)},
		{"1200.5", nil},
		{"nope", nil},
		{nil, nil},
		{[]any{}, nil},
	}
	for _, tc := range cases {
		got := intOrNil(tc.in)
		if !eqInt(got, tc.want) {
			t.Fatalf("intOrNil(%#v) = %v, want %v", tc.in, deInt(got), deInt(tc.want))
		}
	}
}

func TestFloatOrNil(t *testing.T) {
	cases := []struct {
		in   any
		want *float64
	}{
		{3.14, f64(3.14)},
		{int64(7), f64(7)},
		{"0.0123", f64(0.0123)},
		{"nope", nil},
		{nil, nil},
	}
	for _, tc := range cases {
		got := floatOrNil(tc.in)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Fatalf("floatOrNil(%#v) = %v, want %v", tc.in, deFloat(got), deFloat(tc.want))
		}
	}
}

func TestFirstAttrKeepsZeroAndFalse(t *testing.T) {
	attrs := map[string]any{"a": nil, "b": "", "c": int64(0)}
	// nil and "" are skipped, 0 is returned (differs from pyOr).
	if got := firstAttr(attrs, "a", "b", "c"); got != int64(0) {
		t.Fatalf("firstAttr = %v, want 0", got)
	}
	if got := firstAttr(attrs, "a", "b"); got != nil {
		t.Fatalf("firstAttr(all empty) = %v, want nil", got)
	}
}

func TestNormalizeEventNameEdge(t *testing.T) {
	if normalizeEventName("codex.") != nil {
		t.Fatal("codex. with empty suffix should drop")
	}
	if normalizeEventName(123) != nil {
		t.Fatal("non-string name should drop")
	}
	if got := normalizeEventName("codex.a.b"); got == nil || *got != "codex.a.b" {
		t.Fatalf("codex.a.b -> %v, want codex.a.b", got)
	}
}

func boolPtr(b bool) *bool { return &b }

func eqBool(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func eqInt(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func deBool(p *bool) any {
	if p == nil {
		return nil
	}
	return *p
}

func deInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func deFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }
