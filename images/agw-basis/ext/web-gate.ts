// pi extension: offers web_search and web_extract (pi-searxng-suite) to the model only while the
// chat has internet. It queries the switch at the socket (GET /tool/internet).
//
// The extension is not a control point: the switch is enforced at the orchestrator's web proxy,
// through which pi's Node sends the requests of the two tools. This is only about the model not
// seeing tools without internet that it cannot use.
//
// Showing works any time before the next model call (turn_start; pi accepts purely additive
// changes). Hiding happens only between two requests (before_agent_start), so that the tool list
// does not shrink in the middle of a turn.
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

/** New list of active tools: with internet add the web tools, without it (if removeAllowed) remove them. */
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
