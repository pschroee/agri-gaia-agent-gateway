// pi extension: connects the orchestrator's MCP endpoint at the slot's Unix
// socket. pi has no built-in MCP; this extension speaks JSON-RPC 2.0 in
// stateless Streamable HTTP mode (POST /mcp) via Node http with socketPath
// and registers every MCP tool as a pi tool with the prefix "mcp_".
//
// The extension is not a control point. Checking and approval happen only in
// the orchestrator at the socket.
//
// mcp_upload_artifact names a path in the execution sandbox (E9). The file is not in the
// pi container; the orchestrator reads it there itself (POST /tool/upload, logged like
// a read, only inside /workspace) and sends it to the user like the MCP tool: stored at
// once, no approval (issue #62).
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
						reject(new Error(`unreadable response from the orchestrator (${res.statusCode})`));
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

// Response as JSON or as an SSE stream with one "message" event.
function parseResponse(data: string, contentType: string): any {
	let msg: any;
	if (contentType.includes("text/event-stream")) {
		const lines = data.split("\n").filter((l) => l.startsWith("data:"));
		const last = lines.map((l) => l.slice(5).trim()).filter(Boolean).pop();
		if (!last) throw new Error(`empty SSE response from the MCP endpoint: ${data.slice(0, 200)}`);
		msg = JSON.parse(last);
	} else {
		msg = JSON.parse(data);
	}
	if (msg.error) throw new Error(`MCP error ${msg.error.code}: ${msg.error.message}`);
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
		console.error(`[agw-mcp] MCP endpoint not reachable: ${(e as Error).message}`);
		return;
	}

	for (const tool of tools) {
		const name = `mcp_${tool.name}`;
		let schema = tool.inputSchema ?? { type: "object", properties: {} };
		if (tool.name === "upload_artifact") {
			// The model names a path; the extension reads the content itself.
			schema = {
				type: "object",
				properties: {
					path: { type: "string", description: "Path of the file in the sandbox" },
					name: { type: "string", description: "Name of the artifact (default: file name)" },
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
					if (typeof args.path !== "string") throw new Error("path is missing");
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
					const text = r.text || (r.status === "stored" ? `sent: "${r.name}" is in the chat for the user (${r.size} bytes, sha256 ${r.sha256})` : `not sent: "${r.name}" was not stored (${r.message || "not stored"})`);
					if (r.status !== "stored") throw new Error(text);
					return { content: [{ type: "text", text }], details: { structured: null } };
				}
				const result = await rpc("tools/call", { name: tool.name, arguments: args }, signal);
				const content = (result?.content ?? []).map((c: any) =>
					c.type === "text" ? { type: "text", text: c.text } : { type: "text", text: JSON.stringify(c) },
				);
				if (result?.isError) {
					throw new Error(content.map((c: any) => c.text).join("\n") || "MCP tool reports an error");
				}
				return { content, details: { structured: result?.structuredContent ?? null } };
			},
		});
	}
}
