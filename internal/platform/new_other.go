//go:build !darwin && !windows

package platform

// New reports that service management is unsupported on this platform. Darwin
// and Windows have real backends; everything else falls through to here.
func New() (Service, error) {
	return nil, ErrUnsupported
}
