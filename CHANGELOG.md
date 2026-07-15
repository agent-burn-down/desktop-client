# Changelog

All notable changes to `burndown-cli` are documented here.

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

[0.4.0]: https://github.com/agent-burn-down/desktop-client/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/agent-burn-down/desktop-client/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/agent-burn-down/desktop-client/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/agent-burn-down/desktop-client/releases/tag/v0.1.0
