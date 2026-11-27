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

## Choosing the model through the environment

Which model the gateway uses is set in `.env` (in the platform's Compose stack: the platform `.env`) and
nowhere else. `models.json` holds the DeepSeek provider; one further provider can be built from `AGW_LLM_*`
variables, so a self-hosted or a university endpoint needs no code change, no second file, and its address
never has to stand in this repository. `AGW_MODEL` names the default model as `provider/model`; the model ID of
the provider may contain slashes itself (`farm/Org/Model-27B`). Examples for three cases (DeepSeek, a
self-hosted endpoint behind a model farm, a public OpenAI-compatible service) are in `.env.example`.

| Variable | Default | Effect |
|---|---|---|
| `AGW_MODEL` | empty: `default` of `models.json` | default model for chats and titles |
| `AGW_LLM_PROVIDER` | empty: no environment provider | provider ID (`[a-z0-9-]+`, not an ID of `models.json`) |
| `AGW_LLM_BASE_URL` | required with a provider | upstream, `http(s)`, without `/chat/completions` |
| `AGW_LLM_MODEL_ID` | required | model ID at the provider |
| `AGW_LLM_API_KEY_ENV` | required | **name** of the variable that holds the key (the key itself stays in `.env` or the secret file) |
| `AGW_LLM_API` | `openai-completions` | pi API kind |
| `AGW_LLM_CONTEXT_WINDOW` | required | context window in tokens |
| `AGW_LLM_MAX_TOKENS` | required | output limit per call; must stay below the context window minus `AGW_COMPACT_RESERVE_TOKENS` |
| `AGW_LLM_INPUT` | `text` | `text` or `text,image` (the model reads images) |
| `AGW_LLM_THINKING` | `user` | `on` (always think), `off`, `user` (pi's levels) |
| `AGW_LLM_TAGS` | empty | tags the proxy sets in every request (a tag group of a model farm) |
| `AGW_LLM_SAMPLING` | empty | request fields the proxy sets in an experiment: `temperature`, `seed` |
| `AGW_LLM_SELF_HOSTED` | `false` | whether the model may see every dataset's images (otherwise only the public ones) |
| `AGW_LLM_INSTANCE_HEADERS` | empty | response headers to record with every call (the instance that served it) |
| `AGW_LLM_EXTRA_HOST` | empty | `name:ip` for Compose `extra_hosts`; the name must be the host of the base URL |
| `AGW_LLM_PROVIDER_JSON` | `{}` | further pi provider fields: `name`, `headers`, `compat` |
| `AGW_LLM_MODEL_JSON` | `{}` | further pi model fields: `name`, `thinkingLevelMap`, `inputLimits`, `cost`, `promptCache`, `samplingParams`, `headers`, `compat` |

The gateway checks everything at start and refuses to run on a mistake. The JSON fragments are checked against
a mirror of pi 0.87.1's `models.json` schema (pi itself lets unknown fields pass silently, so a typo in
`thinkingFormat` would do nothing); an unknown field, a wrong type, a value outside an enumeration, a field that
has a variable of its own (`input`, `maxTokens`, `contextWindow`, `reasoning`, …), a header that carries a secret
and a `thinkingLevelMap` next to `AGW_LLM_THINKING=on|off` are errors that name the variable and the field. A
model that has no `cost` has no list price: the plan's cost limit does not apply to it (`max_cost_usd: 0`).

Who sets which field of a request, once:

| Field | Set by | Source |
|---|---|---|
| `model`, `messages`, `tools`, `stream` | pi | the chat |
| `max_tokens` | pi | `AGW_LLM_MAX_TOKENS` |
| `chat_template_kwargs` (thinking) | pi | `thinkingFormat`, the thinking level and `AGW_LLM_THINKING` |
| other `samplingParams` (`top_p`, …) | pi | `AGW_LLM_MODEL_JSON` |
| `tags` | the proxy (overwrites whatever the agent sends) | `AGW_LLM_TAGS` |
| `temperature`, `seed` | the proxy, in an experiment only | `AGW_LLM_SAMPLING` and the run's seed |
| `Authorization` | the proxy | the variable named by `AGW_LLM_API_KEY_ENV` |

`samplingParams` may not contain a field of the proxy or of another variable. Every call is recorded with the
thinking tokens (`reasoning`), the headers named in `AGW_LLM_INSTANCE_HEADERS` (`upstream_headers`) and the
shape of the request as sent (`request_meta`: thinking on or off, `max_tokens`, `tags`), and all three are in
the experiment export.

**Effect on the experiment lock.** `catalog_sha256` is the SHA-256 of the *effective* catalog (the file, the
environment provider, `AGW_MODEL`, `AGW_LLM_EXTRA_HOST`; canonical JSON; never a key value), so switching the
model, a tag, `AGW_LLM_MAX_TOKENS`, the thinking mode or the extra host in `.env` changes it and breaks a frozen
lock. `GET /api/exp/environment` also reports `catalog_file_sha256` (the file alone) and `catalog` (the effective
catalog).

Plans may count tokens by output (`limits.token_basis: output`) instead of by uncached input plus output, for a
provider that reports cached input unreliably (`API.md`).

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
./dev.sh test            # everyday and pre-push check: Go with Postgres (cached, no -race), Vitest
./dev.sh test --full     # the same without the cache and with -race, on request
./dev.sh test --docker   # adds Docker integration, S3 and slot tests (off by default), about 7 min
./dev.sh e2e             # end-to-end tests with the real model
./dev.sh stop
```

The HTTP API is described in `API.md`; design decisions and their rationale in `docs/design.md`.

## License

MIT, see `LICENSE`. The copies under `third_party/` are MIT-licensed by their authors; REUSE annotations are in
`REUSE.toml`.
