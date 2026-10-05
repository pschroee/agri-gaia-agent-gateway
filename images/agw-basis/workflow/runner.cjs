// Laufzeit für workflowScript (pi-subagents) in der Ausführungs-Sandbox. Gestartet von
// agw-exec (Operation workflow) als Agent-Nutzer; ersetzt den Worker-Thread, in dem
// pi-subagents das Skript sonst im pi-Prozess ausführt (P4b: Ausbruch aus node:vm).
//
// stdin, je Zeile JSON: zuerst {"source": <Quelltext des Workers von pi-subagents>}, danach
// {"m": <Nachricht des Hosts>}. stdout, je Zeile: {"m": <Nachricht des Workers>} oder
// {"__agw": "error", "message": …}. Der Worker läuft hier in einem Worker-Thread wie bei
// pi-subagents; ein Ausbruch aus dessen node:vm erreicht nur diesen Prozess.
"use strict";
const { Worker } = require("node:worker_threads");
const readline = require("node:readline");

const acornPath = require.resolve("acorn", { paths: [__dirname] });
const out = (obj) => process.stdout.write(JSON.stringify(obj) + "\n");
let worker;
let exited = false;
// Endet die Eingabe, beenden wir den Worker selbst (terminate). Node meldet das als exit mit
// Code 1; das ist dann kein Fehler des Skripts.
let terminating = false;

const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
rl.on("line", (line) => {
	if (!line.trim()) return;
	let v;
	try {
		v = JSON.parse(line);
	} catch {
		return;
	}
	if (!worker) {
		if (typeof v.source !== "string") process.exit(2);
		worker = new Worker(v.source, { eval: true, workerData: { acornPath } });
		worker.on("message", (m) => out({ m }));
		worker.on("error", (e) => out({ __agw: "error", message: String((e && e.message) || e) }));
		worker.on("exit", (code) => {
			exited = true;
			process.exit(terminating ? 0 : code);
		});
		return;
	}
	if (v && typeof v === "object" && "m" in v) worker.postMessage(v.m);
});
rl.on("close", () => {
	if (!worker) process.exit(0);
	if (!exited) {
		terminating = true;
		worker.terminate().then(() => process.exit(0));
	}
});
