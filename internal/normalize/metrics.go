package normalize

import "github.com/agent-burn-down/desktop-client/internal/api"

// aggregationKeys are the OTLP metric aggregation types that carry dataPoints.
var aggregationKeys = []string{"sum", "gauge", "histogram", "summary"}

// NormalizeMetricsBatch normalizes an OTLP/HTTP metrics batch into MetricPoints.
//
// It iterates resourceMetrics[].scopeMetrics[].metrics[], and for each metric's
// sum/gauge/histogram/summary aggregation, its dataPoints[]. Points that lack a
// usable metric name or numeric value are counted in dropped and never abort
// the batch; a partial result is always returned. The repo argument, when
// non-empty, overrides any per-point repo attribute.
//
// Privacy: output is built only from an allowlist of metadata attributes,
// mirroring NormalizeLogBatch. No free-text value is ever copied out.
func NormalizeMetricsBatch(payload map[string]any, repo string) ([]api.MetricPoint, int) {
	var points []api.MetricPoint
	dropped := 0
	for _, rm := range asSlice(payload["resourceMetrics"]) {
		rmm, ok := rm.(map[string]any)
		if !ok {
			continue
		}
		for _, sm := range asSlice(rmm["scopeMetrics"]) {
			smm, ok := sm.(map[string]any)
			if !ok {
				continue
			}
			for _, metric := range asSlice(smm["metrics"]) {
				pts, drop := flattenMetric(metric, repo)
				points = append(points, pts...)
				dropped += drop
			}
		}
	}
	return points, dropped
}

// flattenMetric flattens one OTLP metric's data points across whichever
// aggregation type (sum/gauge/histogram/summary) it carries. A metric without
// a usable name yields no points and no drops (nothing to count yet).
func flattenMetric(metric any, repo string) ([]api.MetricPoint, int) {
	m, ok := metric.(map[string]any)
	if !ok {
		return nil, 0
	}
	name, ok := m["name"].(string)
	if !ok || name == "" {
		return nil, 0
	}
	var points []api.MetricPoint
	dropped := 0
	for _, key := range aggregationKeys {
		agg, ok := m[key].(map[string]any)
		if !ok {
			continue
		}
		for _, dp := range asSlice(agg["dataPoints"]) {
			if p := flattenDataPoint(dp, name, repo); p != nil {
				points = append(points, *p)
			} else {
				dropped++
			}
		}
	}
	return points, dropped
}

// flattenDataPoint flattens one OTLP data point into a MetricPoint, or nil
// when the point is not a map or carries no numeric value.
func flattenDataPoint(dp any, name, repo string) *api.MetricPoint {
	m, ok := dp.(map[string]any)
	if !ok {
		return nil
	}
	value := floatOrNil(firstPresent(m["asDouble"], m["asInt"]))
	if value == nil {
		return nil
	}
	attrs := attrsToMap(m["attributes"])
	return &api.MetricPoint{
		MetricName: name,
		Value:      *value,
		Timestamp:  metricTimestamp(m),
		Model:      asString(pyOr(attrs["model"], attrs["slug"])),
		Repo:       asString(pyOr(repo, attrs["repo"], attrs["repository"])),
		SessionID:  asString(firstAttr(attrs, sessionAliases...)),
	}
}

// metricTimestamp derives the data point's timestamp from timeUnixNano,
// falling back to the current time. Unlike eventTimestamp there is no
// attribute-based override — timeUnixNano is the data point's canonical time.
func metricTimestamp(dp map[string]any) *string {
	v := pyOr(nanoToIso(dp["timeUnixNano"]), nowISO())
	if s := asString(v); s != nil {
		return s
	}
	now := nowISO()
	return &now
}

// firstPresent returns the first non-nil value, unlike pyOr's Python-truthy
// selection — a legitimate zero value (asDouble: 0) must not be skipped in
// favor of a later argument.
func firstPresent(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}
