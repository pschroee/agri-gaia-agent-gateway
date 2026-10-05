// pi extension of the "api" variant (step 2, REST variant): a single tool platform_http with
// which the agent calls the REST API of the Agri-Gaia platform directly, as described in its
// OpenAPI description. It does not log in; the request goes through the slot's Unix socket to
// the orchestrator's REST endpoint (/platform-api/…), which checks it against the delegated
// rights, has the user approve writing calls and inserts the token itself.
//
// The extension is not a control point: it passes on method, path, query and body and shows
// status and response. Checking happens only in the orchestrator.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { request } from "node:http";

const SOCKET = process.env.AGW_SOCKET || "/run/agw/agw.sock";
const PREFIX = "/platform-api";
// What the model sees of a response; the orchestrator delivers up to 8 MiB.
const MAX_SHOWN = 64 * 1024;

const description = `Calls the REST API of the Agri-Gaia platform (datasets, models, training, tasks, edge devices, container images). You do not log in; the orchestrator takes care of login, checking against your delegated rights and the user's approval of writing calls.
- Paths as in the platform's OpenAPI description, e.g. GET /datasets, GET /datasets/3, GET /train/providers, POST /train/config, GET /tasks/12.
- Which paths exist: GET /_agw/paths (one line per operation, narrow down with query prefix=/train). Which rights you have: GET /_agw/rights.
- Body only as JSON; files cannot be uploaded this way.
- Response 403 with "denied by the authorization service": outside your rights, do not try another way. 403 with "rejected by the user": the user rejected the writing call.
- Writing calls wait until the user decides.`;

function call(method: string, path: string, query: Record<string, string> | undefined, body: unknown, signal?: AbortSignal): Promise<{ status: number; headers: any; text: string }> {
	let full = PREFIX + (path.startsWith("/") ? path : "/" + path);
	if (query && Object.keys(query).length > 0) {
		full += "?" + new URLSearchParams(Object.entries(query).map(([k, v]) => [k, String(v)])).toString();
	}
	const payload = body === undefined || body === null ? "" : typeof body === "string" ? body : JSON.stringify(body);
	return new Promise((resolve, reject) => {
		const req = request(
			{
				socketPath: SOCKET,
				path: full,
				method: method.toUpperCase(),
				signal,
				timeout: 0, // writing calls wait for the user
				headers: payload ? { "content-type": "application/json", "content-length": Buffer.byteLength(payload) } : {},
			},
			(res) => {
				let data = "";
				res.setEncoding("utf8");
				res.on("data", (c) => (data += c));
				res.on("end", () => resolve({ status: res.statusCode ?? 0, headers: res.headers, text: data }));
			},
		);
		req.on("error", reject);
		req.end(payload);
	});
}

export default function (pi: ExtensionAPI) {
	pi.registerTool({
		name: "platform_http",
		label: "Platform API",
		description,
		promptSnippet: "platform_http: call the REST API of the Agri-Gaia platform (no login, checked by the orchestrator)",
		parameters: {
			type: "object",
			properties: {
				method: { type: "string", description: "GET, POST, PUT, PATCH or DELETE" },
				path: { type: "string", description: "Path of the platform API, e.g. /datasets/3 (without query)" },
				query: { type: "object", additionalProperties: { type: "string" }, description: "Query parameters, e.g. {\"limit\":\"10\"}" },
				body: { description: "JSON body (only for POST, PUT, PATCH, DELETE)" },
			},
			required: ["method", "path"],
		},
		async execute(_toolCallId: string, params: any, signal: AbortSignal) {
			const p = params ?? {};
			if (typeof p.method !== "string" || typeof p.path !== "string") throw new Error("method and path are missing");
			const r = await call(p.method, p.path, p.query, p.body, signal);
			let text = r.text;
			if (text.length > MAX_SHOWN) text = text.slice(0, MAX_SHOWN) + "\n[… truncated; ask again with skip/limit or a narrower call]";
			const loc = r.headers?.location ? ` · Location: ${r.headers.location}` : "";
			return { content: [{ type: "text", text: `HTTP ${r.status}${loc}\n${text}` }], details: { status: r.status } };
		},
	});
}
