//go:build darwin

package platform

import (
	"path/filepath"

	"github.com/agent-burn-down/desktop-client/internal/config"
)

// serviceLogDir returns the directory launchd writes the service's stdout and
// stderr into: <config dir>/logs.
func serviceLogDir() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "logs"), nil
}
