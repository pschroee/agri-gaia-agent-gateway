// Stand-in for the worker thread in which pi-subagents runs a workflow's script
// (workflowScript) (E9, the author's decision on P4b). The copy of pi-subagents in the
// repo (third_party/pi-subagents, see VENDORED.md) takes `Worker` from the module in
// PI_SUBAGENTS_WORKFLOW_WORKER; the pi image sets the variable to this file
// (images/agw-basis/Dockerfile). pi-subagents uses the worker only through
// postMessage, terminate and the events message, error and exit; this class offers exactly
// that and runs the worker in the execution sandbox (POST /tool/workflow at the socket).
//
// What comes back from there is output of the agent's code. Before pi-subagents sees a message,
// the guard from exec-bridge.ts checks it (checkWorkflowMessage): runs.host is blocked, every
// run (runs.run, runs.all) is subject to the same rules as a single subagent. pi-subagents
// still starts the runs themselves in the pi process; they load the redirection, so their
// tools also run in the execution sandbox.
//
// Node built-ins only: pi-subagents loads this file directly, without pi's resolution.
import { EventEmitter } from "node:events";
import { request } from "node:http";

const SOCKET = process.env.AGW_SOCKET || "/run/agw/agw.sock";
// Set by exec-bridge.ts: claim(script) → {toolCallId, sessionFile} for a script approved by
// the guard, checkMessage(msg) → reason to block or undefined.
const REGISTRY = Symbol.for("agw.exec-bridge.workflow");

export class Worker extends EventEmitter {
	constructor(source, options) {
		super();
		this._source = typeof source === "string" ? source : String(source);
		this._req = undefined;
		this._queue = [];
		this._exited = false;
		this._buf = "";
		if (!(options && options.eval === true && options.workerData && typeof options.workerData.acornPath === "string")) {
			// Only the workflow worker of pi-subagents is redirected; pi has no other workers
			// through this import.
			queueMicrotask(() => this._fail(new Error("agw: unsupported worker")));
		}
	}

	postMessage(msg) {
		if (this._exited) return;
		if (this._req) {
			this._send(msg);
			return;
		}
		if (msg && msg.type === "start") this._open(msg);
		else this._queue.push(msg);
	}

	// pi-subagents ends the worker after "complete" with terminate(). The end of the request tells
	// the sandbox that no more messages are coming; the runner then ends normally. If it hangs,
	// closing the connection aborts it after five seconds.
	terminate() {
		const req = this._req;
		if (req && !req.destroyed) {
			req.end();
			const t = setTimeout(() => req.destroy(), 5000);
			t.unref?.();
		}
		this._exit(1);
		return Promise.resolve(1);
	}

	ref() {}
	unref() {}

	_open(start) {
		const reg = globalThis[REGISTRY];
		const meta = reg && typeof reg.claim === "function" ? reg.claim(start.script) : undefined;
		if (!meta) {
			this._fail(new Error("workflowScript was not approved by the execution sandbox guard (E9)"));
			return;
		}
		const req = request({ socketPath: SOCKET, path: "/tool/workflow", method: "POST", headers: { "content-type": "application/x-ndjson" }, timeout: 0 }, (res) => {
			res.setEncoding("utf8");
			res.on("data", (chunk) => this._onData(chunk));
			res.on("end", () => this._exit(this._exitCode ?? 1));
			res.on("error", (e) => this._fail(e));
			if (res.statusCode !== 200) this._statusError = res.statusCode;
		});
		req.on("error", (e) => this._fail(e));
		this._req = req;
		req.write(JSON.stringify({ toolCallId: meta.toolCallId, tool: "subagent", sessionFile: meta.sessionFile || "", source: this._source }) + "\n");
		this._send(start);
		for (const m of this._queue.splice(0)) this._send(m);
	}

	_send(msg) {
		if (!this._req || this._req.destroyed) return;
		this._req.write(JSON.stringify({ m: msg }) + "\n");
	}

	_onData(chunk) {
		this._buf += chunk;
		let i;
		while ((i = this._buf.indexOf("\n")) >= 0) {
			const line = this._buf.slice(0, i);
			this._buf = this._buf.slice(i + 1);
			if (line.trim()) this._onLine(line);
		}
	}

	_onLine(line) {
		let v;
		try {
			v = JSON.parse(line);
		} catch {
			return;
		}
		if (!v || typeof v !== "object") return;
		if (this._statusError) {
			this._fail(new Error(`orchestrator rejected the workflow: ${v.error ?? this._statusError}`));
			return;
		}
		if ("m" in v) {
			const msg = v.m;
			if (!msg || typeof msg !== "object") return;
			const reg = globalThis[REGISTRY];
			const reason = reg && typeof reg.checkMessage === "function" ? reg.checkMessage(msg) : "guard unavailable";
			if (reason) {
				// Script request blocked: reply directly to the worker, pi-subagents does not see it.
				if (msg.type === "call" && typeof msg.callId === "number") this._send({ type: "response", callId: msg.callId, ok: false, error: reason });
				return;
			}
			this.emit("message", msg);
			return;
		}
		if (v.__agw === "error") {
			this.emit("error", new Error(String(v.message ?? "workflow worker failed")));
			return;
		}
		if (v.done) {
			this._exitCode = typeof v.exit === "number" ? v.exit : 1;
			if (v.error && typeof v.exit !== "number") this.emit("error", new Error(String(v.error)));
		}
	}

	_fail(e) {
		if (this._exited) return;
		this.emit("error", e instanceof Error ? e : new Error(String(e)));
		this._exit(1);
	}

	_exit(code) {
		if (this._exited) return;
		this._exited = true;
		if (this._req && !this._req.destroyed) this._req.end();
		this.emit("exit", code);
	}
}
