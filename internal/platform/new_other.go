//go:build !darwin

package platform

// New reports that service management is unsupported on this platform. A
// Windows implementation can replace this stub without changing callers.
func New() (Service, error) {
	return nil, ErrUnsupported
}
