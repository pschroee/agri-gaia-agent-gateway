// pi-Extension: bindet den MCP-Endpunkt des Orchestrators am Unix-Socket des
// Platzes an. pi bringt kein MCP mit; diese Extension spricht JSON-RPC 2.0 im
// zustandslosen Streamable-HTTP-Modus (POST /mcp) über Node-http mit socketPath
// und registriert jedes MCP-Werkzeug als pi-Werkzeug mit der Vorsilbe "mcp_".
//
// Die Extension ist kein Kontrollpunkt. Geprüft und bestätigt wird allein im
// Orchestrator am Socket.
//
// mcp_upload_artifact nennt einen Pfad in der Ausführungs-Sandbox (E9). Die Datei liegt nicht im
// Container von pi; der Orchestrator liest sie dort selbst (POST /tool/upload, protokolliert wie
// ein read) und reicht sie an denselben Upload mit Bestätigung weiter wie das MCP-Werkzeug.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { request } from "node:http";
import { resolve } from "node:path";

const SOCKET = process.env.AGW_SOCKET || "/run/agw/agw.sock";
let nextId = 1;

function postJSON(path: string, body: unknown, signal?: AbortSignal): Promise<any> {
	const payload = JSON.stringify(body);
	return new Promise((resolvePromise, reject) => {
		const req = request(
			{ socketPath: SOCKET, path, method: "POST", signal, timeout: 0,
				headers: { "content-type": "application/json", "content-length": Buffer.byteLength(payload) } },
			(res) => {
				let data = "";
				res.setEncoding("utf8");
				res.on("data", (c) => (data += c));
				res.on("end", () => {
					try {
						resolvePromise(JSON.parse(data));
					} catch {
						reject(new Error(`unlesbare Antwort vom Orchestrator (${res.statusCode})`));
					}
				});
			},
		);
		req.on("error", reject);
		req.end(payload);
	});
}

function rpc(method: string, params: unknown, signal?: AbortSignal): Promise<any> {
	const body = JSON.stringify({ jsonrpc: "2.0", id: nextId++, method, params });
	return new Promise((resolvePromise, reject) => {
		const req = request(
			{
				socketPath: SOCKET,
				path: "/mcp",
				method: "POST",
				headers: {
					"content-type": "application/json",
					accept: "application/json, text/event-stream",
					"content-length": Buffer.byteLength(body),
					"mcp-protocol-version": "2025-06-18",
				},
				signal,
				timeout: 0,
			},
			(res) => {
				let data = "";
				res.setEncoding("utf8");
				res.on("data", (c) => (data += c));
				res.on("end", () => {
					try {
						resolvePromise(parseResponse(data, res.headers["content-type"] || ""));
					} catch (e) {
						reject(e);
					}
				});
			},
		);
		req.on("error", reject);
		req.end(body);
	});
}

// Antwort als JSON oder als SSE-Strom mit einem "message"-Ereignis.
function parseResponse(data: string, contentType: string): any {
	let msg: any;
	if (contentType.includes("text/event-stream")) {
		const lines = data.split("\n").filter((l) => l.startsWith("data:"));
		const last = lines.map((l) => l.slice(5).trim()).filter(Boolean).pop();
		if (!last) throw new Error(`leere SSE-Antwort vom MCP-Endpunkt: ${data.slice(0, 200)}`);
		msg = JSON.parse(last);
	} else {
		msg = JSON.parse(data);
	}
	if (msg.error) throw new Error(`MCP-Fehler ${msg.error.code}: ${msg.error.message}`);
	return msg.result;
}

export default async function (pi: ExtensionAPI) {
	let tools: any[] = [];
	try {
		await rpc("initialize", {
			protocolVersion: "2025-06-18",
			capabilities: {},
			clientInfo: { name: "agw-pi-mcp", version: "0.1.0" },
		});
		tools = (await rpc("tools/list", {})).tools ?? [];
	} catch (e) {
		console.error(`[agw-mcp] MCP-Endpunkt nicht erreichbar: ${(e as Error).message}`);
		return;
	}

	for (const tool of tools) {
		const name = `mcp_${tool.name}`;
		let schema = tool.inputSchema ?? { type: "object", properties: {} };
		if (tool.name === "upload_artifact") {
			// Das Modell nennt einen Pfad; den Inhalt liest die Extension selbst.
			schema = {
				type: "object",
				properties: {
					path: { type: "string", description: "Pfad der Datei in der Sandbox" },
					name: { type: "string", description: "Name des Artefakts (Standard: Dateiname)" },
				},
				required: ["path"],
			};
		}
		pi.registerTool({
			name,
			label: `MCP: ${tool.name}`,
			description: tool.description ?? tool.name,
			promptSnippet: tool.description ?? tool.name,
			parameters: schema,
			async execute(toolCallId: string, params: any, signal: AbortSignal, _onUpdate: any, ctx: any) {
				const args = params ?? {};
				if (tool.name === "upload_artifact") {
					if (typeof args.path !== "string") throw new Error("path fehlt");
					let sessionFile = "";
					try {
						sessionFile = ctx?.sessionManager?.getSessionFile?.() ?? "";
					} catch {}
					const r = await postJSON(
						"/tool/upload",
						{ toolCallId, tool: name, sessionFile, path: resolve("/workspace", args.path), name: args.name ?? "" },
						signal,
					);
					if (r.error) throw new Error(r.error);
					if (r.status === "approved") {
						return { content: [{ type: "text", text: `bestätigt: Artefakt "${r.name}" gespeichert (${r.size} Bytes, sha256 ${r.sha256})` }], details: { structured: null } };
					}
					return { content: [{ type: "text", text: `abgelehnt: Artefakt "${r.name}" wurde nicht gespeichert (${r.message || "vom Nutzer abgelehnt"})` }], details: { structured: null } };
				}
				const result = await rpc("tools/call", { name: tool.name, arguments: args }, signal);
				const content = (result?.content ?? []).map((c: any) =>
					c.type === "text" ? { type: "text", text: c.text } : { type: "text", text: JSON.stringify(c) },
				);
				if (result?.isError) {
					throw new Error(content.map((c: any) => c.text).join("\n") || "MCP-Werkzeug meldet einen Fehler");
				}
				return { content, details: { structured: result?.structuredContent ?? null } };
			},
		});
	}
}
