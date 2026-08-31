# Installing the server

`rc-mcp-server` is the relay hub: it speaks MCP to your LLM client and a
WebSocket protocol to agents, and needs to run somewhere reachable by both
— typically a small always-on box or VM you control. Pick one of the
three install paths below.

## Option A: Docker Compose (recommended)

Gets you the server, TLS termination (nginx), and persistent volumes for
the audit log and device registry in one command. This is the path the
repo's [`docker-compose.yml`](../../docker-compose.yml) is built for.

**Requirements:** Docker Engine + Compose plugin.

```sh
git clone https://github.com/champi-ai/rc-mcp.git
cd rc-mcp
cp .env.example .env
```

Edit `.env`:

- `AUTH_TOKEN` — required, the server refuses to start without it.
  Generate one with `openssl rand -hex 64`.
- Everything else has a working default; see
  [`.env.example`](../../.env.example) and
  [`docs/specs/backend.md` Section 15](../specs/backend.md) for the full
  list.

nginx needs a TLS certificate. For local dev, a self-signed one is enough:

```sh
openssl req -x509 -nodes -newkey rsa:2048 \
  -keyout docker/nginx/certs/server.key \
  -out docker/nginx/certs/server.crt \
  -days 365 \
  -subj "/CN=localhost"
```

For a real deployment, put certificates from your CA (Let's Encrypt via a
separate ACME client, or an internal CA) at that same path instead — see
[`docker/nginx/certs/README.md`](../../docker/nginx/certs/README.md).

```sh
docker compose up -d
```

This builds the image locally from the `Dockerfile`. To run a published
release image instead of building, see
[`docs/operations/server-releases.md`](../operations/server-releases.md).

Verify it's up:

```sh
curl -k https://localhost/health
# {"status":"ok","agents_online":0}
```

MCP clients and agents connect through nginx on `:443`. The admin API
(`127.0.0.1:9090`) is loopback-only by design — it is never proxied, so it
stays reachable only from the server host itself. See
[Security](../../README.md#security) for why that boundary matters.

## Option B: Published Docker image, no nginx

If you already terminate TLS elsewhere (a reverse proxy, a cloud load
balancer) and just want the server container:

```sh
docker pull ghcr.io/champi-ai/rc-mcp:<X.Y.Z>   # see server-releases.md for tags
docker run -d --name rc-mcp-server \
  --env-file .env \
  -p 127.0.0.1:8080:8080 \
  -p 127.0.0.1:9090:9090 \
  -v rc-mcp-audit-log:/var/log/rc-mcp \
  -v rc-mcp-device-registry:/var/lib/rc-mcp \
  ghcr.io/champi-ai/rc-mcp:<X.Y.Z>
```

Point your own TLS-terminating proxy at `127.0.0.1:8080` for MCP/agent
traffic. Do **not** expose `9090` (admin API) through it.

## Option C: From source, as a systemd service

No Docker: build the binary and run it directly, e.g. on a small VM.

**Requirements:** Go 1.25+.

```sh
git clone https://github.com/champi-ai/rc-mcp.git
cd rc-mcp
go build -ldflags="-s -w" -o /usr/local/bin/rc-mcp-server ./cmd/server
```

Create an env file (same variables as `.env.example`) at
`/etc/rc-mcp/server.env`, then a systemd unit:

```ini
# /etc/systemd/system/rc-mcp-server.service
[Unit]
Description=rc-mcp Server
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/rc-mcp/server.env
ExecStart=/usr/local/bin/rc-mcp-server
Restart=always
RestartSec=5
DynamicUser=yes
StateDirectory=rc-mcp
LogsDirectory=rc-mcp

[Install]
WantedBy=multi-user.target
```

`DynamicUser=yes` plus `StateDirectory`/`LogsDirectory` avoid running the
server as root; set `DEVICE_REGISTRY_PATH=/var/lib/rc-mcp/devices.json`
and `RC_AUDIT_LOG_PATH=/var/log/rc-mcp/audit.log` in the env file to match
(systemd creates and owns those directories for the service's dynamic
user). You'll need your own TLS termination in front (nginx, Caddy,
Traefik) — this path doesn't include one.

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now rc-mcp-server
sudo systemctl status rc-mcp-server
```

## Next steps

- [Install an agent](install-agent.md) on a machine you want to control.
- [`docs/operations/scaling.md`](../operations/scaling.md) — running more
  than one server replica behind a load balancer.
