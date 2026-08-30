# ADR 0001: Nx <-> Go integration via nx:run-commands

## Decision

Use the built-in `nx:run-commands` executor to wrap the plain Go toolchain
(`go build`, `go test`, `golangci-lint run`) in each project's `project.json`,
rather than adopting a community Nx Go plugin (e.g. `@nx-go/nx-go`).

## Rationale

- `rc-mcp` is a single Go module with three logical projects (`cmd/server`,
  `cmd/agent`, `internal/protocol`). The only thing we need from Nx is the
  affected-graph computation (`nx affected -t lint,test,build`) driven by
  `implicitDependencies`/`namedInputs` -- we do not need Nx to manage Go
  module resolution, workspace generation, or code generators.
- `nx:run-commands` has zero extra dependencies beyond `nx` itself, is fully
  declarative, and is trivial to reason about: each target is just the Go
  command a developer would run by hand.
- Community Go plugins add an extra moving part (plugin version compatibility
  with the installed Nx version) for a benefit (generators, richer caching
  heuristics) this project does not need at its current size.

## Revisit if

- The Go module is split into multiple `go.mod` files (a Go workspace), where
  a plugin's built-in dependency inference might outperform manually
  maintained `implicitDependencies`.
- The team wants Nx-native Go project generators for scaffolding new
  packages.
