//go:build !darwin

package credstore

// newKeychainBackend has no OS keychain implementation on this platform yet
// — only darwin ships today (see internal/platform's build-tag split for the
// same convention) — so it falls back to the file backend, identical to what
// an explicit BURNDOWN_CONFIG_DIR override forces.
func newKeychainBackend() Backend { return fileBackend{} }
