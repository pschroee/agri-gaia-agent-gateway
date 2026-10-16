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

## Long-running platform jobs (autonomy A-E2)

- A call that starts platform work of its own (`POST /train/config`, `/train/containers/<id>/run`,
  `/container-images/build`) does not end its plan step: `watch.For(access, req, res)` decides in the gateway which
  registered route starts which job, the row goes into `watch_jobs`, and the step waits in state `waiting`
  (`plan_steps.watch_job`). The loop `Manager.RunWatchers` (ticker 5 s, interval by age, at most four reads at a time)
  polls with the user's token, writes progress, publishes SSE `job` and `plan` and finishes the step at the end.
  Progress never comes from the agent, and the watcher never writes (`TestWatchReaderRefusesWrites`).
- **Kinds live in their own file with their own `init()`** (`internal/watch/task.go`, `training.go`, `image.go`; other
  plans bring `notebook_run`, `annotation_run`, `edge_deployment`), so that parallel plans do not touch each other's
  files. A kind that only starts another job sets `Progress.Next`, and the same row is observed on under the new kind:
  the run of a training chains `platform_task` → `training_run` (the task ends when the container is *started*). `Lookup`
  finds a kind by the name its row carries; a name this build does not know is skipped, never guessed.
- **`plan_steps.confirm_each` exists because of this:** `delegation.Access.ConfirmEach` is a trait of the route, and
  without the column a plan loaded from the database treated such a step as covered — it would have run the deploy of a
  container deployment without asking. Whoever adds a field to a plan step re-checks `scanPlanStep` against `BuildStep`.
- **The answer of a call that starts a watched job tells the agent to end its turn** (`Result.Note`, set in
  `PlatformCall` from `watchNote`): otherwise the agent holds its turn open and polls `/tasks/<id>` itself, which keeps
  the sandbox alive and the chat `active` for the whole job — seen live on 16.10.2026, with no wake-up entry in the log
  at all and `state=active` throughout. The wording promises a wake-up only for an autonomous, approved plan (D2.6);
  every other plan is told to keep an eye on the job itself. `Result.Text` also renders `StatusPendingUser`
  (`pending_user`), which used to come out as "HTTP 0" with the message dropped.
- **The platform reports no intermediate percent** for the tasks of this instance (`completion_percentage` 0.0 until the
  end), so a waiting job showing no percent is the platform's answer, not a gateway bug.
- **The end of a step wakes a resting chat** (`ensureLive` + `deliverQueue`) only for an autonomous plan that was
  approved while the step ran, and the wake-up counts against the plan's `max_auto_turns` (`planWakeLimit`) instead of
  `AGW_AUTO_TURNS_MAX`/`AGW_BG_WAKES_PER_HOUR`; over the budget the plan is paused with `budget_turns` and the note stays
  queued. A job outside a plan is only observed for the cards: no note, no wake-up.
- **Parked approvals (D2.8) hold no turn:** a non-covered step of an autonomous plan answers `pending_user` at once, and
  a goroutine of the gateway repeats the stored call through `PlatformCall` when the user decides. The marker
  `WithParkedApproval` is set only there and counts only for the call that matches that step (`parkedHere`); a step
  whose call carries files from the sandbox is asked synchronously, because the gateway cannot repeat it without the
  running sandbox.
- **"Stop job" is the user's action,** not a tool: `POST …/plans/{plan}/steps/{idx}/stop` and `POST …/jobs/{job}/stop`
  call the kind's `Stop` with the user's token and log it with `via: "user"`. Rules in `API.md`, *Long-running platform
  jobs*.

## Plans over hours (autonomy A-E3)

- An autonomous plan outlives the browser, so the **login has to be kept alive**: `Manager.RunPlanEndurance` (ticker
  4 min, `internal/chat/endurance.go`) calls `oidc.Service.KeepFresh` for every owner that `Store.AutonomousPlanOwners`
  names (an approved autonomous plan, joined onto `chats.owner`). It renews only an access token that expires within
  five minutes, and only for those owners — the loop is off when `chat.Options.Sessions` is nil (`AGW_AUTH_MODE != oidc`).
- **A lost login pauses, it does not fail.** `oidc.ErrNoSession` from `KeepFresh` or from a platform call pauses the
  plan with `pause_reason` `login` (D3.5) and publishes SSE `attention`; a claimed step goes back to `open` through
  `Store.ReleasePlanStep` and keeps its attempt, because the call never reached the platform. `loginGone(err)` in
  `internal/chat/endurance.go` is the one place that recognises that error — it comes from the token exchange's subject
  token (`platform.Config.Subject` → `oidc.AccessToken`).
- **A login continues those plans by itself** (D3.6): `oidc.Service.OnSession` reports every successful login to the
  manager, which resumes the plans of **that** owner that are paused for `login`. The owner is part of the store's
  query, so a login never continues a foreign plan (`TestLoginResumesOnlyOwnPlans`). Plans paused for `restart` (D3.4,
  after a gateway restart) or for `user`/`abort`/`budget_*` wait for the user.
- `Store.RecoverPlans` pauses an autonomous plan after a restart instead of failing it and keeps `ended_at` empty
  (also when it was paused already: the failed step would block it); steps in state `waiting` are untouched (D3.4).
  The step that was `running` still fails, with a text that says the call may have reached the platform. **A failed step blocks a plan** (the agent may only run its next `open` step), so
  `Manager.ResumePlan` puts the failed steps of an autonomous plan back to `open` (`Store.ReopenFailedPlanSteps`,
  attempts kept) — that resume *is* the repeat D3.4 asks the user for.
- The plan's end is capped one hour before the owner's session ends (`Autonomy.EndsAt`, D3.3; the session's own end is
  `oidc.Service.SessionEnds` = `created + MaxSession`). The preview names that time; a session that ends within the
  hour refuses the proposal. No refresh token is ever stored (D3.2) — a restart without a login in the browser pauses
  the plan until the user opens a page of the platform again. Rules in `API.md`, *Autonomy over hours*.

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
- The approval of a run carries, next to its text preview, the **structured data** the frontend card shows
  (`approvals.notebook`, `notebook.ApprovalData`, own code C-E4): entry, files with size, SHA-256 and text, datasets with
  the names the gateway read, and the limits. Every string in it is escaped for display
  (`platform.VisibleControls`; in the JSON of a notebook with the extra backslash of `VisibleControlsJSON`), and a file
  the preview does not show gets no text either. The dataset names are read through the **same reader as the summary**
  (`cachedReads`, one read per path), so they appear in the summary's single activity entry and not as calls of their
  own. A re-read at the moment of the approval (`summary_recheck`) replaces the summary but **never** the code — the
  files, sizes and hashes are what the user was asked about. A notebook is delivered as **its code cells in JSON**
  without outputs, so `notebookCells` in the frontend can show them and a hidden character cannot come back through the
  JSON parse. The **link** of a run ("Open in JupyterHub") reaches the card as `link` of the `job` event and of
  `GET /api/chats/{id}/jobs`: the kind `notebook_run` writes the run's link into `watch_jobs.link` with its first poll
  (`watch.Progress.Link`, cleared when a row is handed over to another kind, empty for every other kind), and no card
  builds the address itself.

## Result checks (R0)

- The gateway checks the result of a long step itself: `internal/assess` runs the model in a **container of its own**
  (never in the agent's sandbox and never in the agent's notebook server — the agent must not see the model run, the
  measured images or the script) and works out the figures, the baseline and the verdict in Go. The container comes
  from the execution image (`AGW_IMAGE`), has no network, no socket, no volume, a read-only root and the `tmpfs` from
  `assess.CheckTmpfs`; the script `/opt/agw/check/predict.py` is part of the image, and the gateway alone decides what
  goes in (model and images as one tar on stdin, one JSON line back, strictly parsed with `DisallowUnknownFields`).
- **`sandbox.Spec.Entrypoint` had to be added for this** (R0): the execution image's entrypoint is `agw-exec idle`,
  which ignores arguments and never ends, so a check that set only `Args` would hang until its time limit. An empty
  `Entrypoint` keeps the image's one, as pi's container and the execution sandbox need.
- **The one-shot container has two rules, both paid for in the live test of R0** (`sandbox.Runtime.RunOnce`): one
  hijacked connection carries stdin *and* stdout, so the gateway must not half-close it — an early EOF ends the whole
  stream, the answer is lost, and the exit code of a container that is still running then looks like a clean "0". The
  write side therefore stays open until the answer has arrived, and a container that is still running when its output
  ends is an error. Consequence: **the command has to stop reading its input at the end of the input, not at EOF.**
  That is also why `assess.BuildTar` pads the archive to whole `assess.TarRecord`s (10 240 bytes, as `tar(1)` does):
  python's `tarfile` reads 10 240 bytes at a time and would otherwise wait for the rest of its last record — with the
  write side open, forever (the check hung after its inputs had arrived). Both are covered by the Docker tests in
  `internal/sandbox/runonce_test.go` (`AGW_DOCKER_TESTS=1`), which the everyday run does not execute: **change `RunOnce`
  or `BuildTar` only with those tests run.**
- **One check at a time** (`Service.sem`, a channel of one; `Service.Start` waits for it in a goroutine): the model runs
  with 1 CPU, and several checks at once would starve the chat. `Start` answers at once with the check id (the row is
  `queued`) — a tool call must not wait for minutes; `assess_status` and the check card follow the row. A check that
  broke may keep a partial report (figures without verdict), and a check that was running when the gateway stopped ends
  as `failed` (`RecoverAssessments`, called from `Manager.Recover`).
- The paths are **virtual** (`/_agw/assess…`, `RegisterVirtual` G1.5) with the resource `assessment`; `platform_request`
  and the REST binding refuse them (`platform.AssessCallRefusal`), so only the check tools reach them. R1–R3 add their
  tools, paths, routes and handlers in their own files (`Service.Handle`, the delegation's `routes`). Rules, limits and
  fields in `API.md`, *Result checks*; the plan with its decisions is `planung/ergebnispruefung.md` of the thesis
  repository.

## Training checks (R1)

- A training check measures a model from a training of this chat on images the training did not see.
  `split_dataset` creates the training dataset before the training (per class, by SHA-256, 70 % into the
  new dataset), `assess_model` measures afterwards. The rules are in `internal/assess/training.go`
  (`JudgeClassification` is a pure function: verdict, reasons, hints and suggestions come from there and
  nowhere else), the log parsing in `trainlog.go`, the split in `split_dataset.go`.
- **The gateway alone says what is "seen"** (`model_origins`, written through `platform.OnModelCreated`
  when a model is created from a training container). The chat of a tool call reaches that callback through
  `platform.WithChat`, which the socket sets when it runs a tool with steps; a raw call carries no chat and
  records nothing. Without an origin row every check answers `not measured – origin unknown`.
- **The agent never names the measured set:** the check downloads the datasets itself (with the user's
  token through `Client.Stream`, temp file, `AGW_ASSESS_DOWNLOAD_MAX_MB`) and decides the split. The report
  carries figures, the mapping and the baseline, never a file name or a SHA-256 of the measured set.
- **The platform's own figure** is read out of `train.log` right after the model download, before every
  `not measured` exit except "origin unknown" (L6 of the check card), and always named as the platform's
  figure — the templates measure on their own internal test split.
- The approval of an image build adds the line `Model check: …` (`registerDescriber` in
  `internal/platform/tools_assess_training.go`). It reads `GET /_agw/assess/models/{id}/latest`; the
  summary reader (`internal/chat/plan.go`) answers a path under a virtual prefix through its doer, because
  `platform.Client.Do` would send it to the platform. Without a readable answer the line is left out.

## Annotation cards (V4)

- `GET /api/chats/{id}/annotation` (issue #112) is what the cards of pre-annotation in the history read: the runs of
  V1 (`annotation_runs`) and the checks and fills of V3 (`annotation_jobs`) with their results, each oldest first, plus
  `tool_call_id` and the percent the observation wrote out of `watch_jobs` (kinds `annotation_run`, `annotation_job`).
  Own chats only. The handler reads the database through `Manager.AnnotationRuns`/`AnnotationJobs`/`Jobs` and **never
  calls CVAT**: a card of a dormant chat has to load, and the counts of a finished run stay the agent's
  `annotation_summary` — nothing is guessed to fill the gap. Route in `internal/api/annotation.go`, Manager methods in
  `internal/chat/annotation.go`, so parallel plans do not touch their files.
- The route answers a job's **whole** `result` (the list of uncertain images too, up to the job's `limit`, at most
  1 000): the card shows the first rows and expands them; the agent's tool `annotation_job` keeps its pages of 50 for
  its own context. `params` goes along as well — the card reads its threshold (`uncertain_below`, `min_confidence`)
  from there.
- **`cvat_url` of the answer is `AGW_CVAT_PUBLIC_URL`** and every link of a card begins with it; the UI opens no other
  link. Without that address the field and the task links stay empty.

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
