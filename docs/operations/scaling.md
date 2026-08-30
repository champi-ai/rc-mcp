# Multi-replica scaling

The default `docker compose up` runs a single `mcp-server` instance with an
in-memory session store and a file-backed device registry, exactly as
`docs/specs/backend.md` describes. This document covers the opt-in
infrastructure for running more than one replica behind nginx.

Bringing this up does **not** change default single-instance behavior:
the `redis` service is gated behind the `scaling` Compose profile, and the
second replica plus the sticky-session nginx config live entirely in
`docker-compose.scaling.yml`, an overlay file that only applies when
explicitly included.

## What's here vs. what's still needed

This infrastructure layer (Redis service + sticky-session nginx routing)
is necessary but not sufficient for a correct multi-replica deployment on
its own:

| Piece | Status |
|---|---|
| Redis service, opt-in via the `scaling` profile | done (this doc) |
| nginx sticky sessions (`ip_hash`, keying an MCP session to one replica) | done (this doc) |
| Device registry backed by Redis, consistent across replicas (`MCP_SESSION_STORE=redis`) | done -- `internal/devices.RedisRegistry` |
| Session metadata mirrored to Redis with a TTL matching `MCP_SESSION_IDLE_TIMEOUT` (`MCP_SESSION_STORE=redis`) | done -- `internal/session.RedisStore` |
| Cross-replica dispatch routing (an MCP session on replica A reaching an agent connected to replica B) | done -- `internal/agent.ReplicaBridge` |

All four pieces share the single `MCP_SESSION_STORE=redis` toggle -- there
is no separate flag for cross-replica dispatch routing. With it enabled,
every server replica is a fully interchangeable member of the fleet: any
replica can serve any MCP session and successfully dispatch to any agent,
regardless of which replica that agent is physically connected to.

`MCP_SESSION_STORE=redis` switches **both** the device registry and the
session store to their Redis-backed implementations at once (see
`cmd/server/main.go`); there is no separate toggle for each. Set
`REDIS_ADDR` to point at the shared instance (`redis:6379` when using the
`scaling` overlay above). The server pings Redis at startup and refuses to
start if it's unreachable, rather than discovering that on the first
request.

**Caveat inherited from the session store's own design, still true today:**
an MCP session's live state -- its SSE event channel, replay buffer, and
in-flight dispatch bridges -- lives entirely in the process memory of
whichever replica created it. `RedisStore` mirrors session *metadata*
(existence, negotiated version, last-activity, TTL) to Redis for
cluster-wide visibility, but it cannot reconstruct a working session on a
different replica from that mirror. A session created on replica A still
only works when requests for it land back on replica A, which `ip_hash`
gives you as long as the client's source IP doesn't change mid-session.
This is why the nginx sticky-session layer above remains necessary even
with cross-replica dispatch routing in place: routing solves "the agent I
need is on a different replica than me," not "my own session can live on
any replica."

The device registry has no such caveat: `RedisRegistry` is genuinely
consistent across replicas, since a device record has no live
process-local state to lose (see the two-replica consistency test in
`internal/devices/redis_registry_test.go`).

## Cross-replica dispatch routing

`internal/agent.ReplicaBridge` wraps the normal local dispatch bridge:

- A dispatch to a device connected to *this* replica goes straight through
  the existing in-process path (Section 8) -- zero added latency, exactly
  as in single-instance mode.
- A dispatch to a device connected to a *different* replica is relayed via
  Redis Pub/Sub: the request is published to that replica's request
  channel, the owning replica runs the dispatch against its own live
  WebSocket connection to the agent, and progress/binary/result messages
  are published back to a channel scoped to that one dispatch's
  correlation ID.
- Each replica tracks which devices it currently holds a connection for in
  a Redis key per device (`rc-mcp:agent-location:<deviceId>`), refreshed
  on a heartbeat while connected and cleared on disconnect, so any replica
  can look up who currently owns a given agent's connection.
- `notifications/cancelled` still works across the relay: cancelling a
  relayed dispatch publishes a cancel message to the owning replica, which
  cancels its local dispatch exactly as if the request had originated
  there.

**Measured latency:** relaying adds one Redis publish + one Redis-delivered
message round trip per dispatch, on top of the existing agent WebSocket
round trip. Each relayed dispatch logs its total duration
(`agent: relayed dispatch <id> (tool=..., replica A -> B) took <duration>`)
so an operator can observe the actual overhead in their own deployment
rather than relying on a number measured against a different network
topology; in the test suite (`internal/agent/relay_test.go`, using an
in-process pub/sub fake rather than a real Redis network hop) the relay
overhead itself is sub-millisecond, so in a real deployment the dominant
added cost is expected to be Redis's own network round trip (typically
low single-digit milliseconds for a Redis instance in the same
datacenter/VPC as the server replicas).

## Bringing it up

```sh
docker compose -f docker-compose.yml -f docker-compose.scaling.yml \
  --profile scaling up --build
```

This starts: `redis`, two `mcp-server` replicas (`mcp-server` and
`mcp-server-2`), and `nginx` configured with both replicas in its
upstream and `ip_hash` sticky routing (`docker/nginx/nginx.scaling.conf`).

To go back to the default single-instance setup, just omit both the `-f
docker-compose.scaling.yml` and `--profile scaling`:

```sh
docker compose up
```

## Verifying sticky routing with two replicas

1. Bring up the scaling stack (above).
2. Confirm both replicas are healthy:
   ```sh
   docker compose -f docker-compose.yml -f docker-compose.scaling.yml ps
   ```
3. From one client (so nginx's `ip_hash` sees one source IP), open an MCP
   session (`POST /mcp` with `initialize`) and note the `Mcp-Session-Id`
   response header.
4. Tail each replica's logs (`docker compose logs -f mcp-server` and
   `... mcp-server-2`) while sending several more requests carrying that
   same `Mcp-Session-Id` header. All of them should log against the same
   replica — the one `ip_hash` picked for your client IP on the first
   request.
5. Repeat step 3 from a different source IP (a second machine, or `docker
   compose exec` into a throwaway container on the same Docker network)
   and confirm it can land on the *other* replica, demonstrating nginx is
   actually load-balancing rather than always picking one instance.

## Sticky-cookie alternative

`docker/nginx/nginx.scaling.conf` documents (in comments) a
`Mcp-Session-Id`-keyed alternative to `ip_hash`, for deployments where
many MCP clients share one source IP (a shared NAT/proxy) and `ip_hash`
would incorrectly pin them all to the same replica.
