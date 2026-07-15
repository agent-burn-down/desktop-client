# Troubleshooting

Run `burndown-cli doctor` first. It checks version, config, backend reachability,
heartbeat, the daemon, agent OTEL setup, queue integrity, and the service, and
prints a one-line fix for each failing check. Exit code is 0 (pass), 1 (warn), or
2 (fail); add `--json` for machine-readable output.

## Daemon not running

`status` reports `daemon: not running`, or `doctor` fails the daemon check. Start
the service:

```
burndown-cli service start
burndown-cli service status
```

To run in the foreground instead (for example while debugging), use
`burndown-cli serve --verbose`.

## Port 8765 already in use

`serve` exits with `cannot bind 127.0.0.1:8765 (another burndown instance may be
running)`. Either a collector is already running or another process holds the
port.

```
burndown-cli service status          # is a collector already running?
lsof -iTCP:8765 -sTCP:LISTEN         # what holds the port?
```

To use a different port, point both the agents and the daemon at it:

```
burndown-cli setup --port <port>
burndown-cli serve --port <port>
```

## Key rejected

`login` reports `collector key rejected`. The key is wrong or has been revoked.
Get a fresh key from [app.agentburndown.com](https://app.agentburndown.com) and
run `burndown-cli login` again.

## Agent not detected

`setup` skips an agent it cannot detect (no config directory and no binary on
`PATH`). Force it:

```
burndown-cli setup --claude
burndown-cli setup --codex
burndown-cli setup --all
```

## No data in the dashboard or stats

1. Confirm you restarted Claude Code and Codex after running `setup`; they read
   the OTEL settings at startup.
2. Check counters:

   ```
   burndown-cli status
   ```

   `received` should climb as you use the agents, and `uploaded` should follow on
   the next flush.
3. Confirm the receiver-to-queue path directly:

   ```
   burndown-cli send-test
   ```

   A healthy run prints `send-test ok: receiver accepted 1, queued advanced by 1`.
4. Check `burndown-cli stats` for recent local usage. Note that `stats` only
   counts events already uploaded and still inside the retention window (default
   7 days), so a brand-new event appears after the next upload cycle.

## Agent events show an unknown project

Repository attribution applies to new telemetry received after installing the
fixed client. The collector resolves Codex `conversation.id` and Claude Code
`session.id` values against local session metadata; it does not rewrite events
or rollups that the hosted service already ingested as `(unknown)`.

For a new Codex session, confirm its file exists below `~/.codex/sessions` (or
`~/.codex/archived_sessions`) and contains `session_meta.payload.cwd` or
`turn_context.payload.cwd`. For Claude Code, confirm the session UUID exists as
a JSONL filename below `~/.claude/projects` and its records contain `cwd`.
Missing or unreadable metadata safely leaves the project unknown without
dropping telemetry. Historical repair is deliberately out of scope for the
client: it must be performed as a separate, auditable backend migration using
the original event timestamps and session IDs, rather than mutating local queue
history.

## Logs

The service writes stdout and stderr to:

```
~/.burndown/logs/collector.out.log
~/.burndown/logs/collector.err.log
```

## Reset

To start over, uninstall the service, remove local state, and re-run the
quickstart:

```
burndown-cli service uninstall
rm -rf ~/.burndown
```

Then revert the agent config changes as described in the
[README uninstall section](https://github.com/agent-burn-down/desktop-client#uninstall).
