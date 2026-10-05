// Test der lokalen Änderung 2 (VENDORED.md): Woher stammt der Worker von workflowScript?
//
// Aufruf (aus diesem Paketverzeichnis): PI_SUBAGENTS_DIR=<Kopie mit Abhängigkeiten> node --test test-agw/*.test.mjs
// Die Abhängigkeiten von pi-subagents (acorn u. a.) liegen nicht im Repo. PI_SUBAGENTS_DIR zeigt
// deshalb auf eine Kopie des Pakets mit `npm install --omit=dev`; ohne die Variable wird das
// Paket geprüft, in dem dieser Test liegt. Kein Netz nötig.
//
// Die Variable PI_SUBAGENTS_WORKFLOW_WORKER wird beim Laden des Moduls gelesen. Jeder Fall
// läuft deshalb in einem eigenen Node-Prozess.
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
		assert.equal(r.status, 0, `Kindprozess fehlgeschlagen:\n${r.stdout}\n${r.stderr}`);
		let lines = [];
		try {
			lines = readFileSync(log, "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l));
		} catch {}
		return { out: JSON.parse(r.stdout.trim().split("\n").at(-1)), log: lines };
	} finally {
		rmSync(dir, { recursive: true, force: true });
	}
}

// Ruft runWorkflowScript so weit auf, dass `new Worker(...)` erreicht wird; die jeweilige
// Attrappe wirft dort, runWorkflowScript endet mit „Workflow worker could not start: …“.
const RUN = `
const m = await import(process.env.AGW_MODULE_URL);
let error = null;
try { await m.runWorkflowScript({ script: "return 1;" }); } catch (e) { error = String(e?.message ?? e); }
console.log(JSON.stringify({ workflowWorkerModule: m.workflowWorkerModule, error }));
`;

test("ohne Variable: Worker aus node:worker_threads", () => {
	// Attrappe in das eingebaute Modul setzen, bevor pi-subagents es importiert. Landet der
	// Aufruf dort, stammt Worker nachweislich aus node:worker_threads.
	const code = `
import { createRequire, syncBuiltinESMExports } from "node:module";
import { appendFileSync } from "node:fs";
const wt = createRequire(import.meta.url)("node:worker_threads");
wt.Worker = class { constructor(s, o) {
  appendFileSync(process.env.AGW_MOCK_LOG, JSON.stringify({ module: "node:worker_threads", eval: o?.eval === true, hasAcornPath: typeof o?.workerData?.acornPath === "string" }) + "\\n");
  throw new Error("builtin-attrappe");
} };
syncBuiltinESMExports();
${RUN}`;
	const { out, log } = runChild(code, {});
	assert.equal(out.workflowWorkerModule, "node:worker_threads");
	assert.match(out.error, /Workflow worker could not start: builtin-attrappe/);
	assert.deepEqual(log, [{ module: "node:worker_threads", eval: true, hasAcornPath: true }]);
});

test("leere Variable gilt als nicht gesetzt", () => {
	const { out } = runChild(`const m = await import(process.env.AGW_MODULE_URL); console.log(JSON.stringify({ workflowWorkerModule: m.workflowWorkerModule }));`, {
		PI_SUBAGENTS_WORKFLOW_WORKER: "",
	});
	assert.equal(out.workflowWorkerModule, "node:worker_threads");
});

test("mit Variable: Worker aus dem angegebenen Modul (absoluter Pfad)", () => {
	const { out, log } = runChild(RUN, { PI_SUBAGENTS_WORKFLOW_WORKER: mockPath });
	assert.equal(out.workflowWorkerModule, mockPath);
	assert.match(out.error, /Workflow worker could not start: agw-attrappe/);
	assert.deepEqual(log, [{ module: "mock-worker.mjs", sourceIsString: true, eval: true, hasAcornPath: true }]);
});

test("mit Variable: node:worker_threads bleibt unberührt", () => {
	// Gegenprobe: Ist die Umleitung aktiv, darf die eingebaute Worker-Klasse nicht aufgerufen
	// werden.
	const code = `
import { createRequire, syncBuiltinESMExports } from "node:module";
import { appendFileSync } from "node:fs";
const wt = createRequire(import.meta.url)("node:worker_threads");
wt.Worker = class { constructor() { appendFileSync(process.env.AGW_MOCK_LOG, JSON.stringify({ module: "node:worker_threads" }) + "\\n"); throw new Error("falscher Weg"); } };
syncBuiltinESMExports();
${RUN}`;
	const { out, log } = runChild(code, { PI_SUBAGENTS_WORKFLOW_WORKER: mockPath });
	assert.equal(out.workflowWorkerModule, mockPath);
	assert.deepEqual(log.map((l) => l.module), ["mock-worker.mjs"]);
});

test("mit Variable auf ein fehlendes Modul: Laden schlägt fehl statt still zurückzufallen", () => {
	const { out } = runChild(
		`let error = null; try { await import(process.env.AGW_MODULE_URL); } catch (e) { error = e.code ?? String(e); } console.log(JSON.stringify({ error }));`,
		{ PI_SUBAGENTS_WORKFLOW_WORKER: join(here, "fixtures/gibt-es-nicht.mjs") },
	);
	assert.equal(out.error, "ERR_MODULE_NOT_FOUND");
});
