// Package doctor implements health checks with remediation hints. Each check
// reports a status (pass/warn/fail/skip) plus a one-line fix, and the command
// aggregates them into an exit code. Every check is designed to run safely with
// the collector daemon down.
package doctor
