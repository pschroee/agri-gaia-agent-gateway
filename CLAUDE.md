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

- The platform UI sends the current page and the open or selected objects with a message (`context` of
  `POST …/messages`, issue #13). `chat.ParsePageContext` checks it against fixed lists (pages, kinds, integer ids, one-line
  names up to 200 characters, no unknown fields); objects come as one `object` (old form, still accepted) or as
  `objects` (issue #45, at most 50, each checked alike, no duplicates); the result has `objects` and, for exactly one,
  `object` too, and code reads it through `PageContext.List()` (old rows have only `object`). It goes to pi as a
  `page_context` note with `audience: "agent"` before the text it belongs to, the names in a fence, and is stored on
  queue entries (`chat_queue.context`) and in the source. The note says the page is **background, not a question
  about the page** (`contextUse`); keep that when rewording it (live, the model once asked about the page instead of
  answering a general question).
  New pages or kinds go into `contextPages`/`contextKinds` in `internal/chat/pagecontext.go` with a test. **It never
  grants rights**; the delegation stays authoritative (`TestPageContextGrantsNoAccess`). Rules in `API.md`, *Page context*.

## Subagent limit

- At most `AGW_MAX_SUBAGENTS` (default 5) subagents run **at the same time** per chat; fixed for the service, no
  setting per chat (issue #24). The stored `chats.max_subagents` of older chats is ignored, `POST /api/chats/{id}/subagents`
  answers 410. The monitoring counts running runs in memory per sandbox (`runningSubagents` in
  `internal/chat/subagents.go`), not runs started in total; do not reintroduce `maxSubagentSpawnsPerSession` in the
  pi-subagents config, it caps runs in total. Rules in `API.md`, *Limit for subagents*.

## Messages to subagents

- The user can write to a subagent (issue #81): a running one gets a steer, a finished one a resume, and when
  pi-subagents cannot resume the run, a new run of the same agent with the transcript the gateway recorded
  (`restart`). The relay is the extension command `/agw-subagent-relay` in `images/agw-basis/ext/exec-bridge.ts`, which
  sends one request to pi-subagents' in-process RPC (`subagents:rpc:v1:request`, methods steer, resume, spawn) and
  reports the reply over the socket (`POST /subagent-relay/result`). The gateway stores the message first
  (`subagent_messages`) and is the authority for every check; the extension only lets through the three methods with
  fixed parameters. Rules in `API.md`, *Messages to subagents*.
- `/agw-…` commands are the gateway's own: `SendMessage` refuses them from the user and `Commands` hides them. Do not
  send them as user text; the relay goes through `Worker.Call` (prompt), not through the queue.
- Passive note `subagent_user`: it never wakes the agent and never starts a turn. `deliverQueue` skips queues that hold
  only such notes; `steerQueue` takes them along at a tool step.
- The continuation of a run is linked only by the marker `ref agw-xxxxxxxx` in the subagent's session (`user_echo` in
  `parseChildSessionKnown`), and `continues_run_id` is only written by the gateway (`SetRunContinues`; upserts never
  touch it). **Checked live (12.10.2026):** pi-subagents' `resume` revives the finished run **in the same session**, so the
  run keeps its ID, the echo marks the message `delivered` and no `continues_run_id` is written; only `restart` (new run
  with the recorded transcript) creates a new run and the link. A dormant chat always restarts.
- **The reply route `POST /subagent-relay/result` belongs to pi's socket** (`NewPiHandlerFg`), where the extension runs;
  at the execution sandbox's socket it answered 404 and every message ended as `relay timed out` (found live, fixed with
  `TestSubagentRelayResultOnlyAtPiSocket`). The execution sandbox, which the agent controls, must not have it.
- pi's `prompt` of an extension command answers only **after the handler has finished** (`prompt()` awaits it before
  `preflightResult`), so the relay's `Worker.Call` takes as long as pi-subagents needs (limit 10 s, reply wait 20 s).
- A steer reaches a subagent **after its current step**: a running `sleep 60` is not interrupted; tasks for tests need
  several short steps. A run that pi-subagents has not reported a `pi_run_id` for yet answers 409: `run_starting` when its
  status file is known (a background run in its first seconds), `foreground_run` when it has none (a run inside a step of
  the main agent). Both come from `relayRoute`, which also derives `message_state` of the run lists (issue #82); keep them
  one rule. Workflow children have their own `pi_run_id`, so no `#n` index is needed for them.
- The mock of `parity.mjs` must offer `registerCommand` (the bridge registers the relay command); `guard.test.mjs` runs
  only inside `TestBridgeParity`, so a failure there hides the guard tests.
- Relay tests: `images/agw-basis/test/guard.test.mjs` (node --test, agw-parity image); the Go tests are in
  `internal/chat/subagent_messages_test.go`, `internal/api/subagent_messages_test.go`, `internal/store/subagent_messages_test.go`.

## Bindings (toolsets)

- `AGW_TOOLSETS` (default `cli`) fixes the bindings of every new chat as any combination of `cli`, `mcp`, `api`
  (issue #29); there is no choice per chat. The canonical key (order cli, mcp, api: `cli,api`) is what chats store as
  `variant` and what the pool is keyed by; `both` of older chats means `cli,mcp`. Parsing and keys in
  `internal/toolset`, tools and pi arguments in `internal/worker` (`Tools`, `piArgs`). `cli,mcp` must stay exactly the
  former `both` (`TestOldVariantsUnchanged` holds the old arguments literally); a change to a binding's tools goes into
  `bindingTools` and that test. Old chats of any combination resume through slots started on demand
  (`pool.SetKnown`). Rules in `API.md`, *Bindings of new chats*.

## Internet switch of the agent

- The agent requests internet (approval) and switches it off itself (no approval) in every binding (issue #34):
  `agw-internet "<reason>"` / `agw-internet off`, MCP `request_internet` / `disable_internet`, and in the REST binding
  the tools `request_internet` / `disable_internet` of `api.ts` (socket `POST /internet`, `POST /internet/off` at pi's
  socket, logged as `api`). `Manager.DisableInternet` reuses `SetInternet` (chat lock, `chat` event) and is a no-op
  with `already_off` when internet is off. `agw-internet off` with further arguments is refused on purpose; a reason
  that is literally `off` goes after `--`. Rules in `API.md`, *Internet switch of the agent*.
- **Guidance on web research** (issue #44) has three layers that must stay consistent: the tool texts in
  `images/agw-basis/ext/web-tools.ts` (loaded in place of pi-searxng-suite, which stays unchanged; pi keeps the first
  registration of a tool name, so the texts are set while the suite registers, through a proxy of the API), the
  system note (`internetNote` in `internal/worker/worker.go`, only the switch tools of the chat's bindings) and the
  skill `web-research` (cli and mcp; pi lists skills only with `read` or `bash`, so `api` alone has none). Tests in
  `internal/worker/web_research_test.go`. Check a change to web-tools.ts by loading it with pi's `loadExtensions`
  in the `agw-pi` image (`docker run --entrypoint node …`), since no everyday test runs pi.
- `GET /tool/internet` (web-gate.ts) is asked once by every fresh, unassigned slot; it answers 409 **without** a log
  entry. Do not route it through `handler.chat`, which logs `refused: not assigned`.

## Mermaid diagrams

- The rules the web UI enforces on mermaid blocks (strict, no HTML labels: no HTML tags in labels, labels with
  spaces, umlauts or brackets in double quotes, no `%%{init}%%` or `click`) live in three places that must stay
  consistent (issue #59): the **description** of the skill `mermaid` (pi puts only the description into the
  context; the agent read the skill's text only after the UI had refused a diagram), the skill's text, and
  `mermaidRules` in the system note (every binding; MCP and REST list no skill `mermaid`). Tests in
  `internal/worker/mermaid_test.go`; the description stays a plain YAML scalar on one line (no `: `, no ` #`,
  at most 1024 characters).
- `mermaid-check` (`images/agw-basis/mmdc/`, exec image only) renders with `ui-config.json`, the same settings as
  the UI's `mermaidConfig`; a change there belongs into both. Measured with mermaid 12: `<br/>` alone renders,
  `<b>` shows as text, `#quot;` shows literally as `&quot;`, and unquoted brackets (`A[Training (CPU)]`) are the
  usual parse error. Changes to skills or `images/agw-basis/mmdc/` need both slot images rebuilt (see *Warm pool*).

## Links in the chat

- Links are Markdown links with a meaningful title, never bare URLs (issue #61). The rule lives in the
  **description** and text of the skill `web-research`, in the citing guideline of `ext/web-tools.ts` and in
  `linkRule` of the system note (every binding; also platform links and files). Tests in
  `internal/worker/links_test.go`. Skill changes need both slot images rebuilt (see *Warm pool*).

## Files the agent sends

- The agent sends result files to the user **without approval** (issue #62): `agw-artifact upload`, `mcp_upload_artifact`
  and the MCP tool `upload_artifact` store an output artifact at once (`Manager.Upload`), publish `artifact` and log
  `upload` with name, size, SHA-256 and the tool call. `platform_write` and internet keep their approval. Old
  `artifact_upload` approvals stay in the database and in the types; do not drop the kind.
- The path checks (inside `/workspace`, no symbolic link on the way or at the end, regular files, `O_NOFOLLOW|O_NONBLOCK`)
  live in one function, `execproto.OpenInside`, used by `agw-artifact` and by `agw-exec` for reads with `Request.Root`.
  The CLI's workspace is a package variable, not an environment variable, so the agent cannot move it.
- The guidance has three places that must agree: the **description** and text of the skill `artifacts`, `sendNote` in
  the system note (per binding; REST has no file tools) and `sock.UploadToolDescription`. Tests in
  `internal/worker/artifacts_test.go`. Skill changes need both slot images rebuilt (see *Warm pool*).

## Creating chats

- `POST /api/chats` with `async: true` (issue #30) returns at once also when the pool is empty: the chat is marked
  `starting`/`resuming` and `startLater` assigns the slot in the background while holding the chat lock, so a message
  sent meanwhile waits in `ensureLive` instead of taking a second slot. With a free slot it is assigned before the
  response. Keep slot assignment cheap: anything that is the same for every chat belongs into `worker.Create` (slot
  start), not into `attachSlot` (pi-subagents' config moved there; internet off is skipped because a fresh slot never
  had egress). The pool starts every replacement on its own (`Pool.fill` does not wait for a batch). Rules in
  `API.md`, *Creating a chat without waiting*.

## Resuming on open

- `POST /api/chats/{id}/resume` (issue #31) resumes an idle chat in the background (`Manager.Resume`): it sets
  `resuming` before answering and runs `ensureLive` in a goroutine, so the steps and the chat lock are the same as for a
  resume through a message and a message sent meanwhile waits instead of taking a second slot. It must stay a no-op for
  live, starting and resuming chats (`internal/chat/resume_open_test.go`). Do not confuse it with the wake-ups of
  background tasks (`BgWakesPerHour`), which start a turn. Rules in `API.md`, *Resuming a chat when it is opened*.

## Subagent runs of a chat that is not open

- `GET /api/chats/{id}/subagent-runs` (issue #60) is the short list the chat selector loads when the user expands the
  subagents of a chat that is not open. It must stay a read of the database (`store.ListSubagentRunSummaries`): no
  `View`, `Resume` or `ensureLive`, so that expanding never wakes a chat or takes a slot (`TestSubagentRunsList`
  counts slot requests). It carries `task_head` instead of a finished title, so the UI's `runTitle` stays the only
  title rule. Rules in `API.md`, *Subagent runs without opening a chat*.

## Warm pool and slot teardown

- Every `slot torn down` log line carries `reason=` (issue #55). The reason travels in the context
  (`pool.WithReason`, read by `worker.Factory.Destroy` via `pool.Reason`); `Pool.Release` takes it from the caller
  (`chat_suspended`, `agent_died:<cause>`, `attach_failed`), the pool sets `shutdown`, `worker.Create` sets
  `start_failed`. A new path that tears a slot down passes its own reason; `unspecified` in the log means one was
  forgotten.
- A failed start is a WARN `pool slot discarded before first use` with the error; the pool used to keep the error
  only in `lastErr` (never shown) and retried every 3 s, which looked like a silent teardown loop. The warm start of
  a combination now backs off (3 s doubling to 5 min, `backoffDelay`), reports the streak once as ERROR from the
  third failure and the recovery once as INFO. Tests in `internal/pool/pool_test.go` (no Docker).
- **Hot reload rebuilds Go, not the slot images.** Go code that passes pi a new extension or skill fails every
  slot start against the old `agw-pi:dev` (pi: `Extension path does not exist`). That was the loop of issue #55:
  `-e /opt/agw/ext/web-tools.ts` against an image from two days before. `dev/go-hot.sh` warns when `images/` or
  `third_party/` change; then run `./dev.sh start`, which rebuilds both images.

## Chat trash and deleted chats

- **Every table with a `chat_id` is classified:** it goes with the chat (`ON DELETE CASCADE`), or its rows are
  anonymised before the chat is deleted (`store.DeleteChatAnonymised`, issue #76). A new table with `chat_id` or a new
  call log needs a line in one of the two; the call logs (`socket_calls`, `tool_executions`, `llm_calls`,
  `aux_llm_calls`, `web_requests`) are measurement data and must keep their classes. Rules in `API.md`, *Chat trash*.
- **Refusals in the trash are in two places:** the routes (`writable` in `internal/api/api.go`, 409 `chat_in_trash`) and
  the manager (`ensureLive`, `Resume`, `send`, `PlatformCall`, `ProposePlan`, `Upload`, `RequestInternet`). Both stay.
- **Tests:** `internal/store/trash_test.go`, `internal/chat/trash_test.go`, `internal/api/trash_test.go`.

## Activity across chats

- `GET /api/activity` (issue #12) reads platform calls of the user's chats in one query (`internal/store/activity.go`):
  the owner filter is part of the SQL (`chats.owner`), never applied afterwards in Go, so a page size and the
  summary stay right. The outcome classification (`outcomeSQL`) mirrors the result texts written in
  `sock.runPlatform`; a new result text there needs a case there and in `TestListActivity`. `duration_ms` is the
  round trip of `Platform.Do` handed from `platform.Result.Duration` to the log entry through the context
  (`store.WithDuration`); it never reaches the agent (`json:"-"`). Rules in `API.md`, *Activity across chats*.
- **Internet switches in the activity (issue #37):** `kind=internet|all` adds the entries `op` `internet`,
  `internet_off` and `internet_set`; the default stays `platform`, and `summary` counts platform calls only for every
  kind (the key figures of the evaluation). Only the user's switch (`Manager.SetInternetByUser`, the API handler) logs
  `internet_set` with `via: "user"`; `SetInternet` stays unlogged, because the switch after an approval and the
  agent's off are already logged as their request. `UploadResult.Log` (`json:"-"`) gives the log a finer result than
  the agent sees (`already on`, `expired`). A new result text needs a case in `internetSQL` and in
  `TestListActivityInternet`.

## Events across chats

- `GET /api/events` (issue #32) streams the approvals of all the user's chats: first an `approvals` snapshot of the
  pending ones, then `approval` events. `Manager.publish` forwards only the kinds in `crossChatKinds` to
  `SubscribeAll`, which has **no** owner check; the API filters every event through `owns` (cached per connection).
  Subscribe before reading the snapshot. A new kind there needs the owner filter and a case in
  `internal/api/events_test.go` (foreign events must not appear). Rules in `API.md`, *SSE `GET /api/events`*.

## Request sizes at the LLM proxy

- `llmproxy.Call.RequestBytes` (issue #69) is the length of the request body that `Proxy.ServeHTTP` read from the
  sandbox and forwarded unchanged to the provider. It is stored in `llm_calls.request_bytes` (NULL for older rows and
  for 0, see `chat.requestBytes`), returned as `request_bytes` by `GET /api/chats/{id}/llm_calls` and the SSE event
  `llm_call`. Sizes only, never contents. The frontend shows it as "Sent to model" in the expanded activity row.

## Effect classes

- Every platform call has one class (`read`, `write`, `compute`, `irreversible`, issue #63), decided only in
  `delegation.Classify` for all three bindings. A new route gets its class through `.as(…)` where its action does not
  say it (compute, irreversible, a writing GET), and a case in `TestClassifyEffect`; unknown paths fall back to the
  class of their method. Rules and the table in `API.md`, *Effect classes*.
- Who is asked: `chat.needsApproval` (`internal/chat/effect.go`). Irreversible always, with confirm `none` too; a
  compute call is refused before the approval once the chat has used `AGW_COMPUTE_MAX_PER_CHAT` calls, and counts only
  when the platform accepts it (`store.ReserveCompute` / `ReleaseCompute`). A call without a class is refused
  (fail closed).
- The gateway's own refusals start with `platform.RefusedPrefix`; the socket logs them as `refused` in the activity.
  Columns `effect` in `approvals` and `socket_calls`, `chats.compute_calls`; see `API.md`.

## Summaries by the gateway

- What an approval says will happen (single platform calls and every plan step) is written by the gateway in
  `platform.Summarize` / `SummarizePlan` (`internal/platform/summary.go`, issue #72) from the call that goes out and
  platform state read right then; the agent's plan title and step descriptions are only `agent_title` / `agent_note`.
  A new writing route gets a template there with a case in `summary_test.go`; whatever a template does not name is
  listed as "Also sends …", so never drop that tail. The reads go through `Manager.summaryReader` (reading GETs only)
  and are not logged; tests that count platform calls use `withPlatform`, which leaves reading GETs out. Rules in
  `API.md`, *Summaries by the gateway*.

## Notebook runs (own code, C-E3)

- The agent's own code runs in the chat's notebook server in JupyterHub, one server per chat under the user's account
  (`<AGW_NOTEBOOK_PREFIX>-<first 8 hex digits of the chat id>`); server and runs belong to the chat, and the agent
  never names them. `internal/notebook` starts and stops the server through the hub API, finds its container **by the
  labels the instance's spawner sets** (`agw.notebook=1`, `agw.user=<user>`, `agw.server=<name>`) and runs everything
  in it with `sandbox.Runtime.ExecAs` as **1000:100** (the notebook image's user, not the sandboxes' 10001).
- **No kernel API on purpose** (plan decision): docker exec gives an exit code and a log that do not hang on a
  connection, needs no websocket library, and the gateway holds the docker socket anyway (E9). The price is that the
  code runs with the same uid as the marker files `.agw/` in its run directory, so it can fake exit code and log —
  that reaches only what the agent is told, never a decision of the gateway (plan 1.5).
- The code of a run is what the user approved (`sock.readFiles` put the SHA-256 into `Uploads`); it goes in as one tar
  over stdin and starts detached with `setsid --fork`. State, log and results come back out of the run directory
  (`runs/<run-id>/.agw/`, `run.log`). `notebook_fetch` reads a result with `realpath` and a prefix check (no symlink
  out of the run directory) and hands it to the socket as `platform.Result.File`, which writes it into
  `/workspace/notebook/<run-id>/`.
- The paths are **virtual** (`/_agw/notebook…`, `RegisterVirtual` G1.5): delegation, class (`compute`), the compute
  limit, the plan and the log run unchanged in `PlatformCall`. `POST /_agw/notebook/runs` carries **confirm each**
  (G1.1), so it always asks and no plan covers it; its approval shows the code instead of the form fields and is
  limited to 48 000 characters (`maxNotebookPreview`) instead of 16 000.
- `platform_request` and the REST binding refuse the paths (`platform.NotebookCallRefusal`): only the notebook tools
  read the files of a run from the sandbox. Without `AGW_JUPYTERHUB_API_URL` no doer is registered and the tools answer
  `unavailable` (G1.13), never an error.
- The reaper (`go nb.RunReaper`) stops a server whose last run ended more than `AGW_NOTEBOOK_IDLE_MIN` ago and that has
  no run going; the trash stops it too (`chat/trash.go`). Both leave the volume, and neither rewrites a run: the end of
  a run is recorded where its exit code is read (`Status`, the watcher), never by the reaper.
- One run at a time per chat; every run counts against `AGW_COMPUTE_MAX_PER_CHAT`. Rules, limits and paths in `API.md`,
  *Notebook tools*.

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
