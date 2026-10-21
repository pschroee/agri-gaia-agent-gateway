// pi extension of the "api" variant (step 2, REST variant): the tool platform_http with
// which the agent calls the REST API of the Agri-Gaia platform directly, as described in its
// OpenAPI description. It does not log in; the request goes through the slot's Unix socket to
// the orchestrator's REST endpoint (/platform-api/…), which checks it against the delegated
// rights, has the user approve writing calls and inserts the token itself.
//
// Since issue #34 also request_internet (POST /internet, waits for the user's approval) and
// disable_internet (POST /internet/off, no approval): the binding has no shell, so without them
// it could neither ask for internet nor give it back. Since issue #175 a request names a kind (search or
// domains here; packages and full need the command line) and may list host names.
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
- Which paths exist: the system note lists the platform's operations and the gateway's own paths (/_agw/…). Which rights you have: GET /_agw/rights. What this setup offers (vision, internet, notebook, edge, pre-annotation, compute calls left, bindings): GET /_agw/capabilities.
- Body only as JSON; files cannot be uploaded this way.
- Response 403 with "denied by the authorization service" means the call was refused. 403 with "rejected by the user": the user rejected the writing call.
- Every call has an effect class: read runs directly; write, compute (building or starting a training; a chat has a limited number) and irreversible (deleting, publishing) wait until the user decides, irreversible ones always.
- Two or more writing calls in a row: propose them once with POST /_agw/plan (body {"title": …, "steps": [{"method","path","query","body","description"}]}); the user approves the plan once, and each call that matches the next step exactly then runs without a new approval. An id from an earlier step is written as "{{stepN.id}}": the object that step created. For a step that starts a platform job (POST /train/config, POST /container-images/build) it is known only when the job ends; the gateway's note names it. A placeholder never points at a step that only starts something (POST /train/containers/{train_container_id}/run). For a long task done without approving each call, add "mode": "autonomous" and a "budget" {"max_duration_minutes","max_llm_cost_usd","max_auto_turns"}; a step may set "max_attempts" (1-3). While an autonomous plan runs, a call outside the rights derived from its steps is refused without asking; to change course, propose a new plan with "replaces": "<plan_id>". Irreversible steps and steps that run code still ask.`;

function call(method: string, path: string, query: Record<string, string> | undefined, body: unknown, signal?: AbortSignal, toolCallId?: string): Promise<{ status: number; headers: any; text: string }> {
	let full = PREFIX + (path.startsWith("/") ? path : "/" + path);
	if (query && Object.keys(query).length > 0) {
		full += "?" + new URLSearchParams(Object.entries(query).map(([k, v]) => [k, String(v)])).toString();
	}
	const payload = body === undefined || body === null ? "" : typeof body === "string" ? body : JSON.stringify(body);
	// X-Agw-Tool-Call names the tool call of the session; the orchestrator stores it with the approval, so that a
	// plan proposed here carries its tool_call_id like the CLI and MCP bindings do (issue L2/#119).
	const headers: Record<string, string> = {};
	if (payload) {
		headers["content-type"] = "application/json";
		headers["content-length"] = String(Buffer.byteLength(payload));
	}
	// X-Agw-Pi-Tool-Call is the binding reference of the protocol (X5.3): the socket trusts it only on pi's socket,
	// where this extension runs. X-Agw-Tool-Call stays for approvals and plans and is recorded as claimed only.
	if (toolCallId) {
		headers["X-Agw-Tool-Call"] = toolCallId;
		headers["X-Agw-Pi-Tool-Call"] = toolCallId;
	}
	return new Promise((resolve, reject) => {
		const req = request(
			{
				socketPath: SOCKET,
				path: full,
				method: method.toUpperCase(),
				signal,
				timeout: 0, // writing calls wait for the user
				headers,
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

/** POST to the socket with a JSON body; resolves with the parsed JSON response or rejects with its error. */
function postSocket(path: string, body: unknown, signal?: AbortSignal): Promise<any> {
	const payload = body === undefined ? "" : JSON.stringify(body);
	return new Promise((resolve, reject) => {
		const req = request(
			{
				socketPath: SOCKET,
				path,
				method: "POST",
				signal,
				timeout: 0, // a request waits for the user
				headers: payload ? { "content-type": "application/json", "content-length": Buffer.byteLength(payload) } : {},
			},
			(res) => {
				let data = "";
				res.setEncoding("utf8");
				res.on("data", (c) => (data += c));
				res.on("end", () => {
					let parsed: any;
					try {
						parsed = JSON.parse(data);
					} catch {
						reject(new Error(`unreadable response from the orchestrator (${res.statusCode})`));
						return;
					}
					if (res.statusCode !== 200) reject(new Error(parsed?.error || `orchestrator responds ${res.statusCode}`));
					else resolve(parsed);
				});
			},
		);
		req.on("error", reject);
		req.end(payload);
	});
}

export default function (pi: ExtensionAPI) {
	pi.registerTool({
		name: "request_internet",
		label: "Request internet",
		description:
			"Asks the user for internet access, only as much as the task needs. kind: search (web_search and web_extract only), domains (https downloads from the listed host names, no wildcards). Give a short, concrete reason; the call waits for the user's decision. Access ends by itself after a time or data limit; switch it off earlier with disable_internet.",
		promptSnippet: "request_internet: ask the user for internet access of one kind (search or domains), with a reason; waits for the decision",
		parameters: {
			type: "object",
			properties: {
				kind: { type: "string", enum: ["search", "domains"], description: "search: web_search and web_extract only. domains: https to the listed host names." },
				reason: { type: "string", description: "Short, concrete reason for the user: what is the access needed for?" },
				domains: { type: "array", items: { type: "string" }, description: "kind domains: exact host names, no wildcards, at most 10" },
				minutes: { type: "integer", description: "Proposed duration in minutes; the gateway limits it." },
				megabytes: { type: "integer", description: "Proposed data limit in MB; the gateway limits it." },
			},
			required: ["kind", "reason"],
		},
		async execute(_toolCallId: string, params: any, signal: AbortSignal) {
			const body: Record<string, unknown> = {
				kind: typeof params?.kind === "string" ? params.kind : "",
				reason: typeof params?.reason === "string" ? params.reason : "",
			};
			if (Array.isArray(params?.domains)) body.domains = params.domains.filter((d: unknown) => typeof d === "string");
			if (Number.isInteger(params?.minutes)) body.minutes = params.minutes;
			if (Number.isInteger(params?.megabytes)) body.megabytes = params.megabytes;
			const r = await postSocket("/internet", body, signal);
			const word = r?.status === "approved" || r?.status === "refused" ? r.status : "rejected";
			return { content: [{ type: "text", text: `${word}: ${r?.message ?? ""}` }], details: { status: r?.status } };
		},
	});
	pi.registerTool({
		name: "disable_internet",
		label: "Switch internet off",
		description:
			"Ends internet access for this chat again (all grants, or the one named by grant_id), once downloads or web research are done. Needs no approval; if nothing is active, nothing changes.",
		promptSnippet: "disable_internet: end internet access again (no approval needed)",
		parameters: {
			type: "object",
			properties: { grant_id: { type: "integer", description: "The grant to end; omit to end all." } },
		},
		async execute(_toolCallId: string, params: any, signal: AbortSignal) {
			const body = Number.isInteger(params?.grant_id) ? { grant_id: params.grant_id } : undefined;
			const r = await postSocket("/internet/off", body, signal);
			return { content: [{ type: "text", text: `${r?.status}: ${r?.message ?? ""}` }], details: { status: r?.status } };
		},
	});

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
		async execute(toolCallId: string, params: any, signal: AbortSignal) {
			const p = params ?? {};
			if (typeof p.method !== "string" || typeof p.path !== "string") throw new Error("method and path are missing");
			const r = await call(p.method, p.path, p.query, p.body, signal, toolCallId);
			let text = r.text;
			if (text.length > MAX_SHOWN) text = text.slice(0, MAX_SHOWN) + "\n[… truncated; ask again with skip/limit or a narrower call]";
			const loc = r.headers?.location ? ` · Location: ${r.headers.location}` : "";
			// The gateway's note to the agent (X-Agw-Note, URL-encoded) comes last, as in the other bindings.
			let note = "";
			const rawNote = r.headers?.["x-agw-note"];
			if (typeof rawNote === "string" && rawNote !== "") {
				try {
					note = "\n" + decodeURIComponent(rawNote);
				} catch {
					note = "\n" + rawNote;
				}
			}
			return { content: [{ type: "text", text: `HTTP ${r.status}${loc}\n${text}${note}` }], details: { status: r.status } };
		},
	});
}
