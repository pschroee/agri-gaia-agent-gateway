// Unit tests of the guard in exec-bridge.ts (test gap from the code review: checkSubagentCall;
// plus the check of messages from workflowScript). Runs with node --test in the test image
// agw-parity (TestBridgeParity).
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { existsSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { bgStatus, bgTailLines, checkSubagentCall, checkWorkflowMessage, formatDuration, REMOTE_WORKER, workflowRuntimeRedirected } from "../ext/exec-bridge.ts";

test("single subagents and read-only actions pass", () => {
	for (const input of [
		{ agent: "worker", task: "x" },
		{ agent: "scout", task: "x", async: true, context: "fork", model: "deepseek/deepseek-flash", timeoutMs: 1000 },
		{ action: "list", capabilities: true },
		{ action: "status", id: "abc" },
		{ action: "get", agent: "worker" },
		{ action: "steer", id: "abc", message: "continue" },
	]) {
		assert.equal(checkSubagentCall(input), undefined, JSON.stringify(input));
	}
});

test("blocked: code, agent definitions, foreign runtimes, paths", () => {
	for (const [input, want] of [
		[null, "invalid parameters"],
		[[], "invalid parameters"],
		[{ action: "create", config: {} }, "parameter config is blocked"],
		[{ action: "create" }, "action create is blocked"],
		[{ action: "delete", agent: "worker" }, "action delete is blocked"],
		[{ agent: "claude-code", task: "x" }, "agent claude-code is not allowed"],
		// L2: agent is also checked next to an action
		[{ action: "get", agent: "codex-exec" }, "agent codex-exec is not allowed"],
		[{ agent: "worker", task: "x", context: "profile" }, "context profile is blocked"],
		[{ agent: "worker", task: "x", cwd: "/agent" }, "parameter cwd is blocked"],
		[{ agent: "worker", task: "x", gate: "id" }, "parameter gate is blocked"],
		[{ workflow: "review" }, "parameter workflow is blocked"],
		[{ workflowScriptPath: "/x.js" }, "parameter workflowScriptPath is blocked"],
	]) {
		assert.equal(checkSubagentCall(input), want, JSON.stringify(input));
	}
});

test("workflowScript only with a redirected runtime and without dangerous parameters", () => {
	const ok = { workflowScript: "return 1", async: false, args: { a: 1 }, timeoutMs: 5000 };
	assert.match(checkSubagentCall(ok), /not redirected/);
	assert.equal(checkSubagentCall(ok, { workflowReady: true }), undefined);
	for (const [extra, want] of [
		[{ workflowScriptPath: "/x" }, "parameter workflowScriptPath is blocked with workflowScript"],
		[{ gate: "id" }, "parameter gate is blocked with workflowScript"],
		[{ output: "/agent/config/settings.json" }, "parameter output is blocked with workflowScript"],
		[{ cwd: "/agent" }, "parameter cwd is blocked with workflowScript"],
		[{ agent: "worker" }, "parameter agent is blocked with workflowScript"],
		[{ context: "profile" }, "context profile is blocked"],
	]) {
		assert.equal(checkSubagentCall({ ...ok, ...extra }, { workflowReady: true }), want, JSON.stringify(extra));
	}
	assert.match(checkSubagentCall({ workflowScript: " " }, { workflowReady: true }), /non-empty/);
});

test("worker messages: runs like single subagents, runs.host blocked", () => {
	const run = (params) => ({ type: "call", callId: 1, method: "run", args: { key: "k", params } });
	assert.equal(checkWorkflowMessage({ type: "emit", value: 1 }), undefined);
	assert.equal(checkWorkflowMessage({ type: "complete", value: [1] }), undefined);
	assert.equal(checkWorkflowMessage(run({ agent: "worker", task: "x", async: true, phase: "p" })), undefined);
	assert.equal(checkWorkflowMessage({ type: "call", callId: 2, method: "status", args: { keyOrRunId: "k" } }), undefined);
	assert.match(checkWorkflowMessage(run({ agent: "claude-code", task: "x" })), /agent claude-code is not allowed/);
	assert.match(checkWorkflowMessage(run({ task: "x" })), /agent undefined is not allowed/);
	assert.match(checkWorkflowMessage(run({ agent: "worker", task: "x", cwd: "/agent" })), /parameter cwd is blocked/);
	assert.match(checkWorkflowMessage(run({ agent: "worker", task: "x", output: "/agent/x" })), /parameter output is blocked/);
	assert.match(checkWorkflowMessage(run({ agent: "worker", task: "x", context: "profile" })), /profile/);
	assert.match(checkWorkflowMessage(run([])), /params object/);
	assert.match(checkWorkflowMessage({ type: "call", callId: 3, method: "host", args: { key: "h", params: { command: "id" } } }), /runs.host is blocked/);
	assert.match(checkWorkflowMessage({ type: "call", callId: 4, method: "spawn", args: {} }), /runs.spawn is blocked/);
	assert.match(checkWorkflowMessage(null), /invalid/);
});

test("runtime redirection is recognised by workflowWorkerModule, otherwise blocked", async () => {
	const dir = mkdtempSync(join(tmpdir(), "wf-"));
	const mod = (name, src) => {
		const f = join(dir, name);
		writeFileSync(f, src);
		return f;
	};
	assert.equal(REMOTE_WORKER, "/opt/agw/ext/remote-worker.mjs");
	assert.equal(await workflowRuntimeRedirected(mod("on.mjs", `export const workflowWorkerModule = ${JSON.stringify(REMOTE_WORKER)};\n`)), true);
	// without the variable: pi-subagents reports node:worker_threads
	assert.equal(await workflowRuntimeRedirected(mod("off.mjs", 'export const workflowWorkerModule = "node:worker_threads";\n')), false);
	assert.equal(await workflowRuntimeRedirected(mod("foreign.mjs", 'export const workflowWorkerModule = "/tmp/remote-worker.mjs";\n')), false);
	// an unmodified pi-subagents version does not have the export
	assert.equal(await workflowRuntimeRedirected(mod("without.mjs", 'export const x = 1;\n')), false);
	assert.equal(await workflowRuntimeRedirected(mod("throws.mjs", 'throw new Error("broken");\n')), false);
	assert.equal(await workflowRuntimeRedirected(join(dir, "missing.mjs")), false);
});

// On the real module of the copy (in the test image agw-parity under /opt/pi/node_modules). The variable
// is read at load time, hence a separate Node process per case.
const VENDORED = "/opt/pi/node_modules/pi-subagents/src/workflows/scripted-workflow.js";
const BRIDGE = new URL("../ext/exec-bridge.ts", import.meta.url).pathname;
function guardInChild(env) {
	const code = `import { checkSubagentCall, workflowRuntimeRedirected } from ${JSON.stringify(BRIDGE)};
const m = await import(${JSON.stringify(VENDORED)});
const ready = await workflowRuntimeRedirected(${JSON.stringify(VENDORED)});
process.stdout.write(JSON.stringify({ module: m.workflowWorkerModule, ready, reason: checkSubagentCall({ workflowScript: "return 1" }, { workflowReady: ready }) ?? null }));`;
	const e = { ...process.env };
	delete e.PI_SUBAGENTS_WORKFLOW_WORKER;
	return JSON.parse(execFileSync(process.execPath, ["--input-type=module", "-e", code], { env: { ...e, ...env }, encoding: "utf8" }));
}

test("copy of pi-subagents: without PI_SUBAGENTS_WORKFLOW_WORKER workflowScript stays blocked", { skip: !existsSync(VENDORED) && "copy of pi-subagents missing (only in the agw-parity image)" }, () => {
	const off = guardInChild({});
	assert.deepEqual(off, { module: "node:worker_threads", ready: false, reason: "workflowScript is unavailable here (the workflow runtime is not redirected to the execution sandbox)" });
	const empty = guardInChild({ PI_SUBAGENTS_WORKFLOW_WORKER: "" });
	assert.equal(empty.ready, false);
	// A path other than the pi image's: the module loads, but the block stays.
	const other = guardInChild({ PI_SUBAGENTS_WORKFLOW_WORKER: "/opt/pi/ext/remote-worker.mjs" });
	assert.deepEqual(other, { module: "/opt/pi/ext/remote-worker.mjs", ready: false, reason: off.reason });
	// As in the pi image: allowed.
	assert.deepEqual(guardInChild({ PI_SUBAGENTS_WORKFLOW_WORKER: REMOTE_WORKER }), { module: REMOTE_WORKER, ready: true, reason: null });
});

test("background tasks: state and duration as one line", () => {
	assert.equal(formatDuration(8_400), "8s");
	assert.equal(formatDuration(83_000), "1m 23s");
	assert.equal(formatDuration(3_723_000), "1h 2m");
	const t = { id: "bg-3", command: "x", started_at: "2026-09-29T20:00:00Z", log_path: "/tmp/agw-bg/bg-3.log", output_bytes: 0, output_lines: 0 };
	const now = Date.parse("2026-09-29T20:01:23Z");
	assert.equal(bgStatus({ ...t, state: "running" }, now), "bg-3 is running (for 1m 23s)");
	assert.equal(bgStatus({ ...t, state: "exited", exit_code: 0, ended_at: "2026-09-29T20:00:08Z" }, now), "bg-3 exited with code 0 after 8s");
	assert.equal(bgStatus({ ...t, state: "stopped", stopped_by: "user", ended_at: "2026-09-29T20:00:08Z" }, now), "bg-3 was stopped by the user after 8s");
	assert.match(bgStatus({ ...t, state: "suspended" }, now), /chat was suspended/);
	assert.equal(bgStatus({ ...t, state: "failed", error: "boom" }, now), "bg-3 failed: boom");
});

// Review 3, N5: tail_lines comes from the model; anything that is not a number yields the default instead of NaN.
test("bg_output: tail_lines is checked", () => {
	for (const [v, want] of [
		[undefined, 2000],
		[null, 2000],
		["abc", 2000],
		[Number.NaN, 2000],
		[Infinity, 2000],
		[{}, 2000],
		["25", 25],
		[25.7, 25],
		[0, 1],
		[-5, 1],
		[10 ** 9, 2000],
	]) {
		assert.equal(bgTailLines(v), want, String(v));
	}
});
