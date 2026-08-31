# Server image releases

`rc-mcp-server` is built as a multi-arch (`linux/amd64`, `linux/arm64`)
Docker image and published to GHCR by
`.github/workflows/release-server.yml`, triggered by pushing a tag of the
form `server-vX.Y.Z` (e.g. `server-v0.3.0`).

## Published image

```
ghcr.io/champi-ai/rc-mcp:<X.Y.Z>
ghcr.io/champi-ai/rc-mcp:latest
```

## Cutting a release

**Automatic (default):** `release-please` (`.github/workflows/release-please.yml`)
watches Conventional Commits on `main` and keeps an open release PR for the
server component up to date with the next version and changelog. Merging
that PR pushes the `server-vX.Y.Z` tag itself, which triggers this build.
See [`docs/operations/releases.md`](releases.md).

**Manual override:**

```sh
git tag server-v0.3.0
git push origin server-v0.3.0
```

Either way, the workflow builds and pushes automatically once the tag
lands; no manual `docker push` step.

## Deploying a published image

`docker-compose.yml` builds the image locally from the `Dockerfile` by
default. To run a published release instead, point the `mcp-server`
service at the GHCR image rather than building:

```sh
docker pull ghcr.io/champi-ai/rc-mcp:0.3.0
docker run --env-file .env -p 127.0.0.1:8080:8080 -p 127.0.0.1:9090:9090 \
  ghcr.io/champi-ai/rc-mcp:0.3.0
```

or replace the `build:` block in `docker-compose.yml` with
`image: ghcr.io/champi-ai/rc-mcp:<X.Y.Z>` for a given deployment.
