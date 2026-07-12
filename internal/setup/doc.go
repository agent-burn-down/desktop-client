// Package setup detects installed coding agents (Claude Code, Codex) and writes
// their OTEL configuration idempotently, so their telemetry is exported to the
// local collector receiver. It ports the semantics of the reference
// scripts/setup_otel.py: only missing keys are added (user values are never
// overwritten), every changed file is backed up with a timestamped suffix
// before mutation, and a second run over an already-configured file is a no-op.
package setup
