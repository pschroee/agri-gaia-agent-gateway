// Stellvertreter für den Worker-Thread, in dem pi-subagents das Skript eines Workflows
// (workflowScript) ausführt (E9, Entscheidung des Verfassers zu P4b). Die Kopie von pi-subagents
// im Repo (third_party/pi-subagents, siehe VENDORED.md) nimmt `Worker` aus dem Modul in
// PI_SUBAGENTS_WORKFLOW_WORKER; das pi-Abbild setzt die Variable auf diese Datei
// (images/agw-basis/Dockerfile). pi-subagents benutzt den Worker nur über
// postMessage, terminate und die Ereignisse message, error und exit; diese Klasse bietet genau
// das und führt den Worker in der Ausführungs-Sandbox aus (POST /tool/workflow am Socket).
//
// Was von dort zurückkommt, ist Ausgabe von Code des Agenten. Bevor pi-subagents eine Nachricht
// sieht, prüft der Wächter aus exec-bridge.ts sie (checkWorkflowMessage): runs.host ist gesperrt,
// jeder Lauf (runs.run, runs.all) unterliegt denselben Regeln wie ein einzelner Subagent. Die
// Läufe selbst startet pi-subagents weiter im pi-Prozess; sie laden die Umleitung, ihre
// Werkzeuge laufen also ebenfalls in der Ausführungs-Sandbox.
//
// Nur Node-Bordmittel: pi-subagents lädt diese Datei direkt, ohne die Auflösung von pi.
import { EventEmitter } from "node:events";
import { request } from "node:http";

const SOCKET = process.env.AGW_SOCKET || "/run/agw/agw.sock";
// Von exec-bridge.ts gesetzt: claim(script) → {toolCallId, sessionFile} für ein vom Wächter
// freigegebenes Skript, checkMessage(msg) → Grund zum Sperren oder undefined.
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
			// Nur der Workflow-Worker von pi-subagents wird umgeleitet; andere Worker gibt es in pi
			// nicht über diesen Import.
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

	// pi-subagents beendet den Worker nach „complete“ mit terminate(). Das Ende der Anfrage sagt
	// der Sandbox, dass keine Nachrichten mehr kommen; der Runner endet dann regulär. Hängt er,
	// bricht das Schließen der Verbindung ihn nach fünf Sekunden ab.
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
				// Anfrage des Skripts gesperrt: Antwort direkt an den Worker, pi-subagents sieht sie nicht.
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
