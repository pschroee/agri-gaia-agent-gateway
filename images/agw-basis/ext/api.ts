// pi-Extension der Variante "api" (Schritt 2, REST-Variante): ein einziges Werkzeug platform_http,
// mit dem der Agent die REST-API der Agri-Gaia-Plattform direkt aufruft, so wie sie in deren
// OpenAPI-Beschreibung steht. Er meldet sich nicht an; die Anfrage geht über den Unix-Socket des
// Platzes an den REST-Endpunkt des Orchestrators (/platform-api/…), der sie gegen die übertragenen
// Rechte prüft, schreibende Aufrufe vom Nutzer bestätigen lässt und das Token selbst einsetzt.
//
// Die Extension ist kein Kontrollpunkt: Sie reicht Methode, Pfad, Abfrage und Körper weiter und
// zeigt Status und Antwort. Geprüft wird allein im Orchestrator.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { request } from "node:http";

const SOCKET = process.env.AGW_SOCKET || "/run/agw/agw.sock";
const PREFIX = "/platform-api";
// Was das Modell von einer Antwort sieht; der Orchestrator liefert bis 8 MiB.
const MAX_SHOWN = 64 * 1024;

const description = `Ruft die REST-API der Agri-Gaia-Plattform auf (Datensätze, Modelle, Training, Aufgaben, Edge-Geräte, Container-Abbilder). Du meldest dich nicht an; Anmeldung, Prüfung gegen deine übertragenen Rechte und die Bestätigung schreibender Aufrufe durch den Nutzer übernimmt der Orchestrator.
- Pfade wie in der OpenAPI-Beschreibung der Plattform, etwa GET /datasets, GET /datasets/3, GET /train/providers, POST /train/config, GET /tasks/12.
- Welche Pfade es gibt: GET /_agw/paths (eine Zeile je Operation, mit query prefix=/train eingrenzbar). Welche Rechte du hast: GET /_agw/rights.
- Körper nur als JSON; Dateien lassen sich so nicht hochladen.
- Antwort 403 mit „verweigert vom Autorisierungsdienst": außerhalb deiner Rechte, nicht auf anderem Weg versuchen. 403 mit „vom Nutzer abgelehnt": der Nutzer hat den schreibenden Aufruf abgelehnt.
- Schreibende Aufrufe warten, bis der Nutzer entscheidet.`;

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
				timeout: 0, // schreibende Aufrufe warten auf den Nutzer
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
		label: "Plattform-API",
		description,
		promptSnippet: "platform_http: REST-API der Agri-Gaia-Plattform aufrufen (ohne Anmeldung, geprüft vom Orchestrator)",
		parameters: {
			type: "object",
			properties: {
				method: { type: "string", description: "GET, POST, PUT, PATCH oder DELETE" },
				path: { type: "string", description: "Pfad der Plattform-API, etwa /datasets/3 (ohne Abfrage)" },
				query: { type: "object", additionalProperties: { type: "string" }, description: "Abfrageparameter, etwa {\"limit\":\"10\"}" },
				body: { description: "JSON-Körper (nur bei POST, PUT, PATCH, DELETE)" },
			},
			required: ["method", "path"],
		},
		async execute(_toolCallId: string, params: any, signal: AbortSignal) {
			const p = params ?? {};
			if (typeof p.method !== "string" || typeof p.path !== "string") throw new Error("method und path fehlen");
			const r = await call(p.method, p.path, p.query, p.body, signal);
			let text = r.text;
			if (text.length > MAX_SHOWN) text = text.slice(0, MAX_SHOWN) + "\n[… gekürzt; mit skip/limit oder einem engeren Aufruf nachfragen]";
			const loc = r.headers?.location ? ` · Location: ${r.headers.location}` : "";
			return { content: [{ type: "text", text: `HTTP ${r.status}${loc}\n${text}` }], details: { status: r.status } };
		},
	});
}
