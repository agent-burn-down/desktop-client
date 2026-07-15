# Privacy

The collector uploads metadata only. Free text never leaves your machine.

## The allowlist

Every uploaded event is built from a fixed allowlist. The normalizer copies these
14 fields out of each incoming OTLP record and drops everything else:

| Field | Description |
|-------|-------------|
| `event_name` | Event type (for example `api_request`, `tool_result`) |
| `timestamp` | When the event occurred |
| `session_id` | Agent session or conversation id |
| `model` | Model slug |
| `tool_name` | Tool invoked |
| `tool_success` | Whether the tool call succeeded |
| `tool_duration_ms` | Tool call duration |
| `cost_usd` | Reported cost |
| `input_tokens` | Prompt token count |
| `output_tokens` | Completion token count |
| `cache_read_tokens` | Cache-read token count |
| `cache_create_tokens` | Cache-write token count |
| `repo` | Repository name |
| `error_message` | Error string, truncated to 2 KB |

## What never leaves the machine

No prompt text, no completion text, no tool call arguments or results, and no
file contents are ever read into an uploaded event. Those values are not on the
allowlist, so the normalizer never copies them.

The collector also folds those same allowlisted normalized events into a
session summary. It uploads session id, timestamps, safe repository label,
source, outcome, token totals, cost/tool/error counters, and a structured list
of actual model token contributions. It does not scan prompts or completions,
and it drops absolute repository paths and composite `mixed:` model labels.

`error_message` is the only free-text field that passes through. It is capped at
2 KB on a UTF-8 boundary so a misbehaving agent cannot stream large diagnostics
(or accidental prompt fragments) through it.

On the Codex side, `setup` also writes `log_user_prompt = false`, so Codex does
not emit prompt text to the collector in the first place.

## Enforced by construction

This is a structural guarantee, not a policy. The normalizer names each field it
copies; anything not named is impossible to include. That code path is covered by
regression tests and fuzz tests that assert no free text escapes the allowlist,
so a future change that tried to widen it would fail the suite.

## Local retention

Uploaded events are kept locally in a SQLite queue at `~/.burndown/queue.db` for
a short window (default 7 days) so `burndown-cli stats` can show your recent
usage. Rows older than the retention window are pruned automatically. The
retention window is configurable via `retention_days` in
`~/.burndown/config.json`.

## Local files

| Path | Contents | Permissions |
|------|----------|-------------|
| `~/.burndown/config.json` | Collector key, api url, machine, policy | `0600` |
| `~/.burndown/queue.db` | Queued and recently uploaded events | default |
| `~/.burndown/logs/` | Service stdout and stderr | default |

The config directory itself is created with mode `0700`.
