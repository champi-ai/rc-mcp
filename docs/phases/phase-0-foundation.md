# Phase 0: Foundation

## Goal
Establish the Nx monorepo, shared wire protocol package, Docker Compose stack, and end-to-end pairing/auth flow -- proving that an agent can pair with and authenticate to the server, with no tool dispatch yet.

## Deliverables

### Shared protocol
- [ ] `internal/protocol/envelope.go` -- `Envelope` struct, `MessageType` enum (all 13 values), all payload types (`HelloPayload`, `HelloAckPayload`, `PairRequestPayload`, `PairCodePayload`, `PairApprovedPayload`, `DispatchPayload`, `ResultPayload`, `ProgressPayload`, `ErrorPayload`, `CancelPayload`, `ClosePayload`), JSON marshal/unmarshal with round-trip unit tests
- [ ] `internal/protocol/binary.go` -- `BinaryHeader` struct (9-byte fixed header), `BinaryFrameType` constants (`0x01`-`0x04`), `EncodeBinaryHeader`/`DecodeBinaryHeader` functions with round-trip unit tests
- [ ] `internal/protocol/version.go` -- Protocol version constant (`"1"`), version negotiation validation logic, version mismatch error generation

### Server
- [ ] `cmd/server/main.go` -- Entry point: starts HTTP server on `MCP_BIND_ADDR`, admin API on `ADMIN_BIND_ADDR`, wires dependencies, graceful shutdown on SIGTERM/SIGINT
- [ ] `internal/devices/registry.go` + `types.go` -- `DeviceRegistry` interface, file-backed JSON implementation (`DEVICE_REGISTRY_PATH`), `Device` and `PairingCode` types
- [ ] `internal/devices/pairing.go` -- Pairing code generation (crypto/rand, `XXXX-XXXX` format, no ambiguous chars), TTL enforcement (`PAIRING_CODE_TTL` default 5m), single-use semantics
- [ ] `internal/agent/hub.go` -- WebSocket upgrade on `GET /agent/ws`, accept agent connections, maintain online/offline device map
- [ ] `internal/agent/connection.go` -- Per-agent reader/writer goroutines, `hello`/`hello_ack` handshake, `pair_request`/`pair_code`/`pair_approved` flow, device token validation (bcrypt), heartbeat ping/pong (30s interval, 10s timeout)
- [ ] `internal/admin/api.go` -- Localhost-only admin API on port 9090: `POST /admin/approve` (approve pairing code), `POST /admin/reject` (reject code), `GET /admin/pending` (list pending codes)
- [ ] `internal/auth/token.go` -- Constant-time bearer token validation (`crypto/subtle`) for MCP client auth (used later but implemented now for completeness)
- [ ] `GET /health` endpoint -- Returns `200 OK` with `{ "status": "ok", "agents_online": N }`

### Agent
- [ ] `cmd/agent/main.go` -- Entry point: reads `AGENT_SERVER_URL` and `AGENT_TOKEN_PATH`, dials server, runs connection lifecycle
- [ ] `agent/client/ws.go` -- WebSocket client: connect to server, exponential backoff reconnect (1s-30s, +/-20% jitter), heartbeat ping/pong
- [ ] `agent/client/pairing.go` -- First-run pairing flow: send `pair_request`, display pairing code on stdout, wait for `pair_approved`, persist device token to `~/.rc-mcp/agent-token` (mode 0600)
- [ ] `agent/client/dispatch.go` -- Dispatch handler stub (receives `dispatch` messages, returns `error` with `"not_implemented"` -- real executors come in Phase 1)

### Infrastructure
- [ ] Root `go.mod` / `go.sum` -- Single Go module (`module github.com/<owner>/rc-mcp` or appropriate path) with initial dependency set (gorilla/websocket or nhooyr/websocket, golang.org/x/crypto for bcrypt)
- [ ] `nx.json` -- Nx workspace configuration with `targetDefaults`: `build` and `test` depend on `^build` so `internal/protocol` changes mark both `cmd/server` and `cmd/agent` as affected
- [ ] `project.json` in `cmd/server/`, `cmd/agent/`, `internal/protocol/` -- Each defines `build`, `test`, `lint` targets via `nx:run-commands` wrapping Go toolchain directly (`go build`, `go test`, `golangci-lint run`)
- [ ] Decision: use `nx:run-commands` (not a community Go plugin) unless the team identifies a compelling plugin -- document the decision in `nx.json` comments or a brief ADR
- [ ] CI pipeline (GitHub Actions or equivalent) -- Runs `nx affected -t lint,test,build` against the PR's base branch, not a blanket rebuild
- [ ] `Dockerfile` -- Multi-stage build for `rc-mcp-server` (Go 1.23 builder -> distroless/static-debian12:nonroot)
- [ ] `docker-compose.yml` -- `mcp-server` (ports 8080, 9090) + `nginx` (port 443) with TLS termination, SSE/WebSocket proxy config, named volumes for audit-log and device-registry
- [ ] `docker/nginx/nginx.conf` -- Reverse proxy config: `proxy_buffering off` for SSE on `/mcp`, WebSocket upgrade headers on `/agent/ws`, `proxy_read_timeout 86400s` on both
- [ ] `.env.example` -- Documented template of all server and agent env vars

## Done Definition
- `go test ./internal/protocol/...` passes: JSON envelope round-trips all 13 message types with field fidelity; binary header encode/decode round-trips correctly; protocol version mismatch is detected and produces the correct error
- `nx affected -t build` from `internal/protocol` change correctly marks both `cmd/server` and `cmd/agent` as affected (verify this dependency edge explicitly)
- `nx affected -t lint,test,build` runs successfully in CI against a PR
- Agent can pair with the server end-to-end: agent starts without a token -> sends `pair_request` -> server returns `pair_code` -> operator runs `rc-mcp-admin approve <code>` -> agent receives `pair_approved` with device token -> agent persists token to disk
- Agent can reconnect with its persisted device token: agent restarts -> sends `hello` with token -> server responds `hello_ack` with device ID -> device marked online
- Pairing code expiry works: unapproved code past TTL -> server sends `error` with `"pairing_expired"`, closes WebSocket
- Pairing code single-use works: approve a code, attempt to approve same code again -> rejected
- Admin API is accessible only on loopback (127.0.0.1:9090)
- Health endpoint returns agent count: `GET /health` returns `{ "status": "ok", "agents_online": 1 }` with a connected agent
- `docker compose up` starts server + nginx, agent can connect through nginx TLS

## Parallel work
- Server: agent hub + pairing flow can run alongside Agent: WS client + pairing flow, once Shared protocol: envelope types and pairing payload types are frozen
- Infrastructure: Docker/nginx/CI can proceed independently of server/agent application logic

## Phase dependencies
- Requires: none

## Complexity
- Shared protocol: S
- Server: L
- Agent: M
- Infra: M

## Risks
- WebSocket library choice (gorilla/websocket vs. nhooyr/websocket) affects the agent reader/writer pattern and binary frame handling -- decide early and document
- Nx affected graph for Go packages relies on `project.json` `implicitDependencies` or input globs matching `internal/protocol/**` -- verify the dependency edge fires correctly before moving to Phase 1
- File-backed device registry has no locking for concurrent writes -- acceptable for single-instance but needs a mutex around file writes
