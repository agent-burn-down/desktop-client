# Quickstart

A fresh machine goes from nothing to a running collector in five steps. The
[README](https://github.com/agent-burn-down/desktop-client#quickstart) is the
canonical version; this page adds a little more detail.

macOS only. Install the prebuilt binary first — no repo checkout needed. Pick
`arm64` for Apple silicon or `amd64` for Intel:

```
ARCH=arm64   # Intel Macs: amd64
REPO=agent-burn-down/desktop-client
TAG=$(curl -fsSL https://api.github.com/repos/$REPO/releases/latest | grep -m1 '"tag_name"' | cut -d'"' -f4)
curl -fsSL "https://github.com/$REPO/releases/download/$TAG/burndown-cli_${TAG#v}_darwin_${ARCH}.tar.gz" | tar -xzf - burndown-cli
sudo mv burndown-cli /usr/local/bin/
```

The binary is not notarized yet (issue #14); fetching it with `curl` avoids
Gatekeeper quarantine. To build from source instead (requires Go 1.26 or newer):

```
git clone https://github.com/agent-burn-down/desktop-client.git
cd desktop-client
make build
cp bin/burndown-cli /usr/local/bin/
```

## 1. Get a collector key

Sign in to [app.agentburndown.com](https://app.agentburndown.com) and create a
collector key. It looks like `abd_...`.

## 2. Log in

`login` validates the key by registering this machine with the backend, then
stores the credentials in `~/.burndown/config.json` (file mode `0600`, directory
`0700`). With no `--key` flag the key is read from a hidden prompt; the reporting
email is prompted too.

```
burndown-cli login
```

```
Reporting user email: you@example.com
Collector key (abd_...):
Logged in. key abd_a1b2c3d4… collector_id 1 machine your-hostname
```

Pass values as flags to skip the prompts (useful for CI, where the key can also
be piped on stdin):

```
burndown-cli login --email you@example.com --key abd_... --machine your-hostname
```

Override the backend with `--api-url` if you are pointing at a non-default
environment. If your key ever changes, run `login` again; use `register` to
refresh the collector id and policy without re-entering the key.

Clients upgrading from a release that used `https://app.agentburndown.com` as
the default automatically migrate that exact stored URL to the dedicated
`https://collector.agentburndown.com` endpoint. Custom URLs are preserved.

## 3. Configure your agents

`setup` detects Claude Code and Codex and adds the OTEL settings that point them
at the local collector. It only adds missing keys, preserves every existing
value, and backs up each file it touches with a timestamped copy first. A second
run is a no-op.

```
burndown-cli setup
```

What it writes to `~/.claude/settings.json` (under `env`):

| Key | Value |
|-----|-------|
| `CLAUDE_CODE_ENABLE_TELEMETRY` | `1` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://localhost:8765` |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/json` |
| `OTEL_METRICS_EXPORTER` | `otlp` |
| `OTEL_LOGS_EXPORTER` | `otlp` |
| `OTEL_LOG_TOOL_DETAILS` | `1` |

What it writes to `~/.codex/config.toml` (under `[otel]`):

| Key | Value |
|-----|-------|
| `environment` | `"control-center"` |
| `metrics_exporter` | `"none"` |
| `trace_exporter` | `"none"` |
| `log_user_prompt` | `false` |
| `exporter` | `{ otlp-http = { endpoint = "http://127.0.0.1:8765/v1/logs", protocol = "json" } }` |

Note `log_user_prompt = false`: Codex never sends prompt text to the collector.

Useful flags:

- `--check` — dry run; prints pending changes and exits non-zero if any are pending.
- `--claude` / `--codex` — configure a specific agent even if it was not detected.
- `--all` — configure both agents regardless of detection.
- `--port <port>` — point agents at a non-default receiver port.
- `--yes` — skip the confirmation prompt.

Restart Claude Code and Codex afterward so they pick up the new settings.

## 4. Install the background service

`service install` writes a launchd agent
(`~/Library/LaunchAgents/com.agentburndown.collector.plist`) that starts the
collector at login and restarts it if it crashes.

```
burndown-cli service install
burndown-cli service status
```

To run the collector in the foreground instead (for example while debugging),
use `burndown-cli serve --verbose`.

## 5. Verify

`doctor` checks version, config, backend reachability, heartbeat, the daemon,
agent OTEL setup, queue integrity, and the service. Exit code is 0 (pass), 1
(warn), or 2 (fail).

```
burndown-cli doctor
```

```
[pass] version    running dev; no published releases yet
[pass] config     present, key set, permissions 0600/0700
[pass] backend    reachable at https://collector.agentburndown.com
[pass] heartbeat  ok (collector_id 1)
[pass] daemon     listening on 127.0.0.1:8765
[pass] agents     OTEL configured: Claude Code, Codex
[pass] queue      depth 0 (reported by daemon)
[pass] service    running (running (pid 51234))

overall: pass
```

Then use your agents and confirm data flows:

```
burndown-cli status   # received/queued/uploaded counters
burndown-cli stats     # local daily tokens, cost, and top tools
```

`burndown-cli send-test` posts a synthetic event through the receiver-to-queue
path if you want to confirm the pipeline without running an agent.
