# Phase 2: MVP Complete

## Goal
Ship all remaining tool categories (screenshot, filesystem, process, sysinfo), PTY-backed interactive shell sessions, the `clients://list` device registry resource, device revocation, long-running job infrastructure (dispatch pattern a), all resources and prompts, and the full error-handling taxonomy -- completing the MVP feature set.

## Deliverables

### Shared protocol
- [ ] No new envelope or binary header types needed -- all frame types (`FrameScreenshotPNG`, `FrameFileContent`) were defined in Phase 0

### Server
- [ ] `internal/mcp/tools/shell_session.go` -- `shell_session_start`, `shell_session_write`, `shell_session_close` handlers: server-side `ShellSessionMap` management (shellSessionId -> clientId routing), elicitation on start (unless `RC_SHELL_SKIP_CONFIRM`), optional per-write confirmation (`RC_SHELL_CONFIRM_EVERY_WRITE`), max concurrent sessions enforcement (`RC_MAX_SHELL_SESSIONS`, default 5), two-hop PTY output streaming via binary frames
- [ ] `internal/mcp/tools/screenshot.go` -- `screenshot_capture` (synchronous dispatch, binary PNG frame -> base64 MCP image content response) and `screenshot_watch` (dispatch pattern a: return jobId immediately, stream PNG frames as `notifications/progress`, terminal job result persisted to job store)
- [ ] `internal/mcp/tools/fs.go` -- `fs_read`, `fs_write`, `fs_list`, `fs_delete`, `fs_stat` handlers: JSON Schema validation, elicitation for `fs_write` overwrite (pre-check via `fs_stat` dispatch) and `fs_delete` (always, with `confirmPath` match), binary file content frame assembly for large reads, dispatch and bridge
- [ ] `internal/mcp/tools/process.go` -- `process_list`, `process_info`, `process_signal` handlers: elicitation for `process_signal` (unless `RC_PROCESS_SKIP_CONFIRM`), self-signal protection (agent rejects signaling its own PID)
- [ ] `internal/mcp/tools/sysinfo.go` -- `sysinfo_get` handler: synchronous dispatch, partial-result tolerance (null sections if agent cannot read /proc subsystem)
- [ ] `internal/jobs/store.go` + `memory_store.go` + `types.go` -- `JobStore` interface and in-memory implementation: `Create`, `Get`, `UpdateStatus`, `ListBySession`. `Job` struct with full lifecycle (pending -> running -> succeeded/failed/cancelled). Idempotency key enforcement (`SHA-256(sessionId:tool:requestId)`). `JOB_TIMEOUT` enforcement (default 300s, server-side timer -> cancel to agent).
- [ ] `internal/mcp/resources/clients.go` -- `clients://list` resource: reads from `DeviceRegistry.List()`, emits `notifications/resources/updated` on device connect/disconnect/capability change via `Session.EventCh`
- [ ] `internal/mcp/resources/job.go` -- `job://{id}` resource: reads from `JobStore.Get()`, supports `resources/subscribe` + `notifications/resources/updated` on job status change
- [ ] `internal/mcp/resources/sysinfo.go` -- `sysinfo://{clientId}/overview` resource: dispatches to agent on read, supports subscribe (updated every 30s while subscribed and device online)
- [ ] `internal/mcp/resources/audit.go` -- `audit://log` resource: cursor-based pagination via offset parameter, newest-first, subscribe for new entries
- [ ] `internal/mcp/resources/shell.go` -- `shell://sessions` resource: lists active shell sessions in the current MCP session, subscribe on open/close
- [ ] `internal/mcp/prompts/diagnose_system.go` -- `diagnose_system` prompt: dynamic template built from device sysinfo snapshot and symptom argument
- [ ] `internal/mcp/prompts/safe_cleanup.go` -- `safe_cleanup` prompt: dynamic template calling fs_list + process_list
- [ ] `internal/mcp/prompts/shell_workflow.go` -- `shell_workflow` prompt: static preamble + shell_session_start setup
- [ ] Device revocation -- `rc-mcp-admin revoke <deviceId>` via admin API (`DELETE /admin/devices/{id}`), removes device from registry, closes agent WS on next heartbeat/reconnect, emits `notifications/resources/updated` for `clients://list`
- [ ] Full error taxonomy implementation -- all error categories from Section 13 (protocol errors with JSON-RPC codes, tool errors with `isError: true`, job failures, elicitation declined/timeout), consistent error shapes
- [ ] `notifications/resources/list_changed` -- emitted when agents connect/disconnect or change capabilities (tool list changes)
- [ ] Agent reconnect grace period -- server holds in-flight dispatch state for `AGENT_RECONNECT_GRACE_PERIOD` (default 60s), `hello_ack` with `resume: true` on reconnect within grace period, orphaned jobs marked `failed` with `"agent_disconnect"` beyond grace period
- [ ] Input validation -- JSON Schema validation in transport layer before tool handler (returns `-32602`), consistent for all tools

### Agent
- [ ] `agent/executor/shell.go` (extend) -- PTY allocation for `shell_session_start`: spawn shell process (`$SHELL` or `/bin/bash`), configurable rows/cols, session lifecycle management. `shell_session_write`: write to PTY stdin, stream PTY output as binary frames (200ms or 4KB intervals), detect idle (2s no output) or read timeout (30s). `shell_session_close`: SIGTERM then SIGKILL after 5s, return exit code and final buffered output. Max sessions enforced agent-side.
- [ ] `agent/executor/screenshot.go` -- `screenshot_capture`: X11 display capture (via `xdpyinfo` + `import` or Go X11 bindings), configurable display/monitor/quality/maxWidth, returns PNG bytes as binary `FrameScreenshotPNG` frame. `screenshot_watch`: periodic capture loop at configured interval, sends PNG binary frames, respects maxFrames/durationSecs limits, handles cancel.
- [ ] `agent/executor/fs.go` -- `fs_read`: read file with offset/limit, detect encoding (utf8/base64), send large files as binary `FrameFileContent` frames. `fs_write`: write/append with configurable mode/permissions, create parent dirs. `fs_list`: directory listing with optional recursion, depth limit, hidden files, entry limit. `fs_delete`: delete file or recursive directory delete. `fs_stat`: stat with optional symlink follow. Path traversal protection: resolve to absolute path, enforce `AGENT_FS_ALLOWED_ROOTS` if configured.
- [ ] `agent/executor/process.go` -- `process_list`: read from `/proc`, filter by name/user, sort by pid/cpu/memory/name, limit results. `process_info`: detailed info for a single PID (cmdline, exe, cwd, threads, fds, environ). `process_signal`: send signal (SIGTERM/SIGKILL/SIGHUP/SIGINT/SIGUSR1/SIGUSR2/SIGSTOP/SIGCONT), reject signaling agent's own PID.
- [ ] `agent/executor/sysinfo.go` -- Gather hostname, OS info, uptime, CPU (model/cores/threads/usage/load), memory (total/used/available/swap), disk mounts, network interfaces. Section-selective gathering.
- [ ] Agent reconnect with state preservation -- keep local shell sessions and in-progress screenshot_watch alive during disconnect for `AGENT_RECONNECT_GRACE_PERIOD` (default 60s). On reconnect within grace period, resume streaming. Beyond grace period, kill orphaned shell sessions, clean up resources.
- [ ] Agent-side input re-validation -- defense-in-depth: validate dispatch payload fields before executing (never trust the wire)

### Infrastructure
- [ ] `completions` capability -- argument auto-completion for `clientId` (from device registry), file paths (via agent dispatch for fs tools)
- [ ] Add `project.json` for new Nx project boundaries as needed (e.g. `internal/jobs/`, `agent/executor/`)

## Done Definition
- **Shell sessions:** MCP client can `shell_session_start` -> confirm elicitation -> receive shellSessionId -> `shell_session_write` with `"echo hello\n"` -> receive progress with stdout -> `shell_session_write` with `"pwd\n"` -> cwd is preserved -> `shell_session_close` -> exitCode 0
- **Screenshot capture:** `screenshot_capture` with valid clientId returns MCP image content (PNG, base64-encoded)
- **Screenshot watch:** `screenshot_watch` returns jobId immediately, progress notifications deliver PNG frames, terminal notification fires on completion, `job://{jobId}` resource reflects final status
- **Filesystem:** `fs_read` returns file content; `fs_write` creates a file (elicitation on overwrite); `fs_list` returns directory entries; `fs_delete` requires `confirmPath` match in elicitation; `fs_stat` returns metadata
- **Process:** `process_list` returns process entries; `process_info` returns detailed PID info; `process_signal` requires elicitation, rejects signaling agent PID
- **Sysinfo:** `sysinfo_get` returns system overview with all sections
- **Device registry resource:** `clients://list` returns all paired devices with online/offline status; `notifications/resources/updated` fires on connect/disconnect
- **Device revocation:** `rc-mcp-admin revoke <deviceId>` removes device, agent auth fails on next attempt, device disappears from `clients://list`
- **Prompts:** `prompts/list` returns all three prompts; `prompts/get diagnose_system` returns a dynamic template
- **Resources:** `resources/list` returns all five resource URIs; each is readable and subscribable
- **Error taxonomy:** device offline -> tool error `isError: true`; capability disabled -> tool error; invalid params -> `-32602`; agent disconnect mid-op -> tool error with `"agent_disconnect"`
- **Reconnect resilience:** agent disconnects mid-shell-session, reconnects within 60s -> shell session resumes; beyond 60s -> shell session cleaned up, write returns tool error
- All tests pass with `go test -race ./...`

## Parallel work
- Server: screenshot/fs/process/sysinfo tool handlers can be built in parallel with Agent: screenshot/fs/process/sysinfo executors, since the dispatch bridge from Phase 1 is stable and the DispatchPayload/ResultPayload shapes are frozen
- Server: shell_session handlers can be built alongside Server: job store (they are independent server subsystems)
- Server: resources + prompts can be built independently of tool handlers (they read from existing stores/registry)
- Agent: all five executor modules (shell PTY, screenshot, fs, process, sysinfo) can be built in parallel -- they share only the dispatch routing layer from Phase 1

## Phase dependencies
- Requires: Phase 1 (MCP transport, session management, dispatch bridge, shell_exec round-trip, elicitation flow, audit logger)

## Complexity
- Shared protocol: S (no changes)
- Server: XL
- Agent: XL
- Infra: S

## Risks
- PTY management on the agent is OS-sensitive -- `os/exec` + `github.com/creack/pty` is the standard Go approach but needs testing across distros
- X11 screenshot capture library choice affects build dependencies (CGo vs pure Go) -- evaluate `github.com/kbinani/screenshot` or shelling out to `import`/`scrot` as a pragmatic alternative
- `screenshot_watch` at 500ms intervals for 5 minutes generates up to 600 PNG frames -- memory pressure on server-side replay buffer and SSE stream must be tested
- `fs_write` elicitation requires a pre-check `fs_stat` dispatch to the agent to determine if the file exists -- this adds a round-trip before the actual write; ensure the elicitation flow handles this two-step cleanly
- The volume of new tool handlers and executors is large -- prioritize tools by standalone testability (sysinfo and process_list are pure reads and easiest to validate, shell sessions and screenshot_watch are most complex)
