// Parity of the redirected tools (exec-bridge.ts) with pi's built-in tools on the same
// file tree (code review, test gap "parity of the bridge tools against pi"; M2 find).
//
// Runs in the test image agw-parity (execution sandbox plus pi) as the agent user. pi's tools
// work directly on this container's file system; the bridge sends every operation through
// the socket to the test's orchestrator (internal/worker, TestBridgeParity), which runs it with
// agw-exec serve in exactly this container. Output: JSON on stdout.
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

// --- load the bridge with a mock ExtensionAPI ---
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

// --- file tree ---
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
	w("a.txt", "one\ntwo\nthree\n");
	w("empty.txt", "");
	w("sub/b.md", "# Title\n");
	w("sub/deep/c.md", "deep\n");
	w("sub/deep/d.ts", "export const x = 1;\n");
	w(".hidden/h.txt", "hidden\n");
	w("g/one.txt", "before\ntwo Needle here\nafter\none more NEEDLE\na.b and axb\n" + "L".repeat(700) + " Needle\n");
	w("g/two.txt", "Needle two\n");
	w("img.png", png1x1());
	w("big.txt", Array.from({ length: 3000 }, (_, i) => `Line ${i + 1}`).join("\n") + "\n");
	w("long.txt", "x".repeat(60000) + "\nshort\n");
	w("bin.dat", Buffer.from([0x61, 0x00, 0xff, 0xfe, 0x0a, 0x62]));
	w("git/.gitignore", "ignored.txt\nbuild/\n");
	w("git/ignored.txt", "x\n");
	w("git/kept.txt", "x\n");
	w("git/build/out.txt", "x\n");
	symlinkSync(`${ROOT}/doesnotexist`, `${ROOT}/broken`);
	// above MaxFileBytes (64 MiB): read reads an excerpt
	const line = "Line with some text for padding\n";
	const n = Math.ceil((70 << 20) / Buffer.byteLength(line));
	writeFileSync(`${ROOT}/huge.txt`, line.repeat(n) + "End");
}

// --- make results comparable ---
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

// --- cases ---
// setup: before each of the two runs (writing tools change the tree).
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
	["write", { path: "new/dir/f.txt", content: "new\n" }, () => rmSync(`${ROOT}/new`, { recursive: true, force: true })],
	["write", { path: "a2.txt", content: "über\n" }, () => w("a2.txt", "old\n")],
	["write", { path: "/usr/local/x.txt", content: "x" }],
	["write", { path: "sub", content: "x" }],
	["edit", { path: "e.txt", edits: [{ oldText: "hello", newText: "hi" }] }, () => w("e.txt", "hello world\n")],
	["edit", { path: "e.txt", edits: [{ oldText: "missing", newText: "x" }] }, () => w("e.txt", "hello world\n")],
	["edit", { path: "nope.txt", edits: [{ oldText: "a", newText: "b" }] }],
	["ls", { path: "." }],
	["ls", { path: "sub" }],
	["ls", { path: "nope" }],
	["ls", { path: "a.txt" }],
	["ls", { path: ".", limit: 2 }],
	["grep", { pattern: "Needle", path: "g/one.txt" }],
	["grep", { pattern: "needle", path: "g/one.txt", ignoreCase: true, context: 1 }],
	["grep", { pattern: "a.b", path: "g/one.txt", literal: true }],
	["grep", { pattern: "Needle", path: "g/one.txt", limit: 1 }],
	["grep", { pattern: "doesnotexist", path: "g" }],
	["grep", { pattern: "x", path: "nope" }],
	["grep", { pattern: "(", path: "g" }],
	["grep", { pattern: "Needle", path: "g" }, null, { sortLines: true }],
	["grep", { pattern: "Needle", glob: "two.*", path: "g" }],
	["find", { pattern: "*.txt" }, null, { sortLines: true }],
	["find", { pattern: "*.md" }, null, { sortLines: true }],
	["find", { pattern: "deep" }, null, { sortLines: true }],
	["find", { pattern: "sub/**/*.md" }, null, { sortLines: true }],
	["find", { pattern: "deep/*.ts" }, null, { sortLines: true }],
	["find", { pattern: "*.txt", path: "git" }, null, { sortLines: true }],
	["find", { pattern: "*.nothing" }],
	["find", { pattern: "*", path: "nope" }],
	["find", { pattern: "*.txt", limit: 2 }, null, { sortLines: true, countOnly: true }],
	["bash", { command: "echo hello" }],
	["bash", { command: "true" }],
	["bash", { command: "echo error >&2; exit 3" }],
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
		// Which matches fd returns at a limit depends on the order.
		const count = (r) => ({ ...r, content: r.content?.map((c) => ({ ...c, text: c.text.split("\n").length })) });
		na = count(na);
		nb = count(nb);
	}
	const equal = JSON.stringify(na) === JSON.stringify(nb) && aFull === bFull;
	results.push({ name, equal, builtin: na, bridge: nb, fullEqual: aFull === bFull, fullSaved: !!bFull });
}
// Bridge only: sandbox limits pi does not know (M3, M2). The notice must not suggest a
// limit the sandbox does not deliver (otherwise the model asks in circles).
w("many.txt", "match\n".repeat(1500));
const checks = [];
for (const [tool, params, want, notWant] of [
	["grep", { pattern: "match", path: "many.txt", limit: 5000 }, "1000 matches limit reached (maximum per call)", "limit=10000"],
	["grep", { pattern: "match", path: "many.txt", limit: 600 }, "Use limit=1000 for more (maximum per call)", "limit=1200"],
	["grep", { pattern: "match", path: "many.txt", limit: 200 }, "Use limit=400 for more", "maximum"],
]) {
	n++;
	const r = await run(bridged[tool], tool, params, `call_parity_${n}`);
	const text = r.error ?? r.value?.content?.[0]?.text ?? "";
	checks.push({ name: `${tool} ${JSON.stringify(params)}`, ok: text.includes(want) && !text.includes(notWant), text: text.slice(-200) });
}
// None of this may be left behind in the container where it does not belong.
const leftovers = execFileSync("bash", ["-c", "ls /tmp | grep -c '^pi-bash-' || true"]).toString().trim();
process.stdout.write(JSON.stringify({ results, checks, leftovers }) + "\n");
