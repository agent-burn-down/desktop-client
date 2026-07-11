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
)

// knownFields lists the JSON keys represented by typed Config fields. Any other
// key found on load is preserved in Config.extra across a save round-trip.
var knownFields = map[string]struct{}{
	"api_url":        {},
	"collector_key":  {},
	"collector_id":   {},
	"user_email":     {},
	"machine":        {},
	"policy":         {},
	"key_expires_at": {},
}

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
	// KeyExpiresAt is reserved for M2 key rotation; unused today.
	KeyExpiresAt string `json:"key_expires_at"`

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
// A missing file yields an error that satisfies errors.Is(err, os.ErrNotExist).
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
