# Server releases

`rc-mcp-server` is published two ways by
`.github/workflows/release-server.yml`, triggered by pushing a tag of the
form `server-vX.Y.Z` (e.g. `server-v0.3.0`): a multi-arch Docker image
(the primary path, matching `docker-compose.yml`) and standalone binaries
(for a from-source/systemd install, no Docker needed -- see
[`docs/guides/install-server.md`](../guides/install-server.md) Option C).
Both report the same version at startup (`rc-mcp-server: version X.Y.Z`,
`dev` for a local build), stamped in from the release tag.

## Published image

Multi-arch (`linux/amd64`, `linux/arm64`):

```
ghcr.io/champi-ai/rc-mcp:<X.Y.Z>
ghcr.io/champi-ai/rc-mcp:latest
```

## Published binaries

For every supported architecture (`linux/amd64`, `linux/arm64`), attached
to the GitHub Release for the tag:

- `rc-mcp-server-linux-<arch>` — the binary, stripped and built with
  `-trimpath` (`CGO_ENABLED=0`, no cgo dependencies).
- `rc-mcp-server-linux-<arch>.sha256` — a standalone checksum file for
  that one binary (`sha256sum` format:
  `<hex digest>  rc-mcp-server-linux-<arch>`).
- `SHA256SUMS` — combined checksums for every binary in the release, for
  verifying by hand with `sha256sum -c SHA256SUMS`.

```sh
version=0.3.0
arch=amd64  # or arm64
base="https://github.com/champi-ai/rc-mcp/releases/download/server-v${version}"

curl -fLO "${base}/rc-mcp-server-linux-${arch}"
curl -fLO "${base}/rc-mcp-server-linux-${arch}.sha256"
sha256sum -c "rc-mcp-server-linux-${arch}.sha256"
chmod +x "rc-mcp-server-linux-${arch}"
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
