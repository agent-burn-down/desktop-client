package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/agent-burn-down/desktop-client/internal/api"
)

const (
	dirName  = "burndown"
	fileName = "config.json"
	dirPerm  = 0o700
	filePerm = 0o600
	// EnvConfigDir overrides the config directory (used by tests and doctor).
	EnvConfigDir = "BURNDOWN_CONFIG_DIR"
	// DefaultAPIURL is the canonical Agent Burndown collector endpoint.
	DefaultAPIURL = "https://collector.agentburndown.com"
	// legacyDefaultAPIURL was the client default before the dedicated collector
	// hostname was introduced. Exact matches are migrated on load; custom URLs
	// remain untouched.
	legacyDefaultAPIURL = "https://app.agentburndown.com"
)

// knownFields lists the JSON keys represented by typed Config fields. Any other
// key found on load is preserved in Config.extra across a save round-trip.
var knownFields = map[string]struct{}{
	"api_url":                {},
	"collector_key":          {},
	"collector_id":           {},
	"user_email":             {},
	"machine":                {},
	"policy":                 {},
	"key_expires_at":         {},
	"retention_days":         {},
	"key_id":                 {},
	"pending_key":            {},
	"pending_key_id":         {},
	"pending_key_expires_at": {},
	"old_key_valid_until":    {},
	"last_rotation_at":       {},
	"rotation_failures":      {},
	"auth_reason":            {},
	"receiver_port":          {},
}

const (
	// DefaultRetentionDays is the retention window applied when retention_days is
	// unset or non-positive: acked queue rows older than this are pruned locally.
	DefaultRetentionDays = 7
	// maxRetentionDays clamps an out-of-range retention_days (~10 years) so a
	// typo cannot disable pruning or drive an absurd stats window.
	maxRetentionDays = 3650
)

// Config is the persisted collector configuration.
//
// Unknown JSON fields are preserved verbatim across load/save so a newer client
// writing extra keys does not lose them when an older client rewrites the file.
type Config struct {
	APIURL       string     `json:"api_url"`
	CollectorKey string     `json:"collector_key"`
	CollectorID  int64      `json:"collector_id"`
	UserEmail    string     `json:"user_email"`
	Machine      string     `json:"machine"`
	Policy       api.Policy `json:"policy"`
	// KeyExpiresAt is the current collector key's expiry (empty = never/legacy
	// key, no expiry). Drives the T-7-day rotation trigger.
	KeyExpiresAt string `json:"key_expires_at"`
	// RetentionDays bounds how long acked queue rows are kept for local stats.
	// Zero or negative means use DefaultRetentionDays; read via Retention.
	RetentionDays int `json:"retention_days"`

	// KeyID is the id of the currently active collector key.
	KeyID int64 `json:"key_id,omitempty"`
	// PendingKey, if set, is a rotated-to key awaiting a successful heartbeat
	// verification before it replaces CollectorKey. Its presence on load means
	// a rotation was in progress when the process last stopped, so the
	// uploader resumes verification rather than starting a new rotation.
	PendingKey string `json:"pending_key,omitempty"`
	// PendingKeyID is the id of PendingKey.
	PendingKeyID int64 `json:"pending_key_id,omitempty"`
	// PendingKeyExpires is PendingKey's expiry, applied to KeyExpiresAt once
	// PendingKey is committed.
	PendingKeyExpires string `json:"pending_key_expires_at,omitempty"`
	// OldKeyValidUntil is the overlap deadline the backend gave for the key
	// being rotated away from (informational; the backend enforces it).
	OldKeyValidUntil string `json:"old_key_valid_until,omitempty"`
	// LastRotationAt is when a rotation last completed or was last attempted,
	// gating the server's one-rotation-per-day limit.
	LastRotationAt string `json:"last_rotation_at,omitempty"`
	// RotationFailures counts consecutive failed rotation attempts, surfaced
	// as an escalating warning in status/doctor.
	RotationFailures int `json:"rotation_failures,omitempty"`
	// AuthReason is the last standardized 401 code that put uploads in
	// Degraded mode (key_revoked/key_invalid); empty while Active. Persisted
	// so status/doctor are accurate even when the daemon is not running.
	AuthReason string `json:"auth_reason,omitempty"`
	// ReceiverPort is the loopback OTLP receiver port `serve` last started on.
	// It lets `doctor`/`status` probe the right port when the daemon was
	// started with a non-default --port (for example via a hand-edited
	// launchd plist), since that flag is otherwise known only to the running
	// process's argv.
	ReceiverPort int `json:"receiver_port,omitempty"`

	// extra holds JSON fields not represented by the typed fields above, so
	// they survive a load/save round-trip.
	extra map[string]json.RawMessage
}

// UnmarshalJSON decodes known fields and stashes unknown keys in extra.
func (c *Config) UnmarshalJSON(data []byte) error {
	type alias Config
	var known alias
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*c = Config(known)
	c.extra = make(map[string]json.RawMessage)
	for k, v := range raw {
		if _, ok := knownFields[k]; !ok {
			c.extra[k] = v
		}
	}
	return nil
}

// MarshalJSON emits known fields plus any preserved unknown keys.
func (c *Config) MarshalJSON() ([]byte, error) {
	type alias Config
	known, err := json.Marshal(alias(*c))
	if err != nil {
		return nil, err
	}
	if len(c.extra) == 0 {
		return known, nil
	}
	merged := make(map[string]json.RawMessage, len(c.extra)+len(knownFields))
	if err := json.Unmarshal(known, &merged); err != nil {
		return nil, err
	}
	for k, v := range c.extra {
		if _, ok := knownFields[k]; ok {
			continue
		}
		merged[k] = v
	}
	return json.Marshal(merged)
}

// Retention returns the local retention window in days, clamped to a sane range:
// a zero or negative value falls back to DefaultRetentionDays, and anything above
// maxRetentionDays is capped there.
func (c *Config) Retention() int {
	switch {
	case c.RetentionDays <= 0:
		return DefaultRetentionDays
	case c.RetentionDays > maxRetentionDays:
		return maxRetentionDays
	default:
		return c.RetentionDays
	}
}

// Store abstracts loading and saving collector configuration so a future
// Keychain or Credential Manager backend can replace the file backend without
// changing callers.
type Store interface {
	Load() (*Config, error)
	Save(*Config) error
}

// FileStore persists configuration as JSON on the local filesystem with
// restrictive permissions (dir 0700, file 0600).
type FileStore struct {
	path string
}

// Dir returns the directory holding the config file. It honours the
// BURNDOWN_CONFIG_DIR override, then falls back to a platform default:
// %APPDATA%\burndown on Windows, ~/.burndown elsewhere.
func Dir() (string, error) {
	if override := os.Getenv(EnvConfigDir); override != "" {
		return override, nil
	}
	if runtime.GOOS == "windows" {
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", errors.New("APPDATA is not set; cannot locate config directory")
		}
		return filepath.Join(appData, dirName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, "."+dirName), nil
}

// NewFileStore returns a FileStore rooted at the resolved config directory.
func NewFileStore() (*FileStore, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	return &FileStore{path: filepath.Join(dir, fileName)}, nil
}

// Path returns the absolute path of the config file.
func (s *FileStore) Path() string { return s.path }

// Load reads and decodes the config file, repairing its permissions to 0600.
// Configs using the former default API URL are atomically migrated to the
// dedicated collector endpoint. A missing file yields an error that satisfies
// errors.Is(err, os.ErrNotExist).
func (s *FileStore) Load() (*Config, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("config not found at %s: %w", s.path, err)
		}
		return nil, fmt.Errorf("read config %s: %w", s.path, err)
	}
	if err := os.Chmod(s.path, filePerm); err != nil {
		return nil, fmt.Errorf("repair permissions on %s: %w", s.path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf(
			"config at %s is corrupt; delete it and re-run login to recreate it: %w",
			s.path, err)
	}
	if cfg.APIURL == legacyDefaultAPIURL {
		cfg.APIURL = DefaultAPIURL
		if err := s.Save(&cfg); err != nil {
			return nil, fmt.Errorf("migrate api_url in %s: %w", s.path, err)
		}
	}
	return &cfg, nil
}

// Save writes the config atomically (temp file + rename) with dir 0700 and
// file 0600 permissions.
func (s *FileStore) Save(cfg *Config) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create config dir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("set permissions on %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	return s.writeAtomic(dir, data)
}

func (s *FileStore) writeAtomic(dir string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set permissions on temp config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("rename temp config to %s: %w", s.path, err)
	}
	return nil
}
