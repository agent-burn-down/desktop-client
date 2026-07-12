package platform

import "testing"

// runningPrint is a trimmed real `launchctl print gui/501/<label>` snippet for a
// live job.
const runningPrint = `com.agentburndown.collector = {
	active count = 1
	path = /Users/test/Library/LaunchAgents/com.agentburndown.collector.plist
	state = running
	program = /usr/local/bin/burndown-cli
	pid = 55892
	immediate reason = speculative
	last exit code = (never exited)
}`

// crashedPrint is a trimmed snippet for a loaded job that is not running (it
// exited non-zero and KeepAlive has not restarted it yet).
const crashedPrint = `com.agentburndown.collector = {
	active count = 0
	path = /Users/test/Library/LaunchAgents/com.agentburndown.collector.plist
	state = not running
	program = /usr/local/bin/burndown-cli
	last exit code = 2
}`

func TestParsePrintOutput(t *testing.T) {
	tests := []struct {
		name      string
		out       string
		wantState State
		wantPID   int
	}{
		{"running with pid", runningPrint, StateRunning, 55892},
		{"loaded but crashed", crashedPrint, StateStopped, 0},
		{"empty", "", StateStopped, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePrintOutput(tc.out)
			if got.State != tc.wantState || got.PID != tc.wantPID {
				t.Errorf("parsePrintOutput = %+v, want state=%s pid=%d",
					got, tc.wantState, tc.wantPID)
			}
		})
	}
}
