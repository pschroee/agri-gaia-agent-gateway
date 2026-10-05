// pi-Extension: bietet web_search und web_extract (pi-searxng-suite) dem Modell nur an, solange der
// Chat Internet hat. Den Schalter fragt sie am Socket ab (GET /tool/internet).
//
// Die Extension ist kein Kontrollpunkt: Durchgesetzt wird der Schalter am Web-Proxy des
// Orchestrators, über den pis Node die Anfragen der beiden Werkzeuge schickt. Hier geht es nur
// darum, dass das Modell ohne Internet keine Werkzeuge sieht, die es nicht nutzen kann.
//
// Einblenden geht jederzeit vor dem nächsten Modellaufruf (turn_start; pi übernimmt rein additive
// Änderungen). Ausgeblendet wird nur zwischen zwei Aufträgen (before_agent_start), damit sich die
// Werkzeugliste nicht mitten im Durchgang verkleinert.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { request } from "node:http";

const SOCKET = process.env.AGW_SOCKET || "/run/agw/agw.sock";
export const WEB_TOOLS = ["web_search", "web_extract"];

function internetOn(): Promise<boolean> {
	return new Promise((resolve) => {
		const req = request({ socketPath: SOCKET, path: "/tool/internet", method: "GET", timeout: 5000 }, (res) => {
			let data = "";
			res.setEncoding("utf8");
			res.on("data", (c) => (data += c));
			res.on("end", () => {
				try {
					resolve(JSON.parse(data)?.enabled === true);
				} catch {
					resolve(false);
				}
			});
		});
		req.on("error", () => resolve(false));
		req.on("timeout", () => {
			req.destroy();
			resolve(false);
		});
		req.end();
	});
}

/** Neue Liste aktiver Werkzeuge: mit Internet die Web-Werkzeuge dazu, ohne (wenn removeAllowed) weg. */
export function nextActive(active: string[], registered: string[], on: boolean, removeAllowed: boolean): string[] | undefined {
	const has = WEB_TOOLS.filter((t) => active.includes(t));
	const avail = WEB_TOOLS.filter((t) => registered.includes(t));
	if (on && has.length < avail.length) return [...new Set([...active, ...avail])];
	if (!on && removeAllowed && has.length > 0) return active.filter((t) => !WEB_TOOLS.includes(t));
	return undefined;
}

export default function (pi: ExtensionAPI) {
	const sync = async (removeAllowed: boolean) => {
		const registered = pi.getAllTools().map((t: any) => t.name);
		const next = nextActive(pi.getActiveTools(), registered, await internetOn(), removeAllowed);
		if (next) pi.setActiveTools(next);
	};
	pi.on("session_start", async () => sync(true));
	pi.on("before_agent_start", async () => {
		await sync(true);
	});
	pi.on("turn_start", async () => sync(false));
}
