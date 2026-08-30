# Phase 1: MVP Core

## Goal
One full tool round-trip end-to-end: `shell_exec` dispatched from an MCP client through the Streamable HTTP transport, relayed to a paired agent over WebSocket, executed, and the result (with streaming stdout/stderr progress) returned to the client -- including elicitation confirmation and audit logging.

## Deliverables

### Shared protocol
- [ ] No new types required -- Phase 0 envelope and binary header types cover `dispatch`, `result`, `progress`, `cancel`, and binary `FrameShellStdout` frames needed for `shell_exec`

### Server
- [ ] `internal/transport/handler.go` -- Streamable HTTP handler: `POST /mcp` (JSON-RPC messages), `GET /mcp` (SSE stream), `DELETE /mcp` (session termination). Validates `Mcp-Session-Id` header on all post-initialize requests (404 on missing/unknown). Handles `initialize` request/response with capability declaration.
- [ ] `internal/transport/sse.go` -- SSE writer goroutine (1 per MCP session): drains `Session.EventCh`, writes SSE frames with monotonically increasing `id:` fields, appends to replay buffer. Supports `Last-Event-ID` replay on reconnect.
- [ ] `internal/transport/auth.go` -- Bearer token middleware: validates `Authorization: Bearer <token>` against `AUTH_TOKEN` env var using constant-time compare. Rejects with JSON-RPC `-32002` on failure.
- [ ] `internal/transport/origin.go` -- Origin header validation against `MCP_ALLOWED_ORIGINS` allowlist.
- [ ] `internal/session/store.go` + `memory.go` + `session.go` -- `SessionStore` interface, in-memory implementation (`sync.Map`), `Session` struct with `EventCh` (buffered channel, size 256), `ShellSessionMap`, `ReplayBuffer`, `CancelFunc`. Background goroutine for idle session expiry (`MCP_SESSION_IDLE_TIMEOUT`, default 30m).
- [ ] `internal/session/replay.go` -- Circular SSE replay buffer (`SSE_REPLAY_BUFFER_SIZE`, default 500). Supports `Last-Event-ID`-based replay.
- [ ] `internal/agent/dispatch.go` -- Dispatch bridge goroutine: sends `dispatch` envelope to the correct agent WS connection (matched by `clientId`), waits for `progress` and `result` messages correlated by ID, writes them to `Session.EventCh` as `notifications/progress` and `tools/call` result. Handles context cancellation -> sends `cancel` to agent. Backpressure: blocks 5s on full EventCh, then drops + logs.
- [ ] `internal/mcp/tools/shell_exec.go` -- `shell_exec` tool: JSON Schema input validation, elicitation confirmation flow (unless `RC_SHELL_SKIP_CONFIRM=true`), dispatch to agent via bridge, stdout/stderr binary frame bridging to `notifications/progress`, terminal result assembly.
- [ ] `internal/mcp/tools/registry.go` -- Tool registration framework: capability-gated `tools/list` (aggregates union of capabilities across online agents), `tools/call` routing by tool name.
- [ ] Elicitation flow (in transport layer) -- `elicitation/create` request sent to client via SSE, per-elicitation channel in session state, response routing from client POST, 120s timeout (`RC_ELICITATION_TIMEOUT`).
- [ ] `internal/audit/log.go` + `types.go` -- Append-only audit logger: writes `audit.Entry` records as newline-delimited JSON to `RC_AUDIT_LOG_PATH`. Entry includes: timestamp, MCP session ID, target device ID, tool name, args SHA-256 digest, args hint, status, duration. File opened with `O_APPEND | O_WRONLY | O_CREATE`. Detects inode change for log rotation.
- [ ] `Mcp-Session-Id` generation -- 128-bit cryptographically random token, hex-encoded (32 chars), issued on `initialize` response header.

### Agent
- [ ] `agent/executor/shell.go` -- `shell_exec` executor: receives `DispatchPayload` with tool=`shell_exec`, spawns `/bin/sh -c <command>` with configurable cwd, env, timeout (default 30s, max 300s), optional stdin pipe. Streams stdout/stderr as binary `FrameShellStdout` frames over WS (minimum interval 500ms or 4KB). Returns `ResultPayload` with stdout, stderr, exitCode, killed, durationMs. Handles timeout (SIGKILL after timeout, killed=true).
- [ ] `agent/client/dispatch.go` -- Dispatch handler (upgrade from Phase 0 stub): routes incoming `dispatch` messages to the correct executor by tool name, sends `result`/`progress` back. Handles `cancel` messages (kills in-flight command). Validates dispatch payload before executing (defense-in-depth).
- [ ] Binary frame output -- Agent sends shell stdout/stderr as binary WS frames with proper `BinaryHeader` (correlation prefix from dispatch UUID, monotonically increasing stream sequence number, `FrameShellStdout` type byte).

### Infrastructure
- [ ] `internal/transport/ratelimit.go` -- Per-session rate limiter: requests/minute (`RC_RATE_LIMIT_SESSION`, default 120), tool calls/minute (`RC_RATE_LIMIT_TOOLS`, default 60), concurrent dispatches (`RC_MAX_CONCURRENT_DISPATCHES`, default 10). Returns JSON-RPC `-32000` on exceeded.
- [ ] Add `project.json` for new Nx project boundaries if needed (e.g. `internal/transport/`, `internal/session/` if they warrant independent test targets)

## Done Definition
- An MCP client (MCP Inspector or scripted JSON-RPC client) can complete the full round-trip: `initialize` -> `tools/list` (sees `shell_exec`) -> `tools/call shell_exec` with `clientId` of a paired agent and `command: "echo hello"` -> receives elicitation confirmation request -> confirms -> receives `notifications/progress` with stdout chunk -> receives `tools/call` result with `stdout: "hello\n"`, `exitCode: 0`, `killed: false`
- `tools/call shell_exec` with a long-running command (e.g. `sleep 5 && echo done`) streams progress notifications with stdout chunks before the final result
- `tools/call shell_exec` with `timeout: 2` and `command: "sleep 10"` returns `killed: true` with partial output
- Elicitation declined (confirm: false) returns non-error result `{ "declined": true }`, no dispatch sent to agent
- Elicitation timeout (120s, no response) returns non-error result with `reason: "elicitation_timeout"`
- `Mcp-Session-Id` enforced: requests after initialize without the header return 404
- SSE replay works: disconnect SSE stream, reconnect with `Last-Event-ID`, missed events replayed
- Audit log contains an entry for the `shell_exec` call with correct session ID, device ID, tool name, args digest
- Device offline: `tools/call shell_exec` with an offline `clientId` returns tool error `isError: true`
- Rate limiting: exceeding 60 tool calls/minute returns `-32000`
- All tests pass with `go test -race ./...`

## Parallel work
- Server: MCP transport (handler.go, sse.go, auth.go, session store) can be built in parallel with Server: dispatch bridge + shell_exec tool handler, once the Session struct interface is agreed
- Server: audit logger can be built independently of transport and dispatch
- Agent: shell executor can be built in parallel with Server: dispatch bridge, once Shared protocol: DispatchPayload/ResultPayload/ProgressPayload shapes are frozen (they already are from Phase 0)

## Phase dependencies
- Requires: Phase 0 (wire protocol types, pairing/auth flow, agent WS connection, Docker stack)

## Complexity
- Shared protocol: S (no new types)
- Server: XL
- Agent: M
- Infra: S

## Risks
- SSE writer + replay buffer correctness under concurrent dispatch bridges writing to the same EventCh -- must be tested with `-race` and stress tests
- Elicitation flow depends on the MCP client supporting the `elicitation` capability -- Claude Desktop does, but verify with MCP Inspector
- Binary frame demuxing on the server side (correlation prefix -> dispatch bridge channel routing) needs careful locking in the agent reader goroutine
- `POST /mcp` must handle both the held-open response pattern (dispatch pattern b, where the HTTP response is returned only after the tool completes) and standard request-response -- ensure the transport layer supports this cleanly
