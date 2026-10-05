// Gleichlauf der umgeleiteten Werkzeuge (exec-bridge.ts) mit pis eingebauten Werkzeugen am selben
// Dateibaum (Code-Review, Testlücke „Gleichlauf der Bridge-Werkzeuge gegen pi“; M2 find).
//
// Läuft im Test-Abbild agw-parity (Ausführungs-Sandbox plus pi) als Agent-Nutzer. pis Werkzeuge
// arbeiten direkt auf dem Dateisystem dieses Containers; die Bridge schickt jede Operation über
// den Socket an den Orchestrator des Tests (internal/worker, TestBridgeParity), der sie mit
// agw-exec serve in genau diesem Container ausführt. Ausgabe: JSON auf stdout.
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import {
	createBashTool,
	createEditTool,
	createFindTool,
	createGrepTool,
	createLsTool,
	createReadTool,
	createWriteTool,
} from "@earendil-works/pi-coding-agent";
import bridge from "../ext/exec-bridge.ts";

const ROOT = "/workspace/parity";
const only = process.env.PARITY_ONLY ? new RegExp(process.env.PARITY_ONLY) : null;

// --- Bridge mit einem nachgebildeten ExtensionAPI laden ---
const bridged = {};
bridge({
	registerTool: (t) => {
		bridged[t.name] = t;
	},
	on: () => {},
	getActiveTools: () => [],
	setActiveTools: () => {},
});
const builtin = {
	read: createReadTool(ROOT),
	write: createWriteTool(ROOT),
	edit: createEditTool(ROOT),
	bash: createBashTool(ROOT),
	ls: createLsTool(ROOT),
	grep: createGrepTool(ROOT),
	find: createFindTool(ROOT),
};
const ctx = { cwd: ROOT, sessionManager: { getSessionFile: () => "", getSessionId: () => "parity" } };

// --- Dateibaum ---
function w(p, content) {
	mkdirSync(`${ROOT}/${p}`.replace(/\/[^/]*$/, ""), { recursive: true });
	writeFileSync(`${ROOT}/${p}`, content);
}
function png1x1() {
	return Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==", "base64");
}
function setupTree() {
	rmSync(ROOT, { recursive: true, force: true });
	mkdirSync(ROOT, { recursive: true });
	w("a.txt", "eins\nzwei\ndrei\n");
	w("empty.txt", "");
	w("sub/b.md", "# Titel\n");
	w("sub/deep/c.md", "tief\n");
	w("sub/deep/d.ts", "export const x = 1;\n");
	w(".hidden/h.txt", "versteckt\n");
	w("g/one.txt", "vorher\nzwei Nadel hier\nnachher\nnoch eine NADEL\na.b und axb\n" + "L".repeat(700) + " Nadel\n");
	w("g/two.txt", "Nadel zwei\n");
	w("img.png", png1x1());
	w("big.txt", Array.from({ length: 3000 }, (_, i) => `Zeile ${i + 1}`).join("\n") + "\n");
	w("long.txt", "x".repeat(60000) + "\nkurz\n");
	w("bin.dat", Buffer.from([0x61, 0x00, 0xff, 0xfe, 0x0a, 0x62]));
	w("git/.gitignore", "ignoriert.txt\nbuild/\n");
	w("git/ignoriert.txt", "x\n");
	w("git/drin.txt", "x\n");
	w("git/build/out.txt", "x\n");
	symlinkSync(`${ROOT}/gibtsnicht`, `${ROOT}/kaputt`);
	// über MaxFileBytes (64 MiB): read liest einen Ausschnitt
	const line = "Zeile mit etwas Text zum Auffüllen\n";
	const n = Math.ceil((70 << 20) / Buffer.byteLength(line));
	writeFileSync(`${ROOT}/huge.txt`, line.repeat(n) + "Ende");
}

// --- Ergebnisse vergleichbar machen ---
const FULL_RE = /\/tmp\/pi-bash-[0-9a-f]+\.log/g;
function normalize(r, opts = {}) {
	if (r.error !== undefined) return { error: r.error.replace(FULL_RE, "<FULL>") };
	const content = (r.value?.content ?? []).map((c) => {
		if (c.type !== "text") return c;
		let text = c.text.replace(FULL_RE, "<FULL>");
		if (opts.sortLines) {
			const [body, ...rest] = text.split("\n\n[");
			text = body.split("\n").sort().join("\n") + (rest.length ? "\n\n[" + rest.join("\n\n[") : "");
		}
		return { ...c, text };
	});
	const details = r.value?.details ? JSON.parse(JSON.stringify(r.value.details).replace(FULL_RE, "<FULL>")) : undefined;
	return { content, details: opts.ignoreDetails ? undefined : details };
}
async function run(impl, name, params, id) {
	try {
		const value = await impl.execute(id, structuredClone(params), undefined, undefined, ctx);
		return { value };
	} catch (e) {
		return { error: e instanceof Error ? e.message : String(e) };
	}
}
function fullFile(r) {
	const p = r.value?.details?.fullOutputPath ?? (r.error ?? r.value?.content?.[0]?.text ?? "").match(FULL_RE)?.[0];
	return p && existsSync(p) ? readFileSync(p).toString("base64") : null;
}

// --- Fälle ---
// setup: vor jeder der beiden Ausführungen (Schreibwerkzeuge verändern den Baum).
const cases = [
	["read", { path: "a.txt" }],
	["read", { path: "a.txt", offset: 2, limit: 1 }],
	["read", { path: "nope.txt" }],
	["read", { path: "sub" }],
	["read", { path: "empty.txt" }],
	["read", { path: "big.txt" }],
	["read", { path: "big.txt", offset: 2500 }],
	["read", { path: "big.txt", offset: 5000 }],
	["read", { path: "big.txt", offset: 10, limit: 5 }],
	["read", { path: "long.txt" }],
	["read", { path: "img.png" }],
	["read", { path: "bin.dat" }],
	["read", { path: "huge.txt" }],
	["read", { path: "huge.txt", offset: 1000000, limit: 3 }],
	["read", { path: "huge.txt", offset: 2000000, limit: 3 }],
	["write", { path: "neu/verz/f.txt", content: "neu\n" }, () => rmSync(`${ROOT}/neu`, { recursive: true, force: true })],
	["write", { path: "a2.txt", content: "über\n" }, () => w("a2.txt", "alt\n")],
	["write", { path: "/usr/local/x.txt", content: "x" }],
	["write", { path: "sub", content: "x" }],
	["edit", { path: "e.txt", edits: [{ oldText: "hallo", newText: "moin" }] }, () => w("e.txt", "hallo welt\n")],
	["edit", { path: "e.txt", edits: [{ oldText: "fehlt", newText: "x" }] }, () => w("e.txt", "hallo welt\n")],
	["edit", { path: "nope.txt", edits: [{ oldText: "a", newText: "b" }] }],
	["ls", { path: "." }],
	["ls", { path: "sub" }],
	["ls", { path: "nope" }],
	["ls", { path: "a.txt" }],
	["ls", { path: ".", limit: 2 }],
	["grep", { pattern: "Nadel", path: "g/one.txt" }],
	["grep", { pattern: "nadel", path: "g/one.txt", ignoreCase: true, context: 1 }],
	["grep", { pattern: "a.b", path: "g/one.txt", literal: true }],
	["grep", { pattern: "Nadel", path: "g/one.txt", limit: 1 }],
	["grep", { pattern: "gibtsnicht", path: "g" }],
	["grep", { pattern: "x", path: "nope" }],
	["grep", { pattern: "(", path: "g" }],
	["grep", { pattern: "Nadel", path: "g" }, null, { sortLines: true }],
	["grep", { pattern: "Nadel", glob: "two.*", path: "g" }],
	["find", { pattern: "*.txt" }, null, { sortLines: true }],
	["find", { pattern: "*.md" }, null, { sortLines: true }],
	["find", { pattern: "deep" }, null, { sortLines: true }],
	["find", { pattern: "sub/**/*.md" }, null, { sortLines: true }],
	["find", { pattern: "deep/*.ts" }, null, { sortLines: true }],
	["find", { pattern: "*.txt", path: "git" }, null, { sortLines: true }],
	["find", { pattern: "*.nichts" }],
	["find", { pattern: "*", path: "nope" }],
	["find", { pattern: "*.txt", limit: 2 }, null, { sortLines: true, countOnly: true }],
	["bash", { command: "echo hallo" }],
	["bash", { command: "true" }],
	["bash", { command: "echo fehler >&2; exit 3" }],
	["bash", { command: "printf 'ä\\377\\n'" }],
	["bash", { command: "head -c 60000 /dev/zero | tr '\\0' a" }, null, { full: true }],
	["bash", { command: "seq 1 3000" }, null, { full: true }],
	["bash", { command: "for i in $(seq 1 30); do head -c 3000 /dev/zero | tr '\\0' b; echo; done" }, null, { full: true }],
	["bash", { command: "sleep 5", timeout: 1 }],
	["bash", { command: "seq 1 2500; sleep 5", timeout: 1 }, null, { full: true }],
	["bash", { command: "echo x", timeout: -1 }],
	["bash", { command: "echo x", timeout: 0 }],
	["bash", { command: "echo x", timeout: 3000000 }],
	["bash", { command: "pwd; echo $PI_SESSION_ID" }],
];

setupTree();
const results = [];
let n = 0;
for (const [tool, params, setup, opts = {}] of cases) {
	const name = `${tool} ${JSON.stringify(params)}`;
	if (only && !only.test(name)) continue;
	n++;
	setup?.();
	const a = await run(builtin[tool], tool, params, `call_builtin_${n}`);
	const aFull = opts.full ? fullFile(a) : null;
	setup?.();
	const b = await run(bridged[tool], tool, params, `call_parity_${n}`);
	const bFull = opts.full ? fullFile(b) : null;
	let na = normalize(a, opts);
	let nb = normalize(b, opts);
	if (opts.countOnly) {
		// Welche Treffer fd bei einer Grenze liefert, hängt von der Reihenfolge ab.
		const count = (r) => ({ ...r, content: r.content?.map((c) => ({ ...c, text: c.text.split("\n").length })) });
		na = count(na);
		nb = count(nb);
	}
	const equal = JSON.stringify(na) === JSON.stringify(nb) && aFull === bFull;
	results.push({ name, equal, builtin: na, bridge: nb, fullEqual: aFull === bFull, fullSaved: !!bFull });
}
// Nur die Bridge: Grenzen der Sandbox, die pi nicht kennt (M3, M2). Der Hinweis darf kein
// limit vorschlagen, das die Sandbox nicht liefert (sonst fragt das Modell im Kreis).
w("many.txt", "Treffer\n".repeat(1500));
const checks = [];
for (const [tool, params, want, notWant] of [
	["grep", { pattern: "Treffer", path: "many.txt", limit: 5000 }, "1000 matches limit reached (maximum per call)", "limit=10000"],
	["grep", { pattern: "Treffer", path: "many.txt", limit: 600 }, "Use limit=1000 for more (maximum per call)", "limit=1200"],
	["grep", { pattern: "Treffer", path: "many.txt", limit: 200 }, "Use limit=400 for more", "maximum"],
]) {
	n++;
	const r = await run(bridged[tool], tool, params, `call_parity_${n}`);
	const text = r.error ?? r.value?.content?.[0]?.text ?? "";
	checks.push({ name: `${tool} ${JSON.stringify(params)}`, ok: text.includes(want) && !text.includes(notWant), text: text.slice(-200) });
}
// Nichts davon darf im Container liegen bleiben, das nicht dorthin gehört.
const leftovers = execFileSync("bash", ["-c", "ls /tmp | grep -c '^pi-bash-' || true"]).toString().trim();
process.stdout.write(JSON.stringify({ results, checks, leftovers }) + "\n");
