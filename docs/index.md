# burndown-cli

Local telemetry collector for coding agents. It runs on your machine, receives
OTLP telemetry from Claude Code and Codex on `127.0.0.1:8765`, keeps only
metadata, and uploads that metadata to the Agent Burn Down backend at
`collector.agentburndown.com`.

Metadata-only by design: no prompt text, completion text, tool payloads, or file
contents ever leave your machine. See [Privacy](privacy.md).

## Where to start

- [Quickstart](quickstart.md) — install, log in, configure your agents, run the service, verify.
- [Privacy](privacy.md) — the exact metadata allowlist and what never leaves the machine.
- [Architecture](architecture.md) — how telemetry flows through the collector.
- [Troubleshooting](troubleshooting.md) — diagnosing a collector that is not reporting.

## Install

macOS only. A Homebrew tap is not available yet (tracked in issue #14, on hold
pending Apple Developer ID signing).

The installer selects the correct Apple silicon or Intel archive, verifies its
published checksum, and installs the CLI into the standard binary directory:

```
curl -fsSL https://github.com/agent-burn-down/desktop-client/releases/latest/download/install.sh | sh
```

The binary is not notarized yet (issue #14); fetching it with `curl` avoids
Gatekeeper quarantine. To build from source instead (requires Go 1.26 or newer):

```
git clone https://github.com/agent-burn-down/desktop-client.git
cd desktop-client
make build
cp bin/burndown-cli /usr/local/bin/
```

## Commands

| Command | Purpose |
|---------|---------|
| `login` | Register this machine with a collector key and save credentials |
| `register` | Re-register this machine and refresh collector id and policy |
| `setup` | Configure Claude Code and Codex to export telemetry to the collector |
| `serve` | Run the collector daemon (receiver, normalize, filter, queue, upload) |
| `service` | Manage the background service (`install`, `start`, `status`, `stop`, `uninstall`) |
| `status` | Show daemon state, counters, and configuration |
| `stats` | Show local daily tokens, cost, and top tools from retained events |
| `doctor` | Run health checks and print remediation hints |
| `send-test` | Post a synthetic OTLP log to the local receiver and confirm it queued |

Run any command with `--help` for its flags.
