# Installing an agent

`rc-mcp-agent` runs on each Linux desktop machine you want to control. It
dials out to the server — no inbound port needed, NAT/firewall friendly —
and does the actual work (shell, filesystem, processes, screenshots,
sysinfo, input). Do this once per machine you want the LLM client to
reach.

**Requirements:** Linux (X11 or Wayland for the screenshot/input
capabilities; those are still off if you skip a display). No Docker
needed — the agent is a standalone binary.

## 1. Download the binary

```sh
version=0.1.0        # see docs/operations/agent-releases.md for the latest
arch=amd64            # or arm64
base="https://github.com/champi-ai/rc-mcp/releases/download/agent-v${version}"

curl -fLO "${base}/rc-mcp-agent-linux-${arch}"
curl -fLO "${base}/rc-mcp-agent-linux-${arch}.sha256"
sha256sum -c "rc-mcp-agent-linux-${arch}.sha256"
chmod +x "rc-mcp-agent-linux-${arch}"
mkdir -p ~/.local/bin
mv "rc-mcp-agent-linux-${arch}" ~/.local/bin/rc-mcp-agent
```

No release published yet? Build from source instead (Go 1.25+):

```sh
git clone https://github.com/champi-ai/rc-mcp.git
cd rc-mcp
go build -ldflags="-s -w" -o ~/.local/bin/rc-mcp-agent ./cmd/agent
```

## 2. Configure

The agent reads its config from environment variables — there's no config
file. At minimum:

```sh
export AGENT_SERVER_URL=wss://your-server-host/agent/ws   # required
```

Worth setting explicitly for a persistent install:

- `AGENT_TOKEN_PATH` (default `~/.rc-mcp/agent-token`) — where the device
  token this machine gets on approval is stored. Keep this path stable
  across restarts; deleting it forces re-pairing.
- `AGENT_CAPABILITIES` (default `shell,screenshot,filesystem,process,sysinfo`)
  — comma-separated list of what this agent exposes. `input` (keyboard/
  mouse injection) is available but never on by default — add it only if
  you want it, and every call still requires live confirmation regardless.
- `AGENT_FS_ALLOWED_ROOTS` — comma-separated absolute paths restricting
  what filesystem tools can touch on this machine. Empty means
  unrestricted; consider setting this on a machine with sensitive data
  outside what you want the LLM client to reach.
- `DISPLAY` — needed for `screenshot`/`input` capabilities under X11.

See [`.env.example`](../../.env.example) and
[`docs/specs/backend.md` Section 15](../specs/backend.md) for the full
list, including `AGENT_AUTO_UPDATE` (see
[`docs/operations/agent-releases.md`](../operations/agent-releases.md)).

## 3. Run it as a systemd user service

```ini
# ~/.config/systemd/user/rc-mcp-agent.service
[Unit]
Description=rc-mcp Desktop Agent
After=network-online.target
Wants=network-online.target

[Service]
Environment=AGENT_SERVER_URL=wss://your-server-host/agent/ws
Environment=AGENT_CAPABILITIES=shell,screenshot,filesystem,process,sysinfo
Environment=DISPLAY=:0
ExecStart=%h/.local/bin/rc-mcp-agent
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
```

A user service (not system-wide) keeps the agent running as you, with
access to your session's `DISPLAY` for screenshots/input — that's what
`AGENT_SYSTEMD_UNIT` (see step 5) restarts on auto-update.

```sh
systemctl --user daemon-reload
systemctl --user enable --now rc-mcp-agent
loginctl enable-linger "$USER"   # keep it running after you log out
```

Prefer to just try it first? Skip the unit file and run
`rc-mcp-agent` directly in a terminal with the env vars exported.

## 4. Pair with the server

First run has no device token yet, so the agent prints a pairing code and
waits:

```
$ rc-mcp-agent
Pairing code: ABCD-1234
```

It expires after `PAIRING_CODE_TTL` (server-side default 5m) — approve it
before then, or the agent will print a message and restart pairing with a
fresh code.

Approve it from the server host:

```sh
curl -X POST http://127.0.0.1:9090/admin/approve -d '{"code":"ABCD-1234"}'
```

or open `http://<server-host>:9090/` in a browser (from the server host,
or via an SSH tunnel — the admin API is loopback-only) for the same thing
with a UI: pending codes, paired devices with revoke, and the audit log.

Once approved, the agent saves its device token to `AGENT_TOKEN_PATH` and
reconnects on its own from then on — including after the server or this
machine restarts. You won't see the pairing code again unless you delete
that token file.

## 5. Optional: auto-update

Set `AGENT_AUTO_UPDATE=true` to let the agent download, checksum-verify,
and install a newer build the server advertises, restarting itself via
`systemctl restart <AGENT_SYSTEMD_UNIT>` (default `rc-mcp-agent` — match
the unit name from step 3 if you used a different one). Off by default.
Full mechanics in
[`docs/operations/agent-releases.md`](../operations/agent-releases.md).

## Next steps

Point an MCP client (Claude Desktop, Claude Code, ...) at the server's
`/mcp` endpoint with the `AUTH_TOKEN` bearer token — see the [README's
Quick Start](../../README.md#quick-start). It'll see whichever tools this
agent's `AGENT_CAPABILITIES` enabled.
