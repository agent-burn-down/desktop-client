package platform

import (
	"os/exec"
	"runtime"
)

// commander runs an OS command; a package-level var so tests can stub it
// without touching a real process.
var commander = func(name string, args ...string) error {
	//nolint:gosec // G204: name is one of a fixed GOOS-selected set below, args is the URL to open
	return exec.Command(name, args...).Start()
}

// OpenURL best-effort opens url in the user's default browser. It is never
// fatal: callers should log a failure and keep going, since the caller
// already has a URL to print as the source of truth (e.g. a device-login
// verification link) regardless of whether a browser could be launched.
func OpenURL(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return commander("open", url)
	case "windows":
		// rundll32's url.dll opener avoids cmd.exe quoting pitfalls with "start".
		return commander("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return commander("xdg-open", url)
	}
}
