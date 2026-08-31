# Release automation

Both release artifacts in this repo -- the `rc-mcp-agent` binary
([`docs/operations/agent-releases.md`](agent-releases.md)) and the
`rc-mcp-server` Docker image
([`docs/operations/server-releases.md`](server-releases.md)) -- are
versioned and released independently, driven by
[release-please](https://github.com/googleapis/release-please) reading
Conventional Commits on `main`.

## How it works

1. `.github/workflows/release-please.yml` runs on every push to `main`.
2. It parses commit messages since each component's last release and
   maintains an open PR per component (`cmd/agent`, `cmd/server`) --
   titled e.g. `chore(main): release agent 0.4.0` -- with the version bump
   and generated changelog. `fix:` commits bump patch, `feat:` bumps
   minor, `feat!:`/`BREAKING CHANGE:` bumps major, keyed by whether the
   commit touched files under that component's path.
3. Merging a release PR is the actual release trigger: release-please
   pushes the corresponding `agent-vX.Y.Z` or `server-vX.Y.Z` tag and
   creates a GitHub Release, which in turn fires
   `release-agent.yml` / `release-server.yml` via their own
   `on: push: tags:` triggers.
4. Until a release PR is merged, `main` keeps accumulating unreleased
   commits under that open PR -- nothing is tagged or published
   automatically on every merge, only on merging the release PR itself.

## Component boundaries

- `cmd/agent` -- commits touching `cmd/agent/`, `agent/`, or shared code
  the agent imports version the **agent** component.
- `cmd/server` -- commits touching `cmd/server/` or shared code the
  server imports version the **server** component.

A commit that only touches files outside both paths (docs, CI config
unrelated to either binary, etc.) doesn't move either component's release
PR. If a shared-package change should trigger a release but doesn't
appear to (because it didn't also touch the component directory), amend
the commit to also touch something under `cmd/agent/` or `cmd/server/`,
or fall back to the manual tag push documented in each component's
release doc.

## Configuration

- [`release-please-config.json`](../../release-please-config.json) --
  per-component settings (tag format, changelog path).
- [`.release-please-manifest.json`](../../.release-please-manifest.json)
  -- the last-released version per component; release-please updates this
  file itself as part of each release PR.
