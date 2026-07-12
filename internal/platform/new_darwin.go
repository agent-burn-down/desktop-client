//go:build darwin

package platform

// New returns the darwin launchd-backed service implementation.
func New() (Service, error) {
	return newDarwinService()
}
