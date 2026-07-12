# Architecture

The collector is a single daemon (`burndown-cli serve`) with a linear pipeline.
Claude Code and Codex export OTLP telemetry to a loopback endpoint; the daemon
normalizes it to metadata, queues it durably, and uploads it to the backend.

```mermaid
flowchart LR
    A["Claude Code / Codex"] -->|"OTLP over HTTP<br/>127.0.0.1:8765/v1/logs"| R[receiver]
    R --> N[normalize]
    N --> F[filter]
    F --> Q[("queue<br/>~/.burndown/queue.db")]
    Q --> U[uploader]
    U -->|"POST /ingest/v1/events"| B["app.agentburndown.com"]
```

## Stages

**receiver** — binds `127.0.0.1:8765` and accepts OTLP/HTTP log batches at
`/v1/logs`. It refuses to bind any non-loopback address, so telemetry never
leaves the machine unencrypted or reaches the network.

**normalize** — flattens each OTLP log record into a `NormalizedEvent` built from
a fixed metadata allowlist (see [Privacy](privacy.md)). Records without a usable
event name are dropped; free text is never copied.

**filter** — drops events that are not worth uploading and enforces the metadata
contract before anything reaches the queue.

**queue** — a local SQLite database (`~/.burndown/queue.db`) that persists events
across restarts and network outages. Uploaded rows are retained for the
configured window (default 7 days) so `stats` can summarize local usage, then
pruned.

**uploader** — drains the queue to `POST /ingest/v1/events` on the cadence set by
the backend policy (`flush_interval_seconds`, `max_batch_size`), which the daemon
refreshes on each heartbeat.

## Backend endpoints

| Endpoint | Purpose |
|----------|---------|
| `POST /ingest/v1/register` | Register the machine, resolve collector id, fetch policy |
| `POST /ingest/v1/heartbeat` | Liveness ping; refreshes policy |
| `POST /ingest/v1/events` | Upload a batch of normalized events |
| `GET /api/health` | Unauthenticated reachability check |

## Background service

On macOS the daemon runs under launchd. `service install` writes
`~/Library/LaunchAgents/com.agentburndown.collector.plist` with `RunAtLoad`
(start at login) and `KeepAlive` (restart on crash). The service runs
`burndown-cli serve` and writes stdout and stderr to
`~/.burndown/logs/collector.out.log` and `~/.burndown/logs/collector.err.log`.
