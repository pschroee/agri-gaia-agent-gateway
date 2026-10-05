# Agri-Gaia Agent Gateway

The agent gateway lets users of the [Agri-Gaia platform](https://github.com/pschroee/agri-gaia-platform) hand
tasks to a coding agent. It was built for the master's thesis *Extending a federated AI development environment
with AI agents* (Philipp Schröer, Osnabrück University of Applied Sciences) and runs as the platform service
`services/agent-gateway`.

What it does:

- **Sandboxed agent.** Each chat gets a slot of two hardened containers without network: the agent harness
  [pi](https://pi.dev) in RPC mode, and a separate execution sandbox for shell commands and file tools. The only
  way out is one Unix socket per slot.
- **Authorization service.** The orchestrator (Go) owns every token and API key. Each call the agent makes to
  the platform — through MCP tools, a CLI or the plain REST API — goes through the socket and is checked against
  the **delegation** of the chat: which actions on which objects the user handed to the agent, with an expiry
  and a provenance rule for objects the agent created itself. Everything else is rejected and logged.
- **Token exchange.** The platform sees the agent as the user it works for (`sub`) acting through the client
  `agw-agent` (`azp`), via OAuth 2.0 Token Exchange (RFC 8693) at the platform's Keycloak.
- **Confirmation by the user** for writing calls, artifacts and internet access; a web UI and a CLI.

## Login through the platform

By default the API is protected by a static token (`AGW_AUTH_MODE=token`, `AGW_API_TOKEN`). With
`AGW_AUTH_MODE=oidc` the orchestrator logs users in through the platform's Keycloak (authorization code flow with
PKCE, confidential client `agw-agent`, sessions kept server-side), so the UI can be embedded as an iframe in the
platform frontend (`AGW_FRAME_ANCESTORS`, compact layout at `/?embed=1`) and log in silently with `prompt=none`.
Chats then belong to the logged-in user, and the per-chat token exchange uses that user's own token instead of a
fixed account. Settings are listed in `.env.example`, details in `docs/design.md`.

## Layout

| Path | Content |
|---|---|
| `cmd/orchestrator` | the orchestrator (HTTP API, web UI, sockets, pool, chats) |
| `cmd/agw`, `cmd/agw-exec`, `cmd/agw-artifact` | CLI for users, helpers inside the sandboxes |
| `internal/` | packages: `delegation`, `platform`, `sandbox`, `pool`, `chat`, `llmproxy`, `store`, … |
| `images/agw-basis` | image of the agent sandbox (pi, extensions, skills) |
| `web/` | web UI (React, Vite), embedded into the orchestrator binary |
| `third_party/` | adapted copies of `pi-subagents` and `pi-intercom` (MIT, see `VENDORED.md` in each) |
| `e2e/` | end-to-end tests against a real model |
| `docs/` | design notes from the thesis |

## Development

Requires Docker and Go; `.env` from `.env.example` (never committed).

```bash
./dev.sh init            # build images, create .env values
./dev.sh start           # stack with hot reload, prints the login link
./dev.sh test            # fast tests (Go with Postgres, Vitest), about 25 s
./dev.sh test --full     # adds Docker integration and slot tests, 5–6 min, before pushing
./dev.sh e2e             # end-to-end tests with the real model
./dev.sh stop
```

The HTTP API is described in `API.md`; design decisions and their rationale in `docs/design.md`.

## License

MIT, see `LICENSE`. The copies under `third_party/` are MIT-licensed by their authors; REUSE annotations are in
`REUSE.toml`.
