// Test of local change 2 (VENDORED.md): where does the workflowScript worker come from?
//
// Run (from this package directory): PI_SUBAGENTS_DIR=<copy with dependencies> node --test test-agw/*.test.mjs
// The dependencies of pi-subagents (acorn etc.) are not in the repo. PI_SUBAGENTS_DIR therefore
// points to a copy of the package with `npm install --omit=dev`; without the variable the
// package containing this test is checked. No network needed.
//
// The variable PI_SUBAGENTS_WORKFLOW_WORKER is read when the module loads. Each case
// therefore runs in its own Node process.
import { strict as assert } from "node:assert";
import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { test } from "node:test";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const pkgDir = resolve(process.env.PI_SUBAGENTS_DIR || join(here, ".."));
const moduleUrl = pathToFileURL(join(pkgDir, "src/workflows/scripted-workflow.js")).href;
const mockPath = join(here, "fixtures/mock-worker.mjs");

function runChild(code, env) {
	const dir = mkdtempSync(join(tmpdir(), "agw-wf-worker-"));
	const log = join(dir, "log.jsonl");
	const childEnv = { ...process.env, AGW_MOCK_LOG: log, AGW_MODULE_URL: moduleUrl };
	delete childEnv.PI_SUBAGENTS_WORKFLOW_WORKER;
	Object.assign(childEnv, env);
	try {
		const r = spawnSync(process.execPath, ["--input-type=module", "-e", code], { env: childEnv, encoding: "utf8" });
		assert.equal(r.status, 0, `child process failed:\n${r.stdout}\n${r.stderr}`);
		let lines = [];
		try {
			lines = readFileSync(log, "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l));
		} catch {}
		return { out: JSON.parse(r.stdout.trim().split("\n").at(-1)), log: lines };
	} finally {
		rmSync(dir, { recursive: true, force: true });
	}
}

// Calls runWorkflowScript far enough that `new Worker(...)` is reached; the respective
// mock throws there, runWorkflowScript ends with "Workflow worker could not start: …".
const RUN = `
const m = await import(process.env.AGW_MODULE_URL);
let error = null;
try { await m.runWorkflowScript({ script: "return 1;" }); } catch (e) { error = String(e?.message ?? e); }
console.log(JSON.stringify({ workflowWorkerModule: m.workflowWorkerModule, error }));
`;

test("without the variable: Worker from node:worker_threads", () => {
	// Put a mock into the built-in module before pi-subagents imports it. If the call lands
	// there, Worker demonstrably comes from node:worker_threads.
	const code = `
import { createRequire, syncBuiltinESMExports } from "node:module";
import { appendFileSync } from "node:fs";
const wt = createRequire(import.meta.url)("node:worker_threads");
wt.Worker = class { constructor(s, o) {
  appendFileSync(process.env.AGW_MOCK_LOG, JSON.stringify({ module: "node:worker_threads", eval: o?.eval === true, hasAcornPath: typeof o?.workerData?.acornPath === "string" }) + "\\n");
  throw new Error("builtin-mock");
} };
syncBuiltinESMExports();
${RUN}`;
	const { out, log } = runChild(code, {});
	assert.equal(out.workflowWorkerModule, "node:worker_threads");
	assert.match(out.error, /Workflow worker could not start: builtin-mock/);
	assert.deepEqual(log, [{ module: "node:worker_threads", eval: true, hasAcornPath: true }]);
});

test("an empty variable counts as unset", () => {
	const { out } = runChild(`const m = await import(process.env.AGW_MODULE_URL); console.log(JSON.stringify({ workflowWorkerModule: m.workflowWorkerModule }));`, {
		PI_SUBAGENTS_WORKFLOW_WORKER: "",
	});
	assert.equal(out.workflowWorkerModule, "node:worker_threads");
});

test("with the variable: Worker from the given module (absolute path)", () => {
	const { out, log } = runChild(RUN, { PI_SUBAGENTS_WORKFLOW_WORKER: mockPath });
	assert.equal(out.workflowWorkerModule, mockPath);
	assert.match(out.error, /Workflow worker could not start: agw-mock/);
	assert.deepEqual(log, [{ module: "mock-worker.mjs", sourceIsString: true, eval: true, hasAcornPath: true }]);
});

test("with the variable: node:worker_threads stays untouched", () => {
	// Cross-check: if the redirection is active, the built-in Worker class must not be
	// called.
	const code = `
import { createRequire, syncBuiltinESMExports } from "node:module";
import { appendFileSync } from "node:fs";
const wt = createRequire(import.meta.url)("node:worker_threads");
wt.Worker = class { constructor() { appendFileSync(process.env.AGW_MOCK_LOG, JSON.stringify({ module: "node:worker_threads" }) + "\\n"); throw new Error("wrong path"); } };
syncBuiltinESMExports();
${RUN}`;
	const { out, log } = runChild(code, { PI_SUBAGENTS_WORKFLOW_WORKER: mockPath });
	assert.equal(out.workflowWorkerModule, mockPath);
	assert.deepEqual(log.map((l) => l.module), ["mock-worker.mjs"]);
});

test("with the variable pointing to a missing module: loading fails instead of silently falling back", () => {
	const { out } = runChild(
		`let error = null; try { await import(process.env.AGW_MODULE_URL); } catch (e) { error = e.code ?? String(e); } console.log(JSON.stringify({ error }));`,
		{ PI_SUBAGENTS_WORKFLOW_WORKER: join(here, "fixtures/does-not-exist.mjs") },
	);
	assert.equal(out.error, "ERR_MODULE_NOT_FOUND");
});
