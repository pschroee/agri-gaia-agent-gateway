# pi-subagents: our own copy in the PoC

This directory is a **modified copy** of the npm package `pi-subagents`. It lives in the
repo, is committed and is used instead of the npm package when building the image `agw-basis`.
Changes happen only here. There is **no proposal to the original and no contact with
upstream** (the author's decision, 2026-09-29).

## Origin

| | |
|---|---|
| Package | `pi-subagents` on npm (<https://www.npmjs.com/package/pi-subagents>) |
| Source | <https://github.com/nicobailon/pi-subagents> |
| Version | **0.73.1**, marked here as `0.73.1-agw.2` (`agw.1` on 2026-09-29, `agw.2` on 2026-09-30) |
| Integrity (`npm view pi-subagents@0.73.1 dist.integrity`) | `sha512-IklOqw67DtvIWQFKxFJpDFXsOGcq8RGYZEbokykD5Kvfs9aa/du1cW2NEJ5xx4GUgN2LVt3vAq5IdzEMo7YwJw==` |
| shasum (`dist.shasum`) | `587cd7d2694b74d926f32afc724bd310c2e33c8b` |
| Taken over on | 2026-09-29 |
| Content | the content of the tarball from `npm pack pi-subagents@0.73.1` (folder `package/`), i.e. exactly what `pi install npm:pi-subagents@0.73.1` creates, without `node_modules` (1 223 files) |

Before the first change the copy was byte-for-byte identical to the tarball content (`diff -r`).

**What is foreign and what is our own is shown by the Git history** (of the master's thesis
repository, see the note at the end): commit `d21c5c0` contains the package unchanged
(byte-for-byte identical to the tarball), commit `8dfbd4a` only our own changes. Exactly these
are shown by

```bash
git diff d21c5c0 8dfbd4a -- poc/third_party/pi-subagents
```

Every future change to the copy goes into its own commit and into the list below.

## License

MIT, copyright with the authors of pi-subagents; the license text is unchanged in
[`LICENSE`](LICENSE). The local changes are under the same license.

## Purpose

`workflowScript` runs a script written by the agent in a worker. pi-subagents starts this worker
hard-wired via `node:worker_threads`, i.e. in the pi process. In the PoC the script is to run in
the execution sandbox (E9, see `docs/e9-execution-sandbox.md`, section `workflowScript`).
Previously the Dockerfile rewired the import at build time with `sed`. With this copy the
redirection is a versioned, tested change to the source code instead of a text replacement in
the image.

## Local changes

Everything else is unchanged from the tarball. Added in addition, not part of the package:
`VENDORED.md` (this file) and `test-agw/`.

### 1. `package.json`: version `0.73.1-agw.1`

Only the field `version`, from `0.73.1` to `0.73.1-agw.1`. Reason: the copy should be
recognisable by its version as our own state, so that nobody takes it for the published package.
The suffix `-agw.N` counts the states of our own changes.

### 2. `src/workflows/scripted-workflow.js`: worker runtime configurable

The static import

```js
import { Worker } from "node:worker_threads";
```

is replaced by

```js
// agw: runtime of the workflow worker configurable (see VENDORED.md, change 2).
const workerModule = process.env.PI_SUBAGENTS_WORKFLOW_WORKER || undefined;
const { Worker } = workerModule ? await import(workerModule) : await import("node:worker_threads");
/** agw: module that `Worker` for workflowScript comes from; the bridge checks the redirection with it. */
export const workflowWorkerModule = workerModule ?? "node:worker_threads";
```

- **Variable `PI_SUBAGENTS_WORKFLOW_WORKER`:** module specifier from which `Worker` is loaded,
  in the PoC `/opt/agw/ext/remote-worker.mjs`. The module must export a class `Worker` that
  behaves like the one from `node:worker_threads` (`new Worker(source, { eval: true, workerData })`,
  events, `postMessage`, `terminate`). Unset or empty: `node:worker_threads` as in the
  original.
- **It is read when the module loads**, not per call. The variable must therefore be set in the
  process before pi loads the extension.
- **No silent fallback:** if the variable points to a missing module, loading
  `scripted-workflow.js` already fails (`ERR_MODULE_NOT_FOUND`). workflowScript then does not run
  at all, instead of running unnoticed in the pi process.
- **Export `workflowWorkerModule`:** shows at runtime which module is active. The bridge
  checks with it whether the redirection takes effect.
- The package is ESM (`"type": "module"`), so top-level `await` is allowed; the package's
  `index.js` already uses it itself. `Worker` is instantiated in only one place, in
  `runWorkflowScript` (`new Worker(WORKER_SOURCE, { eval: true, workerData: { acornPath } })`).
- The source map `scripted-workflow.js.map` has not been updated; from line 4 it is off by
  four lines. This only affects stack locations in error messages.

### 3. `src/workflows/scripted-workflow.d.ts`: declaration of the new export

Added at the end:

```ts
export declare const workflowWorkerModule: string;
```

Reason: the bridge is TypeScript and imports the export; without a declaration the type check
does not know it.

### 4. `src/runs/shared/single-output.js`: output in the response instead of as a file (`agw.2`, 2026-09-30)

With the environment variable `PI_SUBAGENTS_OUTPUT_INLINE=1` set, `formatOutputPathInstruction`
always picks the instruction that pi-subagents otherwise gives only to agents without writing
tools: "Return the complete artifact in your final response. The runtime will persist it to
exactly this path". The version in `package.json` is `0.73.1-agw.2` for this.

Reason: pi-subagents gives every child of a workflow (and every run with `output`) a path under
`/agent/sessions/subagent-artifacts/outputs/…`, with the instruction to write exactly there.
The path is in the pi container; but the child's tools run in the execution sandbox
(E9), where `/agent` does not exist. In the chat of 2026-09-30 ("Recherche Schwanzbeißen") all
three children failed on this with `mkdir: cannot create directory '/agent': Read-only file system`
and fell back to their own paths. pi-subagents stored the file anyway, because
`resolveSingleOutput` writes the final response when the file is unchanged; only the instruction
did not fit. Without the variable the copy behaves like the original. The pi image sets it fixed.
Test: `test-agw/output-inline.test.mjs` (`node --test`, no dependencies): without the variable the
original instruction, with the variable the instruction to return the result in the response.

## Other places with `node:worker_threads` or `node:vm`

As of 0.73.1, for classification within E9; **not changed**:

- `src/workflows/scripted-workflow.js`, constant `WORKER_SOURCE`: the source code that runs
  **in** the worker uses `require("node:worker_threads")` (`parentPort`, `workerData`) and
  `require("node:vm")` for the agent's script. It is started via the redirected `Worker`
  and thus runs wherever the worker module runs it.
- `src/runs/background/async-retention.js` imports `Worker` from `node:worker_threads` and
  starts `async-retention-discovery-worker.mjs` (there `parentPort` from `node:worker_threads`).
  This is cleanup work for finished background runs (searching for old run files), not
  code written by the agent.
- `node:vm` does not occur outside `WORKER_SOURCE`.

## Test

`test-agw/workflow-worker.test.mjs`, without network, with `node --test`. It checks: without the
variable (and with an empty one) `Worker` comes from `node:worker_threads`; with the variable from
the given module, shown by a mock (`test-agw/fixtures/mock-worker.mjs`) that logs every
instantiation; the built-in class is then not called; `workflowWorkerModule` names the right
module in each case; a missing module leads to `ERR_MODULE_NOT_FOUND`. Against the
unchanged tarball all five cases fail.

The dependencies (`acorn`, `jiti`, `undici`, `yaml`) are not in the repo. For testing, create a
copy with dependencies outside the repo:

```bash
S=<scratchpad>/pi-subagents
cp -Rp third_party/pi-subagents "$S"
(cd "$S" && npm install --omit=dev --ignore-scripts --no-audit --no-fund)
cd third_party/pi-subagents && PI_SUBAGENTS_DIR="$S" node --test test-agw/*.test.mjs
```

`node --test test-agw/` (directory instead of pattern) tries under Node 24 to load the directory
as a module and fails; give the pattern `*.test.mjs`.

## Upgrading to a new version

1. Fetch and unpack the tarball, outside the repo:
   `npm pack pi-subagents@<new> && tar xzf pi-subagents-<new>.tgz` (creates `package/`);
   note the integrity: `npm view pi-subagents@<new> dist.integrity dist.shasum`.
2. Look at the upstream changes: unpack the old tarball the same way and read
   `diff -r <old>/package <new>/package`, especially `src/workflows/scripted-workflow.js`
   and all places with `node:worker_threads`, `node:vm` and `new Worker(`.
3. Replace the copy: delete the content of `third_party/pi-subagents/` except `VENDORED.md` and
   `test-agw/`, copy `package/` in, then `diff -r <new>/package third_party/pi-subagents`
   — only `VENDORED.md` and `test-agw` may be reported as additional.
4. Reapply changes 1 to 3 from above (version `<new>-agw.1`). If the import no longer matches
   literally, find the place where `Worker` for workflowScript is created and rewire it
   equivalently there.
5. Run the test (see above), plus `diff -r` against the new tarball: the output must show
   exactly the changes listed here.
6. Update this file: table *Origin*, list *Other places*, new changes if any.

## Note on the separate repository (2026-10-05)

Since 2026-10-05 the gateway lives in its own repository. The commits named above are in the
master's thesis repository (`poc/third_party/…`); here the copy came in with the first commit
**already modified**. Our own changes can still be verified at any time: fetch `npm pack` with the
version named above, unpack it and compare with `diff -r package/ <this directory>`.
