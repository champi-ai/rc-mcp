VERDICT: APPROVED

---

## Blocking Issues

None.

The prior blocking issue (Section 4.5 `shell://sessions` referencing PID, shell, and creation-time data with no backing type) is resolved. `ShellSessionEntry{ClientID, PID, Shell, CreatedAt}` is defined in the session package, `Session.ShellSessionMap` is now `map[string]*ShellSessionEntry`, and the Section 4.5 JSON schema matches field-for-field. The data flow is sound: the server handler populates the entry from the agent's `ShellSessionStartOutput` on session creation and serves it from the map when the resource is read.

All blocking checks pass clean on this pass:

- Pairing security invariant: admin/pairing surface fully isolated on loopback-only admin API (port 9090). No MCP tool, resource, or prompt can approve, reject, or revoke a device.
- Session/connection independence: Section 7 clearly separates MCP sessions (bearer token, Mcp-Session-Id) from device connections (device token, WS), with explicit correlation-per-dispatch.
- clientId coverage: every tool targeting a machine has clientId as a required input (or resolves it from shellSessionId via ShellSessionMap for session-scoped tools).
- Auth boundaries: bearer token on all MCP requests; device token on all agent WS connections; admin API on loopback only.
- Destructive hints and elicitation: all mutating/arbitrary-exec tools carry `destructiveHint: true` with elicitation-based confirmation described.
- Wire protocol: 13 message types in Section 2.2 match 13 Go constants in Section 6; 4 binary frame types match; correlation ID field names/types are consistent between JSON envelope and binary header; protocolVersion negotiation and mismatch handling are defined.
- Streaming/long-running: all three streaming tools have dispatch patterns defined in Section 9 with two-hop bridge accounting.
- Audit log: every mutating tool logs server-side; no exposed tool can reach the server's audit log file.
- Go layout: single `go.mod`, single module, no Nx.
- Undefined dependencies: all types, fields, and message types referenced across sections have concrete definitions in Section 6.

---

## Non-Blocking Issues

These are NEW findings from this pass. Previously flagged non-blocking issues that remain unaddressed are not re-listed.

1. **Stale prose in Section 7 "What lives in session state" after ShellSessionEntry fix.** The bullet "Shell session mapping -- map of `shellSessionId` to `clientId` (device ID)" is now inaccurate. The map value is `*ShellSessionEntry` containing four fields, not a bare clientId. An implementer reading only the prose (skipping the type definition) would build a `map[string]string`. Update the prose to say "map of shellSessionId to ShellSessionEntry (clientId, PID, shell binary, creation time)".

2. **`sysinfo_get` output schema (Section 3.5.1) has no `required` array.** Every other tool output schema explicitly lists required fields including `clientId`. This tool's output schema has properties only, no required. At minimum `clientId` and `hostname` should be required for consistency. The Go type uses `omitempty` on `clientId`, reinforcing the ambiguity.

3. **Admin API endpoints are undefined.** Sections 12.2, 14, and 16 reference the admin API (`approve pairing`, `revoke device`, `list pending codes`) on port 9090 but no HTTP paths, methods, request/response schemas, or authentication mechanism are specified. An implementer of the admin CLI (`rc-mcp-admin`) has nothing to build against. Since the admin API is loopback-only and outside the MCP surface this is not blocking, but it is a completeness gap.

---

## Tool / Resource / Protocol Consistency

### Tools (Section 3)

```
OK  shell_exec           -- clientId present, destructiveHint set, elicitation described, streaming (pattern b) described
OK  shell_session_start  -- clientId present, destructiveHint set, elicitation described
OK  shell_session_write  -- clientId resolved from ShellSessionMap, destructiveHint set, elicitation conditional, streaming (pattern b) described
OK  shell_session_close  -- clientId resolved from ShellSessionMap, destructiveHint false (idempotent close), no elicitation needed
OK  screenshot_capture   -- clientId present, readOnlyHint set, no elicitation needed
OK  screenshot_watch     -- clientId present, readOnlyHint set, streaming (pattern a) described, push notification type is standard notifications/progress
OK  fs_read              -- clientId present, readOnlyHint set
OK  fs_write             -- clientId present, destructiveHint set, elicitation described (conditional on overwrite)
OK  fs_list              -- clientId present, readOnlyHint set
OK  fs_delete            -- clientId present, destructiveHint set, elicitation always required (cannot be skipped)
OK  fs_stat              -- clientId present, readOnlyHint set
OK  process_list         -- clientId present, readOnlyHint set
OK  process_info         -- clientId present, readOnlyHint set
OK  process_signal       -- clientId present, destructiveHint set, elicitation described
OK  sysinfo_get          -- clientId present, readOnlyHint set
```

### Resources (Section 4)

```
OK  clients://list                 -- schema defined, backed by Device type, subscribe/listChanged described
OK  job://{id}                     -- references Job type in Section 6, subscribe described
OK  sysinfo://{clientId}/overview  -- no inline schema (previously flagged, acknowledged as-is)
OK  audit://log                    -- schema references audit.Entry, append-only, not deletable via any tool
OK  shell://sessions               -- schema defined with required fields, backed by ShellSessionEntry type, subscribe described
```

### Prompts (Section 5)

```
OK  diagnose_system  -- clientId required, references sysinfo_get and process_list (both defined)
OK  safe_cleanup     -- clientId required, references fs_list and process_list (both defined)
OK  shell_workflow   -- clientId required, references shell_session_start (defined)
```

---

## Wire Protocol Review

### JSON envelope types (Section 2.2 vs Section 6)

```
OK  hello          -- HelloPayload defined, protocolVersion field present
OK  hello_ack      -- HelloAckPayload defined, resume field present, protocolVersion echoed
OK  pair_request   -- PairRequestPayload defined (hostname only)
OK  pair_code      -- PairCodePayload defined, ExpiresAt is time.Time (units unambiguous)
OK  pair_approved  -- PairApprovedPayload defined (deviceId + deviceToken)
OK  dispatch       -- DispatchPayload defined, correlation ID via Envelope.ID, RequestID + SessionID for server-side mapping
OK  result         -- ResultPayload defined, correlation ID consistent via Envelope.ID
OK  progress       -- ProgressPayload defined, correlation ID consistent via Envelope.ID
OK  error          -- ErrorPayload defined, Code + Message fields
OK  cancel         -- CancelPayload defined, Reason field
OK  ping           -- no payload needed, Envelope.ID omitempty handles "not required" case
OK  pong           -- same as ping
OK  close          -- ClosePayload defined, Reason field
```

### Binary frame types (Section 2.2 vs Section 6)

```
OK  0x01 FrameShellStdout   -- used for shell stdout chunks, 9-byte header consistent (4-byte corr prefix + 4-byte seq + 1-byte type)
OK  0x02 FrameShellStdin    -- used for shell stdin chunks
OK  0x03 FrameScreenshotPNG -- used for screenshot binary data
OK  0x04 FrameFileContent   -- used for large file read chunks
```

Correlation: JSON `id` field (full UUIDv4) maps to binary `CorrelationPrefix` (first 4 bytes). Prefix collision handling is specified (reject second dispatch, client retries with new UUID). Stream sequence number (uint32) allows gap detection.

---

## Server Architecture Notes

**Goroutine design** is well-structured. Five goroutine roles (SSE writer, dispatch bridge, agent reader, agent writer, heartbeat) with clean separation. The single-writer-per-WebSocket pattern (agent writer goroutine) correctly avoids concurrent write races. The fan-in channel (`Session.EventCh`, buffered 256) with 5-second block-then-drop is a reasonable backpressure strategy.

**Device registry consistency**: file-backed `devices.json` with in-memory cache is adequate for the single-instance personal-fleet scale. The registry interface is clean enough to swap to Redis/Postgres for multi-replica scaling.

**Admin API isolation**: port 9090, loopback only, not proxied through nginx. Correctly separated from the MCP and agent surfaces.

**Docker/nginx config**: SSE buffering disabled (`proxy_buffering off`), WebSocket upgrade headers present, read timeout set to 86400s for long-lived connections. The docker-compose port mapping `127.0.0.1:9090:9090` correctly restricts admin API to the Docker host's loopback while the container binds `0.0.0.0:9090` (required for Docker networking). The health check endpoint `/health` on port 8080 is unauthenticated but only exposes `{ "status": "ok", "agents_online": N }`, which is acceptable for a personal tool.

---

## Agent Architecture Notes

**Per-tool executor logic**: well-partitioned by capability area under `agent/executor/`. Each executor handles its tool-specific logic locally.

**Reconnect/backoff**: exponential backoff 1s-30s with +/-20% jitter, capped. Standard and correct. Grace period (`AGENT_RECONNECT_GRACE_PERIOD`, 60s) preserves local PTY sessions and in-progress jobs during brief disconnects. Beyond grace, orphaned sessions are killed and jobs marked failed with reason `"agent_disconnect"`.

**Local token storage**: `~/.rc-mcp/agent-token`, mode `0600`. Correct permissions for a user-scope secret file.

**PTY session lifecycle across disconnects**: PTY sessions survive agent reconnect within the grace period. The server's `hello_ack` with `resume: true` signals the agent to resume streaming. Beyond the grace period, PTYs are cleaned up. The shell session map on the server is cleaned up when sessions are killed. This lifecycle is well-specified.

**Input re-validation**: the agent re-validates dispatch payloads as defense-in-depth. This is a good practice that prevents a compromised or buggy server from sending malformed dispatches.

---

## Protocol & Nx Architecture Notes

**Shared protocol package**: `internal/protocol` is imported by both `cmd/server` and `cmd/agent` within the same Go module (single `go.mod`). This prevents wire protocol drift between server and agent binaries built from the same commit. The risk is version skew when the operator upgrades the server but not the agent (or vice versa). The spec handles this: `protocolVersion` negotiation with hard rejection on mismatch (error type `"version_mismatch"`, WS close code 1002, supported versions included in error payload). This is sufficient for a personal fleet tool.

**Single-module design**: the single `go.mod` approach is the right call here. Server and agent share `internal/protocol`, `internal/mcp/types`, and potentially other internal packages. Splitting into multiple modules would create import cycle management overhead for zero benefit at this scale.

**No Nx**: this is a pure Go project with no Nx, no monorepo tooling beyond Go modules. The folder structure is clean and conventional for a Go project with two binaries (`cmd/server`, `cmd/agent`) and shared internals.

---

## Recommended Edits

**Non-blocking issue #1:** Stale prose after ShellSessionEntry fix

**Edit in:** `docs/specs/backend.md` -- Section 7: "What lives in session state"

**Change:** Replace the shell session mapping bullet:

```
From:
  - **Shell session mapping** -- map of `shellSessionId` to `clientId` (device
    ID). The actual PTY state lives on the agent, not the server. The server
    only needs to know which agent owns which shell session for routing.

To:
  - **Shell session mapping** -- map of `shellSessionId` to `*ShellSessionEntry`
    (clientId, PID, shell binary, creation time). The actual PTY state lives on
    the agent, not the server. The server caches enough metadata to route
    dispatches and serve the `shell://sessions` resource.
```

---

**Non-blocking issue #2:** Missing required array on sysinfo_get output

**Edit in:** `docs/specs/backend.md` -- Section 3.5.1: `sysinfo_get` output schema

**Change:** Add a required array after the properties block in the output schema:

```
Add after the closing brace of "properties":
      "required": ["clientId"]
```

---

**Non-blocking issue #3:** Undefined admin API endpoints

**Edit in:** `docs/specs/backend.md` -- new subsection 12.8 (after Section 12.7)

**Change:** Add:

```
### 12.8 Admin API Endpoints

Base: `ADMIN_BIND_ADDR` (default: `127.0.0.1:9090`). No authentication
(loopback-only; access controlled by bind address).

| Method | Path | Description |
|---|---|---|
| GET    | /admin/pairing/pending | List pending pairing codes |
| POST   | /admin/pairing/approve | Approve a pairing code. Body: `{ "code": "ABCD-1234" }` |
| POST   | /admin/pairing/reject  | Reject a pairing code. Body: `{ "code": "ABCD-1234" }` |
| GET    | /admin/devices         | List all paired devices |
| DELETE | /admin/devices/{id}    | Revoke a paired device |
```
