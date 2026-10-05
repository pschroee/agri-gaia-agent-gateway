// Runtime for workflowScript (pi-subagents) in the execution sandbox. Started by
// agw-exec (operation workflow) as the agent user; replaces the worker thread in which
// pi-subagents otherwise runs the script in the pi process (P4b: escape from node:vm).
//
// stdin, one JSON per line: first {"source": <source code of the pi-subagents worker>}, then
// {"m": <message from the host>}. stdout, per line: {"m": <message from the worker>} or
// {"__agw": "error", "message": …}. The worker runs here in a worker thread as in
// pi-subagents; an escape from its node:vm only reaches this process.
"use strict";
const { Worker } = require("node:worker_threads");
const readline = require("node:readline");

const acornPath = require.resolve("acorn", { paths: [__dirname] });
const out = (obj) => process.stdout.write(JSON.stringify(obj) + "\n");
let worker;
let exited = false;
// When the input ends, we end the worker ourselves (terminate). Node reports that as exit with
// code 1; that is then not an error of the script.
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
