# Security Policy

rc-mcp gives an LLM client shell, filesystem, process, and (opt-in) input
access to machines you control. Vulnerabilities here are taken
seriously — please report them privately, not as a public issue.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting for this repository:

**[Report a vulnerability](https://github.com/champi-ai/rc-mcp/security/advisories/new)**
(also under the repo's **Security** tab → **Report a vulnerability**).

This opens a private draft security advisory visible only to you and the
maintainers — nothing is public until a fix is out and you both agree to
publish it.

Don't open a public GitHub issue for a suspected vulnerability, and don't
post it anywhere else public (forums, chat, etc.) before it's been
reported and addressed.

Please include:

- The affected component (`rc-mcp-server`, `rc-mcp-agent`, or the shared
  wire protocol) and version/commit.
- Steps to reproduce, or a proof of concept.
- The impact as you see it (e.g. auth bypass, sandbox escape, privilege
  escalation, information disclosure).

## Scope

In scope:

- `rc-mcp-server` and `rc-mcp-agent` (this repository).
- The WebSocket wire protocol and pairing/auth flow between them.
- The MCP tool surface exposed to LLM clients, including the
  confirmation/elicitation and audit-logging guarantees described in the
  [README's Security section](README.md#security).

Out of scope:

- Vulnerabilities requiring an attacker to already control the operator's
  MCP client or AUTH_TOKEN — that's the trust boundary the whole design
  sits inside (see the README: single-operator, not multi-tenant).
- Third-party dependencies — report those upstream; feel free to also
  flag it here if rc-mcp needs to react (e.g. bump a version).

## Supported versions

This project doesn't yet maintain long-lived release branches; security
fixes land on the latest release of each component
([`rc-mcp-agent`](docs/operations/agent-releases.md) /
[`rc-mcp-server`](docs/operations/server-releases.md)). Please make sure
you're on the latest tagged version before reporting.

## Response

There's no formal SLA, but reports are triaged as they come in. Expect an
initial response acknowledging the report, followed by either a fix
timeline or a request for more information.
