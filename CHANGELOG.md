# Changelog

All notable changes to `burndown-cli` are documented here.

## [Unreleased]

## [0.8.0] - 2026-08-05

### Security

- The receiver now rejects cross-origin, off-host and non-JSON writes. It is a
  loopback-only listener, but a page in the user's browser could previously
  POST to it; the Origin guard and stricter `Content-Type` parsing close that
  (#66).

### Added

- The collector key is stored in the macOS keychain instead of on disk. The key
  is read back through the `security` tool, so the released `CGO_ENABLED=0`
  binaries keep working (#67).

> Note: `0.6.0` through `0.7.1` were released without changelog entries. The two
> items previously sitting under "Unreleased" had in fact already shipped, and
> are filed below under the releases that contained them. Those sections are
> partial — they list only what was recoverable, not everything in the release.

## [0.7.0] - 2026-07-21

### Fixed

- `doctor` and `status` now probe the receiver port `serve` last actually ran
  on (persisted to config.json), instead of always defaulting to 8765. This
  also fixes `doctor` incorrectly flagging Codex OTEL settings as missing when
  they were written for a non-default port (#59).

## [0.6.0] - 2026-07-17

### Changed

- Document installing from the prebuilt release binaries so a new machine can
  install the collector without cloning the repository. Build-from-source is
  retained as an alternative.

## [0.5.0] - 2026-07-15

### Added

- Build privacy-safe session summaries from allowlisted normalized telemetry,
  including structured per-model token contributions.
- Persist revisioned summaries in the local SQLite queue and upload them to the
  hosted Session Explorer with bounded batches, retry, and stale-ack safety.
- Attribute Claude Code telemetry to project directories using local session
  metadata, including non-Git directories and transient worktree layouts.

### Changed

- Retain session summaries across restarts and prune acknowledged snapshots on
  the configured local retention schedule.
- Use directory names as privacy-safe project keys when no Git repository can
  be resolved.

## [0.4.0] - 2026-07-15

### Changed

- Point new installations at the dedicated
  `https://collector.agentburndown.com` API endpoint.
- Automatically migrate existing configurations that still use the former
  `https://app.agentburndown.com` default. Explicit custom API URLs remain
  unchanged.
- Update collector architecture and diagnostics documentation for the dedicated
  endpoint while keeping dashboard and key-management links on
  `app.agentburndown.com`.

## [0.3.0] - 2026-07-14

### Fixed

- Attribute Codex telemetry to repositories using local session metadata,
  including worktree normalization and per-conversation caching.

## [0.2.0] - 2026-07-14

### Added

- Forward allowlisted OTLP metrics through the collector pipeline.
- Add event idempotency keys and gzip compression for larger upload batches.

## [0.1.0] - 2026-07-13

### Added

- Initial macOS collector release with device-code login, collector key
  rotation, service management, local retention, diagnostics, and metadata-only
  telemetry ingestion.

[Unreleased]: https://github.com/agent-burn-down/desktop-client/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/agent-burn-down/desktop-client/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/agent-burn-down/desktop-client/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/agent-burn-down/desktop-client/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/agent-burn-down/desktop-client/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/agent-burn-down/desktop-client/releases/tag/v0.1.0
