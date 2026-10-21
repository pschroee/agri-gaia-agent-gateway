// pi extension: offers web_search and web_extract (pi-searxng-suite) to the model only while the
// chat's internet grants cover them (issue #175): web_search needs a search or full grant, web_extract a search,
// domains or full grant. It queries the socket (GET /tool/internet → {enabled, search, extract}).
//
// The extension is not a control point: the grants are enforced at the orchestrator's web proxy,
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

/** Which web tools the chat's grants cover right now. */
export type WebWanted = { web_search: boolean; web_extract: boolean };
const NONE: WebWanted = { web_search: false, web_extract: false };

function internetWanted(): Promise<WebWanted> {
	return new Promise((resolve) => {
		const req = request({ socketPath: SOCKET, path: "/tool/internet", method: "GET", timeout: 5000 }, (res) => {
			let data = "";
			res.setEncoding("utf8");
			res.on("data", (c) => (data += c));
			res.on("end", () => {
				try {
					resolve(wantedFrom(JSON.parse(data)));
				} catch {
					resolve(NONE);
				}
			});
		});
		req.on("error", () => resolve(NONE));
		req.on("timeout", () => {
			req.destroy();
			resolve(NONE);
		});
		req.end();
	});
}

/** The answer of GET /tool/internet as the tools to show. An older answer with only "enabled" means both tools. */
export function wantedFrom(answer: any): WebWanted {
	if (answer?.enabled !== true) return NONE;
	if (typeof answer.search !== "boolean" && typeof answer.extract !== "boolean") return { web_search: true, web_extract: true };
	return { web_search: answer.search === true, web_extract: answer.extract === true };
}

/** New list of active tools: add the web tools the grants cover, and (if removeAllowed) remove those they do not. */
export function nextActive(active: string[], registered: string[], wanted: WebWanted, removeAllowed: boolean): string[] | undefined {
	const add = WEB_TOOLS.filter((t) => wanted[t as keyof WebWanted] && registered.includes(t) && !active.includes(t));
	const drop = WEB_TOOLS.filter((t) => !wanted[t as keyof WebWanted] && active.includes(t));
	if (add.length === 0 && !(removeAllowed && drop.length > 0)) return undefined;
	let next = [...active, ...add];
	if (removeAllowed) next = next.filter((t) => !drop.includes(t));
	return [...new Set(next)];
}

export default function (pi: ExtensionAPI) {
	const sync = async (removeAllowed: boolean) => {
		const registered = pi.getAllTools().map((t: any) => t.name);
		const next = nextActive(pi.getActiveTools(), registered, await internetWanted(), removeAllowed);
		if (next) pi.setActiveTools(next);
	};
	pi.on("session_start", async () => sync(true));
	pi.on("before_agent_start", async () => {
		await sync(true);
	});
	pi.on("turn_start", async () => sync(false));
}
