# Agent binary releases

`rc-mcp-agent` binaries are cross-compiled and published to GitHub Releases
by `.github/workflows/release-agent.yml`, triggered by pushing a tag of the
form `agent-vX.Y.Z` (e.g. `agent-v0.3.0`).

## Published artifacts

Each release publishes, for every supported architecture
(`linux/amd64`, `linux/arm64`):

- `rc-mcp-agent-linux-<arch>` — the binary, stripped and built with
  `-trimpath` (`CGO_ENABLED=0`, no cgo dependencies).
- `rc-mcp-agent-linux-<arch>.sha256` — a standalone checksum file for that
  one binary, in the format `sha256sum` produces
  (`<hex digest>  rc-mcp-agent-linux-<arch>`).
- `SHA256SUMS` — the combined checksums for every binary in the release,
  for an operator verifying by hand with `sha256sum -c SHA256SUMS`.

The binary reports its own version at startup (`rc-mcp-agent: version
X.Y.Z`), stamped in at build time from the release tag.

## Download URL pattern

```
https://github.com/<org>/rc-mcp/releases/download/agent-v<X.Y.Z>/rc-mcp-agent-linux-<arch>
https://github.com/<org>/rc-mcp/releases/download/agent-v<X.Y.Z>/rc-mcp-agent-linux-<arch>.sha256
```

This is the URL pattern the auto-update mechanism (`AGENT_AUTO_UPDATE`)
relies on to fetch and verify a new version, and that an operator can use
to download and verify a binary by hand:

```sh
version=0.3.0
arch=amd64  # or arm64
base="https://github.com/<org>/rc-mcp/releases/download/agent-v${version}"

curl -fLO "${base}/rc-mcp-agent-linux-${arch}"
curl -fLO "${base}/rc-mcp-agent-linux-${arch}.sha256"
sha256sum -c "rc-mcp-agent-linux-${arch}.sha256"
chmod +x "rc-mcp-agent-linux-${arch}"
```

## Cutting a release

```sh
git tag agent-v0.3.0
git push origin agent-v0.3.0
```

The workflow builds and publishes automatically; no manual upload step.

## Auto-update

An agent with `AGENT_AUTO_UPDATE=true` checks the version the server
advertises in every `hello_ack` (`AGENT_LATEST_VERSION` on the server,
unset by default -- the server advertises nothing and no agent update
check has anything to react to). When they differ, the agent:

1. Downloads `<AGENT_UPDATE_BASE_URL>/agent-v<version>/rc-mcp-agent-linux-<arch>`
   and its `.sha256` (default `AGENT_UPDATE_BASE_URL`:
   `https://github.com/champi-ai/rc-mcp/releases/download`, i.e. the
   pattern above).
2. Verifies the downloaded binary's SHA-256 against the published
   checksum. A mismatch aborts the update entirely -- the agent keeps
   running its current binary and never installs an unverified one.
3. On success, atomically replaces its own executable and restarts via
   `systemctl restart <AGENT_SYSTEMD_UNIT>` (default `rc-mcp-agent`),
   falling back to re-executing the new binary in-place if systemd isn't
   available.
4. On any failure (network, missing release, checksum mismatch), logs a
   warning and continues running the current binary; it retries on the
   next connect.

With `AGENT_AUTO_UPDATE` unset or `false` (the default), none of this
runs: no version check, no download, no request to the release endpoint
at all.
