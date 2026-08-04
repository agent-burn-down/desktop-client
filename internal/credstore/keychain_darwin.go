//go:build darwin

package credstore

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// keychainService namespaces every entry this client writes to the macOS
// login keychain.
const keychainService = "com.agentburndown.burndown-cli"

// keychainBackend stores secrets in the macOS login keychain via the
// `security` command-line tool (github.com/zalando/go-keyring — the same
// library the GitHub CLI uses for this purpose). It never links CGO, so it
// has no effect on goreleaser's CGO_ENABLED=0 release build.
type keychainBackend struct{}

func newKeychainBackend() Backend { return keychainBackend{} }

func (keychainBackend) Name() string { return BackendKeychain }

func (keychainBackend) Get(account Account) (string, bool, error) {
	secret, err := keyring.Get(keychainService, string(account))
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("keychain get %s: %w", account, err)
	}
	return secret, true, nil
}

func (keychainBackend) Set(account Account, secret string) error {
	if err := keyring.Set(keychainService, string(account), secret); err != nil {
		return fmt.Errorf("keychain set %s: %w", account, err)
	}
	return nil
}

func (keychainBackend) Delete(account Account) error {
	if err := keyring.Delete(keychainService, string(account)); err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("keychain delete %s: %w", account, err)
	}
	return nil
}
