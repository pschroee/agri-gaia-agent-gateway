// Unit tests of the guard in exec-bridge.ts (test gap from the code review: checkSubagentCall;
// plus the check of messages from workflowScript). Runs with node --test in the test image
// agw-parity (TestBridgeParity).
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { existsSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { bgStatus, bgTailLines, checkSubagentCall, checkWorkflowMessage, formatDuration, REMOTE_WORKER, relayCommand, relayCall, relayRef, relayRequest, workflowRuntimeRedirected } from "../ext/exec-bridge.ts";

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

// Issue #81: the relay of messages from the user to subagents. Only steer, resume and spawn, with fixed
// parameters; the same agent list as for the model's subagent calls.
const relayJSON = (v) => Buffer.from(JSON.stringify(v)).toString("base64url");
const REF = "agw-0a1b2c3d";

test("relay: steer and resume keep only id, index and message; steer gets mode steer", () => {
	assert.deepEqual(
		relayRequest(relayJSON({ ref: REF, method: "steer", params: { id: "pi-1", index: 2, message: "Nimm 10", workflowScript: "x", cwd: "/" } })),
		{ ref: REF, method: "steer", params: { id: "pi-1", message: "Nimm 10", index: 2, mode: "steer" } },
	);
	assert.deepEqual(
		relayRequest(relayJSON({ ref: REF, method: "resume", params: { id: "pi-1", message: "Folgefrage" } })),
		{ ref: REF, method: "resume", params: { id: "pi-1", message: "Folgefrage" } },
	);
});

test("relay: spawn keeps agent, task and async only; blocked agents and code are refused", () => {
	assert.deepEqual(
		relayRequest(relayJSON({ ref: REF, method: "spawn", params: { agent: "worker", task: "x", model: "m", async: false } })),
		{ ref: REF, method: "spawn", params: { agent: "worker", task: "x", async: true } },
	);
	for (const params of [{ agent: "claude-code", task: "x" }, { agent: "worker", task: "" }, { agent: "worker", task: "x", context: "profile" }]) {
		assert.equal(typeof relayRequest(relayJSON({ ref: REF, method: "spawn", params })), "string", JSON.stringify(params));
	}
});

test("relay: other methods, bad refs, bad ids and broken requests are refused", () => {
	for (const req of [
		{ ref: REF, method: "stop", params: { id: "x" } },
		{ ref: REF, method: "manage", params: { action: "schedule.run", id: "x" } },
		{ ref: "agw-XYZ", method: "steer", params: { id: "x", message: "m" } },
		{ ref: REF, method: "steer", params: { id: "a b", message: "m" } },
		{ ref: REF, method: "steer", params: { id: "x", message: "  " } },
		{ ref: REF, method: "resume", params: { id: "x", index: -1, message: "m" } },
	]) {
		assert.equal(typeof relayRequest(relayJSON(req)), "string", JSON.stringify(req));
	}
	assert.equal(typeof relayRequest("not base64 json !!"), "string");
	assert.equal(relayRef(relayJSON({ ref: REF, method: "stop" })), REF);
	assert.equal(relayRef("garbage"), "");
});

// A fake pi: the command handler is taken from registerCommand, the events bus records the RPC call.
test("relay: a subagent session never relays; the main session reports the reply to the gateway", async () => {
	const emitted = [];
	const listeners = new Map();
	const bus = {
		on(event, fn) { listeners.set(event, fn); return () => listeners.delete(event); },
		emit(event, data) { emitted.push([event, data]); if (event === "subagents:rpc:v1:request") listeners.get("subagents:rpc:v1:reply:" + data.requestId)?.({ success: true, data: { runId: "run-9" } }); },
	};
	const reports = [];
	const child = { sessionManager: { getSessionFile: () => "/x/main/run-1/run-1/session.jsonl" } };
	const main = { sessionManager: { getSessionFile: () => "/x/main/session.jsonl" } };
	await relayCommand(relayJSON({ ref: REF, method: "steer", params: { id: "p", message: "m" } }), child, bus, async (r) => reports.push(r));
	assert.equal(emitted.length, 0);
	assert.equal(reports.length, 0);
	await relayCommand(relayJSON({ ref: REF, method: "steer", params: { id: "p", message: "m" } }), main, bus, async (r) => reports.push(r));
	assert.equal(emitted[0][0], "subagents:rpc:v1:request");
	assert.deepEqual(emitted[0][1].params, { id: "p", message: "m", mode: "steer" });
	assert.deepEqual(reports, [{ ref: REF, ok: true, run_id: "run-9" }]);
	// a refused request is reported too, and nothing reaches pi-subagents
	emitted.length = 0;
	reports.length = 0;
	await relayCommand(relayJSON({ ref: REF, method: "stop", params: {} }), main, bus, async (r) => reports.push(r));
	assert.equal(emitted.length, 0);
	assert.equal(reports[0].ok, false);
	assert.equal(reports[0].code, "refused");
});

test("relay: no reply from pi-subagents within the time limit is a timeout", async () => {
	const bus = { on: () => undefined, emit: () => undefined };
	const r = await relayCall(bus, { ref: REF, method: "resume", params: { id: "p", message: "m" } }, 5);
	assert.deepEqual(r, { ok: false, code: "timeout", message: "no reply from pi-subagents" });
});
