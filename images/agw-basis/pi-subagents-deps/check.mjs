// Prüfung beim Bau des pi-Abbilds (images/agw-basis/Dockerfile): Die Kopie von pi-subagents unter
// /opt/agw/pihome/npm/node_modules/pi-subagents hat die erwartete Version, ihre
// Laufzeitabhängigkeiten stimmen mit package.json dieses Ordners (und damit mit dem Lockfile)
// überein, und scripted-workflow.js meldet über workflowWorkerModule, woher der Worker kommt:
// ohne Variable node:worker_threads, mit Variable remote-worker.mjs. Jede Abweichung bricht den Bau ab.
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
if (!want || pkg.version !== want) fail(`Version ${pkg.version}, erwartet ${want}`);

const deps = json(new URL("./package.json", import.meta.url)).dependencies;
const a = JSON.stringify(Object.entries(pkg.dependencies ?? {}).sort());
const b = JSON.stringify(Object.entries(deps).sort());
if (a !== b) fail(`Abhängigkeiten der Kopie ${a} weichen von images/agw-basis/pi-subagents-deps ab ${b}`);
for (const [name, version] of Object.entries(deps)) {
	const got = json(join(PKG, "node_modules", name, "package.json")).version;
	if (got !== version) fail(`${name} ${got} installiert, erwartet ${version}`);
}

// Die Variable wird beim Laden des Moduls gelesen, deshalb je Fall ein eigener Prozess.
function workerModule(env) {
	const code = `const m = await import(${JSON.stringify(join(PKG, "src/workflows/scripted-workflow.js"))}); process.stdout.write(String(m.workflowWorkerModule));`;
	const e = { ...process.env };
	delete e.PI_SUBAGENTS_WORKFLOW_WORKER;
	return execFileSync(process.execPath, ["--input-type=module", "-e", code], { env: { ...e, ...env }, encoding: "utf8" });
}
const off = workerModule({});
if (off !== "node:worker_threads") fail(`workflowWorkerModule ohne Variable: ${off}`);
const on = workerModule({ PI_SUBAGENTS_WORKFLOW_WORKER: REMOTE_WORKER });
if (on !== REMOTE_WORKER) fail(`workflowWorkerModule mit Variable: ${on}`);

console.log(`pi-subagents ${pkg.version}: Abhängigkeiten ${b}, workflowWorkerModule ${off} bzw. ${on}`);
