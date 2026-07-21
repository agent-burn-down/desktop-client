package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/api"
)

func newTestStore(t *testing.T) *FileStore {
	t.Helper()
	t.Setenv(EnvConfigDir, t.TempDir())
	s, err := NewFileStore()
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return s
}

func TestRoundTrip(t *testing.T) {
	s := newTestStore(t)
	want := &Config{
		APIURL:       "https://api.example.com",
		CollectorKey: "yaahc_secret",
		CollectorID:  123,
		UserEmail:    "user@example.com",
		Machine:      "laptop",
		Policy: api.Policy{
			FlushIntervalSeconds: 30,
			MaxBatchSize:         500,
			RefreshCadence:       "near-real-time",
			InventoryEnabled:     true,
		},
		InventoryStatus:       "current",
		InventoryLastUploadAt: "2026-07-21T20:00:00Z",
		InventoryItemCount:    7,
	}
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.APIURL != want.APIURL || got.CollectorKey != want.CollectorKey ||
		got.CollectorID != want.CollectorID || got.UserEmail != want.UserEmail ||
		got.Machine != want.Machine || got.KeyExpiresAt != want.KeyExpiresAt ||
		got.InventoryStatus != want.InventoryStatus ||
		got.InventoryLastUploadAt != want.InventoryLastUploadAt ||
		got.InventoryItemCount != want.InventoryItemCount {
		t.Errorf("scalar round-trip mismatch:\n got %+v\nwant %+v", *got, *want)
	}
	if got.Policy != want.Policy {
		t.Errorf("policy round-trip mismatch: got %+v, want %+v", got.Policy, want.Policy)
	}
}

func TestReceiverPortRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save(&Config{APIURL: "https://api.example.com", ReceiverPort: 8766}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ReceiverPort != 8766 {
		t.Errorf("ReceiverPort round-trip = %d, want 8766", got.ReceiverPort)
	}
}

func TestPermissionsOnCreate(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save(&Config{Machine: "m"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if got := fi.Mode().Perm(); got != filePerm {
		t.Errorf("file perm = %o, want %o", got, filePerm)
	}
	di, err := os.Stat(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != dirPerm {
		t.Errorf("dir perm = %o, want %o", got, dirPerm)
	}
}

func TestPermissionsRepairedOnLoad(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save(&Config{Machine: "m"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.Chmod(s.Path(), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != filePerm {
		t.Errorf("file perm after load = %o, want %o", got, filePerm)
	}
}

func TestLoadMigratesLegacyDefaultAPIURL(t *testing.T) {
	s := newTestStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), dirPerm); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	original := `{"api_url":"https://app.agentburndown.com","collector_key":"abd_secret",` +
		`"machine":"laptop","future_key":{"nested":42}}`
	if err := os.WriteFile(s.Path(), []byte(original), filePerm); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIURL != DefaultAPIURL {
		t.Errorf("api_url = %q, want %q", cfg.APIURL, DefaultAPIURL)
	}

	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatalf("read migrated config: %v", err)
	}
	var persisted map[string]json.RawMessage
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("decode migrated config: %v", err)
	}
	if string(persisted["api_url"]) != `"`+DefaultAPIURL+`"` {
		t.Errorf("persisted api_url = %s, want %q", persisted["api_url"], DefaultAPIURL)
	}
	var future map[string]int
	if err := json.Unmarshal(persisted["future_key"], &future); err != nil {
		t.Fatalf("decode preserved unknown field: %v", err)
	}
	if future["nested"] != 42 {
		t.Errorf("unknown field not preserved: %s", persisted["future_key"])
	}
}

func TestLoadPreservesCustomAPIURL(t *testing.T) {
	s := newTestStore(t)
	const customURL = "https://collector.staging.example.com"
	if err := s.Save(&Config{APIURL: customURL}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIURL != customURL {
		t.Errorf("api_url = %q, want custom URL %q", cfg.APIURL, customURL)
	}
}

func TestCorruptConfigError(t *testing.T) {
	s := newTestStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), dirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(s.Path(), []byte("{not valid json"), filePerm); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := s.Load()
	if err == nil {
		t.Fatal("expected error loading corrupt config, got nil")
	}
	if !strings.Contains(err.Error(), s.Path()) {
		t.Errorf("error %q does not mention path %q", err, s.Path())
	}
}

func TestUnknownFieldsPreserved(t *testing.T) {
	s := newTestStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), dirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	original := `{"api_url":"https://a","machine":"m","future_key":{"nested":42},"extra_flag":true}`
	if err := os.WriteFile(s.Path(), []byte(original), filePerm); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal saved: %v", err)
	}
	if _, ok := m["future_key"]; !ok {
		t.Errorf("future_key not preserved; saved keys: %v", keys(m))
	}
	if string(m["extra_flag"]) != "true" {
		t.Errorf("extra_flag = %s, want true", m["extra_flag"])
	}
	if string(m["api_url"]) != `"https://a"` {
		t.Errorf("api_url = %s, want \"https://a\"", m["api_url"])
	}
}

func TestDirEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvConfigDir, dir)
	got, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if got != dir {
		t.Errorf("Dir() = %q, want %q", got, dir)
	}
	s, err := NewFileStore()
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if want := filepath.Join(dir, fileName); s.Path() != want {
		t.Errorf("Path() = %q, want %q", s.Path(), want)
	}
}

func TestLoadMissingFileIsNotExist(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Load()
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error %v does not satisfy os.ErrNotExist", err)
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestRetentionDefaultAndOverride(t *testing.T) {
	if got := (&Config{}).Retention(); got != DefaultRetentionDays {
		t.Errorf("default retention = %d, want %d", got, DefaultRetentionDays)
	}
	if got := (&Config{RetentionDays: -3}).Retention(); got != DefaultRetentionDays {
		t.Errorf("negative retention = %d, want default %d", got, DefaultRetentionDays)
	}
	if got := (&Config{RetentionDays: 14}).Retention(); got != 14 {
		t.Errorf("retention = %d, want 14", got)
	}
	if got := (&Config{RetentionDays: 99999}).Retention(); got != maxRetentionDays {
		t.Errorf("oversized retention = %d, want clamp %d", got, maxRetentionDays)
	}
	if got := (&Config{RetentionDays: maxRetentionDays}).Retention(); got != maxRetentionDays {
		t.Errorf("boundary retention = %d, want %d", got, maxRetentionDays)
	}
}

func TestRetentionDaysRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save(&Config{Machine: "m", RetentionDays: 30}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RetentionDays != 30 {
		t.Errorf("retention_days round-trip = %d, want 30", got.RetentionDays)
	}
}
