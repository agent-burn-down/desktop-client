package normalize

import (
	"testing"
)

// realisticMetricsPayload is a Claude Code OTLP metrics exporter fixture: two
// data points on a "sum" metric (one with attributes, one bare), one on a
// "gauge" metric, and one malformed entry (missing name) that must be dropped
// without a value count, plus a data point with neither asDouble nor asInt.
var realisticMetricsPayload = map[string]any{
	"resourceMetrics": []any{
		map[string]any{
			"scopeMetrics": []any{
				map[string]any{
					"metrics": []any{
						map[string]any{
							"name": "claude_code.commit.count",
							"sum": map[string]any{
								"dataPoints": []any{
									map[string]any{
										"asInt":        "1",
										"timeUnixNano": "1700000000000000000",
										"attributes": []any{
											map[string]any{
												"key":   "repo",
												"value": map[string]any{"stringValue": "test-repo"},
											},
											map[string]any{
												"key":   "session.id",
												"value": map[string]any{"stringValue": "sess-abc123"},
											},
										},
									},
									map[string]any{"asInt": "2"},
								},
							},
						},
						map[string]any{
							"name": "claude_code.cost.usage",
							"gauge": map[string]any{
								"dataPoints": []any{
									map[string]any{"asDouble": "0.0125"},
								},
							},
						},
						map[string]any{
							"name": "",
							"sum": map[string]any{
								"dataPoints": []any{map[string]any{"asInt": "5"}},
							},
						},
						map[string]any{
							"name": "claude_code.token.usage",
							"sum": map[string]any{
								"dataPoints": []any{
									map[string]any{"attributes": []any{}}, // no asDouble/asInt -> dropped
								},
							},
						},
					},
				},
			},
		},
	},
}

func TestNormalizeMetricsBatchHappyPath(t *testing.T) {
	points, dropped := NormalizeMetricsBatch(realisticMetricsPayload, "")
	if len(points) != 3 {
		t.Fatalf("got %d points, want 3: %+v", len(points), points)
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1 (the point with no asDouble/asInt)", dropped)
	}

	first := points[0]
	if first.MetricName != "claude_code.commit.count" {
		t.Errorf("points[0].MetricName = %q, want claude_code.commit.count", first.MetricName)
	}
	if first.Value != 1 {
		t.Errorf("points[0].Value = %v, want 1", first.Value)
	}
	if first.Repo == nil || *first.Repo != "test-repo" {
		t.Errorf("points[0].Repo = %v, want test-repo", first.Repo)
	}
	if first.SessionID == nil || *first.SessionID != "sess-abc123" {
		t.Errorf("points[0].SessionID = %v, want sess-abc123", first.SessionID)
	}
	if first.Timestamp == nil || *first.Timestamp != "2023-11-14T22:13:20Z" {
		t.Errorf("points[0].Timestamp = %v, want 2023-11-14T22:13:20Z", first.Timestamp)
	}

	second := points[1]
	if second.MetricName != "claude_code.commit.count" || second.Value != 2 {
		t.Errorf("points[1] = %+v, want commit.count=2", second)
	}
	if second.Timestamp == nil {
		t.Error("points[1].Timestamp = nil, want a fallback-to-now timestamp")
	}

	third := points[2]
	if third.MetricName != "claude_code.cost.usage" || third.Value != 0.0125 {
		t.Errorf("points[2] = %+v, want cost.usage=0.0125", third)
	}
}

func TestNormalizeMetricsBatchRepoOverride(t *testing.T) {
	payload := map[string]any{
		"resourceMetrics": []any{
			map[string]any{
				"scopeMetrics": []any{
					map[string]any{
						"metrics": []any{
							map[string]any{
								"name": "claude_code.lines_of_code.count",
								"sum": map[string]any{
									"dataPoints": []any{
										map[string]any{
											"asInt": "120",
											"attributes": []any{
												map[string]any{
													"key":   "repo",
													"value": map[string]any{"stringValue": "attr-repo"},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	points, dropped := NormalizeMetricsBatch(payload, "override-repo")
	if dropped != 0 || len(points) != 1 {
		t.Fatalf("got %d points, %d dropped, want 1 point 0 dropped", len(points), dropped)
	}
	if points[0].Repo == nil || *points[0].Repo != "override-repo" {
		t.Errorf("Repo = %v, want override-repo (repo arg wins over attribute)", points[0].Repo)
	}
}

func TestNormalizeMetricsBatchMalformedShapesSkipped(t *testing.T) {
	payload := map[string]any{
		"resourceMetrics": []any{
			"not a map",
			map[string]any{"scopeMetrics": "not a slice"},
			map[string]any{"scopeMetrics": []any{"not a map"}},
			map[string]any{"scopeMetrics": []any{
				map[string]any{"metrics": []any{"not a map"}},
			}},
		},
	}
	points, dropped := NormalizeMetricsBatch(payload, "")
	if len(points) != 0 || dropped != 0 {
		t.Fatalf("got %d points, %d dropped, want 0/0 for malformed shapes", len(points), dropped)
	}
}

func TestFirstPresentTreatsZeroAsPresent(t *testing.T) {
	if got := firstPresent(0.0, "unused"); got != 0.0 {
		t.Fatalf("firstPresent(0.0, ...) = %v, want 0.0 (a real zero must not be skipped)", got)
	}
	if got := firstPresent(nil, "fallback"); got != "fallback" {
		t.Fatalf("firstPresent(nil, fallback) = %v, want fallback", got)
	}
	if got := firstPresent(nil, nil); got != nil {
		t.Fatalf("firstPresent(nil, nil) = %v, want nil", got)
	}
}
