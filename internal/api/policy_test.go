package api

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPolicyDefaults(t *testing.T) {
	tests := []struct {
		name      string
		policy    Policy
		wantFlush time.Duration
		wantBatch int
	}{
		{"empty applies defaults", Policy{}, 30 * time.Second, 500},
		{
			"explicit values honoured",
			Policy{FlushIntervalSeconds: 45, MaxBatchSize: 200},
			45 * time.Second, 200,
		},
		{
			"batch capped at hard limit",
			Policy{MaxBatchSize: 5000},
			30 * time.Second, MaxIngestBatch,
		},
		{
			"negative values fall back to defaults",
			Policy{FlushIntervalSeconds: -1, MaxBatchSize: -5},
			30 * time.Second, 500,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.policy.FlushInterval(); got != tt.wantFlush {
				t.Errorf("FlushInterval() = %v, want %v", got, tt.wantFlush)
			}
			if got := tt.policy.BatchSize(); got != tt.wantBatch {
				t.Errorf("BatchSize() = %d, want %d", got, tt.wantBatch)
			}
		})
	}
}

func TestPolicyIgnoresUnknownKeys(t *testing.T) {
	raw := `{"flush_interval_seconds":45,"max_batch_size":200,` +
		`"refresh_cadence":"near-real-time","unknown_key":123}`
	var p Policy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.FlushIntervalSeconds != 45 || p.MaxBatchSize != 200 {
		t.Errorf("parsed %+v, want flush=45 batch=200", p)
	}
	if p.RefreshCadence != "near-real-time" {
		t.Errorf("refresh_cadence = %q, want near-real-time", p.RefreshCadence)
	}
}
