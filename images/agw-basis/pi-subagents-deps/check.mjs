// Check while building the pi image (images/agw-basis/Dockerfile): the copy of pi-subagents under
// /opt/agw/pihome/npm/node_modules/pi-subagents has the expected version, its runtime
// dependencies match package.json of this folder (and thus the lockfile), and
// scripted-workflow.js reports via workflowWorkerModule where the worker comes from:
// without the variable node:worker_threads, with the variable remote-worker.mjs. Any deviation aborts the build.
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const PKG = "/opt/agw/pihome/npm/node_modules/pi-subagents";
const REMOTE_WORKER = "/opt/agw/ext/remote-worker.mjs";
const want = process.env.PI_SUBAGENTS_VERSION;

function fail(msg) {
	console.error(`pi-subagents: ${msg}`);
	process.exit(1);
}
const json = (p) => JSON.parse(readFileSync(p, "utf8"));

const pkg = json(join(PKG, "package.json"));
if (!want || pkg.version !== want) fail(`version ${pkg.version}, expected ${want}`);

const deps = json(new URL("./package.json", import.meta.url)).dependencies;
const a = JSON.stringify(Object.entries(pkg.dependencies ?? {}).sort());
const b = JSON.stringify(Object.entries(deps).sort());
if (a !== b) fail(`dependencies of the copy ${a} differ from images/agw-basis/pi-subagents-deps ${b}`);
for (const [name, version] of Object.entries(deps)) {
	const got = json(join(PKG, "node_modules", name, "package.json")).version;
	if (got !== version) fail(`${name} ${got} installed, expected ${version}`);
}

// The variable is read when the module loads, hence a separate process per case.
function workerModule(env) {
	const code = `const m = await import(${JSON.stringify(join(PKG, "src/workflows/scripted-workflow.js"))}); process.stdout.write(String(m.workflowWorkerModule));`;
	const e = { ...process.env };
	delete e.PI_SUBAGENTS_WORKFLOW_WORKER;
	return execFileSync(process.execPath, ["--input-type=module", "-e", code], { env: { ...e, ...env }, encoding: "utf8" });
}
const off = workerModule({});
if (off !== "node:worker_threads") fail(`workflowWorkerModule without the variable: ${off}`);
const on = workerModule({ PI_SUBAGENTS_WORKFLOW_WORKER: REMOTE_WORKER });
if (on !== REMOTE_WORKER) fail(`workflowWorkerModule with the variable: ${on}`);

console.log(`pi-subagents ${pkg.version}: dependencies ${b}, workflowWorkerModule ${off} or ${on}`);
