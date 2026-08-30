# rc-mcp Backend Specification

> Go relay hub + desktop agent architecture -- MCP over Streamable HTTP to
> LLM clients, WebSocket wire protocol to desktop agents, hybrid JSON/binary
> framing, pairing-based device authentication, two-hop streaming for
> long-running operations.

---

## 1. Project Overview

**rc-mcp** is a personal fleet remote-control system built on the Model
Context Protocol (MCP). It allows an operator to inspect and control one or
more of their own Linux desktop machines through any MCP-capable LLM client.

The system has three components:

| Component | Role |
|---|---|
| **rc-mcp-server** (relay hub) | Accepts MCP connections from LLM clients over Streamable HTTP. Accepts WebSocket connections from desktop agents. Routes tool calls from clients to the correct agent, bridges responses and streaming progress back. Does **not** execute any tool logic itself. |
| **rc-mcp-agent** (desktop agent) | Runs on each controlled Linux machine. Dials out to the server over WebSocket (outbound-only -- no inbound port required on the desktop, NAT/firewall friendly). Executes all tool logic locally: shell, screenshots, filesystem, process management, sysinfo. |
| **Wire protocol** (shared package) | A hybrid JSON envelope + binary frame format imported by both server and agent, ensuring they cannot drift out of sync. |

### Target MCP clients

| Client | Notes |
|---|---|
| Claude Desktop | Primary expected consumer; uses built-in MCP host |
| Claude Code | CLI-based MCP host |
| Any spec-compliant MCP host | The server implements the 2025-03-26 protocol; any host that speaks it can connect |

### Goals

- Let the operator's LLM client inspect and control any of the operator's
  Linux machines through well-defined MCP tools and resources.
- Stream output from long-running commands and periodic screenshots via
  two-hop push notifications (agent to server to client) -- the client never
  polls.
- Maintain interactive, PTY-backed shell sessions on the target agent machine
  across multiple `tools/call` round-trips within a single MCP session.
- Provide an append-only, tamper-resistant audit log of every tool invocation,
  logged server-side as the authoritative record (per MCP session, target
  device, tool, result).
- Allow the operator to enable/disable each capability area independently
  per agent, not per server.
- Secure device enrollment via a pairing-code flow where the LLM can never
  approve a new device -- this is a deliberate, named security invariant.

### Non-goals

- **Not a penetration-testing / attack tool.** The system is run by and for
  the machine owner.
- **Not a multi-user system.** One operator, personal fleet. There is no user
  management, RBAC, or tenant isolation.
- **Keyboard/mouse input injection** is out of scope for this version (see
  Open Questions, Section 19).
- **GUI application automation** beyond screenshots is out of scope.
- **Horizontal scaling of the server** is not designed for the default
  deployment. See Open Questions for notes on multi-replica considerations.

---

## 2. MCP Protocol Design

### Transport

**Streamable HTTP** (spec revision 2025-03-26).

Single MCP endpoint: `/mcp`

| Method | Purpose |
|---|---|
| `POST /mcp` | Client-to-server JSON-RPC messages (requests, responses, notifications) |
| `GET /mcp` | Server-initiated SSE stream for push notifications |
| `DELETE /mcp` | Session termination |

The deprecated two-endpoint HTTP+SSE transport is **not** supported.

A separate endpoint exists for desktop agents: `/agent/ws` (WebSocket).
This endpoint is **not** part of the MCP protocol surface. See Section 2.1.

### Session lifecycle

#### `initialize` request / response

Client sends:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "initialize",
  "params": {
    "protocolVersion": "2025-03-26",
    "capabilities": {
      "roots": { "listChanged": true },
      "sampling": {},
      "elicitation": {}
    },
    "clientInfo": {
      "name": "claude-desktop",
      "version": "1.2.0"
    }
  }
}
```

Server responds (and includes `Mcp-Session-Id` header):

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "protocolVersion": "2025-03-26",
    "capabilities": {
      "tools":        { "listChanged": true },
      "resources":    { "subscribe": true, "listChanged": true },
      "prompts":      { "listChanged": true },
      "logging":      {},
      "completions":  {}
    },
    "serverInfo": {
      "name": "rc-mcp",
      "version": "0.2.0"
    }
  }
}
```

#### `Mcp-Session-Id` header

- **Generation:** 128-bit cryptographically random token, hex-encoded (32
  characters). Generated via `crypto/rand`.
- **Issued on:** The HTTP response to the `initialize` request, as the
  `Mcp-Session-Id` response header.
- **Validation:** Every subsequent `POST`, `GET`, and `DELETE` request must
  include the `Mcp-Session-Id` header. The server rejects requests with a
  missing or unknown session ID with HTTP `404 Not Found` (per spec). Requests
  before `initialize` that carry no session ID are only valid if the body is an
  `initialize` request.

#### Session termination

| Trigger | Behavior |
|---|---|
| Client sends `DELETE /mcp` with valid session ID | Server closes the SSE stream, cancels all in-flight dispatches (sends `close` to agents for session-scoped streams), removes session state. Returns `204 No Content`. |
| Server-side idle timeout (`MCP_SESSION_IDLE_TIMEOUT`, default 30m) | Same cleanup as above. The SSE stream is closed with a final comment line `; session expired`. |
| Client sends `notifications/cancelled` for a specific request | Cancels that request's context; does not terminate the session. Forwards cancellation to the agent if a dispatch is in-flight. |
| Server shutdown (`SIGTERM`) | Graceful drain: cancel in-flight dispatches, close all SSE streams, close all agent WebSocket connections with a close frame, persist audit log, exit. |

### Capabilities declared by this server

| Capability | Enabled | Rationale |
|---|---|---|
| `tools` | Yes, `listChanged: true` | Core of the server. List changes when agents connect/disconnect or change their capability areas. |
| `resources` | Yes, `subscribe: true`, `listChanged: true` | Device registry, job status, audit log are exposed as resources. |
| `prompts` | Yes, `listChanged: true` | Operator-defined prompt templates for common workflows. |
| `logging` | Yes | Server emits structured log messages to the client for diagnostics. |
| `completions` | Yes | Argument auto-completion for tool inputs (e.g. clientId, file paths). |

**Push notifications for long-running work:** `notifications/progress` is the
primary channel. It rides the existing SSE stream opened by the client's
`GET /mcp`. No separate transport is needed. Each progress notification is
keyed by the `progressToken` from the originating `tools/call` request's
`_meta` field.

### Resumability

- Every SSE event emitted on a session's stream carries a monotonically
  increasing integer `id:` field, starting at 1 per session.
- The server maintains a per-session replay buffer of the last
  `SSE_REPLAY_BUFFER_SIZE` events (default: 500, configurable).
- On reconnect, if the client sends a `Last-Event-ID` header with a value
  within the replay buffer's range, the server replays all events after that ID
  before resuming the live stream.
- If the requested ID is older than the buffer's oldest entry, the server
  returns HTTP `204 No Content` to signal that replay is not possible and the
  client should re-initialize.
- Replay buffers are retained for the session's lifetime (including idle timeout
  window).

### 2.1 Agent WebSocket Endpoint

Endpoint: `GET /agent/ws` (upgrades to WebSocket).

This is a server-internal endpoint for desktop agents, **not** part of the MCP
protocol surface. It uses the wire protocol defined in Section 2.2.

**Connection flow:**

1. Agent dials `wss://server-host/agent/ws` (TLS terminated by nginx).
2. WebSocket upgrade succeeds.
3. Agent sends a `hello` message containing its device token (or a
   `pair_request` if pairing for the first time -- see Section 12.2).
4. Server validates the token against the device registry.
5. Server responds with `hello_ack` containing the negotiated protocol version
   and the device's registered ID.
6. The agent is now registered as online in the device registry. The server
   emits `notifications/resources/updated` for `clients://list` on all active
   MCP sessions so LLM clients see the device come online.

**Heartbeat/liveness:**

- WebSocket ping/pong frames at 30-second intervals, initiated by the server.
- If a pong is not received within 10 seconds, the server marks the device
  as offline and closes the connection.
- The agent side also sends pings at 30-second intervals as a keepalive; if no
  pong is received within 10 seconds, the agent initiates reconnect.

**Reconnect behavior (agent side):**

- Exponential backoff: 1s, 2s, 4s, 8s, 16s, 30s (capped). Jitter: +/- 20%.
- On reconnect, the agent re-authenticates with its persistent device token
  (no re-pairing needed).
- In-flight job state across brief disconnects: the agent keeps local
  session/job state alive (running shell sessions, in-progress screenshot_watch
  streams) for a configurable grace period (`AGENT_RECONNECT_GRACE_PERIOD`,
  default: 60s). If reconnected within the grace period, a `hello_ack` with
  `resume: true` tells the agent to resume streaming; the server reattaches
  correlation IDs to the original MCP sessions. Beyond the grace period,
  orphaned shell sessions are killed, in-progress jobs are marked `failed`
  with reason `"agent_disconnect"`, and the agent cleans up local resources.

### 2.2 Wire Protocol

The wire protocol is a **hybrid format** shared between server and agent via
`internal/protocol`. Both sides import the same Go package, preventing drift.

#### JSON envelope (text WebSocket frames)

All control and RPC-shaped messages use a JSON envelope:

```json
{
  "type": "dispatch",
  "id": "corr-uuid-1234",
  "protocolVersion": "1",
  "ts": "2026-08-06T12:00:00Z",
  "payload": { ... }
}
```

**`type` values:**

| Type | Direction | Purpose |
|---|---|---|
| `hello` | agent -> server | Initial authentication; carries device token (or see `pair_request`) |
| `hello_ack` | server -> agent | Confirms authentication, returns device ID and negotiated protocol version; includes `resume: bool` |
| `pair_request` | agent -> server | First-run pairing; agent requests a pairing code |
| `pair_code` | server -> agent | Server returns a short-lived, human-readable pairing code |
| `pair_approved` | server -> agent | Operator approved the code; carries the persistent device token |
| `dispatch` | server -> agent | Tool call forwarded to the agent for execution |
| `result` | agent -> server | Terminal result of a dispatched tool call |
| `progress` | agent -> server | Streaming update for an in-flight dispatch (stdout chunk, screenshot frame metadata, etc.) |
| `error` | bidirectional | Error related to a specific correlation ID or connection-level |
| `cancel` | server -> agent | Cancel an in-flight dispatch (triggered by MCP `notifications/cancelled`) |
| `ping` | bidirectional | Application-level keepalive (in addition to WS-level ping/pong) |
| `pong` | bidirectional | Response to `ping` |
| `close` | server -> agent | Graceful close of a specific stream or the entire connection |

**Fields:**

| Field | Type | Required | Description |
|---|---|---|---|
| `type` | string | yes | Message type (enum above) |
| `id` | string | yes (except `ping`/`pong`) | Correlation ID (UUIDv4). For `dispatch`/`result`/`progress`/`error`/`cancel`, this correlates a request-response pair. |
| `protocolVersion` | string | yes for `hello`/`hello_ack` | Wire protocol version. Current: `"1"`. Mismatch is a hard rejection. |
| `ts` | string (RFC 3339) | yes | Timestamp of message creation. |
| `payload` | object | type-dependent | Type-specific payload. See per-type schemas in Section 6. |

#### Binary frames (binary WebSocket frames)

High-frequency or large payloads use binary WebSocket frames to avoid
JSON-encoding overhead:

- Screenshot image bytes (PNG)
- Shell stdout/stdin chunks
- screenshot_watch frames

**Binary frame header (fixed 9 bytes):**

```
Offset  Size  Field
0       4     Correlation ID prefix (first 4 bytes of the UUID, big-endian)
4       4     Stream sequence number (uint32, big-endian, monotonically increasing per correlation ID)
8       1     Frame type byte
9..     var   Raw payload bytes
```

**Frame type bytes:**

| Byte | Meaning |
|---|---|
| `0x01` | Shell stdout chunk |
| `0x02` | Shell stdin chunk |
| `0x03` | Screenshot PNG data |
| `0x04` | File content chunk (for large file reads) |

The 4-byte correlation ID prefix is derived from the full UUID in the
corresponding JSON `dispatch` message. The receiver maintains a map of active
correlation IDs and uses the prefix to demux which logical stream a binary
frame belongs to. In the unlikely event of a prefix collision (two active
dispatches sharing the same 4-byte prefix), the server must reject the second
dispatch with an error and ask the client to retry (which will generate a new
correlation ID). This is a degenerate case that will not occur in practice
with the expected concurrency levels of a personal tool.

The stream sequence number allows the receiver to detect reordering or gaps.
Out-of-order frames are reordered up to a 16-frame window; frames beyond that
window are dropped and a `progress` JSON message is sent indicating data loss.

**`protocolVersion` negotiation:**

- The agent sends `protocolVersion: "1"` in its `hello` message.
- The server validates that it supports the requested version.
- If supported, the server echoes `protocolVersion: "1"` in `hello_ack`.
- If not supported, the server sends an `error` with type `"version_mismatch"`
  and immediately closes the WebSocket with code 1002 (protocol error). The
  error payload includes the server's supported versions so the agent can
  display a clear message to the operator.

---

## 3. Tool Inventory

Each capability area is independently toggle-able **per agent** via the
agent's configuration (`AGENT_CAPABILITIES`, comma-separated:
`shell,screenshot,filesystem,process,sysinfo`). The server aggregates the
union of capabilities across all online agents for `tools/list`. A tool call
targeting a specific agent fails with a tool error if that agent does not have
the required capability enabled.

Every tool requires a `clientId` input field identifying the target device.
The LLM client can discover available devices via the `clients://list`
resource (Section 4).

---

### 3.1 Shell Execution

#### 3.1.1 `shell_exec`

```
tool: shell_exec
  Title: Execute Shell Command
  Description: Run a one-shot shell command on the target device, capture stdout/stderr/exit code.
  Input schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "clientId": { "type": "string", "description": "Target device ID" },
        "command":  { "type": "string", "description": "Command to execute (passed to /bin/sh -c)" },
        "cwd":      { "type": "string", "description": "Working directory (default: $HOME)" },
        "env":      { "type": "object", "additionalProperties": { "type": "string" }, "description": "Extra environment variables" },
        "timeout":  { "type": "integer", "description": "Max execution time in seconds (default: 30, max: 300)", "minimum": 1, "maximum": 300 },
        "stdin":    { "type": "string", "description": "Optional stdin to pipe into the command" }
      },
      "required": ["clientId", "command"]
    }
  Output schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "stdout":     { "type": "string" },
        "stderr":     { "type": "string" },
        "exitCode":   { "type": "integer" },
        "killed":     { "type": "boolean", "description": "True if the process was killed due to timeout" },
        "durationMs": { "type": "integer" },
        "clientId":   { "type": "string" }
      },
      "required": ["stdout", "stderr", "exitCode", "killed", "durationMs", "clientId"]
    }
  Annotations:
    readOnlyHint: false
    destructiveHint: true
    idempotentHint: false
    openWorldHint: true
  Streaming behavior:
    - progress notifications (notifications/progress, progressToken)
    - Stdout/stderr chunks streamed as progress messages. Agent sends binary
      shell stdout frames over the WS; server converts them to
      notifications/progress on the MCP SSE stream. Minimum interval: 500ms
      or 4KB, whichever comes first.
  Long-running behavior:
    - Dispatch pattern (b): the JSON-RPC response is held open, progress
      notifications stream stdout/stderr via two-hop (agent WS -> server ->
      MCP SSE), and the final tools/call result is returned on completion
      or timeout. The server forwards the dispatch to the agent and bridges
      the result back.
  Multi-turn behavior:
    - Single-shot. Each invocation is independent. For stateful shell work,
      use shell_session_* tools instead.
    - Requires elicitation-based confirmation (Section 11) before the server
      forwards to the agent, unless the operator has set
      RC_SHELL_SKIP_CONFIRM=true.
  Side effects / state touched:
    - Arbitrary: the command can do anything the agent process's user can do
      on the target machine.
    - Audit log entry written server-side.
  Error modes:
    - Target device offline: tool error (isError: true).
    - Target device does not have shell capability enabled: tool error.
    - Command not found on agent: exitCode 127, not a protocol error.
    - Timeout: killed=true, partial stdout/stderr returned.
    - Working directory does not exist on agent: tool error (isError: true).
    - Command execution failed to start on agent: tool error (isError: true).
    - Agent disconnects mid-execution: tool error with reason
      "agent_disconnect", partial output if available.
```

#### 3.1.2 `shell_session_start`

```
tool: shell_session_start
  Title: Start Interactive Shell Session
  Description: Open a PTY-backed interactive shell on the target device, tied to this MCP session.
  Input schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "clientId": { "type": "string", "description": "Target device ID" },
        "shell":    { "type": "string", "description": "Shell binary (default: $SHELL or /bin/bash)" },
        "cwd":      { "type": "string", "description": "Initial working directory (default: $HOME)" },
        "env":      { "type": "object", "additionalProperties": { "type": "string" }, "description": "Extra environment variables" },
        "rows":     { "type": "integer", "description": "PTY rows (default: 24)", "minimum": 1 },
        "cols":     { "type": "integer", "description": "PTY columns (default: 80)", "minimum": 1 }
      },
      "required": ["clientId"]
    }
  Output schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "shellSessionId": { "type": "string", "description": "Unique ID for this shell session" },
        "pid":            { "type": "integer" },
        "shell":          { "type": "string" },
        "clientId":       { "type": "string" }
      },
      "required": ["shellSessionId", "pid", "shell", "clientId"]
    }
  Annotations:
    readOnlyHint: false
    destructiveHint: true
    idempotentHint: false
    openWorldHint: true
  Streaming behavior: none (returns immediately)
  Long-running behavior: synchronous (the shell is started on the agent, metadata returned)
  Multi-turn behavior:
    - First step in a multi-turn shell workflow. The returned shellSessionId
      is used in subsequent shell_session_write / shell_session_close calls.
    - Requires elicitation-based confirmation before starting, unless
      RC_SHELL_SKIP_CONFIRM=true.
  Side effects / state touched:
    - Agent allocates a PTY and spawns a shell process.
    - Shell session is tracked in both the agent's local state and the
      server's MCP session state (mapping shellSessionId -> clientId).
    - Audit log entry written server-side.
  Error modes:
    - Target device offline: tool error.
    - Shell binary not found on agent: tool error (isError: true).
    - Max concurrent shell sessions per MCP session exceeded (default: 5):
      tool error.
    - Agent does not have shell capability enabled: tool error.
```

#### 3.1.3 `shell_session_write`

```
tool: shell_session_write
  Title: Write to Interactive Shell
  Description: Send input (keystrokes/commands) to an open interactive shell session on the target device.
  Input schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "shellSessionId": { "type": "string" },
        "input":          { "type": "string", "description": "Text to write to the PTY (include \\n for Enter)" }
      },
      "required": ["shellSessionId", "input"]
    }
  Output schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "bytesWritten": { "type": "integer" },
        "output":       { "type": "string", "description": "Accumulated output after idle" },
        "exitCode":     { "type": "integer", "description": "Set if shell exited" },
        "exited":       { "type": "boolean" }
      },
      "required": ["bytesWritten"]
    }
  Annotations:
    readOnlyHint: false
    destructiveHint: true
    idempotentHint: false
    openWorldHint: true
  Streaming behavior:
    - After writing, the agent begins streaming PTY output as binary shell
      stdout frames over the WS. The server bridges these to
      notifications/progress on the MCP SSE stream using the request's
      progressToken.
    - Output is streamed in chunks every 200ms or 4KB, whichever comes first,
      until the shell becomes idle (no new output for 2s) or the client sends
      the next shell_session_write.
  Long-running behavior:
    - Dispatch pattern (b): the JSON-RPC response is held open while output
      streams via two-hop. The final tools/call result is returned once the
      shell goes idle or a configurable read timeout (default 30s) elapses.
  Multi-turn behavior:
    - Depends on prior tool call (shell_session_start) in same MCP session.
    - The server resolves the target agent from its shellSessionId -> clientId
      mapping. The clientId is NOT required as input (it is implicit).
    - Does NOT require per-invocation elicitation confirmation (the session
      start already confirmed). The operator can override this via
      RC_SHELL_CONFIRM_EVERY_WRITE=true.
  Side effects / state touched:
    - Writes to the PTY on the agent; the shell process executes whatever was sent.
    - Audit log entry written server-side (input is logged as a SHA-256 digest, not raw).
  Error modes:
    - Shell session not found or already closed: tool error (isError: true).
    - Agent offline: tool error with reason "agent_disconnect".
    - PTY write failed on agent: tool error.
    - Shell process exited: returns final output + exit code, marks session closed.
```

#### 3.1.4 `shell_session_close`

```
tool: shell_session_close
  Title: Close Interactive Shell Session
  Description: Terminate an interactive shell session on the target device and release its PTY.
  Input schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "shellSessionId": { "type": "string" },
        "signal":         { "type": "string", "enum": ["SIGTERM", "SIGKILL"], "description": "Signal to send (default: SIGTERM, then SIGKILL after 5s)" }
      },
      "required": ["shellSessionId"]
    }
  Output schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "exitCode":    { "type": "integer" },
        "finalOutput": { "type": "string", "description": "Any remaining buffered output" }
      },
      "required": ["exitCode"]
    }
  Annotations:
    readOnlyHint: false
    destructiveHint: false
    idempotentHint: true
    openWorldHint: false
  Streaming behavior: none
  Long-running behavior: synchronous
  Multi-turn behavior: terminal step of the shell session workflow.
    Server resolves clientId from shellSessionId mapping.
  Side effects / state touched:
    - Agent kills the shell process, releases the PTY.
    - Server removes the shellSessionId mapping from MCP session state.
    - Audit log entry written server-side.
  Error modes:
    - Shell session not found: tool error (isError: true).
    - Agent offline: tool error; server cleans up its own mapping.
    - Process already exited on agent: returns the exit code, no error.
```

---

### 3.2 Screenshots

#### 3.2.1 `screenshot_capture`

```
tool: screenshot_capture
  Title: Capture Screenshot
  Description: Capture the current display on the target device and return it as a PNG image.
  Input schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "clientId": { "type": "string", "description": "Target device ID" },
        "display":  { "type": "string", "description": "X11 display (default: :0)" },
        "monitor":  { "type": "integer", "description": "Monitor index (default: -1 = all monitors stitched)", "minimum": -1 },
        "quality":  { "type": "integer", "description": "PNG compression level 0-9 (default: 6)", "minimum": 0, "maximum": 9 },
        "maxWidth": { "type": "integer", "description": "Max width in px; image is downscaled preserving aspect ratio if exceeded" }
      },
      "required": ["clientId"]
    }
  Output schema:
    Returns MCP content of type "image" with mimeType "image/png" and
    base64-encoded data. The agent captures the screenshot and sends the PNG
    bytes as a binary frame over the WS; the server base64-encodes them into
    the MCP image content response.
  Annotations:
    readOnlyHint: true
    destructiveHint: false
    idempotentHint: true
    openWorldHint: false
  Streaming behavior: none
  Long-running behavior: synchronous (capture + encode typically < 1s;
    the dispatch-to-agent round trip adds latency but remains synchronous)
  Multi-turn behavior: single-shot
  Side effects / state touched:
    - Agent reads from X11/Wayland display. No mutations.
    - Audit log entry written server-side.
  Error modes:
    - Target device offline: tool error.
    - No display available on agent ($DISPLAY not set): tool error.
    - Monitor index out of range: tool error.
    - Agent does not have screenshot capability enabled: tool error.
```

#### 3.2.2 `screenshot_watch`

```
tool: screenshot_watch
  Title: Watch Screen (Periodic Screenshots)
  Description: Stream periodic screenshots from the target device as push notifications for a bounded duration.
  Input schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "clientId":     { "type": "string", "description": "Target device ID" },
        "display":      { "type": "string", "description": "X11 display (default: :0)" },
        "monitor":      { "type": "integer", "description": "Monitor index (default: -1 = all)", "minimum": -1 },
        "intervalMs":   { "type": "integer", "description": "Capture interval in ms (default: 2000, min: 500)", "minimum": 500 },
        "maxFrames":    { "type": "integer", "description": "Max frames to capture (default: 30, max: 120)", "minimum": 1, "maximum": 120 },
        "durationSecs": { "type": "integer", "description": "Max duration in seconds (default: 60, max: 300)", "minimum": 1, "maximum": 300 },
        "maxWidth":     { "type": "integer", "description": "Max width in px for downscaling" },
        "quality":      { "type": "integer", "minimum": 0, "maximum": 9, "description": "PNG compression (default: 6)" }
      },
      "required": ["clientId"]
    }
  Output schema (JSON Schema):
    Immediate tools/call result (returned as soon as the agent acknowledges
    the dispatch -- see Section 9, dispatch pattern (a)):
    {
      "type": "object",
      "properties": {
        "jobId":    { "type": "string" },
        "clientId": { "type": "string" }
      },
      "required": ["jobId", "clientId"]
    }
    Terminal job outcome (NOT part of the tools/call response -- delivered as
    the terminal notifications/progress event for this request's
    progressToken, and persisted at job://{id} for clients that reconnect
    after it fires):
    {
      "type": "object",
      "properties": {
        "jobId":           { "type": "string" },
        "framesCaptured":  { "type": "integer" },
        "durationMs":      { "type": "integer" },
        "stoppedReason":   { "type": "string", "enum": ["maxFrames", "duration", "cancelled", "agent_disconnect"] },
        "clientId":        { "type": "string" }
      },
      "required": ["jobId", "framesCaptured", "durationMs", "stoppedReason", "clientId"]
    }
    Each intermediate frame is delivered as a separate notifications/progress
    message. The agent sends binary screenshot PNG frames over the WS; the
    server base64-encodes each and converts it to a notifications/progress
    message with the image content.
  Annotations:
    readOnlyHint: true
    destructiveHint: false
    idempotentHint: true
    openWorldHint: false
  Streaming behavior:
    - progress notifications (notifications/progress, progressToken)
    - Two-hop: agent captures and sends binary screenshot frames over WS,
      server bridges to notifications/progress on the MCP SSE stream.
    - "percent" field reflects framesCaptured / maxFrames (or elapsed / duration).
  Long-running behavior:
    - Dispatch pattern (a): the tool returns a preliminary result containing
      a jobId immediately after the dispatch is acknowledged by the agent.
      Frames are delivered as notifications/progress keyed to the original
      request's progressToken. The final result (summary) is delivered via a
      terminal progress notification and persisted in the job resource
      (job://{id}).
    - Rationale: screenshot_watch can run for up to 5 minutes; the client
      may disconnect and reconnect during that time. Pattern (a) ensures
      the job outlives the individual MCP request.
  Multi-turn behavior: single-shot
  Side effects / state touched:
    - Agent reads from display at configured interval. No mutations.
    - Server creates a job record in the job store.
    - Audit log entry written server-side at start and completion.
  Error modes:
    - Target device offline: tool error.
    - No display on agent: tool error.
    - Agent disconnects mid-watch: job marked failed with reason
      "agent_disconnect", partial frames already delivered are retained.
    - Cancelled via notifications/cancelled: server forwards cancel to agent,
      job cancelled, partial frames retained.
    - Agent does not have screenshot capability enabled: tool error.
```

---

### 3.3 Filesystem

#### 3.3.1 `fs_read`

```
tool: fs_read
  Title: Read File
  Description: Read the contents of a file on the target device. Returns text content or base64 for binary.
  Input schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "clientId": { "type": "string", "description": "Target device ID" },
        "path":     { "type": "string" },
        "offset":   { "type": "integer", "description": "Byte offset to start reading (default: 0)", "minimum": 0 },
        "limit":    { "type": "integer", "description": "Max bytes to read (default: 1048576 = 1MB)", "minimum": 1 },
        "encoding": { "type": "string", "enum": ["utf8", "base64"], "description": "default: utf8; falls back to base64 if not valid UTF-8" }
      },
      "required": ["clientId", "path"]
    }
  Output schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "content":   { "type": "string" },
        "encoding":  { "type": "string", "enum": ["utf8", "base64"] },
        "size":      { "type": "integer", "description": "Total file size in bytes" },
        "truncated": { "type": "boolean" },
        "clientId":  { "type": "string" }
      },
      "required": ["content", "encoding", "size", "truncated", "clientId"]
    }
  Annotations:
    readOnlyHint: true
    destructiveHint: false
    idempotentHint: true
    openWorldHint: false
  Streaming behavior: none (for large files the agent sends content as binary
    file content frames; the server assembles them before returning the result)
  Long-running behavior: synchronous
  Multi-turn behavior: single-shot
  Side effects / state touched: none (read-only on agent). Audit log entry server-side.
  Error modes:
    - Target device offline: tool error.
    - File not found on agent: tool error.
    - Permission denied on agent: tool error.
    - Path is a directory: tool error.
    - Agent does not have filesystem capability enabled: tool error.
```

#### 3.3.2 `fs_write`

```
tool: fs_write
  Title: Write File
  Description: Write content to a file on the target device. Creates parent directories as needed.
  Input schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "clientId":   { "type": "string", "description": "Target device ID" },
        "path":       { "type": "string" },
        "content":    { "type": "string" },
        "encoding":   { "type": "string", "enum": ["utf8", "base64"], "description": "default: utf8" },
        "mode":       { "type": "string", "enum": ["overwrite", "append"], "description": "default: overwrite" },
        "fileMode":   { "type": "string", "description": "Unix file mode as octal string (default: 0644)" },
        "createDirs": { "type": "boolean", "description": "Create parent directories (default: true)" }
      },
      "required": ["clientId", "path", "content"]
    }
  Output schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "bytesWritten": { "type": "integer" },
        "path":         { "type": "string", "description": "Absolute resolved path on the agent" },
        "clientId":     { "type": "string" }
      },
      "required": ["bytesWritten", "path", "clientId"]
    }
  Annotations:
    readOnlyHint: false
    destructiveHint: true
    idempotentHint: false
    openWorldHint: false
  Streaming behavior: none
  Long-running behavior: synchronous
  Multi-turn behavior:
    - Single-shot.
    - Requires elicitation confirmation if overwriting an existing file
      (unless RC_FS_SKIP_CONFIRM=true). Confirmation happens MCP-client-side
      before the server forwards the dispatch to the agent.
  Side effects / state touched: writes to filesystem on agent. Audit log entry server-side.
  Error modes:
    - Target device offline: tool error.
    - Permission denied on agent: tool error.
    - Disk full on agent: tool error.
    - Path traversal outside allowed roots on agent (if configured): tool error.
    - Agent does not have filesystem capability enabled: tool error.
```

#### 3.3.3 `fs_list`

```
tool: fs_list
  Title: List Directory
  Description: List directory contents with stat info on the target device.
  Input schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "clientId":   { "type": "string", "description": "Target device ID" },
        "path":       { "type": "string" },
        "recursive":  { "type": "boolean", "description": "default: false" },
        "maxDepth":   { "type": "integer", "description": "Max recursion depth (default: 3)", "minimum": 1, "maximum": 10 },
        "showHidden": { "type": "boolean", "description": "Include dotfiles (default: false)" },
        "limit":      { "type": "integer", "description": "Max entries to return (default: 1000)", "minimum": 1 }
      },
      "required": ["clientId", "path"]
    }
  Output schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "entries": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "name":     { "type": "string" },
              "path":     { "type": "string" },
              "type":     { "type": "string", "enum": ["file", "dir", "symlink", "other"] },
              "size":     { "type": "integer" },
              "mode":     { "type": "string" },
              "modTime":  { "type": "string", "format": "date-time" }
            }
          }
        },
        "truncated":  { "type": "boolean" },
        "totalCount": { "type": "integer" },
        "clientId":   { "type": "string" }
      },
      "required": ["entries", "truncated", "clientId"]
    }
  Annotations:
    readOnlyHint: true
    destructiveHint: false
    idempotentHint: true
    openWorldHint: false
  Streaming behavior: none
  Long-running behavior: synchronous
  Multi-turn behavior: single-shot
  Side effects / state touched: none on agent. Audit log entry server-side.
  Error modes:
    - Target device offline: tool error.
    - Path not found on agent: tool error.
    - Permission denied: tool error.
    - Path is not a directory: tool error.
    - Agent does not have filesystem capability enabled: tool error.
```

#### 3.3.4 `fs_delete`

```
tool: fs_delete
  Title: Delete File or Directory
  Description: Delete a file or directory on the target device (optionally recursive).
  Input schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "clientId":  { "type": "string", "description": "Target device ID" },
        "path":      { "type": "string" },
        "recursive": { "type": "boolean", "description": "Required for non-empty directories (default: false)" }
      },
      "required": ["clientId", "path"]
    }
  Output schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "deleted":      { "type": "boolean" },
        "path":         { "type": "string" },
        "itemsRemoved": { "type": "integer", "description": "Number of files/dirs removed (recursive)" },
        "clientId":     { "type": "string" }
      },
      "required": ["deleted", "path", "clientId"]
    }
  Annotations:
    readOnlyHint: false
    destructiveHint: true
    idempotentHint: false
    openWorldHint: false
  Streaming behavior: none
  Long-running behavior: synchronous
  Multi-turn behavior:
    - Single-shot.
    - Always requires elicitation confirmation (cannot be skipped).
      Confirmation happens MCP-client-side before dispatch.
  Side effects / state touched: deletes from filesystem on agent. Audit log entry server-side.
  Error modes:
    - Target device offline: tool error.
    - Path not found on agent: tool error.
    - Permission denied: tool error.
    - Non-empty directory without recursive=true: tool error.
    - Agent does not have filesystem capability enabled: tool error.
```

#### 3.3.5 `fs_stat`

```
tool: fs_stat
  Title: File/Directory Stat
  Description: Get detailed metadata about a file or directory on the target device.
  Input schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "clientId":       { "type": "string", "description": "Target device ID" },
        "path":           { "type": "string" },
        "followSymlinks": { "type": "boolean", "description": "default: true" }
      },
      "required": ["clientId", "path"]
    }
  Output schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "name":       { "type": "string" },
        "path":       { "type": "string" },
        "type":       { "type": "string", "enum": ["file", "dir", "symlink", "other"] },
        "size":       { "type": "integer" },
        "mode":       { "type": "string" },
        "modTime":    { "type": "string", "format": "date-time" },
        "owner":      { "type": "string" },
        "group":      { "type": "string" },
        "linkTarget": { "type": "string", "description": "Symlink target, if applicable" },
        "clientId":   { "type": "string" }
      },
      "required": ["name", "path", "type", "size", "mode", "modTime", "clientId"]
    }
  Annotations:
    readOnlyHint: true
    destructiveHint: false
    idempotentHint: true
    openWorldHint: false
  Streaming behavior: none
  Long-running behavior: synchronous
  Multi-turn behavior: single-shot
  Side effects / state touched: none on agent. Audit log entry server-side.
  Error modes:
    - Target device offline: tool error.
    - Path not found: tool error.
    - Permission denied: tool error.
    - Agent does not have filesystem capability enabled: tool error.
```

---

### 3.4 Process Management

#### 3.4.1 `process_list`

```
tool: process_list
  Title: List Processes
  Description: List running processes on the target device with key metadata.
  Input schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "clientId": { "type": "string", "description": "Target device ID" },
        "filter":   { "type": "string", "description": "Filter by process name (substring match)" },
        "user":     { "type": "string", "description": "Filter by user" },
        "sortBy":   { "type": "string", "enum": ["pid", "cpu", "memory", "name"], "description": "default: pid" },
        "limit":    { "type": "integer", "description": "Max results (default: 100)", "minimum": 1 }
      },
      "required": ["clientId"]
    }
  Output schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "processes": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "pid":       { "type": "integer" },
              "ppid":      { "type": "integer" },
              "name":      { "type": "string" },
              "cmdline":   { "type": "string" },
              "user":      { "type": "string" },
              "cpuPct":    { "type": "number" },
              "memPct":    { "type": "number" },
              "memRssKB":  { "type": "integer" },
              "state":     { "type": "string" },
              "startTime": { "type": "string", "format": "date-time" }
            }
          }
        },
        "totalCount": { "type": "integer" },
        "clientId":   { "type": "string" }
      },
      "required": ["processes", "totalCount", "clientId"]
    }
  Annotations:
    readOnlyHint: true
    destructiveHint: false
    idempotentHint: true
    openWorldHint: false
  Streaming behavior: none
  Long-running behavior: synchronous
  Multi-turn behavior: single-shot
  Side effects / state touched: none on agent. Audit log entry server-side.
  Error modes:
    - Target device offline: tool error.
    - /proc unavailable on agent: tool error.
    - Agent does not have process capability enabled: tool error.
```

#### 3.4.2 `process_info`

```
tool: process_info
  Title: Get Process Info
  Description: Get detailed information about a specific process on the target device.
  Input schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "clientId": { "type": "string", "description": "Target device ID" },
        "pid":      { "type": "integer" }
      },
      "required": ["clientId", "pid"]
    }
  Output schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "pid":       { "type": "integer" },
        "ppid":      { "type": "integer" },
        "name":      { "type": "string" },
        "cmdline":   { "type": "string" },
        "exe":       { "type": "string" },
        "cwd":       { "type": "string" },
        "user":      { "type": "string" },
        "state":     { "type": "string" },
        "threads":   { "type": "integer" },
        "cpuPct":    { "type": "number" },
        "memPct":    { "type": "number" },
        "memRssKB":  { "type": "integer" },
        "memVmsKB":  { "type": "integer" },
        "startTime": { "type": "string", "format": "date-time" },
        "fds":       { "type": "integer", "description": "Open file descriptor count" },
        "environ":   { "type": "object", "additionalProperties": { "type": "string" }, "description": "Environment variables (may be empty if not readable)" },
        "clientId":  { "type": "string" }
      },
      "required": ["pid", "name", "state", "clientId"]
    }
  Annotations:
    readOnlyHint: true
    destructiveHint: false
    idempotentHint: true
    openWorldHint: false
  Streaming behavior: none
  Long-running behavior: synchronous
  Multi-turn behavior: single-shot
  Side effects / state touched: none on agent. Audit log entry server-side.
  Error modes:
    - Target device offline: tool error.
    - PID not found on agent: tool error.
    - Permission denied (reading /proc/<pid> on agent): tool error.
    - Agent does not have process capability enabled: tool error.
```

#### 3.4.3 `process_signal`

```
tool: process_signal
  Title: Send Signal to Process
  Description: Send a Unix signal to a process on the target device.
  Input schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "clientId": { "type": "string", "description": "Target device ID" },
        "pid":      { "type": "integer" },
        "signal":   { "type": "string", "enum": ["SIGTERM", "SIGKILL", "SIGHUP", "SIGINT", "SIGUSR1", "SIGUSR2", "SIGSTOP", "SIGCONT"], "description": "default: SIGTERM" }
      },
      "required": ["clientId", "pid"]
    }
  Output schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "signalSent": { "type": "boolean" },
        "pid":        { "type": "integer" },
        "signal":     { "type": "string" },
        "clientId":   { "type": "string" }
      },
      "required": ["signalSent", "pid", "signal", "clientId"]
    }
  Annotations:
    readOnlyHint: false
    destructiveHint: true
    idempotentHint: false
    openWorldHint: false
  Streaming behavior: none
  Long-running behavior: synchronous
  Multi-turn behavior:
    - Single-shot.
    - Requires elicitation confirmation (unless RC_PROCESS_SKIP_CONFIRM=true).
      Confirmation happens MCP-client-side before the server forwards dispatch.
  Side effects / state touched: sends signal to process on agent. Audit log entry server-side.
  Error modes:
    - Target device offline: tool error.
    - PID not found on agent: tool error.
    - Permission denied (EPERM) on agent: tool error.
    - Signaling the agent's own process (rc-mcp-agent): hard reject, tool error.
    - Agent does not have process capability enabled: tool error.
```

---

### 3.5 System Info

#### 3.5.1 `sysinfo_get`

```
tool: sysinfo_get
  Title: Get System Information
  Description: Get system overview (CPU, memory, disk, uptime, OS, hostname) from the target device.
  Input schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "clientId": { "type": "string", "description": "Target device ID" },
        "sections": {
          "type": "array",
          "items": { "type": "string", "enum": ["cpu", "memory", "disk", "network", "os", "uptime", "hostname", "all"] },
          "description": "Which sections to include (default: [\"all\"])"
        }
      },
      "required": ["clientId"]
    }
  Output schema (JSON Schema):
    {
      "type": "object",
      "properties": {
        "hostname":  { "type": "string" },
        "os": {
          "type": "object",
          "properties": {
            "name":    { "type": "string" },
            "version": { "type": "string" },
            "kernel":  { "type": "string" },
            "arch":    { "type": "string" }
          }
        },
        "uptime": {
          "type": "object",
          "properties": {
            "seconds": { "type": "integer" },
            "human":   { "type": "string" }
          }
        },
        "cpu": {
          "type": "object",
          "properties": {
            "model":      { "type": "string" },
            "cores":      { "type": "integer" },
            "threads":    { "type": "integer" },
            "usagePct":   { "type": "number" },
            "loadAvg1":   { "type": "number" },
            "loadAvg5":   { "type": "number" },
            "loadAvg15":  { "type": "number" }
          }
        },
        "memory": {
          "type": "object",
          "properties": {
            "totalKB":     { "type": "integer" },
            "usedKB":      { "type": "integer" },
            "availableKB": { "type": "integer" },
            "usagePct":    { "type": "number" },
            "swapTotalKB": { "type": "integer" },
            "swapUsedKB":  { "type": "integer" }
          }
        },
        "disk": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "mount":       { "type": "string" },
              "device":      { "type": "string" },
              "fsType":      { "type": "string" },
              "totalKB":     { "type": "integer" },
              "usedKB":      { "type": "integer" },
              "availableKB": { "type": "integer" },
              "usagePct":    { "type": "number" }
            }
          }
        },
        "network": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "name":  { "type": "string" },
              "ipv4":  { "type": "string" },
              "ipv6":  { "type": "string" },
              "mac":   { "type": "string" },
              "state": { "type": "string" }
            }
          }
        },
        "clientId": { "type": "string" }
      }
    }
  Annotations:
    readOnlyHint: true
    destructiveHint: false
    idempotentHint: true
    openWorldHint: false
  Streaming behavior: none
  Long-running behavior: synchronous
  Multi-turn behavior: single-shot
  Side effects / state touched: none on agent. Audit log entry server-side.
  Error modes:
    - Target device offline: tool error.
    - Partial data available on agent (e.g. /proc not mounted): returns partial
      result with null sections, not an error.
    - Agent does not have sysinfo capability enabled: tool error.
```

---

## 4. Resource Inventory

### 4.1 `clients://list`

```
URI template: clients://list
MIME type: application/json
Subscribe: yes (notifications/resources/updated on device connect/disconnect/capability change)
Paginated: no
Description: Read-only list of all paired devices in the device registry.
  Includes device ID, human label/hostname, online/offline status, enabled
  capabilities, last-seen timestamp. This is a status surface for the LLM,
  NOT a control surface -- the LLM cannot pair, approve, or revoke devices
  through any MCP tool or resource.
Schema:
  {
    "type": "object",
    "properties": {
      "clients": {
        "type": "array",
        "items": {
          "type": "object",
          "properties": {
            "clientId":     { "type": "string" },
            "label":        { "type": "string", "description": "Human-readable label or hostname" },
            "online":       { "type": "boolean" },
            "capabilities": { "type": "array", "items": { "type": "string" } },
            "lastSeen":     { "type": "string", "format": "date-time" },
            "pairedAt":     { "type": "string", "format": "date-time" }
          }
        }
      }
    }
  }
```

### 4.2 `job://{id}`

```
URI template: job://{id}
MIME type: application/json
Subscribe: yes (resources/subscribe + notifications/resources/updated)
Paginated: no
Description: Status and result of a long-running job (e.g. screenshot_watch).
  Clients that miss the terminal progress notification can fetch the outcome
  here.
Schema: see Job type (Section 6).
```

### 4.3 `sysinfo://{clientId}/overview`

```
URI template: sysinfo://{clientId}/overview
MIME type: application/json
Subscribe: yes (updated every 30s while subscribed, if the target device is online)
Paginated: no
Description: Live system overview (CPU/mem/disk/load) from a specific device.
  Read-only snapshot. Returns an error if the device is offline.
```

### 4.4 `audit://log`

```
URI template: audit://log?limit={limit}&offset={offset}
MIME type: application/json
Subscribe: yes (notifications/resources/updated on every new entry)
Paginated: yes (cursor-based via offset parameter)
Description: Server-side append-only audit log. Read-only. Cannot be deleted
  via any exposed tool. Returns entries newest-first. Each entry includes the
  MCP session ID, target device ID, tool name, args digest, and result status.
```

### 4.5 `shell://sessions`

```
URI template: shell://sessions
MIME type: application/json
Subscribe: yes (updated on session open/close)
Paginated: no
Description: List of active interactive shell sessions in the current MCP
  session, with their IDs, target device IDs, PIDs, and creation times.
Schema:
  {
    "type": "object",
    "properties": {
      "sessions": {
        "type": "array",
        "items": {
          "type": "object",
          "properties": {
            "shellSessionId": { "type": "string" },
            "clientId":       { "type": "string" },
            "pid":            { "type": "integer" },
            "shell":          { "type": "string" },
            "createdAt":      { "type": "string", "format": "date-time" }
          },
          "required": ["shellSessionId", "clientId", "pid", "shell", "createdAt"]
        }
      }
    }
  }
```

---

## 5. Prompt Inventory

### 5.1 `diagnose_system`

```
Name: diagnose_system
Description: Guided system diagnostics workflow for a specific device.
  Gathers system info, checks for high resource usage, suggests investigation
  steps.
Arguments:
  - clientId: string (required) -- target device
  - symptom: string (required) -- e.g. "high cpu", "disk full", "network slow"
Template: dynamic -- built from session state (current sysinfo snapshot from
  the target device) and the symptom argument. Emits a multi-step prompt that
  instructs the LLM to call sysinfo_get, process_list (sorted by the relevant
  metric), and suggest remediation -- all targeting the specified clientId.
```

### 5.2 `safe_cleanup`

```
Name: safe_cleanup
Description: Guided cleanup workflow for a specific device. Identifies
  large/old files, orphaned processes, and temp directories, then proposes
  deletions with confirmation.
Arguments:
  - clientId: string (required) -- target device
  - target: string (optional) -- "disk", "processes", or "all" (default: "all")
  - minSizeMB: integer (optional) -- minimum file size to flag (default: 100)
Template: dynamic -- calls fs_list and process_list targeting the device,
  filters, and presents a confirmation checklist before any deletions.
```

### 5.3 `shell_workflow`

```
Name: shell_workflow
Description: Start an interactive shell session on a specific device with
  context about what the user wants to accomplish.
Arguments:
  - clientId: string (required) -- target device
  - task: string (required) -- natural language description of the task
Template: static preamble + dynamic. Sets up a shell_session_start call
  targeting the specified clientId and provides the LLM with guidelines
  for multi-turn shell interaction.
```

---

## 6. Go Type Definitions

### Wire protocol envelope and binary header (package `internal/protocol`)

```go
package protocol

import (
	"encoding/binary"
	"time"
)

// Version is the current wire protocol version.
const Version = "1"

// MessageType enumerates all JSON envelope message types.
type MessageType string

const (
	MsgHello        MessageType = "hello"
	MsgHelloAck     MessageType = "hello_ack"
	MsgPairRequest  MessageType = "pair_request"
	MsgPairCode     MessageType = "pair_code"
	MsgPairApproved MessageType = "pair_approved"
	MsgDispatch     MessageType = "dispatch"
	MsgResult       MessageType = "result"
	MsgProgress     MessageType = "progress"
	MsgError        MessageType = "error"
	MsgCancel       MessageType = "cancel"
	MsgPing         MessageType = "ping"
	MsgPong         MessageType = "pong"
	MsgClose        MessageType = "close"
)

// Envelope is the JSON envelope for all text WebSocket frames.
type Envelope struct {
	Type            MessageType `json:"type"`
	ID              string      `json:"id,omitempty"` // correlation ID (UUIDv4)
	ProtocolVersion string      `json:"protocolVersion,omitempty"`
	Ts              time.Time   `json:"ts"`
	Payload         any         `json:"payload,omitempty"` // type-specific; see per-type payloads below
}

// --- Per-type payloads ---

// HelloPayload is sent by the agent on initial connection.
type HelloPayload struct {
	DeviceToken  string   `json:"deviceToken"`
	Hostname     string   `json:"hostname"`
	Capabilities []string `json:"capabilities"` // e.g. ["shell","screenshot","filesystem","process","sysinfo"]
}

// HelloAckPayload is sent by the server to confirm authentication.
type HelloAckPayload struct {
	DeviceID string `json:"deviceId"`
	Resume   bool   `json:"resume"` // true if reconnecting within grace period
}

// PairRequestPayload is sent by an unpaired agent.
type PairRequestPayload struct {
	Hostname string `json:"hostname"`
}

// PairCodePayload is sent by the server with the pairing code.
type PairCodePayload struct {
	Code      string    `json:"code"`      // human-readable, e.g. "ABCD-1234"
	ExpiresAt time.Time `json:"expiresAt"` // code expiry (e.g. 5 minutes from now)
}

// PairApprovedPayload is sent by the server after operator approval.
type PairApprovedPayload struct {
	DeviceID    string `json:"deviceId"`
	DeviceToken string `json:"deviceToken"` // persistent bearer token for future connections
}

// DispatchPayload is sent by the server to dispatch a tool call to the agent.
type DispatchPayload struct {
	Tool      string `json:"tool"`
	RequestID string `json:"requestId"` // MCP JSON-RPC request ID for correlation
	SessionID string `json:"sessionId"` // MCP session ID (for server-side correlation, not agent use)
	Input     any    `json:"input"`     // tool-specific input, matches the tool's input schema
}

// ResultPayload is sent by the agent with the terminal result of a dispatch.
type ResultPayload struct {
	Tool    string `json:"tool"`
	Output  any    `json:"output"`          // tool-specific output, matches the tool's output schema
	IsError bool   `json:"isError"`         // true if the tool execution failed
	Error   string `json:"error,omitempty"` // error message if isError
}

// ProgressPayload is sent by the agent for streaming updates.
type ProgressPayload struct {
	Tool    string   `json:"tool"`
	Percent *float64 `json:"percent,omitempty"` // 0.0-100.0
	Message string   `json:"message,omitempty"`
}

// ErrorPayload is used for error messages.
type ErrorPayload struct {
	Code    string `json:"code"`    // e.g. "version_mismatch", "auth_failed", "device_not_found"
	Message string `json:"message"`
}

// CancelPayload is sent by the server to cancel an in-flight dispatch.
type CancelPayload struct {
	Reason string `json:"reason,omitempty"` // e.g. "client_cancelled", "session_expired"
}

// ClosePayload is sent by the server to gracefully close a stream or connection.
type ClosePayload struct {
	Reason string `json:"reason,omitempty"` // e.g. "session_terminated", "server_shutdown"
}

// --- Binary frame header ---

// BinaryFrameType identifies the content of a binary WebSocket frame.
type BinaryFrameType byte

const (
	FrameShellStdout   BinaryFrameType = 0x01
	FrameShellStdin    BinaryFrameType = 0x02
	FrameScreenshotPNG BinaryFrameType = 0x03
	FrameFileContent   BinaryFrameType = 0x04
)

// BinaryHeader is the fixed 9-byte header for binary WebSocket frames.
// Layout: [4 bytes correlation ID prefix] [4 bytes stream sequence] [1 byte frame type]
type BinaryHeader struct {
	CorrelationPrefix [4]byte         // first 4 bytes of the correlation UUID
	StreamSeq         uint32          // monotonically increasing per correlation ID
	FrameType         BinaryFrameType // content type
}

// BinaryHeaderSize is the fixed size of the binary frame header.
const BinaryHeaderSize = 9

// EncodeBinaryHeader writes the header into the first 9 bytes of buf.
func EncodeBinaryHeader(buf []byte, h BinaryHeader) {
	copy(buf[0:4], h.CorrelationPrefix[:])
	binary.BigEndian.PutUint32(buf[4:8], h.StreamSeq)
	buf[8] = byte(h.FrameType)
}

// DecodeBinaryHeader reads the header from the first 9 bytes of buf.
func DecodeBinaryHeader(buf []byte) BinaryHeader {
	var h BinaryHeader
	copy(h.CorrelationPrefix[:], buf[0:4])
	h.StreamSeq = binary.BigEndian.Uint32(buf[4:8])
	h.FrameType = BinaryFrameType(buf[8])
	return h
}
```

### Device registry types (package `internal/devices`)

```go
package devices

import (
	"context"
	"time"
)

// Device represents a paired desktop agent in the registry.
type Device struct {
	ID           string    `json:"clientId"`     // server-assigned UUIDv4; serialized as clientId for consistency with every tool's target-device field
	Label        string    `json:"label"`         // human-readable (typically hostname)
	TokenHash    string    `json:"-"`             // bcrypt hash of the device token; never serialized
	Online       bool      `json:"online"`
	Capabilities []string  `json:"capabilities"`  // enabled capability areas
	LastSeen     time.Time `json:"lastSeen"`
	PairedAt     time.Time `json:"pairedAt"`
}

// PairingCode represents a pending pairing request.
type PairingCode struct {
	Code      string    `json:"code"`      // human-readable, e.g. "ABCD-1234"
	Hostname  string    `json:"hostname"`  // agent-reported hostname
	ExpiresAt time.Time `json:"expiresAt"`
	Used      bool      `json:"used"`      // single-use; true after approval or rejection
}

// DeviceRegistry is the interface for managing paired devices.
type DeviceRegistry interface {
	// Pair creates a new pending pairing code.
	CreatePairingCode(ctx context.Context, hostname string) (*PairingCode, error)
	// ApprovePairing approves a pairing code and registers the device.
	// Returns the new Device and a raw (unhashed) device token.
	ApprovePairing(ctx context.Context, code string) (*Device, string, error)
	// RejectPairing rejects (invalidates) a pairing code.
	RejectPairing(ctx context.Context, code string) error
	// Authenticate validates a device token and returns the device.
	Authenticate(ctx context.Context, token string) (*Device, error)
	// Get returns a device by ID.
	Get(ctx context.Context, id string) (*Device, error)
	// List returns all paired devices.
	List(ctx context.Context) ([]*Device, error)
	// SetOnline marks a device as online/offline and updates LastSeen.
	SetOnline(ctx context.Context, id string, online bool) error
	// UpdateCapabilities updates the capability list for a device.
	UpdateCapabilities(ctx context.Context, id string, caps []string) error
	// Revoke removes a device from the registry (operator-initiated).
	Revoke(ctx context.Context, id string) error
}
```

### Core MCP tool input/output types (package `internal/mcp/types`)

```go
package types

import (
	"time"
)

// --- Shell ---

type ShellExecInput struct {
	ClientID string            `json:"clientId"`
	Command  string            `json:"command"`
	Cwd      *string           `json:"cwd,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Timeout  *int              `json:"timeout,omitempty"` // seconds
	Stdin    *string           `json:"stdin,omitempty"`
}

type ShellExecOutput struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exitCode"`
	Killed     bool   `json:"killed"`
	DurationMs int64  `json:"durationMs"`
	ClientID   string `json:"clientId"`
}

type ShellSessionStartInput struct {
	ClientID string            `json:"clientId"`
	Shell    *string           `json:"shell,omitempty"`
	Cwd      *string           `json:"cwd,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Rows     *int              `json:"rows,omitempty"`
	Cols     *int              `json:"cols,omitempty"`
}

type ShellSessionStartOutput struct {
	ShellSessionID string `json:"shellSessionId"`
	PID            int    `json:"pid"`
	Shell          string `json:"shell"`
	ClientID       string `json:"clientId"`
}

type ShellSessionWriteInput struct {
	ShellSessionID string `json:"shellSessionId"`
	Input          string `json:"input"`
}

type ShellSessionWriteOutput struct {
	BytesWritten int    `json:"bytesWritten"`
	Output       string `json:"output,omitempty"`   // accumulated output after idle
	ExitCode     *int   `json:"exitCode,omitempty"` // set if shell exited
	Exited       bool   `json:"exited"`
}

type ShellSessionCloseInput struct {
	ShellSessionID string  `json:"shellSessionId"`
	Signal         *string `json:"signal,omitempty"` // SIGTERM or SIGKILL
}

type ShellSessionCloseOutput struct {
	ExitCode    int    `json:"exitCode"`
	FinalOutput string `json:"finalOutput,omitempty"`
}

// --- Screenshots ---

type ScreenshotCaptureInput struct {
	ClientID string  `json:"clientId"`
	Display  *string `json:"display,omitempty"`
	Monitor  *int    `json:"monitor,omitempty"`
	Quality  *int    `json:"quality,omitempty"`
	MaxWidth *int    `json:"maxWidth,omitempty"`
}

// Output is MCP image content, not a Go struct.

type ScreenshotWatchInput struct {
	ClientID     string  `json:"clientId"`
	Display      *string `json:"display,omitempty"`
	Monitor      *int    `json:"monitor,omitempty"`
	IntervalMs   *int    `json:"intervalMs,omitempty"`
	MaxFrames    *int    `json:"maxFrames,omitempty"`
	DurationSecs *int    `json:"durationSecs,omitempty"`
	MaxWidth     *int    `json:"maxWidth,omitempty"`
	Quality      *int    `json:"quality,omitempty"`
}

type ScreenshotWatchOutput struct {
	JobID          string `json:"jobId"`
	FramesCaptured int    `json:"framesCaptured,omitempty"`
	DurationMs     int64  `json:"durationMs,omitempty"`
	StoppedReason  string `json:"stoppedReason,omitempty"`
	ClientID       string `json:"clientId"`
}

// --- Filesystem ---

type FsReadInput struct {
	ClientID string  `json:"clientId"`
	Path     string  `json:"path"`
	Offset   *int64  `json:"offset,omitempty"`
	Limit    *int64  `json:"limit,omitempty"`
	Encoding *string `json:"encoding,omitempty"`
}

type FsReadOutput struct {
	Content   string `json:"content"`
	Encoding  string `json:"encoding"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	ClientID  string `json:"clientId"`
}

type FsWriteInput struct {
	ClientID   string  `json:"clientId"`
	Path       string  `json:"path"`
	Content    string  `json:"content"`
	Encoding   *string `json:"encoding,omitempty"`
	Mode       *string `json:"mode,omitempty"` // overwrite | append
	FileMode   *string `json:"fileMode,omitempty"`
	CreateDirs *bool   `json:"createDirs,omitempty"`
}

type FsWriteOutput struct {
	BytesWritten int    `json:"bytesWritten"`
	Path         string `json:"path"`
	ClientID     string `json:"clientId"`
}

type FsListInput struct {
	ClientID   string `json:"clientId"`
	Path       string `json:"path"`
	Recursive  *bool  `json:"recursive,omitempty"`
	MaxDepth   *int   `json:"maxDepth,omitempty"`
	ShowHidden *bool  `json:"showHidden,omitempty"`
	Limit      *int   `json:"limit,omitempty"`
}

type FsEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Type    string    `json:"type"` // file, dir, symlink, other
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"modTime"`
}

type FsListOutput struct {
	Entries    []FsEntry `json:"entries"`
	Truncated  bool      `json:"truncated"`
	TotalCount int       `json:"totalCount,omitempty"`
	ClientID   string    `json:"clientId"`
}

type FsDeleteInput struct {
	ClientID  string `json:"clientId"`
	Path      string `json:"path"`
	Recursive *bool  `json:"recursive,omitempty"`
}

type FsDeleteOutput struct {
	Deleted      bool   `json:"deleted"`
	Path         string `json:"path"`
	ItemsRemoved int    `json:"itemsRemoved,omitempty"`
	ClientID     string `json:"clientId"`
}

type FsStatInput struct {
	ClientID       string `json:"clientId"`
	Path           string `json:"path"`
	FollowSymlinks *bool  `json:"followSymlinks,omitempty"`
}

type FsStatOutput struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	Mode       string    `json:"mode"`
	ModTime    time.Time `json:"modTime"`
	Owner      string    `json:"owner,omitempty"`
	Group      string    `json:"group,omitempty"`
	LinkTarget string    `json:"linkTarget,omitempty"`
	ClientID   string    `json:"clientId"`
}

// --- Process ---

type ProcessListInput struct {
	ClientID string  `json:"clientId"`
	Filter   *string `json:"filter,omitempty"`
	User     *string `json:"user,omitempty"`
	SortBy   *string `json:"sortBy,omitempty"`
	Limit    *int    `json:"limit,omitempty"`
}

type ProcessEntry struct {
	PID       int       `json:"pid"`
	PPID      int       `json:"ppid"`
	Name      string    `json:"name"`
	Cmdline   string    `json:"cmdline"`
	User      string    `json:"user"`
	CPUPct    float64   `json:"cpuPct"`
	MemPct    float64   `json:"memPct"`
	MemRssKB  int64     `json:"memRssKB"`
	State     string    `json:"state"`
	StartTime time.Time `json:"startTime"`
}

type ProcessListOutput struct {
	Processes  []ProcessEntry `json:"processes"`
	TotalCount int            `json:"totalCount"`
	ClientID   string         `json:"clientId"`
}

type ProcessInfoInput struct {
	ClientID string `json:"clientId"`
	PID      int    `json:"pid"`
}

type ProcessInfoOutput struct {
	PID       int               `json:"pid"`
	PPID      int               `json:"ppid,omitempty"`
	Name      string            `json:"name"`
	Cmdline   string            `json:"cmdline,omitempty"`
	Exe       string            `json:"exe,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	User      string            `json:"user,omitempty"`
	State     string            `json:"state"`
	Threads   int               `json:"threads,omitempty"`
	CPUPct    float64           `json:"cpuPct,omitempty"`
	MemPct    float64           `json:"memPct,omitempty"`
	MemRssKB  int64             `json:"memRssKB,omitempty"`
	MemVmsKB  int64             `json:"memVmsKB,omitempty"`
	StartTime *time.Time        `json:"startTime,omitempty"`
	FDs       int               `json:"fds,omitempty"`
	Environ   map[string]string `json:"environ,omitempty"`
	ClientID  string            `json:"clientId"`
}

type ProcessSignalInput struct {
	ClientID string  `json:"clientId"`
	PID      int     `json:"pid"`
	Signal   *string `json:"signal,omitempty"`
}

type ProcessSignalOutput struct {
	SignalSent bool   `json:"signalSent"`
	PID        int    `json:"pid"`
	Signal     string `json:"signal"`
	ClientID   string `json:"clientId"`
}

// --- System Info ---

type SysinfoGetInput struct {
	ClientID string   `json:"clientId"`
	Sections []string `json:"sections,omitempty"`
}

type SysinfoOS struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Kernel  string `json:"kernel"`
	Arch    string `json:"arch"`
}

type SysinfoUptime struct {
	Seconds int    `json:"seconds"`
	Human   string `json:"human"`
}

type SysinfoCPU struct {
	Model     string  `json:"model"`
	Cores     int     `json:"cores"`
	Threads   int     `json:"threads"`
	UsagePct  float64 `json:"usagePct"`
	LoadAvg1  float64 `json:"loadAvg1"`
	LoadAvg5  float64 `json:"loadAvg5"`
	LoadAvg15 float64 `json:"loadAvg15"`
}

type SysinfoMemory struct {
	TotalKB     int64   `json:"totalKB"`
	UsedKB      int64   `json:"usedKB"`
	AvailableKB int64   `json:"availableKB"`
	UsagePct    float64 `json:"usagePct"`
	SwapTotalKB int64   `json:"swapTotalKB"`
	SwapUsedKB  int64   `json:"swapUsedKB"`
}

type SysinfoDisk struct {
	Mount       string  `json:"mount"`
	Device      string  `json:"device"`
	FsType      string  `json:"fsType"`
	TotalKB     int64   `json:"totalKB"`
	UsedKB      int64   `json:"usedKB"`
	AvailableKB int64   `json:"availableKB"`
	UsagePct    float64 `json:"usagePct"`
}

type SysinfoNetwork struct {
	Name  string `json:"name"`
	IPv4  string `json:"ipv4"`
	IPv6  string `json:"ipv6"`
	MAC   string `json:"mac"`
	State string `json:"state"`
}

type SysinfoGetOutput struct {
	Hostname string           `json:"hostname,omitempty"`
	OS       *SysinfoOS       `json:"os,omitempty"`
	Uptime   *SysinfoUptime   `json:"uptime,omitempty"`
	CPU      *SysinfoCPU      `json:"cpu,omitempty"`
	Memory   *SysinfoMemory   `json:"memory,omitempty"`
	Disk     []SysinfoDisk    `json:"disk,omitempty"`
	Network  []SysinfoNetwork `json:"network,omitempty"`
	ClientID string           `json:"clientId,omitempty"`
}
```

### Job and progress types (package `internal/jobs`)

```go
package jobs

import (
	"encoding/json"
	"time"
)

// JobStatus represents the lifecycle state of a long-running job.
type JobStatus string

const (
	JobStatusPending   JobStatus = "pending"
	JobStatusRunning   JobStatus = "running"
	JobStatusSucceeded JobStatus = "succeeded"
	JobStatusFailed    JobStatus = "failed"
	JobStatusCancelled JobStatus = "cancelled"
)

// Job is the envelope for long-running operations bridged through the server.
type Job struct {
	ID             string          `json:"id"`
	SessionID      string          `json:"sessionId"`      // MCP session that initiated the job
	ClientID       string          `json:"clientId"`       // target device
	Tool           string          `json:"tool"`
	Status         JobStatus       `json:"status"`
	Payload        json.RawMessage `json:"payload"`
	Result         json.RawMessage `json:"result,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey"`
	ProgressToken  string          `json:"progressToken,omitempty"`
	CorrelationID  string          `json:"correlationId"` // wire protocol correlation ID
	TimeoutSecs    int             `json:"timeoutSecs"`
	Error          string          `json:"error,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
	CompletedAt    *time.Time      `json:"completedAt,omitempty"`
}

// ProgressEvent is received from the agent and bridged to the MCP client.
type ProgressEvent struct {
	JobID         string          `json:"jobId"`
	SessionID     string          `json:"sessionId"`
	ProgressToken string          `json:"progressToken"`
	Percent       *float64        `json:"percent,omitempty"`
	Message       string          `json:"message"`
	Data          json.RawMessage `json:"data,omitempty"` // e.g. base64 screenshot frame
	Terminal      bool            `json:"terminal"`       // true = this is the final event
}

// JobStore persists job state for retrieval after completion.
type JobStore interface {
	Create(job *Job) error
	Get(id string) (*Job, error)
	UpdateStatus(id string, status JobStatus, result json.RawMessage, errMsg string) error
	ListBySession(sessionID string, limit int) ([]*Job, error)
}
```

### Audit log types (package `internal/audit`)

```go
package audit

import "time"

// Entry is a single append-only audit log record.
type Entry struct {
	Timestamp  time.Time `json:"timestamp"`
	SessionID  string    `json:"sessionId"`  // MCP session
	ClientID   string    `json:"clientId"`   // target device
	Tool       string    `json:"tool"`
	ArgsDigest string    `json:"argsDigest"` // SHA-256 of the JSON-encoded args
	ArgsHint   string    `json:"argsHint"`   // sanitized summary, e.g. "command=ls -la"
	Status     string    `json:"status"`     // ok, error, cancelled
	DurationMs int64     `json:"durationMs"`
	Error      string    `json:"error,omitempty"`
}
```

---

## 7. Session & Multi-Turn State Management

### Important distinction: MCP sessions vs. device connections

An **MCP session** is a logical conversation between an LLM client (e.g.
Claude Desktop) and the rc-mcp-server. It is identified by `Mcp-Session-Id`,
authenticated via a bearer token, and carries session state (negotiated
capabilities, shell session mappings, SSE replay buffer).

A **device connection** is a persistent WebSocket between a desktop agent and
the rc-mcp-server. It is identified by the device ID, authenticated via the
device token from the pairing flow, and carries the agent's capability set
and liveness state.

These are **two independent, differently-authenticated things** that the
server correlates per tool call:

```
MCP Client <-- Mcp-Session-Id + bearer token --> rc-mcp-server <-- device token + WS --> Agent
                                                        |
                                            correlates clientId from
                                            tool input to the correct
                                            agent WS connection
```

A single MCP session can dispatch tool calls to multiple different agents
(if the operator has multiple machines). A single agent can serve tool calls
from multiple concurrent MCP sessions.

### Session store interface

```go
package session

import (
	"context"
	"time"
)

type Session struct {
	ID                string
	CreatedAt         time.Time
	LastActivityAt    time.Time
	NegotiatedVersion string
	ClientInfo        ClientInfo
	ShellSessionMap   map[string]*ShellSessionEntry // shellSessionId -> session metadata
	ReplayBuffer      *ReplayBuffer
	EventCh           chan SSEEvent // fan-in channel for the SSE writer
	CancelFunc        context.CancelFunc
}

type ClientInfo struct {
	Name    string
	Version string
}

// ShellSessionEntry tracks server-side metadata for an active interactive
// shell session. The actual PTY state lives on the agent; the server caches
// enough to serve the shell://sessions resource and route dispatches.
type ShellSessionEntry struct {
	ClientID  string    // target device ID
	PID       int       // shell process PID (cached from agent's start result)
	Shell     string    // shell binary (cached from agent's start result)
	CreatedAt time.Time // when the session was started
}

type SessionStore interface {
	Create(ctx context.Context) (*Session, error)
	Get(ctx context.Context, id string) (*Session, error)
	Touch(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
}
```

### Backing implementations

| Implementation | When to use |
|---|---|
| `MemoryStore` (in-process `sync.Map` + mutex for shell session map) | Single-instance deployments (the expected default). No external dependencies. |
| `RedisStore` | Only if multi-replica scaling is needed (see Open Questions). Session data serialized to Redis with TTL matching idle timeout. The shell session map can be in Redis, but the SSE writer and event channel are process-local -- a load balancer must use session affinity (sticky sessions by `Mcp-Session-Id`). |

### What lives in session state

- **Negotiated capabilities** -- the intersection of client and server caps
  from `initialize`.
- **Shell session mapping** -- map of `shellSessionId` to `clientId` (device
  ID). The actual PTY state lives on the agent, not the server. The server
  only needs to know which agent owns which shell session for routing.
- **SSE replay buffer** -- circular buffer of the last N SSE events, used for
  `Last-Event-ID` replay.
- **Event channel** -- a bounded Go channel (`EventCh`) into which the
  dispatch bridge goroutines fan their SSE events. The SSE writer goroutine
  drains this channel.
- **Pending elicitation requests** -- map of pending elicitation IDs awaiting
  client response, with timeout timers.

### Expiry policy

- A background goroutine runs every 60 seconds, iterating over sessions.
- Sessions with `LastActivityAt` older than `MCP_SESSION_IDLE_TIMEOUT`
  (default: 30 minutes) are expired: in-flight dispatches are cancelled
  (cancel messages sent to agents for any session-scoped streams), SSE stream
  closed, and the session is removed from the store.
- `Touch()` is called on every valid `POST` or `GET` request for a session.

### Multi-turn conversation model (relay architecture)

```
Client                          Server                           Agent
  |                               |                                |
  +-- POST initialize ----------->|                                |
  |<--------- 200 + Session-Id ---+                                |
  |                               |                                |
  +-- GET /mcp (SSE stream) ----->|                                |
  |                               |                                |
  +-- POST tools/call             |                                |
  |   shell_session_start ------->| (elicitation check)            |
  |                               |                                |
  |<-- SSE: elicitation/create ---+ ("Start shell on device X?")   |
  |                               |                                |
  +-- POST elicitation (yes) ---->|                                |
  |                               +-- WS dispatch (shell_start) -->|
  |                               |<-- WS result (session info) ---+
  |<-- SSE: tools/call result ----+ (shellSessionId, pid, clientId)|
  |                               |                                |
  +-- POST tools/call             |                                |
  |   shell_session_write ------->| (resolves clientId from map)   |
  |   progressToken: "tok1"      +-- WS dispatch (shell_write) -->|
  |                               |<-- WS binary stdout frames ----+
  |<-- SSE: progress (tok1) ------+ (bridged stdout chunks)        |
  |<-- SSE: progress (tok1) ------+                                |
  |                               |<-- WS result (idle output) ----+
  |<-- SSE: tools/call result ----+                                |
  |                               |                                |
  +-- POST tools/call             |                                |
  |   shell_session_close ------->+-- WS dispatch (shell_close) -->|
  |                               |<-- WS result (exitCode) -------+
  |<-- SSE: tools/call result ----+                                |
  |                               |                                |
  +-- DELETE /mcp --------------->| (cleanup, close WS streams     |
  |<-- 204 -----------------------+  scoped to this session)       |
```

---

## 8. Streaming & Concurrency Implementation

### Goroutine model

The server uses the following goroutines:

1. **SSE writer** (1 per MCP session) -- reads from `Session.EventCh` and
   writes SSE frames to the HTTP response. Assigns monotonically increasing
   `id:` fields and appends each event to the replay buffer.
2. **Dispatch bridge** (1 per in-flight `tools/call`) -- spawned on each
   `tools/call` that needs to reach an agent. Sends the `dispatch` message
   over the agent's WebSocket, waits for `progress` and `result` messages
   correlated by ID, and writes them to `Session.EventCh` as MCP
   notifications/progress and the final tools/call result.
3. **Agent reader** (1 per connected agent) -- reads from the agent's
   WebSocket in a loop, demuxes JSON and binary frames by correlation ID,
   and routes them to the appropriate dispatch bridge goroutine's channel.
4. **Agent writer** (1 per connected agent) -- serializes outbound messages
   (dispatches, cancels, pings) to the agent's WebSocket. All writes to a
   single WebSocket go through this goroutine to avoid concurrent write
   issues.
5. **Heartbeat** (1 per connected agent) -- sends periodic pings, monitors
   pong responses, marks device offline on timeout.

### Context cancellation propagation

```
Client disconnects (SSE stream or POST drops)
  +-- net/http detects connection close
       +-- request context.Context is cancelled
            +-- dispatch bridge's ctx.Done() fires
                 +-- bridge sends cancel message to agent via WS
                 +-- for long-running jobs (pattern a): the job continues
                     on the agent; only the SSE forwarding stops. On
                     reconnect, replay buffer catches up.
                 +-- for inline dispatches (pattern b): cancel is forwarded
                     to the agent, which aborts the operation.
```

For session-scoped cancellation (DELETE or idle expiry), `Session.CancelFunc`
is called, which cascades to all dispatch bridges. The server sends `close`
messages to agents for any streams scoped to that session.

### Backpressure

- `Session.EventCh` is a buffered channel of size 256.
- If the channel is full (slow MCP client), the dispatch bridge blocks for
  up to 5 seconds, then drops the event and logs a warning.
- The SSE writer detects client disconnect via `context.Done` and stops
  draining, allowing the channel to fill and trigger backpressure.
- Agent-side: the agent's local ring buffers for PTY output (64KB) ensure
  that if progress events are dropped on the server side, the next successful
  delivery coalesces the buffered output.

### Graceful shutdown

On `SIGTERM` or `SIGINT`:

1. Stop accepting new HTTP connections and new WebSocket upgrades.
2. For each active MCP session:
   a. Send a final SSE comment `; server shutting down` on the stream.
   b. Cancel all in-flight dispatch bridges via context cancellation.
3. For each connected agent:
   a. Send a `close` message with reason `"server_shutdown"`.
   b. Close the WebSocket connection with code 1001 (going away).
4. Flush the audit log to disk.
5. Exit with code 0.

Shutdown timeout: 30 seconds. If not drained by then, force-exit.

---

## 9. Long-Running Tasks & Progress Push

### Two-hop streaming model

All long-running progress follows a two-hop path:

```
Agent (executor)                    Server (relay)                MCP Client
  |                                    |                              |
  |-- WS: progress (correlation) ----->|                              |
  |   (JSON or binary frame)          |-- SSE: notifications/progress -->|
  |                                    |   (keyed by progressToken)   |
  |-- WS: result (correlation) ------->|                              |
  |                                    |-- SSE: tools/call result ------>|
```

The server does **not** execute any tool logic. It bridges agent-originated
progress events to the correct MCP session's SSE stream, matching on the
correlation ID established when the dispatch was sent.

### Tools flagged as long-running

| Tool | Dispatch pattern | Max duration | Streaming |
|---|---|---|---|
| `shell_exec` | (b) hold open, two-hop stream | 300s | stdout/stderr binary frames from agent, bridged to progress notifications |
| `shell_session_write` | (b) hold open, two-hop stream | 30s idle timeout | PTY output binary frames from agent, bridged to progress notifications |
| `screenshot_watch` | (a) jobId + push notifications | 300s | PNG binary frames from agent, bridged to progress notifications |

### Dispatch pattern (b) -- shell_exec, shell_session_write

The JSON-RPC response for the `tools/call` is held open. The server dispatches
to the agent over WebSocket and bridges `progress` messages (both JSON and
binary stdout frames) as `notifications/progress` on the MCP SSE stream using
the request's `progressToken`. The final `result` message from the agent
becomes the `tools/call` result.

If the MCP client disconnects:
- For `shell_exec`: the server sends `cancel` to the agent, which kills the
  command. Partial output is buffered in the SSE replay buffer.
- For `shell_session_write`: the shell session stays alive on the agent (it is
  session-scoped, not request-scoped). Output continues to be buffered by the
  agent. On MCP client reconnect, the replay buffer catches up.

### Dispatch pattern (a) -- screenshot_watch

```
tools/call received
  -> validate input
  -> generate jobId (UUIDv4)
  -> create Job record in job store (status: pending)
  -> send dispatch to agent over WS (correlation ID = jobId)
  -> agent acknowledges dispatch
  -> return immediately: { "jobId": "<id>", "clientId": "<id>" } as tools/call result
  -> agent captures frames at configured interval, sends binary PNG frames
  -> server receives each frame, converts to notifications/progress
     (keyed by progressToken), writes to Session.EventCh
  -> agent sends terminal result message
  -> server updates job store to "succeeded", writes terminal progress event
```

If the MCP client is not listening when the terminal event fires, the job
result is persisted in the job store and accessible via `job://{id}` resource.

### Push notification details

- **Primary channel:** `notifications/progress` per the MCP spec, keyed by
  `progressToken` from the originating request's `_meta`.
- **No custom notification types.** All progress data fits the standard
  envelope.
- **Cadence:** minimum interval between progress events bridged to the MCP
  client is configurable via `JOB_PROGRESS_MIN_INTERVAL_MS` (default: 500ms).
  The server throttles by dropping intermediate events and sending only the
  latest when the interval elapses.

### Job lifecycle and status

States: `pending` -> `running` -> `succeeded` | `failed` | `cancelled`

**Status/result retrieval:** The `job://{id}` resource always reflects the
current state. A client that reconnects after missing the terminal event can
`resources/read` the job to get the outcome.

**Cancellation:** Client sends `notifications/cancelled` with the original
request ID. The server:
1. Marks the job as `cancelled` in the job store.
2. Sends a `cancel` message to the agent over WS (correlated by the dispatch's
   correlation ID).
3. The agent stops the capture loop and sends a terminal `result` with
   `stoppedReason: "cancelled"`.

**Timeout:** Each job has a max wall-clock duration (`JOB_TIMEOUT`, default
300s). The server tracks this; if the agent has not sent a terminal result
within the timeout, the server sends `cancel` to the agent and marks the job
as `failed` with reason `"timeout"`.

### Idempotency

Every dispatched job carries an idempotency key:

```
idempotencyKey = SHA-256(sessionId + ":" + tool + ":" + requestId)
```

where `requestId` is the JSON-RPC request `id` from the client. The server
checks the job store before dispatching: if a job with the same idempotency
key already exists and is not in `failed` state, it returns the existing
job's ID rather than dispatching again.

---

## 10. Async Message Broker Design

### Why NO broker in the default deployment

The prior spec used an in-process channel-based broker to decouple MCP request
handling from job execution. In the relay architecture, that decoupling is
provided by the **agent** itself -- the agent is the executor, running on a
separate machine, communicating over WebSocket. The server is purely a bridge.

For the **single-instance default deployment**, the server just bridges
messages in-process:
- A goroutine per connected agent reads from the WebSocket.
- Incoming `progress` and `result` messages are routed to the correct dispatch
  bridge goroutine via a channel keyed by correlation ID.
- No intermediate broker, queue, or pub/sub is needed.

This is simpler, has less operational overhead, and matches the expected scale
(personal fleet, single server instance, a handful of agents).

### When a broker would be needed (multi-replica scaling)

If the server is scaled to multiple replicas behind a load balancer, a problem
arises: the replica holding agent A's WebSocket connection may not be the same
replica handling the MCP session that dispatched a tool call to agent A. In
this case, a shared pub/sub (e.g. Redis Pub/Sub or NATS) would be needed to
route dispatch messages from the session-holding replica to the
agent-holding replica, and progress/result messages back.

**This is flagged as an Open Question (Section 19), not a designed component.**
The default deployment is single-instance and does not need it.

---

## 11. Elicitation & Sampling (Multi-Turn Interactivity)

### Elicitation usage

Destructive tools require elicitation-based confirmation **before the server
forwards the dispatch to the agent**. The confirmation happens on the MCP
client side (e.g. Claude Desktop presents the confirmation to the user).

| Tool | Elicitation trigger | Schema of elicited input |
|---|---|---|
| `shell_exec` | Always (unless `RC_SHELL_SKIP_CONFIRM=true`) | `{ "type": "object", "properties": { "confirm": { "type": "boolean", "description": "Execute this command on device {clientId}?" } }, "required": ["confirm"] }` |
| `shell_session_start` | Always (unless `RC_SHELL_SKIP_CONFIRM=true`) | Same as above. |
| `shell_session_write` | Only if `RC_SHELL_CONFIRM_EVERY_WRITE=true` | Same as above. |
| `fs_write` | When overwriting an existing file (unless `RC_FS_SKIP_CONFIRM=true`). The server dispatches a quick `fs_stat` to the agent first to check existence. | `{ "type": "object", "properties": { "confirm": { "type": "boolean" } }, "required": ["confirm"] }` |
| `fs_delete` | Always (cannot be skipped) | `{ "type": "object", "properties": { "confirm": { "type": "boolean" }, "confirmPath": { "type": "string", "description": "Type the path to confirm deletion" } }, "required": ["confirm", "confirmPath"] }` |
| `process_signal` | Always (unless `RC_PROCESS_SKIP_CONFIRM=true`) | `{ "type": "object", "properties": { "confirm": { "type": "boolean" } }, "required": ["confirm"] }` |

The elicitation message includes a human-readable description of what will
happen, including the target device:
- For `shell_exec`: the command string and target device label.
- For `fs_delete`: the path, target device, and whether it is recursive.
- For `process_signal`: the PID, process name, signal, and target device.

### Elicitation flow

1. Tool handler determines that confirmation is needed.
2. Handler sends `elicitation/create` request to the MCP client via SSE, with
   a unique elicitation ID.
3. Handler blocks (with timeout) waiting on a per-elicitation channel stored
   in session state.
4. Client responds with a `POST` containing the elicitation response.
5. The transport layer routes the response to the waiting handler via the
   channel.
6. If `confirm` is `true` (and `confirmPath` matches for `fs_delete`),
   the server proceeds to dispatch to the agent. Otherwise, the tool returns
   a non-error result indicating the operation was declined.

### Sampling usage

No tools currently use `sampling/createMessage`. The capability is declared
in case future prompt templates or tools benefit from asking the client's LLM
for intermediate reasoning, but it is not wired to any tool in this version.

### Timeout / cancellation

- Elicitation timeout: 120 seconds (configurable via `RC_ELICITATION_TIMEOUT`).
  If the client does not respond within this window, the tool returns a
  non-error result: `{ "declined": true, "reason": "elicitation_timeout" }`.
- If the client sends `notifications/cancelled` for the request that triggered
  the elicitation, the elicitation is cancelled and the tool returns
  immediately with a cancelled indication.

---

## 12. Auth & Security

There are **two distinct authentication boundaries**:

### 12.1 MCP client to server (bearer token)

- **Bearer token required** on every request (including `initialize`), passed
  as `Authorization: Bearer <token>`.
- The token is configured via the `AUTH_TOKEN` environment variable.
- If `AUTH_TOKEN` is not set, the server refuses to start with a clear error
  message. There is no anonymous mode.
- Token comparison uses constant-time equality (`crypto/subtle.ConstantTimeCompare`).
- For deployments exposed beyond localhost (behind a VPN/tunnel), the operator
  should use a long, high-entropy token (minimum 32 bytes, recommended: 64
  bytes hex-encoded).

### 12.2 Desktop agent to server (pairing + device token)

#### First-run pairing flow

```
Agent                               Server                        Operator
  |                                    |                              |
  |-- WS: pair_request (hostname) ---->|                              |
  |                                    |-- generates pairing code     |
  |<-- WS: pair_code (code, expiry) ---|   (e.g. "ABCD-1234")        |
  |                                    |                              |
  |  Agent displays code on stdout     |                              |
  |                                    |                              |
  |                                    |   Operator approves code <---+
  |                                    |   via admin CLI or admin     |
  |                                    |   web UI (NOT an MCP tool)   |
  |                                    |                              |
  |<-- WS: pair_approved ------------- |                              |
  |    (deviceId, deviceToken)         |                              |
  |                                    |                              |
  |  Agent persists token to           |                              |
  |  ~/.rc-mcp/agent-token (0600)      |                              |
```

**Pairing code properties:**
- Human-readable format: `XXXX-XXXX` (uppercase alphanumeric, no ambiguous
  characters like 0/O, 1/I/L).
- Generated via `crypto/rand`.
- Short-lived: expires after 5 minutes (`PAIRING_CODE_TTL`).
- Single-use: consumed on approval or rejection. A code cannot be reused.
- If the operator does not approve within the TTL, the server sends an `error`
  message to the agent with code `"pairing_expired"` and closes the
  WebSocket.

**Security invariant: the LLM can NEVER approve a new device.** The pairing
approval surface is:
- A local admin CLI (e.g. `rc-mcp-admin approve ABCD-1234`) that connects
  to a localhost-only admin API on the server.
- Or an admin-only web interface served by the server on a separate port,
  accessible only to the operator.
- **Explicitly NOT exposed as an MCP tool, resource, or any part of the MCP
  protocol surface.** This is a deliberate security boundary to prevent an
  LLM from autonomously enrolling devices.

#### Subsequent connections

- The agent connects to `/agent/ws` and sends a `hello` message with its
  persistent device token.
- The server validates the token against the device registry (bcrypt hash
  comparison).
- On success: `hello_ack` with the device ID. The device is marked online.
- On failure: `error` with code `"auth_failed"`, WebSocket closed with
  code 1008 (policy violation).

#### Token storage and revocation

- **Agent side:** token stored at `~/.rc-mcp/agent-token`, mode `0600`.
  The agent reads this file on startup.
- **Server side:** the device registry stores bcrypt hashes of device tokens,
  never raw tokens.
- **Revocation:** the operator can revoke a device via the admin CLI
  (`rc-mcp-admin revoke <deviceId>`). This removes the device from the
  registry. The next time the agent tries to connect or its heartbeat fires,
  authentication fails, and the WebSocket is closed. The revoked device must
  re-pair to reconnect.
- **Revocation is reflected in the `clients://list` resource** -- the device
  disappears from the list. An `notifications/resources/updated` event is
  emitted on all MCP sessions.

### 12.3 Origin header validation

- Required for all requests to the `/mcp` endpoint.
- The server maintains an allowlist configured via `MCP_ALLOWED_ORIGINS`
  (comma-separated).
- Origin validation prevents DNS rebinding attacks.
- The `/agent/ws` endpoint does **not** require origin validation (agents are
  not browsers), but does require a valid device token.

### 12.4 Bind address

- Default: `0.0.0.0:8080` (listens on all interfaces, since agents connect
  from other machines -- nginx handles TLS termination and access control).
- The admin API listens on `127.0.0.1:9090` (loopback only, never exposed
  through nginx).

### 12.5 Rate limiting

| Scope | Default | Configurable via |
|---|---|---|
| Requests per MCP session per minute | 120 | `RC_RATE_LIMIT_SESSION` |
| Tool calls per MCP session per minute | 60 | `RC_RATE_LIMIT_TOOLS` |
| Concurrent dispatches per MCP session | 10 | `RC_MAX_CONCURRENT_DISPATCHES` |
| Concurrent shell sessions per MCP session | 5 | `RC_MAX_SHELL_SESSIONS` |
| Dispatches per agent per minute | 120 | `RC_RATE_LIMIT_AGENT` |

Rate limit exceeded returns JSON-RPC error code `-32000` with message
`"rate limit exceeded"`.

Per-agent rate limiting prevents one MCP session from monopolizing an agent
when multiple MCP sessions are dispatching to the same device.

### 12.6 Input validation

- **First boundary:** JSON Schema validation in the MCP transport layer,
  before the tool handler runs. Invalid input returns JSON-RPC error `-32602`
  (invalid params).
- **Second boundary:** the agent re-validates the dispatch payload before
  executing (never trust the wire). The agent's validation is defense-in-depth;
  the server's validation is the primary gate.
- **Path traversal (agent side):** filesystem tools resolve all paths to
  absolute paths and optionally restrict them to configured roots
  (`AGENT_FS_ALLOWED_ROOTS`, comma-separated). If not configured, any path
  the agent process can access is allowed.

### 12.7 Audit log

- **Authoritative location: server-side.** The server logs every tool
  invocation with: timestamp, MCP session ID, target device ID, tool name,
  args digest, result status, duration.
- **Path:** `RC_AUDIT_LOG_PATH` (default: `/var/log/rc-mcp/audit.log` in
  Docker, `$HOME/.rc-mcp/audit.log` when run directly).
- **Format:** newline-delimited JSON (one `audit.Entry` per line).
- **Append-only:** the file is opened with `O_APPEND | O_WRONLY | O_CREATE`.
- **Not deletable via any exposed tool:** no filesystem tool on the server
  can reach the audit log because the server has no filesystem tools -- all
  fs tools target agents. The audit log lives on the server, not on agents.
- **Rotation:** the operator is responsible for external log rotation (e.g.
  `logrotate`). The server detects inode change and reopens the file.
- **Args digest:** tool arguments are logged as a SHA-256 digest. A sanitized
  hint is also logged for quick scanning.
- **Agent-side logging (optional):** agents can independently log the
  dispatches they execute for local forensics (`AGENT_AUDIT_LOG_PATH`), but
  the server-side log is the authoritative record.

---

## 13. Error Handling

### Error taxonomy

| Category | JSON-RPC code | Shape | Example |
|---|---|---|---|
| Parse error | `-32700` | `{ "code": -32700, "message": "Parse error" }` | Malformed JSON body |
| Invalid request | `-32600` | `{ "code": -32600, "message": "..." }` | Missing `jsonrpc` field |
| Method not found | `-32601` | `{ "code": -32601, "message": "..." }` | Unknown method name |
| Invalid params | `-32602` | `{ "code": -32602, "message": "...", "data": { "validationErrors": [...] } }` | JSON Schema validation failure on tool input |
| Internal error | `-32603` | `{ "code": -32603, "message": "..." }` | Unexpected server panic |
| Session not found | `-32001` | `{ "code": -32001, "message": "Session not found" }` | Invalid or expired `Mcp-Session-Id` |
| Auth failure | `-32002` | `{ "code": -32002, "message": "Unauthorized" }` | Missing or invalid bearer token |
| Rate limited | `-32000` | `{ "code": -32000, "message": "Rate limit exceeded" }` | Per-session or per-tool rate limit |
| Device offline | N/A (200 OK) | `{ "content": [{ "type": "text", "text": "Device {clientId} is offline" }], "isError": true }` | Target agent not connected |
| Device not found | N/A (200 OK) | `{ "content": [{ "type": "text", "text": "Unknown device {clientId}" }], "isError": true }` | clientId not in device registry |
| Capability disabled | N/A (200 OK) | `{ "content": [{ "type": "text", "text": "Device {clientId} does not have {cap} enabled" }], "isError": true }` | Agent missing required capability |
| Agent execution error | N/A (200 OK) | `{ "content": [{ "type": "text", "text": "..." }], "isError": true }` | Tool logic failed on agent (file not found, permission denied, etc.) |
| Agent disconnect mid-op | N/A (200 OK) | `{ "content": [{ "type": "text", "text": "Agent disconnected during operation" }], "isError": true }` | WS dropped during dispatch |
| Job execution failure | N/A | Job status `failed` + terminal `notifications/progress`. Surfaced via `job://{id}` resource. | Agent disconnect during screenshot_watch, timeout |
| Elicitation declined | N/A (200 OK) | `{ "content": [{ "type": "text", "text": "Operation declined by user" }], "isError": false }` | User declined confirmation |
| Elicitation timeout | N/A (200 OK) | `{ "content": [{ "type": "text", "text": "Confirmation timed out" }], "isError": false }` | Client did not respond to elicitation |

### Distinction: protocol errors vs. tool errors vs. agent errors

- **Protocol errors** (codes `-327xx`, `-326xx`, `-32001`, `-32002`, `-32000`)
  are returned as JSON-RPC error responses. They indicate the request could
  not be processed at all.
- **Tool execution errors** are returned as successful JSON-RPC responses with
  `isError: true` in the content. They include device-offline,
  capability-disabled, and errors reported by the agent.
- **Job failures** are surfaced as a `failed` job status in the job store and
  via `notifications/progress` with a terminal event. The original `tools/call`
  for pattern (a) jobs already returned successfully with a `jobId`.

---

## 14. Docker / Deployment Design

The docker-compose stack is **server-side only**. The agent is a separately
distributed binary.

```yaml
# docker-compose.yml
services:
  mcp-server:
    build:
      context: .
      dockerfile: Dockerfile
    image: rc-mcp-server:latest
    ports:
      - "127.0.0.1:8080:8080"   # MCP + agent WS (behind nginx)
      - "127.0.0.1:9090:9090"   # admin API (loopback only, not proxied)
    env_file:
      - .env
    environment:
      - MCP_BIND_ADDR=0.0.0.0:8080
      - ADMIN_BIND_ADDR=0.0.0.0:9090
      - MCP_SESSION_STORE=memory
    volumes:
      - audit-log:/var/log/rc-mcp
      - device-registry:/var/lib/rc-mcp  # persistent device registry
    security_opt:
      - no-new-privileges:true
    read_only: true
    tmpfs:
      - /tmp
    healthcheck:
      test: ["CMD", "wget", "--spider", "-q", "http://localhost:8080/health"]
      interval: 30s
      timeout: 5s
      retries: 3
    restart: unless-stopped

  nginx:
    image: nginx:alpine
    ports:
      - "443:443"
    volumes:
      - ./docker/nginx/nginx.conf:/etc/nginx/nginx.conf:ro
      - ./docker/nginx/certs:/etc/nginx/certs:ro
    depends_on:
      - mcp-server
    healthcheck:
      test: ["CMD", "nginx", "-t"]
      interval: 60s
      timeout: 5s
      retries: 3
    restart: unless-stopped

volumes:
  audit-log:
  device-registry:
```

### Key deployment notes

- **`mcp-server`** is a single Go binary built from `cmd/server/main.go`.
  Uses a distroless or scratch base image. Hosts the MCP Streamable HTTP
  endpoint, the agent WebSocket endpoint, the admin API, and in-process
  dispatch bridging.
- **nginx** handles TLS termination and reverse-proxies both `/mcp` (MCP
  clients) and `/agent/ws` (desktop agents) to the mcp-server. Critical
  nginx configuration:
  ```
  # For SSE (MCP client):
  proxy_buffering off;
  proxy_cache off;
  proxy_read_timeout 86400s;
  proxy_http_version 1.1;
  proxy_set_header Connection "";

  # For WebSocket (agents):
  proxy_http_version 1.1;
  proxy_set_header Upgrade $http_upgrade;
  proxy_set_header Connection "upgrade";
  proxy_read_timeout 86400s;
  ```
- **The admin API (port 9090) is NOT proxied through nginx.** It is
  accessible only on localhost on the server machine.
- **No separate worker service.** The server bridges dispatches directly;
  agents are the workers.
- **No broker service in the default stack.** Redis is only needed if
  multi-replica scaling is pursued (Open Questions). It can be added with a
  `redis` service under a `scaling` profile.
- **Health check:** `GET /health` returns `200 OK` with `{ "status": "ok",
  "agents_online": N }`. This is outside the MCP protocol and does not
  require authentication.

### rc-mcp-agent distribution

The agent is **not part of the docker-compose stack**. It runs on target
Linux desktop machines as a systemd user service:

```ini
# ~/.config/systemd/user/rc-mcp-agent.service
[Unit]
Description=rc-mcp Desktop Agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%h/.local/bin/rc-mcp-agent --server wss://server-host/agent/ws
Restart=always
RestartSec=5
Environment=DISPLAY=:0

[Install]
WantedBy=default.target
```

The agent binary is distributed as a standalone Go binary (built from
`cmd/agent/main.go`). No container needed.

### Dockerfile (server)

```dockerfile
FROM golang:1.23-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /rc-mcp-server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /rc-mcp-server /rc-mcp-server
EXPOSE 8080 9090
ENTRYPOINT ["/rc-mcp-server"]
```

---

## 15. Environment Variables

### Server (`rc-mcp-server`)

| Variable | Default | Description |
|---|---|---|
| `AUTH_TOKEN` | (required) | Bearer token for MCP client authentication. Server refuses to start if unset. |
| `MCP_BIND_ADDR` | `0.0.0.0:8080` | Address for MCP + agent WS endpoints. |
| `ADMIN_BIND_ADDR` | `127.0.0.1:9090` | Address for admin API (pairing approval, device revocation). Loopback only. |
| `MCP_ALLOWED_ORIGINS` | (empty) | Comma-separated allowlist of Origin headers for MCP clients. |
| `MCP_SESSION_STORE` | `memory` | Session store backend: `memory` or `redis`. |
| `MCP_SESSION_IDLE_TIMEOUT` | `30m` | Duration after which idle MCP sessions are expired. |
| `SSE_REPLAY_BUFFER_SIZE` | `500` | Number of SSE events retained per session for replay. |
| `PAIRING_CODE_TTL` | `5m` | How long a pairing code remains valid. |
| `DEVICE_REGISTRY_PATH` | `/var/lib/rc-mcp/devices.json` | Path to the persistent device registry file. |
| `AGENT_RECONNECT_GRACE_PERIOD` | `60s` | How long the server keeps an agent's in-flight state alive after disconnect. |
| `JOB_TIMEOUT` | `300s` | Max wall-clock duration for a long-running job (e.g. screenshot_watch). |
| `JOB_PROGRESS_MIN_INTERVAL_MS` | `500` | Minimum ms between progress notifications bridged to MCP client. |
| `RC_SHELL_SKIP_CONFIRM` | `false` | Skip elicitation confirmation for shell tools. |
| `RC_SHELL_CONFIRM_EVERY_WRITE` | `false` | Require confirmation on every shell_session_write. |
| `RC_FS_SKIP_CONFIRM` | `false` | Skip elicitation confirmation for filesystem writes. |
| `RC_PROCESS_SKIP_CONFIRM` | `false` | Skip elicitation confirmation for process signals. |
| `RC_AUDIT_LOG_PATH` | `/var/log/rc-mcp/audit.log` | Path to the server-side audit log. |
| `RC_RATE_LIMIT_SESSION` | `120` | Max requests per MCP session per minute. |
| `RC_RATE_LIMIT_TOOLS` | `60` | Max tool calls per MCP session per minute. |
| `RC_RATE_LIMIT_AGENT` | `120` | Max dispatches per agent per minute. |
| `RC_MAX_CONCURRENT_DISPATCHES` | `10` | Max concurrent in-flight dispatches per MCP session. |
| `RC_MAX_SHELL_SESSIONS` | `5` | Max concurrent shell sessions per MCP session. |
| `RC_ELICITATION_TIMEOUT` | `120s` | Timeout for elicitation responses from MCP clients. |

### Agent (`rc-mcp-agent`)

| Variable | Default | Description |
|---|---|---|
| `AGENT_SERVER_URL` | (required) | WebSocket URL of the server (e.g. `wss://server-host/agent/ws`). |
| `AGENT_TOKEN_PATH` | `~/.rc-mcp/agent-token` | Path to the persistent device token file. |
| `AGENT_CAPABILITIES` | `shell,screenshot,filesystem,process,sysinfo` | Comma-separated list of enabled capability areas. |
| `AGENT_FS_ALLOWED_ROOTS` | (empty = unrestricted) | Comma-separated absolute paths for filesystem tool restrictions. |
| `AGENT_AUDIT_LOG_PATH` | (empty = disabled) | Optional agent-side audit log path. |
| `AGENT_RECONNECT_GRACE_PERIOD` | `60s` | How long the agent keeps local job state alive during a disconnect. |
| `DISPLAY` | `:0` | X11 display for screenshot tools. |

---

## 16. Folder Structure

```
rc-mcp/
  cmd/
    server/
      main.go              # Entry point: starts HTTP server, admin API, wires dependencies
    agent/
      main.go              # Entry point: dials server WS, runs tool executors
  internal/
    protocol/              # Shared wire protocol (imported by both server and agent)
      envelope.go          # Envelope, MessageType, all payload types
      binary.go            # BinaryHeader, encode/decode, frame types
      version.go           # Protocol version constant and negotiation logic
    mcp/
      tools/
        shell_exec.go       # shell_exec: schema, server-side handler (dispatch + bridge)
        shell_session.go    # shell_session_start, _write, _close (dispatch + bridge)
        screenshot.go       # screenshot_capture, screenshot_watch (dispatch + bridge)
        fs.go               # fs_read, fs_write, fs_list, fs_delete, fs_stat
        process.go          # process_list, process_info, process_signal
        sysinfo.go          # sysinfo_get
        registry.go         # tool registration, capability-gated listing
      resources/
        clients.go          # clients://list resource
        job.go              # job://{id} resource
        sysinfo.go          # sysinfo://{clientId}/overview resource
        audit.go            # audit://log resource
        shell.go            # shell://sessions resource
      prompts/
        diagnose_system.go
        safe_cleanup.go
        shell_workflow.go
      types/
        shell.go            # ShellExecInput/Output, ShellSession* types
        screenshot.go       # ScreenshotCapture/Watch types
        fs.go               # Fs* types
        process.go          # Process* types
        sysinfo.go          # Sysinfo* types
    session/
      store.go              # SessionStore interface
      memory.go             # In-memory implementation
      session.go            # Session struct, ShellSessionMap management
      replay.go             # SSE ReplayBuffer (circular buffer)
    transport/
      handler.go            # Streamable HTTP handler (POST/GET/DELETE /mcp)
      sse.go                # SSE writer goroutine, event formatting
      auth.go               # Bearer token middleware for MCP clients
      origin.go             # Origin header validation
      ratelimit.go          # Per-session rate limiter
    devices/
      registry.go           # DeviceRegistry interface + file-backed implementation
      pairing.go            # Pairing code generation, approval, rejection
      types.go              # Device, PairingCode types
    agent/                  # Server-side agent connection management
      hub.go                # Agent WebSocket hub: accept, authenticate, track online/offline
      connection.go         # Per-agent WS reader/writer goroutines
      dispatch.go           # Dispatch bridge: send to agent, collect progress/result
    jobs/
      types.go              # Job, ProgressEvent, JobStatus types
      store.go              # JobStore interface
      memory_store.go       # In-memory JobStore
    audit/
      log.go                # Append-only audit logger
      types.go              # audit.Entry type
    admin/
      api.go                # Admin API: approve pairing, revoke device, list pending codes
    auth/
      token.go              # Token validation (constant-time compare)
  agent/                    # Agent-side packages (used by cmd/agent)
    executor/
      shell.go              # PTY allocation, shell exec, session management
      screenshot.go         # Screenshot capture, watch loop
      fs.go                 # Filesystem operations
      process.go            # Process list/info/signal
      sysinfo.go            # System info gathering
    client/
      ws.go                 # WebSocket client: connect, reconnect, backoff, heartbeat
      pairing.go            # First-run pairing flow
      dispatch.go           # Dispatch handler: receive dispatch, route to executor, send result/progress
  Dockerfile
  docker-compose.yml
  docker/
    nginx/
      nginx.conf            # SSE + WebSocket reverse proxy config, TLS termination
  go.mod
  go.sum
  .env.example
```

---

## 17. Testing Strategy

### Contract tests per tool

For each tool in Section 3:
- Valid input (with a valid `clientId`) passes JSON Schema validation and
  produces the expected output shape.
- Invalid input (missing `clientId`, missing required fields, wrong types) is
  rejected with `-32602` before any dispatch occurs.
- Declared annotations are correct (e.g. `readOnlyHint: true` tools do not
  mutate state).
- Capability gating: tools for a disabled capability area on the target agent
  return tool errors.
- Device offline: dispatching to an offline device returns a tool error.

### Pairing flow tests

1. Agent sends `pair_request` -> server returns `pair_code` with a code and
   expiry.
2. Approve the code via the admin API -> agent receives `pair_approved` with
   a valid device token.
3. Agent disconnects and reconnects with the device token -> `hello_ack`
   succeeds.
4. **Code expiry:** wait for the TTL to elapse -> attempt approval -> rejected
   with "pairing code expired".
5. **Single use:** approve a code -> attempt to approve the same code again ->
   rejected with "pairing code already used".
6. **Rejection:** reject a code via admin API -> agent receives `error` with
   code `"pairing_rejected"`.
7. **Unapproved code on timeout:** agent waits for TTL + buffer -> server
   sends `error` with `"pairing_expired"` and closes WS.
8. **Device revocation:** revoke a device via admin API -> agent's next
   heartbeat/reconnect fails with `"auth_failed"`.

### Session resumption test

1. Establish an MCP session, start an SSE stream, receive several events.
2. Record the last `id:` received.
3. Disconnect the SSE stream.
4. Reconnect with `GET /mcp` including `Last-Event-ID: <recorded-id>`.
5. Assert: all events emitted between disconnect and reconnect are replayed
   in order, with no gaps and no duplicates.
6. Assert: new events after the replay are delivered normally.

### Multi-turn shell session test (end-to-end through relay)

1. `tools/call shell_session_start` with a valid `clientId` -- assert
   elicitation is sent.
2. Respond to elicitation with `confirm: true`.
3. Assert dispatch sent to agent, shell session started, `shellSessionId`
   returned.
4. `tools/call shell_session_write` with `input: "echo hello\n"` -- assert
   progress notifications contain `hello` (via two-hop), final result includes
   output.
5. `tools/call shell_session_write` with `input: "pwd\n"` -- assert cwd is
   preserved.
6. `tools/call shell_session_close` -- assert exit code 0.

### Reconnect and in-flight job survival tests

1. Start a `shell_session_start` on agent A.
2. Disconnect agent A's WebSocket.
3. Assert: within grace period, the server holds the session state.
4. Reconnect agent A within the grace period.
5. Assert: `shell_session_write` to the same shellSessionId works, the PTY
   session is resumed.
6. Repeat but exceed the grace period.
7. Assert: the shell session is cleaned up, `shell_session_write` returns
   tool error.

### Screenshot_watch across disconnect

1. Start `screenshot_watch` targeting agent A.
2. Assert `tools/call` returns `jobId`.
3. Disconnect the MCP client's SSE stream.
4. Reconnect with `Last-Event-ID`.
5. Assert: missed frames are replayed from the replay buffer.
6. Disconnect agent A mid-watch.
7. Assert: job is marked `failed` with reason `"agent_disconnect"`.
8. Assert: `job://{jobId}` resource returns the failure status.

### Wire protocol tests

1. **JSON envelope round-trip:** encode and decode every message type, verify
   field fidelity.
2. **Binary frame round-trip:** encode a BinaryHeader + payload, decode it,
   verify correlation prefix, sequence number, and frame type.
3. **Hybrid demuxing:** interleave JSON and binary frames on a single
   connection, verify they are correctly demultiplexed by correlation ID.
4. **Protocol version mismatch:** agent sends `protocolVersion: "999"` ->
   server rejects with `"version_mismatch"` error and closes WS.
5. **Binary correlation prefix collision:** verify the server rejects the
   second dispatch if a prefix collision would occur.

### Elicitation round-trip test

1. Call `fs_delete` with a valid `clientId` and path.
2. Assert the server sends `elicitation/create` with the confirmation schema.
3. Respond with `confirm: false`.
4. Assert: tool returns non-error result indicating declined. Assert: no
   dispatch was sent to the agent.
5. Repeat with `confirm: true, confirmPath: <wrong path>`.
6. Assert declined (path mismatch), no dispatch.
7. Repeat with correct `confirmPath`.
8. Assert dispatch sent to agent, deletion proceeds.

### End-to-end protocol conformance

- Use the MCP Inspector or an equivalent scripted JSON-RPC client to exercise:
  `initialize`, `tools/list`, `tools/call`, `resources/list`, `resources/read`,
  `prompts/list`, `prompts/get`, `notifications/cancelled`, `DELETE` session.
- Verify `Mcp-Session-Id` handling: requests without the header after
  initialize return 404.
- Verify `clients://list` updates when agents connect/disconnect.

### Race condition testing

- Run all tests with `go test -race` to detect data races in session state,
  shell session map, device registry, replay buffer, and event channel
  operations.
- Stress test: 10 concurrent `tools/call` requests on the same session
  targeting different agents, verify no panics or data corruption.

---

## 18. API Alignment Verification

N/A -- there is no separate frontend specification for this project. rc-mcp
is a standalone server consumed directly by MCP clients. The tool, resource,
and prompt inventory in Sections 3-5 is the authoritative API definition.

---

## 19. Open Questions

1. **Multi-replica server scaling:** The default deployment is a single server
   instance. If scaling to multiple replicas behind a load balancer, the
   following problems must be solved:
   - The replica holding agent A's WebSocket may not be the replica handling
     an MCP session that dispatches to agent A. A shared pub/sub (Redis
     Pub/Sub, NATS) would be needed to route dispatches and bridge
     progress/results across replicas.
   - MCP session state must be shared (Redis-backed session store with sticky
     sessions for SSE).
   - The device registry must be shared (move from file-backed to
     Redis/Postgres).
   This is not designed now. The current architecture is clean enough that the
   pub/sub layer can be inserted between the dispatch bridge and the agent
   hub without restructuring the rest of the system.

2. **Shell command allowlist/denylist:** Should `shell_exec` and interactive
   shell sessions support an operator-configurable allowlist or denylist?
   Current design: fully arbitrary exec gated by elicitation confirmation.

3. **Keyboard/mouse input injection:** Not in scope. If added, it would be a
   separate capability area (`input`) on the agent with mandatory elicitation
   on every action.

4. **Wayland support for screenshots:** Current design assumes X11. Wayland
   screen capture requires different mechanisms on the agent side.

5. **Agent auto-update mechanism:** Should the server be able to push binary
   updates to connected agents? Significant security and reliability
   implications. Deferred.

6. **Admin web UI:** The spec defines pairing approval and device revocation
   via a CLI. A web UI for the admin surface is a convenience feature that
   can be added later.

7. **File content in audit log hints:** The server logs a sanitized hint. An
   opt-in mode for full argument logging (for forensic purposes) is deferred.

8. **Per-agent filesystem root restrictions:** The `AGENT_FS_ALLOWED_ROOTS`
   config is per-agent. Should the server also enforce a global filesystem
   root policy that applies to all agents? Or is per-agent sufficient?

---

## Self-Check

- [x] Every capability area has matching MCP tool definitions with `clientId`
      on every tool
- [x] Every tool's multi-turn behavior (single-shot vs. elicitation vs.
      session-dependent) is explicit
- [x] Session store handles concurrent access safely (sync.Map + mutex,
      `go test -race` required)
- [x] Origin validation is defined for the HTTP transport
- [x] SSE resumability (event IDs + replay) is specified, not just streaming
      happy-path
- [x] Error response shapes are consistent and distinguish protocol errors,
      tool execution errors, and agent errors
- [x] Every long-running tool specifies its dispatch pattern (hold-open
      two-hop stream vs. jobId + push notifications) and its terminal-result
      fallback if the client is not listening when it finishes
- [x] MCP sessions and device connections are explicitly documented as
      independent, differently-authenticated concepts
- [x] Wire protocol is hybrid (JSON envelope + binary frames) with shared
      package preventing drift
- [x] Pairing flow is fully specified: code generation, TTL, single-use,
      operator approval, LLM-never-approves security invariant
- [x] Reconnect-with-backoff and in-flight job survival across brief
      disconnect are specified for agents
- [x] Two-hop streaming (agent -> server WS -> MCP SSE) is the pattern for
      all long-running progress
- [x] No broker in the default single-instance deployment; broker needed only
      for multi-replica scaling (flagged as open question)
- [x] Destructive tools require elicitation confirmation before dispatch to
      agent
- [x] Audit log is server-side authoritative, append-only
- [x] Agent capabilities are per-device, not per-server
- [x] Bearer token required for MCP clients; device token required for agents
