package platform

import (
	"os/exec"
	"regexp"
	"strconv"
)

// commandRunner runs launchctl and returns its combined output. It is an
// interface so tests can record invocations and supply canned output without
// touching the real launchctl.
type commandRunner interface {
	run(args ...string) (output string, err error)
}

// execRunner invokes the real launchctl binary.
type execRunner struct{}

func (execRunner) run(args ...string) (string, error) {
	//nolint:gosec // fixed binary name, arguments are controlled internally
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	return string(out), err
}

// pidRe extracts the "pid = 1234" line from `launchctl print` output.
var pidRe = regexp.MustCompile(`(?m)^\s*pid\s*=\s*(\d+)`)

// parsePrintOutput derives a Status from `launchctl print gui/<uid>/<label>`
// output for a loaded job. A "pid = N" line means running; its absence means
// the job is loaded but not currently running (stopped or crashed).
func parsePrintOutput(out string) Status {
	if m := pidRe.FindStringSubmatch(out); m != nil {
		if pid, err := strconv.Atoi(m[1]); err == nil {
			return Status{State: StateRunning, PID: pid}
		}
	}
	return Status{State: StateStopped}
}
