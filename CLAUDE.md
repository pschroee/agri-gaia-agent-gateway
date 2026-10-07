<!--
SPDX-FileCopyrightText: 2026 Philipp Schröer

SPDX-License-Identifier: MIT
-->

# Agri-Gaia Agent Gateway — working notes for agents

This repository is the agent gateway of the master's thesis (see `README.md`). It is used as submodule
`services/agent-gateway` of [pschroee/agri-gaia-platform](https://github.com/pschroee/agri-gaia-platform)
(branch `ki-agents`); branch model and platform conventions are in that repository's `CLAUDE.md`.

## Origin

Imported on 2026-10-05 from `poc/` of the thesis repository (private, HS GitLab) at commit `a2962bc`. The
history before that point lives there. The design notes came along under `docs/` and
have since been translated to English (`docs/design.md` was `poc/README.md`); paths like `poc/…` in those notes
refer to this repository.

## Conventions

- **Branches:** `main` is the integration branch; feature branches (`feat/…`, `fix/…`) come back by pull
  request (`gh pr create --base main`). The platform's submodule pointer on `ki-agents` is moved after a merge.
- **Commit messages in English**, short, one line, like the platform (`added …`, `fixed …`).
- **Everything in English:** code, comments, log and error messages, UI texts, skills, prompts and docs. Only
  test inputs that deliberately check German user input stay German. The agent still replies in the user's
  language.
- **Licensing:** MIT. `REUSE.toml` annotates all files; third-party copies keep their own `LICENSE`.
- **Own and foreign code apart:** anything taken from elsewhere lands unchanged in its own commit under
  `third_party/`, own changes in a separate commit and listed in that directory's `VENDORED.md`. Never put
  copies under `vendor/` (switches Go to vendor mode).
- **No secrets, no instance data:** keys only in `.env` (ignored) or, in the platform, in
  `secrets/agent-gateway.env`. Before every commit: `git diff --cached | grep -c 'sk-'` must print 0. No host
  names of private instances and no test credentials in code, docs or tests.

## Login

- The `return` target of `/oidc/login` must be an absolute path on the same host (`internal/oidc`, `safeReturn`):
  the UI under the base and, under a path prefix, any page of the platform frontend on that host (`/ai-agent`).
  Schemes, hosts, `//`, backslashes, dot segments and percent-encoded slashes, backslashes, dots or control
  characters fall back to the UI's start page. Rules and examples in `API.md`, *Return after login*; new cases go
  into `internal/oidc/return_test.go`.

## Page context

- The platform UI sends the current page and the open or selected object with a message (`context` of
  `POST …/messages`, issue #13). `chat.ParsePageContext` checks it against fixed lists (pages, kinds, integer ids, one-line
  names up to 200 characters, no unknown fields); it goes to pi as a `page_context` note with `audience: "agent"` before
  the text it belongs to, the name in a fence, and is stored on queue entries (`chat_queue.context`) and in the source.
  New pages or kinds go into `contextPages`/`contextKinds` in `internal/chat/pagecontext.go` with a test. **It never
  grants rights**; the delegation stays authoritative (`TestPageContextGrantsNoAccess`). Rules in `API.md`, *Page context*.

## Subagent limit

- At most `AGW_MAX_SUBAGENTS` (default 5) subagents run **at the same time** per chat; fixed for the service, no
  setting per chat (issue #24). The stored `chats.max_subagents` of older chats is ignored, `POST /api/chats/{id}/subagents`
  answers 410. The monitoring counts running runs in memory per sandbox (`runningSubagents` in
  `internal/chat/subagents.go`), not runs started in total; do not reintroduce `maxSubagentSpawnsPerSession` in the
  pi-subagents config, it caps runs in total. Rules in `API.md`, *Limit for subagents*.

## Bindings (toolsets)

- `AGW_TOOLSETS` (default `cli`) fixes the bindings of every new chat as any combination of `cli`, `mcp`, `api`
  (issue #29); there is no choice per chat. The canonical key (order cli, mcp, api: `cli,api`) is what chats store as
  `variant` and what the pool is keyed by; `both` of older chats means `cli,mcp`. Parsing and keys in
  `internal/toolset`, tools and pi arguments in `internal/worker` (`Tools`, `piArgs`). `cli,mcp` must stay exactly the
  former `both` (`TestOldVariantsUnchanged` holds the old arguments literally); a change to a binding's tools goes into
  `bindingTools` and that test. Old chats of any combination resume through slots started on demand
  (`pool.SetKnown`). Rules in `API.md`, *Bindings of new chats*.

## Creating chats

- `POST /api/chats` with `async: true` (issue #30) returns at once also when the pool is empty: the chat is marked
  `starting`/`resuming` and `startLater` assigns the slot in the background while holding the chat lock, so a message
  sent meanwhile waits in `ensureLive` instead of taking a second slot. With a free slot it is assigned before the
  response. Keep slot assignment cheap: anything that is the same for every chat belongs into `worker.Create` (slot
  start), not into `attachSlot` (pi-subagents' config moved there; internet off is skipped because a fresh slot never
  had egress). The pool starts every replacement on its own (`Pool.fill` does not wait for a batch). Rules in
  `API.md`, *Creating a chat without waiting*.

## Activity across chats

- `GET /api/activity` (issue #12) reads platform calls of the user's chats in one query (`internal/store/activity.go`):
  the owner filter is part of the SQL (`chats.owner`), never applied afterwards in Go, so a page size and the
  summary stay right. The outcome classification (`outcomeSQL`) mirrors the result texts written in
  `sock.runPlatform`; a new result text there needs a case there and in `TestListActivity`. `duration_ms` is the
  round trip of `Platform.Do` handed from `platform.Result.Duration` to the log entry through the context
  (`store.WithDuration`); it never reaches the agent (`json:"-"`). Rules in `API.md`, *Activity across chats*.

## Testing

- `./dev.sh test` is the everyday check, before every commit and before every push: Go with Postgres (with Go's
  test cache, without `-race`) and web tests.
- `./dev.sh test --full`: the same stages without the cache (`-count=1`) and with `-race`, about 30 s. Not part of
  the workflow for now (decision of the author, 06.10.2026); run it only on request, e.g. after concurrency changes,
  or when in doubt about the cache.
- **Go's test cache** keys on the package files (embedded ones such as `internal/store/schema.sql` included) and on
  env vars a test reads via `os.Getenv` (`AGW_TEST_DATABASE_URL` and the like), but not on the contents or state of
  Postgres outside the repo. A cached `ok` therefore does not prove the tests still pass against the running
  database; if in doubt, use `--full`.
- **Docker integration, S3 and slot tests are off by default** (issue #27, decision of the author, 06.10.2026):
  they build and start real containers, loaded the machine and took most of the former 450 s (Docker integration
  124 s, S3 42 s, slot tests 203 s). The tests stay in the repo; `./dev.sh test --docker` runs them in addition
  (about 7 min). Use it only when a change touches what they cover: `internal/sandbox`, the workspace round trip,
  `internal/artifacts` (S3), `internal/worker` and the slot images (`images/`, `exec-bridge.ts`, pi settings), or
  when asked. `./dev.sh test --docker --dry-run` (also with no flag or `--full`) only lists the stages, without
  starting anything. Every run ends with a line naming the skipped stages.
- **Test logs:** the terminal of `./dev.sh test` shows a filtered view; the complete output of every stage is in
  `.dev/test-logs/<run>/<n>-<stage>.log` (ignored, last 10 runs kept), and a failing stage prints its log path.
  Read the failure message there instead of re-running with `tail` (issue #26: a slot test failed once and its
  message was lost).
- **No fixed sleep before asserting something asynchronous:** poll up to a generous deadline instead. A
  `time.Sleep(1600ms)` before checking a marker written after `sleep 1.5` failed once in 20 plain runs
  (`TestServeBgLimitAndStop`, issue #26); the abort was not at fault, the other process groups stay alive. A
  fixed sleep is only acceptable before a negative check ("did not happen"), where it can at worst pass falsely,
  not flake.
- Do not save Go files while `./dev.sh test --docker` or `./dev.sh e2e` runs: each orchestrator restart removes all
  containers labelled `agwpoc.managed=true`.
- Do not run `npx prettier --write` on `web/`: there is no Prettier config; the code is hand-formatted at
  120 columns without semicolons. Check with `tsc -b` and `npm run lint`.
- **`./dev.sh test` from a second checkout (git worktree) is not isolated:** the compose project name is fixed
  (`agwpoc`), so `dc up postgres rustfs` there targets the stack of the main checkout from another directory, and
  without a `.env` it would create one with a new random Postgres password. In a worktree run the fast stage by
  hand against the running Postgres instead: `AGW_TEST_DATABASE_URL` with the main checkout's `POSTGRES_PASSWORD`,
  `go test -race -count=1 ./...` (each test uses its own schema, so parallel runs do not collide) and
  `cd web && npm test -- --run`; build `web/dist` once first (`npm ci && npm run build` in `web/`). The full stage
  (shared images `agwpoc/*:dev`, label cleanup) belongs to one checkout at a time.
- Networks of the stack and of tests are fixed in `10.231.0.0/16`; create test networks only through
  `sandbox.CreateTestNetwork`.

Further pitfalls (Docker Desktop on macOS, pi, pi-subagents, token exchange, Agri-Gaia backend quirks) are
collected in `docs/design.md`, section *Pitfalls found while building*.
