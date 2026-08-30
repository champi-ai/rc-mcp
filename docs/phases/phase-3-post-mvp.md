# Phase 3: Post-MVP

## Goal
Address open questions from the spec (Section 19) and deferred features that extend the system beyond the personal single-instance deployment -- hardening, scaling, and new capability areas.

## Deliverables

### Shared protocol
- [ ] Wire protocol version bump mechanism -- if any post-MVP feature requires envelope changes, establish the version negotiation upgrade path (version `"2"` with backward-compat handshake)
- [ ] New frame type for keyboard/mouse input events (if input injection is pursued)

### Server
- [ ] **Multi-replica scaling** -- Redis-backed session store (`MCP_SESSION_STORE=redis`), Redis Pub/Sub or NATS for cross-replica dispatch routing (session-holding replica -> agent-holding replica), sticky sessions by `Mcp-Session-Id` for SSE, shared device registry (Redis or Postgres instead of file-backed JSON)
- [ ] **Shell command allowlist/denylist** -- Operator-configurable `RC_SHELL_ALLOWLIST` / `RC_SHELL_DENYLIST` (regex patterns) evaluated server-side before dispatch. Blocked commands return tool error without reaching the agent.
- [ ] **Admin web UI** -- Web interface on the admin port for pairing approval, device revocation, pending code management, device status dashboard, audit log viewer. Served as static assets or a lightweight Go template UI.
- [ ] **Full argument logging mode** -- Opt-in `RC_AUDIT_FULL_ARGS=true` for forensic audit logging (full tool arguments instead of SHA-256 digest only)
- [ ] **Global filesystem root policy** -- Server-side `RC_GLOBAL_FS_ALLOWED_ROOTS` enforced before dispatch, in addition to per-agent `AGENT_FS_ALLOWED_ROOTS`

### Agent
- [ ] **Keyboard/mouse input injection** -- New capability area (`input`), separate from `shell`/`screenshot`. Tools: `input_key`, `input_mouse_click`, `input_mouse_move`, `input_type`. Mandatory elicitation on every action. X11 implementation via `xdotool` or XTest extension. Requires new `FrameType` for input event acknowledgment.
- [ ] **Wayland screenshot support** -- Detect display server (X11 vs Wayland) at runtime. Wayland capture via `pipewire`/`xdg-desktop-portal` D-Bus API or `grim`/`slurp`. Transparent fallback: same tool surface, different agent-side implementation.
- [ ] **Agent auto-update mechanism** -- Server publishes agent binary version metadata. Agent checks on connect. If a newer version is available and the operator has opted in (`AGENT_AUTO_UPDATE=true`), the agent downloads the new binary (verified via checksum), replaces itself, and restarts via systemd. Significant security surface -- requires code signing or checksum pinning.

### Infrastructure
- [ ] Redis service in `docker-compose.yml` (under a `scaling` profile, not default)
- [ ] Load balancer configuration guidance (nginx upstream with `ip_hash` or `sticky` for session affinity)
- [ ] Agent binary release pipeline -- cross-compilation for linux/amd64, linux/arm64; GitHub Releases or a self-hosted download endpoint

## Done Definition
- **Multi-replica:** two server replicas behind nginx, agent connects to replica A, MCP client's session is on replica B, `tools/call shell_exec` succeeds end-to-end via pub/sub routing
- **Shell allowlist/denylist:** `shell_exec` with a blocked command pattern returns tool error without dispatch; allowed commands proceed normally
- **Admin web UI:** operator can approve a pending pairing code, revoke a device, and view audit log entries through a browser
- **Keyboard/mouse (if pursued):** `input_key` sends a keypress to the target device's X11 display; `input_mouse_click` clicks at coordinates; elicitation fires on every action
- **Wayland screenshots:** `screenshot_capture` works on a Wayland session (Sway, GNOME on Wayland) without X11 fallback
- **Agent auto-update:** agent connects, detects newer version, downloads and verifies binary, restarts, reconnects with new version

## Parallel work
- Multi-replica scaling (server infrastructure) is independent of keyboard/mouse input injection (agent capability)
- Wayland screenshot support (agent) is independent of shell allowlist/denylist (server policy)
- Admin web UI (server) is independent of agent auto-update (agent distribution)
- All post-MVP items are independent of each other and can be prioritized by operator need

## Phase dependencies
- Requires: Phase 2 (MVP Complete -- all tools, resources, prompts, error handling, revocation)

## Complexity
- Shared protocol: S-M (depending on input injection)
- Server: XL (multi-replica is the largest item)
- Agent: L (Wayland + input injection + auto-update)
- Infra: L (Redis, release pipeline, load balancer)

## Risks
- Multi-replica pub/sub introduces operational complexity (Redis/NATS dependency) and latency -- must benchmark dispatch round-trip time across replicas
- Agent auto-update is a significant security surface -- unsigned binary replacement could be exploited. Code signing or checksum-from-trusted-source is mandatory.
- Wayland screen capture requires portal permissions and may not work in headless or minimal compositor environments -- test matrix needed
- Keyboard/mouse input injection opens a much larger attack surface than read-only screenshots -- mandatory per-action elicitation mitigates but does not eliminate risk
- Shell allowlist/denylist regex evaluation must not introduce ReDoS vulnerability -- use Go's `regexp` (RE2, guaranteed linear time)
