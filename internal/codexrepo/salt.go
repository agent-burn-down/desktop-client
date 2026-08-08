package codexrepo

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-burn-down/desktop-client/internal/config"
)

const (
	// saltFile is the persisted tier-3 salt's filename, stored alongside the
	// collector's other durable state (config.Dir()) rather than inside
	// config.json's schema. codexrepo only needs "where does durable state
	// live" -- which config.Dir() already answers for every other package
	// that persists something locally -- not the Config/Store machinery, so
	// this keeps the package decoupled from that schema.
	//
	// This is a dedicated value, not a reuse of an existing field: Config's
	// Machine is a mutable, often-guessable hostname (unsuitable as salt --
	// see the security note below), and ReceiverToken is an unrelated
	// security secret for authenticating the loopback receiver; keying a
	// hash salt off of it would tie two independent rotation policies
	// together for no reason.
	saltFile     = "repo_key_salt"
	saltBytes    = 32
	saltFilePerm = 0o600
	saltDirPerm  = 0o700
)

// repoKeySalt returns the per-installation salt mixed into the tier-3
// fallback hash, generating and persisting one via crypto/rand on first use.
// A config directory that already has a salt file returns it unchanged, so
// an upgrade or reinstall -- which doesn't wipe the config directory --
// never regenerates it: regenerating would silently change every non-git
// project's repo_key (see RepoKey's fallback tier).
//
// Without a salt, the hash is reversible for guessable paths: an unsalted
// sha256 of a common path shape ("/Users/<name>/code/<repo>") is precomputable.
// If the salt can't be loaded or persisted (e.g. an unwritable config
// directory), resolution still must not fail or drop telemetry, so this
// falls back to a fresh random salt on each call -- keys stay unpredictable
// but aren't stable until the underlying disk issue is fixed.
func repoKeySalt() string {
	dir, err := config.Dir()
	if err != nil {
		return generateSalt()
	}
	path := filepath.Join(dir, saltFile)
	// #nosec G304 -- path is built from config.Dir(), the same directory
	// every other package in this repo persists local state to; it is never
	// derived from an OTLP attribute or other external input.
	if data, err := os.ReadFile(path); err == nil {
		if salt := strings.TrimSpace(string(data)); salt != "" {
			return salt
		}
	}
	salt := generateSalt()
	_ = persistSalt(dir, path, salt)
	return salt
}

func generateSalt() string {
	b := make([]byte, saltBytes)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is effectively unrecoverable, but RepoKey must
		// never error out or drop telemetry over it -- fall back to a fixed,
		// still non-empty salt rather than an empty one.
		return "repo-key-salt-unavailable"
	}
	return hex.EncodeToString(b)
}

func persistSalt(dir, path, salt string) error {
	if err := os.MkdirAll(dir, saltDirPerm); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(salt), saltFilePerm)
}
