//go:build windows

package platform

// New returns the Windows service backend, which manages the collector through
// the OS service control manager via github.com/kardianos/service.
func New() (Service, error) {
	return newWindowsService()
}
