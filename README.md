# rc-mcp

A personal fleet remote-control system built on the [Model Context
Protocol](https://modelcontextprotocol.io) (MCP). It lets you inspect and
control your own Linux desktop machines from any MCP-capable LLM client —
Claude Desktop, Claude Code, or any other spec-compliant MCP host.

You are not a multi-tenant service here: one operator, their own machines.
There's no user management or RBAC — the security model is about keeping
your LLM client from doing anything you didn't approve, not about isolating
users from each other.

## How it fits together

```
 MCP client (Claude Desktop, Claude Code, ...)
          │  MCP over Streamable HTTP (bearer token)
          ▼
   rc-mcp-server  ──────────────  admin API + web UI (loopback only)
          │  WebSocket (device token, outbound from the agent)
          ▼
   rc-mcp-agent  (runs on each controlled machine)
          │
          ▼
   shell, filesystem, processes, screenshots, sysinfo, input
```

- **rc-mcp-server** is a relay hub. It speaks MCP to LLM clients and a
  WebSocket wire protocol to agents, and routes tool calls between them. It
  never executes any tool logic itself.
- **rc-mcp-agent** runs on each machine you want to control. It dials out to
  the server (no inbound port needed on the desktop — NAT/firewall
  friendly) and does the actual work: running commands, reading files,
  taking screenshots, and so on.
- Every capability an agent exposes is opt-in and configured per-agent, so
  a laptop you rarely touch can run with just `sysinfo`, while a workstation
  can run with everything enabled.

The full protocol and architecture design lives in
[`docs/specs/backend.md`](docs/specs/backend.md); this README is the
practical "how do I run this" guide.

## What an LLM client can do through it

| Capability | Tools |
|---|---|
| Shell | `shell_exec` (one-shot), `shell_session_start`/`write`/`close` (interactive PTY sessions) |
| Filesystem | `fs_read`, `fs_write`, `fs_list`, `fs_delete`, `fs_stat` |
| Processes | `process_list`, `process_info`, `process_signal` |
| Screenshots | `screenshot_capture`, `screenshot_watch` (periodic, streamed) — X11 and Wayland |
| System info | `sysinfo_get` |
| Input injection | `input_key`, `input_mouse_click`, `input_mouse_move`, `input_type` — off by default, every call requires confirmation with no bypass |

Plus MCP resources (`clients://list`, `job://{id}`, `sysinfo://.../overview`,
`audit://log`, `shell://sessions`), three guided prompts (`diagnose_system`,
`safe_cleanup`, `shell_workflow`), and argument completion for device IDs
and file paths.

Anything that mutates state (running a command, writing a file, killing a
process) requires the client to confirm via MCP elicitation before it's
dispatched — the agent never receives a destructive call the operator
hasn't explicitly approved in that moment. Every call is recorded in an
append-only audit log.

## Quick start

You'll need Go 1.23+ and, for the container path, Docker.

### 1. Run the server

```sh
export AUTH_TOKEN=$(openssl rand -hex 64)   # required — the server won't start without it
go run ./cmd/server
```

Or via Docker Compose, which additionally fronts the server with nginx over
TLS (see [`docker-compose.yml`](docker-compose.yml) and
[`.env.example`](.env.example) for the full settings list):

```sh
cp .env.example .env   # fill in AUTH_TOKEN at minimum
# nginx needs a TLS cert; for local dev, a self-signed one is enough
# (see docker/nginx/certs/README.md for the one-liner and the real-CA note)
docker compose up
```

MCP clients and agents connect through nginx on `:443`; the admin API stays
on `127.0.0.1:9090`, loopback only by design (see [Security](#security)) and
never proxied.

### 2. Pair a machine

On the machine you want to control:

```sh
export AGENT_SERVER_URL=wss://your-server-host/agent/ws
go run ./cmd/agent
```

First run has no device token yet, so the agent prints a pairing code and
waits. Approve it from the server host:

```sh
curl -X POST http://127.0.0.1:9090/admin/approve -d '{"code":"ABCD-1234"}'
```

or open `http://127.0.0.1:9090/` in a browser for the same thing with a UI
— pending codes, paired devices with revoke, and the audit log. Once
approved, the agent saves its device token locally and reconnects on its
own from then on, including after the server or machine restarts.

### 3. Point an MCP client at it

Configure your MCP client (Claude Desktop, Claude Code, etc.) with the
server's `/mcp` endpoint and the `AUTH_TOKEN` bearer token. The client will
see whichever tools your paired agents have capabilities enabled for.

## Configuration

Every setting is an environment variable — see
[`.env.example`](.env.example) for the full list with defaults, and
[`docs/specs/backend.md` Section 15](docs/specs/backend.md) for the
authoritative descriptions. A few worth knowing up front:

- `AGENT_CAPABILITIES` (agent side) — comma-separated list of what an agent
  exposes: `shell,screenshot,filesystem,process,sysinfo` by default;
  `input` is available but never on by default.
- `RC_SHELL_SKIP_CONFIRM`, `RC_FS_SKIP_CONFIRM`, `RC_PROCESS_SKIP_CONFIRM`
  (server side) — skip the confirmation prompt for that tool group. Input
  injection has no such flag; it always confirms.
- `RC_SHELL_ALLOWLIST` / `RC_SHELL_DENYLIST` — regex patterns (one per
  line) a shell command must pass before it's ever dispatched.
- `RC_GLOBAL_FS_ALLOWED_ROOTS` / `AGENT_FS_ALLOWED_ROOTS` — restrict which
  absolute paths filesystem tools can touch, enforced server-side and
  agent-side respectively.
- `MCP_SESSION_STORE=redis` + `REDIS_ADDR` — switch from the single-instance
  in-memory/file-backed defaults to a Redis-backed session store, device
  registry, and cross-replica dispatch routing, for running more than one
  server replica. See [`docs/operations/scaling.md`](docs/operations/scaling.md).
- `AGENT_AUTO_UPDATE=true` — let an agent download, checksum-verify, and
  install a newer build the server advertises. Off by default. See
  [`docs/operations/agent-releases.md`](docs/operations/agent-releases.md).

## Security

A few things are load-bearing, not incidental:

- **The LLM can never pair, approve, or revoke a device.** Pairing approval
  only exists on the admin API/web UI, which only listens on loopback —
  there is no code path from the MCP surface to device management.
- **Destructive actions require live confirmation** via MCP elicitation,
  shown to the human at the client, not assumed from a prior approval.
- **Every tool call is audited**, append-only, with the session, target
  device, tool, and outcome — SHA-256 digested by default, or in full under
  `RC_AUDIT_FULL_ARGS=true` for forensic use.
- **Agents connect outbound only.** No inbound port on the controlled
  machine, and each connection re-authenticates with a per-device token
  the server can revoke at any time.

## Development

```sh
go build ./...
go test ./...
```

This is a Go module wrapped in an [Nx](https://nx.dev) workspace purely for
affected-graph CI orchestration (`npx nx affected -t lint,test,build`) —
there's no JS/TS runtime code here. See
[`docs/adr/0001-nx-go-integration.md`](docs/adr/0001-nx-go-integration.md)
for why.

## Docs

- [`docs/specs/backend.md`](docs/specs/backend.md) — the full protocol,
  wire format, and architecture specification.
- [`docs/operations/`](docs/operations) — running a release pipeline,
  auto-update, and multi-replica scaling.
- [`docs/phases/`](docs/phases) — how the MVP and post-MVP work was
  sequenced.
- [`docs/adr/`](docs/adr) — architecture decision records.
