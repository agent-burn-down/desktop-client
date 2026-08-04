package credstore

import (
	"errors"
	"fmt"
	"os"

	"github.com/agent-burn-down/desktop-client/internal/config"
)

// ErrBackendUnavailable wraps a hard failure reading the active collector
// key from a backend that config says holds it (a keychain read error, or
// the entry itself missing after migration). errors.Is matches it, so
// callers like doctor's credstore check can distinguish this from "no
// config yet" (os.ErrNotExist) and report it as the genuine credential
// failure it is rather than a fresh install.
var ErrBackendUnavailable = errors.New("credential backend unavailable")

// Store wraps a config.Store, transparently moving the collector key and
// pending key into an OS keychain backend when one is available and
// verified, leaving them in the plaintext file otherwise. It implements
// config.Store, so it is a drop-in replacement for config.FileStore.
//
// A single Store is not safe for concurrent Load/Save calls, matching
// config.FileStore's existing contract; callers that share one across
// goroutines (internal/uploader) already serialize their own access.
type Store struct {
	inner   config.Store
	backend Backend
	forced  bool
	status  BackendStatus
}

// Open resolves the config store and wraps it with credential-backend
// migration: the OS keychain where one is available, the plaintext file
// otherwise.
//
// BURNDOWN_CONFIG_DIR forces the file backend even on platforms with a
// keychain. The override exists for tests (and any other isolated
// config-dir use), and those must stay hermetic: they must never touch the
// developer's real login keychain, and a config dir that is thrown away
// between runs must not leave a credential orphaned in the keychain with no
// file left pointing at it.
func Open() (*Store, error) {
	inner, err := config.NewFileStore()
	if err != nil {
		return nil, err
	}
	forced := os.Getenv(config.EnvConfigDir) != ""
	backend := newKeychainBackend()
	if forced {
		backend = fileBackend{}
	}
	return &Store{inner: inner, backend: backend, forced: forced}, nil
}

// Path returns the underlying config file's path, for callers (doctor) that
// inspect its permissions directly. It returns "" if inner is not a
// *config.FileStore — not reachable via Open() today, but inner is an
// unexported field a future constructor could vary.
func (s *Store) Path() string {
	fs, ok := s.inner.(*config.FileStore)
	if !ok {
		return ""
	}
	return fs.Path()
}

// LastBackend reports which backend served the most recently completed Load
// or Save call.
func (s *Store) LastBackend() BackendStatus { return s.status }

// Load reads the config. If the collector key was previously migrated to
// the backend, it is hydrated from there; otherwise Load opportunistically
// migrates a plaintext key now. A backend read failure for a key that is
// supposed to live there is a hard error: the file no longer holds a copy,
// so silently proceeding would look identical to "no key configured" and
// could trigger an unwanted re-login or rotation.
func (s *Store) Load() (*config.Config, error) {
	cfg, err := s.inner.Load()
	if err != nil {
		return nil, err
	}
	if cfg.CredentialBackend == BackendKeychain {
		if err := s.hydrate(cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}
	s.migrateOnLoad(cfg)
	return cfg, nil
}

// Save persists cfg, moving a non-empty collector key (and pending key, if
// set) into the backend when one is active, falling back to plaintext on
// any keychain failure. Unlike the opportunistic migration in Load, Save
// always durably persists the result and returns any failure to the caller.
func (s *Store) Save(cfg *config.Config) error {
	if s.backend.Name() == BackendFile {
		cfg.CredentialBackend = ""
		s.status = s.forcedFileStatus()
		return s.inner.Save(cfg)
	}
	if cfg.CollectorKey == "" {
		cfg.CredentialBackend = ""
		s.status = s.noKeyStatus()
		return s.inner.Save(cfg)
	}
	if err := s.writeSecrets(cfg); err != nil {
		cfg.CredentialBackend = ""
		s.status = s.fallbackStatus(err)
		return s.inner.Save(cfg)
	}
	return s.saveScrubbed(cfg)
}

// migrateOnLoad opportunistically moves a plaintext key into the backend the
// first time Load sees one. Unlike Save, a keychain failure here is not
// re-persisted: the file already holds a valid plaintext copy, so there is
// nothing new to write, and the next Load simply retries.
func (s *Store) migrateOnLoad(cfg *config.Config) {
	if s.backend.Name() == BackendFile {
		s.status = s.forcedFileStatus()
		return
	}
	if cfg.CollectorKey == "" {
		s.status = s.noKeyStatus()
		return
	}
	if err := s.writeSecrets(cfg); err != nil {
		s.status = s.fallbackStatus(err)
		return
	}
	if err := s.saveScrubbed(cfg); err != nil {
		// The keychain already holds a verified copy (writeSecrets
		// succeeded above); only the disk scrub failed. Leave cfg (with its
		// plaintext values intact) and the on-disk marker untouched so the
		// next Load retries the scrub — never remove the plaintext copy
		// before the keychain copy is durable.
		s.status = BackendStatus{Name: BackendKeychain,
			Detail: "migrated, but could not remove the plaintext copy: " + err.Error()}
	}
}

// writeSecrets writes cfg's active key, and its pending key if any, to the
// backend, verifying each write by reading it back before the caller trusts
// it enough to scrub the file copy.
//
// A pending-key failure does NOT roll back the active key. writeVerified
// runs on every Save once keychain-backed, not just the first migration, so
// the active entry it just (re-)wrote is very often a value that already
// existed there and is durably relied on — deleting it over an unrelated
// pending-key failure can destroy a good key the caller never touched, with
// a crash window that can strand it permanently (config still says
// keychain-backed, keychain now empty, file has nothing). The caller's
// plaintext fallback on error already handles the pending-key failure
// safely without needing the keychain touched at all; at worst this leaves
// a harmless orphaned active-key entry that the next successful migration
// overwrites.
func (s *Store) writeSecrets(cfg *config.Config) error {
	if err := s.writeVerified(AccountActiveKey, cfg.CollectorKey); err != nil {
		return err
	}
	if cfg.PendingKey == "" {
		// Best-effort: drop a stale pending entry left by an earlier
		// rotation that has since committed or been discarded.
		_ = s.backend.Delete(AccountPendingKey)
		return nil
	}
	return s.writeVerified(AccountPendingKey, cfg.PendingKey)
}

// writeVerified writes secret to the backend and reads it back to confirm
// the write landed durably before it is safe to remove the plaintext copy.
func (s *Store) writeVerified(account Account, secret string) error {
	if err := s.backend.Set(account, secret); err != nil {
		return fmt.Errorf("write %s to %s: %w", account, s.backend.Name(), err)
	}
	got, found, err := s.backend.Get(account)
	if err != nil {
		return fmt.Errorf("verify %s in %s: %w", account, s.backend.Name(), err)
	}
	if !found || got != secret {
		return fmt.Errorf("verify %s in %s: value mismatch after write", account, s.backend.Name())
	}
	return nil
}

// saveScrubbed persists cfg to disk with its secrets removed, marking it as
// backend-held. Callers must only invoke this after writeSecrets has
// verified the backend durably holds those values.
func (s *Store) saveScrubbed(cfg *config.Config) error {
	scrubbed := *cfg
	scrubbed.CollectorKey, scrubbed.PendingKey = "", ""
	scrubbed.CredentialBackend = BackendKeychain
	if err := s.inner.Save(&scrubbed); err != nil {
		return fmt.Errorf("persist scrubbed config: %w", err)
	}
	cfg.CredentialBackend = BackendKeychain
	s.status = BackendStatus{Name: BackendKeychain, Detail: "active"}
	return nil
}

// hydrate fills cfg's active key from the backend for a config already
// migrated there, and its pending key if PendingKeyID says one is in
// flight. Only the active key is fatal to lose: it is the collector's sole
// working credential. The pending key is an ephemeral, in-progress rotation
// value the codebase already treats as disposable (rotation.go discards and
// re-rotates on any rejection); losing it here must not take the collector
// down, so hydratePending degrades instead of erroring.
func (s *Store) hydrate(cfg *config.Config) error {
	key, found, err := s.getVerified(AccountActiveKey)
	if err != nil {
		return err
	}
	if !found {
		return s.lostKeyErr(AccountActiveKey)
	}
	cfg.CollectorKey = key
	if cfg.PendingKeyID != 0 {
		s.hydratePending(cfg)
	}
	s.status = BackendStatus{Name: BackendKeychain, Detail: "active"}
	return nil
}

// hydratePending fills cfg.PendingKey from the backend. Any problem doing
// so — not found, or a read error — discards the pending rotation state
// (PendingKey, PendingKeyID, PendingKeyExpires, OldKeyValidUntil) instead of
// failing Load, and persists that discard so it is not repeated on every
// subsequent Load for the life of the daemon. The next rotationDue check
// simply starts a fresh rotation; the active key is unaffected.
func (s *Store) hydratePending(cfg *config.Config) {
	pending, found, err := s.backend.Get(AccountPendingKey)
	if err == nil && found {
		cfg.PendingKey = pending
		return
	}
	cfg.PendingKey, cfg.PendingKeyExpires, cfg.OldKeyValidUntil = "", "", ""
	cfg.PendingKeyID = 0
	if saveErr := s.saveScrubbed(cfg); saveErr != nil {
		// Best-effort: the in-memory cfg is already correct for this Load;
		// a failed persist just means the next Load repeats the discard.
		s.status = BackendStatus{Name: BackendKeychain,
			Detail: "pending key discarded, but could not persist: " + saveErr.Error()}
	}
}

func (s *Store) getVerified(account Account) (string, bool, error) {
	v, found, err := s.backend.Get(account)
	if err != nil {
		s.status = BackendStatus{Name: BackendKeychain, Detail: "read failed: " + err.Error()}
		return "", false, fmt.Errorf("read %s from %s: %w: %w",
			account, s.backend.Name(), ErrBackendUnavailable, err)
	}
	return v, found, nil
}

// lostKeyErr reports the active key: config says it is backend-held but the
// backend does not have it. This is a genuine loss, not a normal "not
// logged in yet" state (checked separately in Save/migrateOnLoad), so it is
// a hard error rather than a silent fallback.
func (s *Store) lostKeyErr(account Account) error {
	s.status = BackendStatus{Name: BackendKeychain, Detail: string(account) + " missing"}
	return fmt.Errorf("collector key missing from %s; run `burndown-cli login`: %w",
		s.backend.Name(), ErrBackendUnavailable)
}

func (s *Store) forcedFileStatus() BackendStatus {
	detail := "no OS keychain backend on this platform"
	if s.forced {
		detail = "BURNDOWN_CONFIG_DIR override forces the plaintext file backend"
	}
	return BackendStatus{Name: BackendFile, Detail: detail, Forced: true}
}

func (s *Store) noKeyStatus() BackendStatus {
	return BackendStatus{Name: BackendFile, Detail: "no collector key stored yet", Forced: true}
}

func (s *Store) fallbackStatus(cause error) BackendStatus {
	return BackendStatus{Name: BackendFile,
		Detail: "keychain unavailable, using plaintext fallback: " + cause.Error()}
}
