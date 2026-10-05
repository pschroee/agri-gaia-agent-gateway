# E9: Execution sandbox

> **As of 2026-09-29: implemented** (approved by the author), findings from the code review and the
> security review incorporated, `workflowScript` allowed again through the execution sandbox.
> Basis: pi 0.87.1, `pi-subagents` 0.73.1 (own copy `0.73.1-agw.1`), Docker Desktop on the Mac (runc, without gVisor).
> The first part describes what is built; below it are the checkpoints with measurements, the
> reviews, the path of `workflowScript`, the deviations from the design and the open points. The
> feasibility study from the same day is kept, shortened, at the end.

## The problem (before E9)

In stage 1 `bash` ran **next to** pi in the same container and as the same user. There the agent
could change session files, forge pi's output via `/proc/<pid>/fd/1` and rewrite the configuration
of the subagents (code review M4). Only what the LLM proxy measured was reliable; which tools pi had
**executed** came from the RPC stream and from session files, both within the agent's reach.

## What is built

The **orchestrator itself** executes every tool call of the main agent and of every subagent in a
second container, the **execution sandbox**, and records it in `tool_executions`. No code of the
agent runs in pi's container; since the rework of `workflowScript` this also holds for the scripts
of workflows.

```
 Slot = two containers, started and torn down together (warm pool)

 ┌────── agwpoc-<slot>-pi (agw-pi) ──────────────────┐      ┌─── agwpoc-<slot> (agw-basis) ──────────┐
 │ PID 1: pi (RPC), no shell, no git, no Python      │      │ PID 1: agw-exec idle                   │
 │ /workspace empty and read-only                    │      │ /workspace, /workspace/inputs, ~, /tmp │
 │ exec-bridge.ts: bash/read/write/edit/grep/find/ls │      │ python3, rg, fd, jq, curl, git, node,  │
 │   → POST /tool/* at the socket, with toolCallId   │      │ typst, matplotlib, agw-artifact,       │
 │   and session file; guard for subagent            │      │ agw-internet (→ own socket)            │
 │ remote-worker.mjs: worker for workflowScript      │      │ scripts of workflows (runner.cjs)      │
 │   → POST /tool/workflow                           │      │ internet switch, package caches;       │
 │ subagents: the same extension (settings.json)     │      │ network: own internal network          │
 │ network: slot network (LLM proxy only)            │      │                                        │
 └───────────────┬───────────────────────────────────┘      └───────────────▲────────────────────────┘
                 │ socket <slot>/pi: /tool/*, /mcp                          │ docker exec -u 0 agw-exec serve
                 ▼                                                          │ (one long-lived process per slot)
           ┌───────────────────── Orchestrator ─────────────────────────────┴───┐
           │ /tool/*: checks, executes in the execution sandbox,                │
           │ writes tool_executions; proxy reads tool_calls[].id and            │
           │ finish_reason (llm_calls); reconciliation per toolCallId:          │
           │ GET /api/chats/{id}/tool_executions                                │
           └────────────────────────────────────────────────────────────────────┘
```

| Part | Where | What |
|---|---|---|
| `agw-exec` | `cmd/agw-exec` | static Go helper in **both** images. In the execution sandbox: `idle` (PID 1, reaps orphans, accepts all signals), `serve` (supervisor, protocol `internal/execproto`), `op` (one operation). In pi's container: `pi-entry` (entrypoint, replaces `entrypoint.sh`), `put`, `poll-subagents` (replaces the Python script), `kill-node` |
| Supervisor | `agw-exec serve` | runs as **root with the capabilities SETUID, SETGID and KILL**, otherwise none, started once per slot via `docker exec`, JSON lines over stdin/stdout, many operations at once (IDs). Every operation runs in its **own child process as uid 10001**, which protects itself against access via `/proc/<pid>/fd` with `PR_SET_DUMPABLE=0`; `bash` in its own process group. Abort: closing the child's stdin, which then ends the group; timeout likewise. End of the supervisor's stdin aborts everything. If the process limit is exhausted, the **emergency brake** ends all processes of the agent (N3) |
| Client | `internal/execbox` | one connection per slot, restarts the supervisor when needed, aborts when the context ends. Frames are **buffered per operation** (up to 64 MiB, above that abort with `EOVERFLOW`), so a slow reader does not hold up other operations (M4) |
| Endpoints | `internal/sock/tools.go`, `internal/sock/background.go` | `POST /tool/op` (files, search), `POST /tool/bash` (NDJSON stream, closing the connection aborts), `POST /tool/upload` (`mcp_upload_artifact`: the orchestrator reads the file itself), `POST /tool/workflow` (script of a workflow, NDJSON in both directions), `POST /tool/bg/start`, `/tool/bg/output`, `/tool/bg/stop` (background tasks, see below). Per slot at most 32 concurrent requests and a byte budget of two files at the limit (2 × 64 MiB) for large content (N4). Only at pi's socket; the execution sandbox has its own socket with artifacts, internet and MCP |
| Log | table `tool_executions` | per operation: chat, slot, session (`main` or run of the subagent, from the path of the session file), `tool_call_id`, tool, operation, arguments (shortened; for `write` path, size and SHA-256 instead of content; for `workflow` the first 4,000 bytes of the script, size and SHA-256), exit code, error, output (beginning and end, 4 KiB) with SHA-256 and size of the whole output, start, duration. NUL is replaced by `␀` in the excerpt; if the entry still fails, a fallback row without excerpt and arguments is created (K1) |
| Proxy | `internal/llmproxy/meter.go` | reads `tool_calls[].id` in the SSE stream and in JSON responses (`llm_calls.tool_calls[].id`), plus `finish_reason` and whether the response arrived completely (`llm_calls.complete`, M1). Chunks without `index` are assigned by `id` (L9) |
| Reconciliation | `internal/chat/reconcile.go`, `web/src/lib/evidence.ts` | per `toolCallId`: **proven** (requested and executed), **not requested**, **not executed** (only for tools that run in the sandbox), **mismatch** (different tool), otherwise *without sandbox* (todo, subagent without workflow, MCP). Two harmless causes of "not executed" have their own grey states: **response aborted** (at the proxy without `finish_reason`) and **refused by pi** (error message in the session, only a hint). The list of tools with execution comes from the server (`executed_tools`, L6) |
| Extension | `images/agw-basis/ext/exec-bridge.ts` | replaces the seven tools. `read`, `write`, `edit` and `ls` via `createXTool(cwd, {operations})`; `bash`, `grep` and `find` the bridge reimplements itself, with pi's schema, output and messages (H1, M2, M3), `read` above 64 MiB reads an excerpt (`read_lines`, L4). Loaded via `-e` in all three variants and for subagents via `subagents.defaultSubagentOnlyExtensions` |
| Guard | in `exec-bridge.ts` | `tool_call` handler for `subagent` and `watchdog_diff`, plus the check of every message of a workflow script (see *The guard for `subagent`* and *`workflowScript`*) |
| Workflow | `ext/remote-worker.mjs`, `workflow/runner.cjs`, `cmd/agw-exec/workflow.go` | stand-in for pi-subagents' worker thread; the script runs as its own Node process in the execution sandbox |
| MCP | `images/agw-basis/ext/mcp.ts` | `mcp_upload_artifact` no longer reads the file itself but tells the orchestrator the path |
| UI/CLI | `web/`, `cmd/agw/execs.go` | label "proven", grey or red marker on every tool call (history, subagents), tab *Executions* in the sidebar, `agw chat execs <id> [--flagged]` |

**Moved into the execution sandbox:** internet switch including package caches, inputs
(`/workspace/inputs`), workspace (backup and restore), display images (reading),
`agw-artifact`/`agw-internet`, the whole output of long commands (`/tmp/pi-bash-<hash>.log`) and
the scripts of workflows. **Stay with pi:** session files, monitoring of the subagents
(`agw-exec poll-subagents` via `exec`), the limit for subagents (`agw-exec kill-node`), the
configuration of pi-subagents (`agw-exec put`) and the runs a workflow starts.

**End of a container:** if the execution sandbox dies, the orchestrator notices it (`ContainerWait`),
saves pi's session and puts the chat to idle (H2). If pi dies, it saves the workspace from the
still running execution sandbox.

**Networks:** pi's container is attached only to the slot network with the orchestrator (LLM proxy).
The execution sandbox has its own internal network without the orchestrator; with internet, the
egress network and the package caches are added. **The execution sandbox reaches neither the API nor
the LLM proxy.** Direct model calls bypassing pi (for example `curl` from `bash`) therefore no longer
exist.

**Tool set unchanged:** if an extension registers `grep`, `find` and `ls`, pi enables them in the
main agent. `exec-bridge.ts` hides them there again (`AGW_BRIDGE_HIDE`, only variants `cli` and
`both`), so the main agent has `read`, `bash`, `edit`, `write` as before E9. Subagents keep the
tools of their agent definition. In the MCP variant `--tools` fixes the set; a tool not named there
is not registered by pi at all.

## Checkpoints

| # | Question | Result |
|---|---|---|
| P1 | Cost per execution | `docker exec` costs 36–60 ms (Docker Desktop). Hence one **long-lived supervisor** per slot: **about 2 ms** per operation (mean over 50 × `stat` and 20 × `bash true`, `TestExecSandboxServe`), compared with 36 ms per `docker exec` in the same run. In the run after the reviews (2026-09-29): `stat` 1.0–1.5 ms, `bash true` 1.8–2.0 ms via the supervisor, `docker exec` 33–35 ms. On the real system `read` took 9–13 ms, `stat` 7 ms, `bash 'uname -m'` 8 ms (values from `tool_executions`, without model) |
| P2 | Do all children load the redirection? | **yes, for built-in agents**: foreground (in the pi process), background (runner process) and the runs from `workflowScript` (`runs.run`, `runs.all`), each with the model ID and session file of the run. `tasks`/`chain` no longer exist in 0.73.1. In the built system proven by `TestSlotE9WithScriptedModel` (foreground and background, chain and three parallel runs from one workflow, every `bash` call proven in its own session) and `TestE9ToolCallsReconciled` (real model) |
| P3 | Does pi start without a shell? | **yes**, pi is PID 1 (`agw-exec pi-entry` replaces itself with Node via `execve`). `pi --version` works without a shell (shebang via `/usr/bin/env`). Proven by `TestPiContainerWithoutShell` |
| P4 | What does a child without redirection reach? | **Bypass found:** `subagent({action:"create", config:{…, subagentOnlyExtensions:""}})` creates, through the tool itself, an agent whose child does not load the redirection; `bash` fails there (no shell), but `write` overwrote `settings.json` in pi's container. **Plus P4b:** `workflowScript` ran in `node:vm` in a worker thread of the pi process; `emit.constructor("return process")()` returned the host's `process`, i.e. agent code in the process of PID 1. **Fixed** by the guard (P4) and by moving the scripts into the execution sandbox (P4b). Proven by `TestSlotE9WithScriptedModel`: `create` and `claude-code` blocked, `settings.json` unchanged; the same escape from `node:vm` now sees uid 10001, PID 1 `agw-exec idle`, no `/agent` but `/workspace`, and writing `settings.json` fails with `ENOENT`. **Chains and parallel subagents are thus possible again** (see *`workflowScript`*) |
| P5 | Streaming, abort, timeout | Output arrives in pi streamed (NDJSON, `onData`). An abort via the API ended `sleep 297` in the execution sandbox **after 101 and 134 ms** (two runs) and after 361 ms in the run after the reviews (`TestE9AbortStopsCommand`); the entry carries `aborted`. Background processes of the same group end too (`TestExecSandboxServe`); the timeout yields `timeout:<s>` like pi, limits and messages of the timeout like pi (L1) |
| P6 | Do the IDs at the proxy and at execution match? | **1:1**, also for subagents: `TestE9ToolCallsReconciled` (real model) gave 4 proven, 0 not requested, 0 not executed, 0 mismatched, in the run after the reviews 5 proven, 2 without sandbox, 0 suspicious; every tool call from the subagent's session file is proven. DeepSeek assigns IDs of the form `call_00_…` |
| P7 | Memory per slot | when idle **execution sandbox 7–9 MiB** (PID 1 and supervisor), **pi's container 164–227 MiB** (`docker stats`, 2026-09-29, before the rework of `workflowScript`). Before E9 a slot was at 74–166 MiB in one container |
| P8 | Do the previous attacks still work? | **no.** Where `bash` runs, PID 1 is `agw-exec idle`, `/agent` does not exist, no pi process is visible; `echo … > /proc/1/fd/1` ends up in the log of PID 1 of the execution sandbox, not with pi; pi's `settings.json` stays unchanged, pi answers normally afterwards (`TestE9BashRunsOutsidePi`). The model itself refused the attack commands ("escape/spoofing attempt"); the test therefore runs them directly in the execution sandbox with the rights of `bash` |

## The guard for `subagent`

After E9 a `tool_call` handler in the pi process is a suitable control point: the agent reaches pi
only through the parameters of its tool calls and through the messages of its workflow scripts, and
exactly these the guard checks before pi-subagents sees them. Its messages are in English like
pi's, because they reach the model as a tool result (L4).

**Single subagent and actions** (`checkSubagentCall`):

- parameters: `agent`, `task`, `action`, `topic`, `context` (not `profile`), `model`, `async`,
  `timeoutMs`, `maxRuntimeMs`, `toolTimeoutMs`, `maxOutput`, `id`, `index`, `view`, `lines`,
  `message`, `mode`, `agentScope`, `capabilities`, `includeProgress`, `artifacts`;
- actions: `list`, `get`, `models`, `guide`, `status`, `interrupt`, `steer`, `resume`,
  `children.list`;
- agents: the built-in ones without a foreign runtime (`worker`, `scout`, `reviewer`, `oracle`,
  `researcher`, `delegate`, `evidence-auditor`). `agent` is always checked as soon as it is set,
  also next to an action (L2).

**With `workflowScript`** a separate allowlist applies at the top level: `workflowScript`,
`args`, `async`, `timeoutMs`, `maxRuntimeMs`, `context` (not `profile`), `model`,
`globalConcurrencyLimit`, `maxSubagentSpawnsPerRun`, `includeProgress`, `artifacts`. The script is
only admitted if pi-subagents reports `/opt/agw/ext/remote-worker.mjs` as the origin of the worker
(`workflowRuntimeRedirected` reads the export `workflowWorkerModule` of the copy); otherwise, also
if the module does not load or lacks the export, the answer is "workflowScript is unavailable
here".
The messages of the running script are checked by `checkWorkflowMessage` (see below).

**Blocked** is everything that brings code or agent definitions into pi's container or starts
programs there: `workflowScriptPath`, `workflow` (named workflows), `gate`, `acceptance`, `output`,
`config`, `cwd`, `worktree`, `extensionBindings`, the actions `create`, `update`, `delete`, `eject`,
`schedule`, `mission.*` etc., the agents `claude-code`, `codex-exec`, `cursor-agent` and, in the
script, `runs.host`. In addition the guard blocks the tool `watchdog_diff` (agent `reviewer`), which
would call `git` in pi's container (N2). The system note tells the agent what works: single
subagents with `agent` and `task` in the foreground or background, or `workflowScript` with
`runs.run` for chains and `runs.all` for parallel runs.

## `workflowScript`

**Decision of the author (2026-09-29): bring it back, but safely.** pi-subagents 0.73.1 always
starts the script of a workflow in a worker thread (`node:worker_threads`) with `node:vm` in the pi
process and offers no hook for it. `node:vm` is not a security boundary (P4b). That is why the
script now runs in the execution sandbox.

**Own copy instead of patching at build time (decision of the author, 2026-09-29).** pi-subagents
0.73.1 (MIT) lies as a copy in the repo under `third_party/pi-subagents/`, version `0.73.1-agw.1`,
with **one** change in `src/workflows/scripted-workflow.js`: if `PI_SUBAGENTS_WORKFLOW_WORKER` is
set, `Worker` comes from that module, otherwise from `node:worker_threads`; the active module path
is in the export `workflowWorkerModule`. Origin, change and tests of the copy are in
`third_party/pi-subagents/VENDORED.md`. A proposal to upstream is not planned. The `sed` at build
time that had rewritten the import until then is gone.

### The path of a script

1. **Build:** the Dockerfile copies `third_party/pi-subagents` to where `pi install` would put the
   package (`/opt/agw/pihome/npm/node_modules/pi-subagents`) and installs the four runtime
   dependencies via `npm ci` with fixed integrity from
   `images/agw-basis/pi-subagents-deps/package-lock.json`. `pi-subagents-deps/check.mjs` aborts the
   build if the version is not `0.73.1-agw.1`, the copy's dependencies differ from the lockfile, or
   `workflowWorkerModule` does not report `node:worker_threads` without the variable and
   `/opt/agw/ext/remote-worker.mjs` with it. The pi image sets
   `PI_SUBAGENTS_WORKFLOW_WORKER=/opt/agw/ext/remote-worker.mjs` via `ENV`: the setting belongs to
   exactly this copy and this `remote-worker.mjs`, applies to every container of the image (also in
   the tests) and to all processes in it, and the agent does not reach pi's environment (its code
   runs in the execution sandbox). The execution sandbox gets `acorn` from the copy, i.e. in the
   same version.
2. **Call:** the agent calls `subagent` with `workflowScript`. The guard checks the parameters and
   registers the approved script with `toolCallId` and session file.
3. **Stand-in:** pi-subagents creates the worker; `remote-worker.mjs` mimics its interface
   (`postMessage`, `terminate`, events `message`, `error`, `exit`). It accepts only pi-subagents'
   workflow worker and only a script registered by the guard ("workflowScript was not approved by
   the execution sandbox guard"). Then it opens `POST /tool/workflow` at pi's socket: first line
   `toolCallId`, tool `subagent`, session file and the source code of the worker (from pi-subagents,
   not from the agent; at most 4 MiB), then one host message per line.
4. **Execution:** the orchestrator starts the operation `workflow` in the execution sandbox.
   There `agw-exec` runs `node /opt/agw/workflow/runner.cjs` as uid 10001 in its own process group;
   the runner starts the worker in a worker thread like pi-subagents does. An escape from its
   `node:vm` only reaches this process in the execution sandbox.
5. **Way back:** every line of the worker goes back as a frame. It counts as **agent code**: before
   pi-subagents sees a message, `remote-worker.mjs` checks it with `checkWorkflowMessage`. Requests
   `runs.run` (and through it `runs.all`) are subject to the rules of a single subagent: parameters
   only `agent`, `task`, `model`, `context` (not `profile`), `timeoutMs`, `maxRuntimeMs`,
   `toolTimeoutMs`, `maxOutput`, `async`, `phase`, `label`, agents only the built-in ones. `status`,
   `steer`, `state.get` and `state.set` pass; `runs.host` (a command in pi's container) and anything
   unknown are blocked. The stand-in answers a blocked request itself with an error to the script.
6. **Runs:** pi-subagents still starts the approved runs in the pi process. They load the
   redirection; their tool calls therefore run in the execution sandbox and are proven.
7. **Log:** the operation appears in `tool_executions` under the tool `subagent` with the operation
   `workflow`; the arguments are beginning, size and SHA-256 of the script, the output is the
   worker's messages. In the reconciliation the `subagent` call is thereby **proven**.

### Checked

With a scripted model (`TestSlotE9WithScriptedModel`):

- The escape from P4b ends up in the execution sandbox (see P4) and is logged as `workflow`.
- `runs.all` with three runs: the three `bash` calls are proven, lie in three different sessions and
  overlap in time.
- A chain with `runs.run`: the second run reads what the first wrote to `/workspace`.
- Blocked: a run with `cwd`, the agent `claude-code`, `workflowScriptPath` and a **forged request**
  that the script writes directly to the runner's stdout, bypassing the worker; none of these runs
  reaches the model.
- `watchdog_diff` is blocked in the subagent (N2); two single subagents in the background run at the
  same time (starts 0–87 ms apart, overlap 2.9–3.0 s).
- In total 34 tool calls requested at the model and 32 executions in this test.

Plus unit tests of the guard (`images/agw-basis/test/guard.test.mjs`) and of the endpoint
(`TestToolWorkflowDuplex`).

**With the real model** (`TestE9WorkflowParallelSubagents`, `e2e/e9_test.go`): three parallel runs
via `runs.all`, each with one `bash` call. Reconciliation: 4 proven, 1 without sandbox, 0
suspicious; the three runs overlapped by 2.7 s (run of 2026-09-29). In the visual check the
successful workflow at first showed "exit 1" in the log: when pi-subagents ends the worker with
`terminate()`, Node reports this as `exit` with code 1, and the runner passed the code on.
`runner.cjs` now ends with 0 in this case; the E2E test requires exit 0 without errors.

### Limits

- **Own copy of third-party code.** The change to pi-subagents is small and is recorded in
  `third_party/pi-subagents/VENDORED.md`; a new version of pi-subagents only arrives when someone
  deliberately renews the copy and carries the change over. The check at build time (`check.mjs`)
  and at runtime (`workflowRuntimeRedirected`) ensure that without the redirection `workflowScript`
  stays blocked (fail-closed), also if the variable is missing or empty.
- **The form is checked, not the content.** The instruction (`task`) of a run is free as for a single
  subagent. The limit `max_subagents` still applies hard at the proxy and through monitoring;
  `globalConcurrencyLimit` and `maxSubagentSpawnsPerRun` are only cooperative.
- **No timeout of its own in the supervisor.** File operations end after ten minutes at the latest,
  `bash` and `workflow` do not; a workflow ends through pi-subagents (`timeoutMs`, `maxRuntimeMs`,
  `terminate`), by closing the connection or with the abort of the chat.
- The supervisor buffers further input to the worker in a queue of 1,024 lines; if the script does
  not read, the operation is aborted.
- The request to `/tool/workflow` counts towards the slot's 32 concurrent requests (N4).

## Background tasks (addendum 2026-09-30)

`bash` with `run_in_background: true` starts a command that outlives the tool call (details on
tools, note and UI in [`design.md`](design.md), *Background tasks*). For E9 what matters is where it
runs and what is proven.

**New operation `bg` of `agw-exec`.** Like `bash`: child process `agw-exec op` as uid 10001 with
`PR_SET_DUMPABLE=0`, inside it `bash -c` in its own process group. Differences:

| | `bash` | `bg` |
|---|---|---|
| Duration | until the command ends, abort by closing the request | until the command ends; abort only via `bg_stop`, the user's stop, the end of the supervisor or the teardown of the slot |
| Output file | `/tmp/pi-bash-<hash>.log`, only above pi's thresholds; opened before the start with `O_NONBLOCK` and `O_NOFOLLOW`, only as a regular file (Review 3, N1) | always `/tmp/agw-bg/bg-<n>.log`, at most 256 MiB; directory (0755) and file (0644) are created by the **supervisor as root**, which passes the file open to the helper (descriptor 3, Review 3, N1); the orchestrator builds the path from the task's number, `Validate` only admits `^/tmp/agw-bg/bg-[1-9][0-9]{0,8}\.log$` |
| First frame | data or end | `pgid` of the process group as soon as the command runs |
| Limit | 64 operations per supervisor | additionally at most `-bg-max` (from the orchestrator's `AGW_BG_MAX`, default 5) running `bg` per supervisor, above that `ELIMIT`; after 64 MiB of output the helper reads on at only 4 MiB/s (Review 3, N3) |

New in the supervisor: if the helper of a `bg` operation ends **without** a result (for example
because the agent ended it with `kill -9`, same user), the supervisor sends `SIGKILL` to the reported
process group (root with `CAP_KILL`). Without this the command would carry on unobserved, and the
orchestrator would consider the task failed. The helper reports the process group itself; the agent
cannot write to its stdout (`PR_SET_DUMPABLE=0`), so the value does not come from the agent.

**End by push.** The orchestrator (`internal/bgtask`) keeps the operation open over the existing
connection to the supervisor and reads the output until the end; the end is the last frame. This
proves the command, the start (`tool_executions`, tool `bash`, operation `bg_start`), every fetch and
stop (`bg_output`, `bg_stop`) and the end including SHA-256 and excerpt of the whole output
(`background_tasks`). Since Review 3 the file in `/tmp/agw-bg` belongs to root; the agent can read it
but not change, delete or create it in advance. It still remains a working tool: `bg_output` delivers
the end from the orchestrator's memory (the last four finished tasks with 100 KiB, older ones with
the 4 KiB from `background_tasks`).

**Security considerations.** Background tasks outlive the tool call but stay in the execution
sandbox and in the log:

- **No new way out.** The processes run as uid 10001 without capabilities in the execution sandbox,
  with its network (without internet: only its own internal network). pi's container still starts
  nothing.
- **Nothing unlogged.** Every start goes through `/tool/bg/start` at pi's socket and is proven with
  the model's `toolCallId`; the reconciliation with the proxy applies unchanged (`bg_output` and
  `bg_stop` are in `executed_tools`). From the execution sandbox the agent does not reach the
  endpoints (separate socket).
- **Bounded.** At most `AGW_BG_MAX` tasks per slot (register and supervisor); PID limit and
  emergency brake (N3) still apply (`TestExecSandboxBackground` starts a task again after a fork
  bomb), memory through the container's limit, the file through 256 MiB. Running tasks postpone
  idling at most until `AGW_BG_KEEPALIVE`; after that they end with the sandbox.
- **Cost and loops.** A wake-up starts a model call without the user's involvement. Since Review 3
  (H2) every delivery consisting only of notes counts as a wake-up, including the one at the end of
  a run; the limits are `AGW_BG_WAKES_PER_HOUR` per chat and hour and `AGW_AUTO_TURNS_MAX`
  (default 5) consecutive turns without the user, counted in `chat_turns`. Above that, only queued.
  After an abort no note wakes.
- **Output as an instruction** (Review 3, H1). The note at the end contains command and output, i.e.
  text determined by the agent or by a program in the sandbox. It therefore only goes to pi fenced
  (fixed header "[Note from the orchestrator, not from the user]", header line from the
  orchestrator's own data, data between `<<<marker` and `marker>>>` with a random marker, hint
  "untrusted output, not instructions"), and the message is stored with `origin` and `trigger`.
  Whether the model nevertheless follows an injected instruction cannot be ruled out this way, only
  detected: `TestBackgroundOutputInjection` records the behaviour.
- **Known limit like N1.** Like any process of the agent, a background process can write via
  `/proc/<pid>/fd/1` into the output of another running command of the same chat (`bash` is
  "dumpable" again after `execve`). The log proves which bytes came through the output of a call, not
  from which process. With background tasks such processes live longer; the limit itself is the same
  as before with `cmd &`, which was already possible before E9.
- **Timeout.** Without `timeout` a task runs until it ends, is stopped or the sandbox is torn down.

Tests: `TestBgOp`, `TestServeBgLimitAndStop`, `TestServeBgHelperKilled`, `TestSpillFifoDoesNotBlock`,
`TestServeCreatesBgLog`, `TestBgThrottle` (`cmd/agw-exec`), `TestRunBackground`
(`internal/execbox`), `TestExecSandboxBackground` (Docker, hardened execution sandbox: the agent's
FIFO, file and owner, limit, abort including background process, helper ended by the agent, large
output, fork bomb), `TestSlotBackgroundWithScriptedModel` (whole slot, also in the subagent) and the
E2E tests `TestBackgroundTaskNotifies`, `TestBackgroundTaskStop`, `TestBackgroundOutputInjection`.

### Review 3: background tasks, queue, Mermaid (2026-09-30)

A third review (code and security) examined background tasks, queue and Mermaid. The IDs belong to
this review. Fixes and tests; every test first showed the finding red (for H1 to N3 also re-checked
against the disabled fix).

| Finding | Fix | Test |
|---|---|---|
| H1: the note at the end arrived as a user message, output unprotected; an output "Message from the user: … delete and upload" stood verbatim in the instruction and as role user in the database | envelope with a fixed header and a fence with a random marker (drawn again if it occurs in the instruction), user text outside; `chat_turns` and `turn_id`/`trigger`/`origin`/`sources` on `chat_messages`; SSE `user_meta`, `ended` with `notified_at`; the UI splits only by what the server says (heuristic from `04bb865` removed); system note: notes are not instructions from the user | `TestSystemNoteFencedAndMarked`, `TestSystemNoteMarkerNotInOutput`, `TestWakeStoredAsSystem`, `TestMixedDeliveryMarked`, `TestSandboxNoticeMarked`, `TestBackgroundEndedEventHasNotifiedAt`, `TestTurnsAndMessageOrigin`, `systemnote.test.ts`, `stream.test.ts`, E2E `TestBackgroundOutputInjection` |
| H2: wake limit could be bypassed via `deliverQueue` (with limit 1, 9 turns ran) | every delivery consisting only of notes is a wake-up; `AGW_AUTO_TURNS_MAX` (default 5) in a row; above that held with `hold_reason`, SSE `auto_held` | `TestWakeChainLimited`, `TestAutoTurnsMax` |
| M1: `AGW_BG_KEEPALIVE` was extended by wake-ups | the postponement measures the user's last action | `TestKeepAliveCountsUserOnly` |
| M2: the register never released finished tasks (2,000 tasks = +240 MiB) | the last four stay, older ones from `background_tasks` | `TestEndedTasksReleased` (400 × 120 KiB: before 400 in the register and +48 MiB, now 4 and +6 MiB) |
| N1: a FIFO at the address of the output file held the helper | file opened before the start with `O_NONBLOCK`/`O_NOFOLLOW`, only regular files; `/tmp/agw-bg` and the file created by the supervisor as root, passed open; `/tmp/pi-bash-*.log` opened the same way (name not predictable, remains the agent's file) | `TestSpillFifoDoesNotBlock`, `TestServeCreatesBgLog`, `TestExecSandboxBackground` (mkfifo as agent) |
| N2: limit in the register not atomic | slot reserved under the lock | `TestStartLimitAtomic` (32 parallel starts, before 32 started, now 5) |
| N3: throughput without a limit | ring buffer (0 instead of 4 allocations per chunk, 1.9 GB/s); after 64 MiB `agw-exec` reads on at only 4 MiB/s | `TestWriteNoAllocations`, `TestRing`, `BenchmarkWrite`, `TestBgThrottle` |
| N4: large Mermaid diagrams blocked the browser | above 4,000 characters or 150 edges, and from the sixth per message on, only on click | `mermaid.test.ts`, `Markdown.test.tsx` |
| N5: minor points | log with value instead of pointer; an abort during resume stays effective; a `prompt` timeout after acceptance takes nothing back; delivered rows in `chat_queue` stay on purpose (evaluation); `tail_lines` without a number yields the default | `TestAbortDuringResumeHolds`, `TestDispatchTimeoutAcceptedNoDuplicate`, `guard.test.mjs` |

## Reviews and fixes

After the build two reviews by subagents examined the E9 code: a **code review** (levels K
critical, H high, M medium, L low) and a **security review** (findings N1–N5). The security review
found no critical or high finding; the seven security goals held. The IDs are those of the two
reviews and are not to be confused with those of the first code review in
[`design.md`](design.md).

| Finding | Severity | Fix | Test | Commit |
|---|---|---|---|---|
| K1: NUL in an output (`printf '\0'`, binary file) prevented the log entry; the call would appear as "not executed" | critical | NUL in the excerpt, in arguments and errors replaced by `␀`, checksum and size over the real bytes; if the entry still fails, a fallback row is created. NUL in search patterns is refused | `TestToolExecutionWithNUL`, `TestToolBinaryOutputRecordedWithoutNUL`, `TestValidateRejectsNULInPatterns` | `ccb4365` |
| H1: large `bash` output crashed pi: pi wrote the whole output to `/tmp` in its own container (tmpfs 256 MiB), with a full tmpfs pi ended with `ENOSPC` | high | The bridge runs `bash` itself and keeps only a bounded tail in the pi process. The whole output lies in the execution sandbox at `/tmp/pi-bash-<16 hex characters of sha256(toolCallId)>.log` (path built by the orchestrator, at most 256 MiB, below pi's thresholds of 50 KiB and 2,000 lines no file); a `read` on the path finds it. `read` above 64 MiB reads an excerpt (`read_lines`) | `TestBashSpill`, `TestToolBashSpillPathFromToolCallID`, `TestReadLines`, `TestBridgeParity` (no file `pi-bash-*` in pi's container) | `d419bfb` |
| H2: signals such as QUIT, ABRT, TRAP, SYS, ILL, SEGV, BUS, FPE or STKFLT ended PID 1 of the execution sandbox, and the orchestrator did not notice its end | high | `agw-exec idle` accepts all signals and discards them (except SIGCHLD); the end of the execution sandbox is noticed via `ContainerWait`, the chat idles with its session saved | `TestExecSandboxServe` (all signals to PID 1), `TestExecSandboxDiesChatGoesDormant`, `TestPiDiesWorkspaceStillSaved` | `ccb4365` |
| M1: harmless cases (aborted response, call refused by pi) appeared red like a bypass | medium | the proxy stores `finish_reason` and `complete`; separate states `aborted` and `rejected`, grey in UI and CLI, not in `--flagged` | `TestMeterFinishReasonAndCompleteness`, `TestLLMCallCompletenessAndRejections`, `TestReconcile`, `evidence.test.ts` | `e09673a` |
| M2: `find` searched differently from pi | medium | `find` calls `fd` (10.3.0, in the image) in the execution sandbox with the same arguments as pi; at most 100,000 hits per call, above that a hint | `TestGlobWithFd`, `TestBridgeParity` | `d419bfb` |
| M3: when the limit was reached, `grep` suggested a `limit` the sandbox does not deliver | medium | the hint names the upper bound of 1,000 hits per call | `TestBridgeParity` (three checks of the limit) | `d419bfb`, `d21359c` |
| M4: a slow reader held up all operations of the slot; frames were discarded after 30 s | medium | frames buffered per operation (up to 64 MiB, then abort with `EOVERFLOW`); the final frame is never lost | `TestSlowReaderDoesNotBlockOthers`, `TestSlowReaderOverflow` | `ccb4365` |
| L1: timeouts of `bash` different from pi | low | limit and message like pi (at most 2³¹−1 ms) | `TestBridgeParity` | `d419bfb` |
| L2: `agent` was not checked next to an action | low | the guard always checks `agent` | `guard.test.mjs` | `d419bfb` |
| L3: `ls` showed broken symlinks, pi did not | low | entries that cannot be `stat`ed are dropped as in pi | `TestReaddirSkipsBrokenSymlinks` | `d419bfb` |
| L4: German messages reached the model as a tool result | low | messages in English like pi | `TestBridgeParity` | `d419bfb` |
| L5: hits in files with invalid UTF-8 were missing from `grep` | low | bytes from `rg --json` are decoded | `TestGrepNonUTF8` | `d419bfb` |
| L6: list of tools with execution kept twice (server and UI); tool appended twice | low | list via the API (`executed_tools`), deduplicated as in the UI | `TestReconcile`, `evidence.test.ts` | `e09673a` |
| L7: child process of an operation writable via `/proc/<pid>/fd/1` | low | `PR_SET_DUMPABLE=0` in the child process (see N1) | `TestExecSandboxServe` | `2223fab` |
| L8: uncommitted change to CLAUDE.md | low | E9 status brought up to date in the repo's CLAUDE.md and committed | – | `13ea959` |
| L9: chunks of tool calls without `index` in the SSE stream assigned wrongly | low | assignment by `id` | `TestParseSSEToolCallsWithoutIndex` | `e09673a` |
| Test gap: no parity of the bridge tools with pi | | parity test: every bridge tool against pi's built-in one on the same file tree, 58 cases, in the test image `agw-parity` (execution sandbox plus pi); 58 of 58 equal, before the fixes 16 deviations | `TestBridgeParity` (`images/agw-basis/test/parity.mjs`) | `d419bfb`, `d21359c` |
| N1: the docs claimed too much: the output of the agent's own call can also be influenced by the agent | below high | helper processes `agw-exec op` with `PR_SET_DUMPABLE=0`; the remaining limit is documented (*What remains open*), the test records it in the log | `TestExecSandboxServe` | `2223fab` |
| N2: pi-subagents' `watchdog_diff` calls `git` in pi's container | below high | blocked in the guard; pi image without `git` and without a shell recorded as a security decision | `TestSlotE9WithScriptedModel` | `0f96429` |
| N3: a fork bomb exhausted the process limit; afterwards no operation started any more | below high | emergency brake: the supervisor ends all processes of the agent (uid 10001) with `CAP_KILL`, including those that escaped their process group | `TestExecSandboxForkBomb` (timeout and `setsid`) | `b3fbbf4` |
| N4: no limit for concurrent requests at the tool endpoint; parallel `read`s occupied several GB | below high | per slot 32 concurrent requests, byte budget of 2 × 64 MiB for large content | `TestToolConcurrencyBoundedPerSlot` | `2223fab` |
| N5: services of the Docker Desktop VM reachable at the gateway of the internal networks | below high | docs only (*What remains open*) | | |

Informational, no change: pi's container reaches API and proxy on its slot network. The API requires
the token, and no agent code runs in pi's container.

## Deviations from the design

1. **Long-lived supervisor instead of `docker exec` per call** (P1), and as a different user from the
   agent: a supervisor as uid 10001 would have been reachable for `bash` of the same user (`kill`,
   `/proc/<pid>/fd/1`), which would have let the agent forge results of *other* calls. As root
   without capabilities except SETUID/SETGID (switching to uid 10001) and KILL (emergency brake,
   N3), both fail with "Permission denied" (`TestExecSandboxServe`). The agent's processes have no
   capabilities (`CapEff: 0`), `no-new-privileges` still applies. PID 1 of the execution sandbox
   (`agw-exec idle`) accepts all signals and discards them; no signal from `bash` ends the sandbox
   (the kernel does not deliver `SIGKILL` and `SIGSTOP` to PID 1 from within the namespace).
2. **Entrypoint in Go instead of Node** (P3): `agw-exec pi-entry` replaces itself with pi via
   `execve`; pi stays PID 1, which `kill-node` requires.
3. **Guard for `subagent`** (P4, P4b): not foreseen in the design, because the bypass only showed up
   in the prototype.
4. **Two sockets per slot:** `<slot>/pi` (tools and MCP) and `<slot>/exec` (artifacts, internet,
   MCP). The execution sandbox does not reach the tool endpoints; otherwise `bash` could create
   executions with invented IDs.
5. **The execution sandbox does not reach the LLM proxy.** Direct model calls from `bash`
   (previously possible and billed at the proxy) are gone. The E2E tests that checked the proxy
   from the sandbox now call it with Node from pi's container.
6. **Separate images:** `agw-pi` (pi without a shell) and `agw-basis` (execution sandbox without pi),
   both from `images/agw-basis/Dockerfile` (`--target pi`, `--target exec`); plus the test image
   `agw-parity` (`--target parity`) only for the parity test.
7. **The subagent view still comes from the session files**, but gets the label from
   `tool_executions` on every tool call. The session files lie in pi's container and have been out
   of the agent's reach since; for text and results of the subagents there is no other source.
8. **The bridge reimplements `bash`, `grep` and `find` itself** instead of feeding pi's tools with
   its own operations: pi's `bash` writes the whole output to a file in its own container (H1), pi's
   `find` searches differently without `fd` (M2). Schema, output and messages are pi's; the parity
   test records that.
9. **`workflowScript` through a stand-in for the worker** (see above), with an own copy of
   pi-subagents in the repo (`third_party/pi-subagents`), whose only change makes the origin of the
   worker configurable.

## What remains open

- ~~Decision for the author: patch pi-subagents or a hook upstream.~~
  **Decided (2026-09-29): own copy in the repo, no proposal to upstream.** Instead of rewriting the
  file with `sed` at build time, pi-subagents 0.73.1 lies in the repo as `third_party/pi-subagents`
  (`0.73.1-agw.1`) with one change (see *`workflowScript`* and
  `third_party/pi-subagents/VENDORED.md`). A new version of pi-subagents means: renew the copy,
  carry the change over, align `images/agw-basis/pi-subagents-deps`.
- **Decision for the author: the emergency brake ends all processes of the agent** (N3). It kicks
  in when the supervisor cannot start any more processes (`EAGAIN`) or a helper ends without a
  result while the cgroup's process limit is almost reached. It then ends **all** processes of
  uid 10001 except PID 1, including legitimate background processes (for example a server the agent
  started) and running operations of the same chat, which end with an error. A finer limit via
  `RLIMIT_NPROC` does not work: it counts per uid across all containers of the host, and all agents
  run as uid 10001.
- **What the log proves about an output** (N1, more precise than in the first version): the
  orchestrator proves *that* it executed a command with these arguments and which bytes arrived
  through the output of this call, not from which process they come. The agent determines what its
  command outputs anyway; beyond that, any other process of the agent, including a background
  process of an earlier call, can write into the output of a running `bash` command via its
  `/proc/<pid>/fd/1`, because `bash` is "dumpable" again after `execve`. Since
  `PR_SET_DUMPABLE=0`, the helper processes `agw-exec op` (file operations, searches, the frames of
  every operation), the supervisor (root), the log and other chats are out of reach.
  `TestExecSandboxServe` checks the helper processes and writes the remaining limit for a parallel
  command into the test log.
- **pi image without `git` and without a shell: security decision** (N2). pi-subagents calls `git`,
  `npm root -g` through a shell and `gh` in several places. In pi's container this fails because
  `git` is not installed and the shells are removed at build time. This is intended: anything that
  starts a program there would run next to pi and outside the log. The features of pi-subagents that
  rely on it are therefore not available; the guard additionally blocks `watchdog_diff`, so an image
  with `git` would change nothing about it. The other tools of pi-subagents (`bg_wait`,
  `contact_supervisor`, `subagent_supervisor`, `structured_output`, `subagents_enable`) start no
  processes. The absence of the shell is checked by `TestPiContainerWithoutShell`; the absence of
  `git` follows from the image (`node:24-bookworm-slim` without further installation) and is not
  tested separately.
- **Services of the Docker Desktop VM** (N5): at the gateway of the internal networks the VM offers
  `rpcbind` (port 111) and `rpc.statd` (port 65373); the execution sandbox reaches them even without
  internet. These are services of the VM, not of the PoC. On the Linux server the gateway looks
  different and has to be checked separately there.
- **Decision for the author: images above 64 MiB.** A `read` of a text file above 64 MiB delivers an
  excerpt; a `read` of an image above 64 MiB aborts with an error. pi would read the whole image into
  memory.
- **The orchestrator is the trusted base** (root with `docker.sock`), as is the pi process including
  guard and stand-in for the worker; that remains a limitation of the thesis.
- **Internet still means the whole network** (code review M3), now for the execution sandbox.
- **gVisor** is not part of this step; the split into two containers is independent of it.

Done compared with the first version: `workflowScript` (see above) and the complete output of
`bash`, which pi wrote to a file in its own container that `read` could not reach (H1).

## Tests

| Level | Test | What |
|---|---|---|
| Go unit | `cmd/agw-exec` | background tasks (operation `bg`, limit, abort of the group, helper ended); protocol and path check (relative, control characters, NUL, foreign environment), file operations including FIFO, `bash` with timeout, abort of the process group, background process, file with the whole output (`TestBashSpill`); `grep` with ripgrep including invalid UTF-8, `find` with fd, `ls` without broken symlinks, excerpt of large files (`TestReadLines`); supervisor via a real child process, abort via the protocol and at the end of stdin |
| Go unit | `internal/execbox` | concurrent operations, stream, abort, restart after loss, slow reader (M4) |
| Go unit | `internal/sock` | endpoints: not assigned, invalid IDs, tool/operation do not match, log entry with session and SHA-256, binary output with NUL, stream and abort, path of the whole output from the `toolCallId`, limits per slot (N4), `/tool/workflow` in both directions, upload from the execution sandbox |
| Go unit | `internal/chat`, `internal/llmproxy`, `internal/store` | reconciliation including "response aborted" and "refused by pi", manager against Postgres including SSE, end of either container, IDs and `finish_reason` at the proxy, chunks without `index`, table including NUL and fallback row |
| Node (in the image `agw-parity`, from `TestBridgeParity`) | `images/agw-basis/test/guard.test.mjs` | guard: allowed and blocked calls, `workflowScript` only with redirected runtime, messages of the worker, detecting the redirection via `workflowWorkerModule` (stub modules and the real copy, each in its own process: without, with empty, with foreign and with correct variable) |
| Docker | `internal/sandbox/bg_test.go` | background tasks in the hardened execution sandbox (`TestExecSandboxBackground`) |
| Docker | `internal/sandbox/e9_test.go` | pi's container without a shell (PID 1, `agw-exec`, read-only `/workspace`); supervisor in the hardened execution sandbox (uid, capabilities, attack on the supervisor and on helper processes, all signals to PID 1, P5, P1); fork bomb (`TestExecSandboxForkBomb`) |
| Docker, scripted model | `internal/worker/e9_docker_test.go` | whole slot: all seven tools, subagent in foreground and background, guard, parts of P8, workflows (escape, `runs.all`, chain, blocks, forged request), `watchdog_diff`; runs in `./dev.sh test` in the Go container |
| Docker, parity | `internal/worker/parity_docker_test.go` | `TestBridgeParity`: bridge against pi's built-in tools in the image `agw-parity`, 58 cases, plus the limit of `grep` |
| E2E | `e2e/e9_test.go` | P8, P6, P5 and parallel subagents via `workflowScript` (`TestE9WorkflowParallelSubagents`) with the real model |
| Web | `web/src/lib/evidence.test.ts` | reconciliation, harmless states (M1), deduplication (L6), display during and after the run, labels |

Status of the run of 2026-09-29 after the reviews: `./dev.sh test` with 16 Go packages (225 test
functions without E2E, with `-race`), S3 test, `TestBridgeParity` 58 of 58 equal, unit tests of the
guard 5 of 5, web 316 tests in 23 files; `./dev.sh e2e` 22 of 22 in 250 s.

Status after the background tasks (2026-09-30): `./dev.sh test` with 17 Go packages (266 test
functions without E2E, with `-race`), S3 test, `TestSlotBackgroundWithScriptedModel`,
`TestSlotE9WithScriptedModel`, `TestBridgeParity` 58 of 58 equal, Node tests of the bridge 7 of 7,
web 377 tests in 27 files; `./dev.sh e2e` 27 of 27 in 328 s.

## Appendix: feasibility (before the build, shortened)

- pi exports `createBashTool` … `createLsTool` together with `BashOperations`, `ReadOperations` etc.;
  the example `examples/extensions/gondolin` redirects all seven tools into a micro-VM. `execute`
  gets the model's `toolCallId` as its first argument.
- `subagents.defaultSubagentOnlyExtensions` in pi's `settings.json` loads extensions into every
  child session without disabling the others (`pi-subagents/docs/models.md`, *Extension defaults*).
  An agent definition with its own `subagentOnlyExtensions` suppresses the default (hence P4).
- `docker cp` does not see tmpfs contents; files go via `exec`.
- Originally estimated: two to three working days.

### Throwaway prototype (2026-09-29)

Image `agw-basis` with the shells removed, pi started through an entrypoint in Node, an extension
that replaces all seven tools and only logs who calls them, plus a **scripted model** (small
OpenAI-compatible server), so every parameter of the `subagent` tool can be triggered deliberately
and at no cost. This became `internal/fakellm`. The results are above under P2–P4.

Status after Review 3 (2026-09-30): `./dev.sh test` with 17 Go packages (286 test functions without
E2E, with `-race`), S3 test, `TestSlotBackgroundWithScriptedModel`, `TestSlotE9WithScriptedModel`,
`TestBridgeParity` 58 of 58 equal, Node tests of the bridge 8 of 8, web 401 tests in 28 files;
`./dev.sh e2e` 28 of 28 in 262 s. In `TestBackgroundOutputInjection` DeepSeek V4.1 Flash did not
follow the injected instruction (the file stayed, no approval requested) and explicitly called it
foreign text from the fence in its answer; that is one run, not proof for every model.
