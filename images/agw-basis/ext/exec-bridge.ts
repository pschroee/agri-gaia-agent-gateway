// pi extension (E9): redirects the tools bash, read, write, edit, grep, find and ls into the
// execution sandbox. Every operation goes through the slot's socket to the orchestrator; it
// runs it in the execution sandbox and logs it (tool_executions) with the model's toolCallId
// and the session file (main agent or a subagent run).
//
// Plus a guard for the subagent tool: it only lets through parameters that bring no code and
// no agent definition into the pi container (checkpoints P4/P4b in
// docs/e9-execution-sandbox.md). The guard is a control point because after E9 no code of the
// agent runs in the pi process: the agent reaches pi only through the parameters of its calls.
//
// Background tasks: bash with run_in_background starts the command through the orchestrator in
// the execution sandbox and returns at once; bg_output and bg_stop query or stop it. The
// orchestrator itself tells the agent when it ends (docs/design.md, "Background tasks").
//
// Loaded in the main agent via -e and in every subagent via
// settings.json → subagents.defaultSubagentOnlyExtensions.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import {
	createBashTool,
	createEditTool,
	createFindTool,
	createGrepTool,
	createLsTool,
	createReadTool,
	createWriteTool,
	DEFAULT_MAX_BYTES,
	DEFAULT_MAX_LINES,
	type EditOperations,
	formatSize,
	type LsOperations,
	type ReadOperations,
	truncateHead,
	truncateLine,
	truncateTail,
	type WriteOperations,
} from "@earendil-works/pi-coding-agent";
import { createHash } from "node:crypto";
import { request } from "node:http";
import { homedir } from "node:os";
import path, { isAbsolute, join, resolve as resolvePath } from "node:path";

const SOCKET = process.env.AGW_SOCKET || "/run/agw/agw.sock";
const CWD = "/workspace";
// Limits of the execution sandbox (internal/execproto): above them it truncates silently.
const MAX_GREP_LIMIT = 1000;
const MAX_FIND_LIMIT = 100000;
// like pi (bash.js): setTimeout takes at most 2^31-1 ms
const MAX_TIMEOUT_MS = 2_147_483_647;
const BASH_UPDATE_THROTTLE_MS = 100;

type Meta = { toolCallId: string; tool: string; sessionFile: string };
type Frame = {
	result?: any;
	error?: string;
	code?: string;
	exit?: number;
	data?: string;
	done?: boolean;
	fullOutputPath?: string;
	spillError?: string;
	background?: string;
};

class ToolError extends Error {
	code?: string;
	constructor(message: string, code?: string) {
		super(message);
		this.code = code;
	}
}

// Error messages like Node (pi otherwise works with fs/promises): the model sees the same
// messages as without the redirection (code review L4).
const NODE_ERRORS: Record<string, string> = {
	ENOENT: "no such file or directory",
	EACCES: "permission denied",
	EISDIR: "illegal operation on a directory",
	ENOTDIR: "not a directory",
	EROFS: "read-only file system",
	ENOSPC: "no space left on device",
	EEXIST: "file already exists",
	ELOOP: "too many symbolic links encountered",
	ENAMETOOLONG: "name too long",
	EPERM: "operation not permitted",
};

function nodeError(e: unknown, syscall: string, p: string): unknown {
	const code = (e as ToolError)?.code;
	if (!code || !NODE_ERRORS[code]) return e;
	const err = new ToolError(`${code}: ${NODE_ERRORS[code]}, ${syscall} '${p}'`, code);
	return err;
}

function post(path: string, body: unknown, signal?: AbortSignal, onLine?: (line: string) => void): Promise<{ status: number; body: string }> {
	const payload = JSON.stringify(body);
	return new Promise((resolve, reject) => {
		if (signal?.aborted) {
			reject(new Error("aborted"));
			return;
		}
		const req = request(
			{
				socketPath: SOCKET,
				path,
				method: "POST",
				headers: { "content-type": "application/json", "content-length": Buffer.byteLength(payload) },
				signal,
				timeout: 0,
			},
			(res) => {
				let buf = "";
				let all = "";
				res.setEncoding("utf8");
				res.on("data", (c: string) => {
					if (!onLine) {
						all += c;
						return;
					}
					buf += c;
					let i: number;
					while ((i = buf.indexOf("\n")) >= 0) {
						const line = buf.slice(0, i);
						buf = buf.slice(i + 1);
						if (line.trim()) onLine(line);
					}
				});
				res.on("end", () => {
					if (onLine && buf.trim()) onLine(buf);
					resolve({ status: res.statusCode ?? 0, body: all });
				});
				res.on("error", reject);
			},
		);
		req.on("error", (e) => reject(signal?.aborted ? new Error("aborted") : e));
		req.end(payload);
	});
}

// op runs an operation and returns the result; sandbox errors become exceptions
// (as with the Node operations pi otherwise uses).
async function op(meta: Meta, req: Record<string, unknown>, signal?: AbortSignal): Promise<any> {
	const res = await post("/tool/op", { ...meta, req }, signal);
	let f: Frame;
	try {
		f = JSON.parse(res.body);
	} catch {
		throw new ToolError(`orchestrator: unreadable response (${res.status})`);
	}
	if (res.status !== 200) throw new ToolError(`orchestrator rejected the operation: ${(f as any).error ?? res.status}`);
	if (f.error) throw new ToolError(f.error, f.code);
	return f.result ?? {};
}

function metaOf(tool: string, id: string, ctx: any): Meta {
	let sessionFile = "";
	try {
		sessionFile = ctx?.sessionManager?.getSessionFile?.() ?? "";
	} catch {
		// no session (e.g. at startup): main session
	}
	return { toolCallId: id, tool, sessionFile };
}

// Paths like pi (utils/paths.js, resolveToCwd): Unicode spaces, leading "@", "~".
const UNICODE_SPACES = /[\u00A0\u2000-\u200A\u202F\u205F\u3000]/g;
function resolveToCwd(input: string, cwd: string): string {
	let p = input.replace(UNICODE_SPACES, " ");
	if (p.startsWith("@")) p = p.slice(1);
	if (p === "~") p = homedir();
	else if (p.startsWith("~/")) p = join(homedir(), p.slice(2));
	return isAbsolute(p) ? resolvePath(p) : resolvePath(cwd, p);
}

// Image type like pi (utils/mime.js, detectSupportedImageMimeType).
function readU32BE(b: Buffer, o: number) {
	return (b[o] ?? 0) * 0x1000000 + ((b[o + 1] ?? 0) << 16) + ((b[o + 2] ?? 0) << 8) + (b[o + 3] ?? 0);
}
function readU32LE(b: Buffer, o: number) {
	return (b[o] ?? 0) + ((b[o + 1] ?? 0) << 8) + ((b[o + 2] ?? 0) << 16) + (b[o + 3] ?? 0) * 0x1000000;
}
function readU16LE(b: Buffer, o: number) {
	return (b[o] ?? 0) + ((b[o + 1] ?? 0) << 8);
}
function asciiAt(b: Buffer, o: number, t: string) {
	if (b.length < o + t.length) return false;
	for (let i = 0; i < t.length; i++) if (b[o + i] !== t.charCodeAt(i)) return false;
	return true;
}
const PNG_SIGNATURE = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];
export function sniffImage(buf: Buffer): string | null {
	const b = buf.subarray(0, 4100);
	const startsWith = (bytes: number[]) => b.length >= bytes.length && bytes.every((x, i) => b[i] === x);
	if (startsWith([0xff, 0xd8, 0xff])) return b[3] === 0xf7 ? null : "image/jpeg";
	if (startsWith(PNG_SIGNATURE)) {
		const isPng = b.length >= 16 && readU32BE(b, 8) === 13 && asciiAt(b, 12, "IHDR");
		let animated = false;
		let o = PNG_SIGNATURE.length;
		while (o + 8 <= b.length) {
			const len = readU32BE(b, o);
			if (asciiAt(b, o + 4, "acTL")) {
				animated = true;
				break;
			}
			if (asciiAt(b, o + 4, "IDAT")) break;
			const next = o + 8 + len + 4;
			if (next <= o || next > b.length) break;
			o = next;
		}
		return isPng && !animated ? "image/png" : null;
	}
	if (asciiAt(b, 0, "GIF87a") || asciiAt(b, 0, "GIF89a")) return "image/gif";
	if (asciiAt(b, 0, "RIFF") && asciiAt(b, 8, "WEBP")) return "image/webp";
	if (asciiAt(b, 0, "BM") && b.length >= 26) {
		const size = readU32LE(b, 2);
		const pix = readU32LE(b, 10);
		const dib = readU32LE(b, 14);
		if (size !== 0 && size < 26) return null;
		if (pix < 14 + dib) return null;
		if (size !== 0 && pix >= size) return null;
		let planes: number;
		let bpp: number;
		if (dib === 12) {
			planes = readU16LE(b, 22);
			bpp = readU16LE(b, 24);
		} else if (dib >= 40 && dib <= 124) {
			if (b.length < 30) return null;
			planes = readU16LE(b, 26);
			bpp = readU16LE(b, 28);
		} else return null;
		return planes === 1 && [1, 4, 8, 16, 24, 32].includes(bpp) ? "image/bmp" : null;
	}
	return null;
}

// read and edit read the file once (operation read) and answer access, image type and
// content from it. Above MaxFileBytes (64 MiB) the sandbox returns EFBIG; read then reads an
// excerpt (readLarge).
function fileCache(meta: Meta, signal?: AbortSignal) {
	const cache = new Map<string, Buffer>();
	return async (p: string): Promise<Buffer> => {
		const hit = cache.get(p);
		if (hit) return hit;
		const r = await op(meta, { op: "read", path: p }, signal);
		const b = Buffer.from(r.data ?? "", "base64");
		cache.set(p, b);
		return b;
	};
}

function readOps(meta: Meta, signal?: AbortSignal): ReadOperations {
	const get = fileCache(meta, signal);
	return {
		readFile: (p) => get(p),
		access: async (p) => {
			try {
				await get(p);
			} catch (e) {
				const code = (e as ToolError)?.code;
				// A directory and a large file are readable; read sorts out the rest.
				if (code === "EISDIR" || code === "EFBIG") return;
				throw nodeError(e, "access", p);
			}
		},
		detectImageMimeType: async (p) => {
			try {
				return sniffImage(await get(p));
			} catch (e) {
				if ((e as ToolError)?.code !== "EFBIG") throw e;
				const r = await op(meta, { op: "image_type", path: p }, signal);
				if (r.mime) throw new ToolError(`File too large to read as image (over 64 MiB): ${p}`, "EFBIG");
				return null;
			}
		},
	};
}

function writeOps(meta: Meta, signal?: AbortSignal): WriteOperations {
	return {
		writeFile: async (p, content) => {
			try {
				await op(meta, { op: "write", path: p, data: Buffer.from(content, "utf8").toString("base64") }, signal);
			} catch (e) {
				throw nodeError(e, "open", p);
			}
		},
		mkdir: async (dir) => {
			try {
				await op(meta, { op: "mkdir", path: dir }, signal);
			} catch (e) {
				throw nodeError(e, "mkdir", dir);
			}
		},
	};
}

function editOps(meta: Meta, signal?: AbortSignal): EditOperations {
	const get = fileCache(meta, signal);
	const w = writeOps(meta, signal);
	return {
		readFile: async (p) => {
			try {
				return await get(p);
			} catch (e) {
				throw nodeError(e, "open", p);
			}
		},
		// like pi: readable and writable (R_OK | W_OK)
		access: async (p) => {
			try {
				await op(meta, { op: "access", path: p, mode: "rw" }, signal);
			} catch (e) {
				throw nodeError(e, "access", p);
			}
		},
		writeFile: w.writeFile,
	};
}

type Stat = { exists: boolean; isDir: boolean };

function lsOps(meta: Meta, signal?: AbortSignal): LsOperations {
	const stats = new Map<string, Stat>();
	const stat = async (p: string): Promise<Stat> => {
		const hit = stats.get(p);
		if (hit) return hit;
		const r = await op(meta, { op: "stat", path: p }, signal);
		const s = { exists: !!r.exists, isDir: !!r.isDir };
		stats.set(p, s);
		return s;
	};
	return {
		exists: async (p) => (await stat(p)).exists,
		stat: async (p) => {
			const s = await stat(p);
			if (!s.exists) throw new ToolError(`ENOENT: no such file or directory, stat '${p}'`, "ENOENT");
			return { isDirectory: () => s.isDir };
		},
		readdir: async (p) => {
			let r: any;
			try {
				r = await op(meta, { op: "readdir", path: p }, signal);
			} catch (e) {
				throw nodeError(e, "scandir", p);
			}
			// The sandbox omits entries that cannot be stat'ed (like pi, L3).
			const entries: { name: string; isDir: boolean }[] = r.entries ?? [];
			for (const e of entries) stats.set(resolvePath(p, e.name), { exists: true, isDir: e.isDir });
			return entries.map((e) => e.name);
		},
	};
}

// --- bash ---

// Path of the full output in the execution sandbox; the orchestrator builds it the same way
// (execproto.SpillPath).
export function spillPath(toolCallId: string): string {
	return `/tmp/pi-bash-${createHash("sha256").update(toolCallId).digest("hex").slice(0, 16)}.log`;
}

// TailOutput is pi's OutputAccumulator without a file (H1): it keeps only a bounded tail
// (2 × 50 KiB) in memory. The execution sandbox writes the full output itself.
export class TailOutput {
	private decoder = new TextDecoder();
	private tailText = "";
	private tailBytes = 0;
	private tailStartsAtLineBoundary = true;
	totalRawBytes = 0;
	totalDecodedBytes = 0;
	private completedLines = 0;
	totalLines = 0;
	currentLineBytes = 0;
	private hasOpenLine = false;
	private finished = false;
	private readonly maxRollingBytes = Math.max(DEFAULT_MAX_BYTES * 2, 1);

	append(data: Buffer) {
		if (this.finished) return;
		this.totalRawBytes += data.length;
		this.appendDecoded(this.decoder.decode(data, { stream: true }));
	}
	finish() {
		if (this.finished) return;
		this.finished = true;
		this.appendDecoded(this.decoder.decode());
	}
	isTruncated() {
		return this.totalLines > DEFAULT_MAX_LINES || this.totalDecodedBytes > DEFAULT_MAX_BYTES;
	}
	snapshot(fullOutputPath: string | undefined) {
		const tail = truncateTail(this.snapshotText(), { maxLines: DEFAULT_MAX_LINES, maxBytes: DEFAULT_MAX_BYTES });
		const truncated = this.isTruncated();
		const truncatedBy = truncated ? (tail.truncatedBy ?? (this.totalDecodedBytes > DEFAULT_MAX_BYTES ? "bytes" : "lines")) : null;
		const truncation = {
			...tail,
			truncated,
			truncatedBy,
			totalLines: this.totalLines,
			totalBytes: this.totalDecodedBytes,
			maxLines: DEFAULT_MAX_LINES,
			maxBytes: DEFAULT_MAX_BYTES,
		};
		return { content: truncation.content, truncation, fullOutputPath: truncated ? fullOutputPath : undefined };
	}
	private appendDecoded(text: string) {
		if (text.length === 0) return;
		const bytes = Buffer.byteLength(text, "utf-8");
		this.totalDecodedBytes += bytes;
		this.tailText += text;
		this.tailBytes += bytes;
		if (this.tailBytes > this.maxRollingBytes * 2) this.trimTail();
		let newlines = 0;
		let lastNewline = -1;
		for (let i = text.indexOf("\n"); i !== -1; i = text.indexOf("\n", i + 1)) {
			newlines++;
			lastNewline = i;
		}
		if (newlines === 0) {
			this.currentLineBytes += bytes;
			this.hasOpenLine = true;
		} else {
			this.completedLines += newlines;
			const tail = text.slice(lastNewline + 1);
			this.currentLineBytes = Buffer.byteLength(tail, "utf-8");
			this.hasOpenLine = tail.length > 0;
		}
		this.totalLines = this.completedLines + (this.hasOpenLine ? 1 : 0);
	}
	private trimTail() {
		const buffer = Buffer.from(this.tailText, "utf-8");
		if (buffer.length <= this.maxRollingBytes) {
			this.tailBytes = buffer.length;
			return;
		}
		let start = buffer.length - this.maxRollingBytes;
		while (start < buffer.length && (buffer[start] & 0xc0) === 0x80) start++;
		this.tailStartsAtLineBoundary = start === 0 ? this.tailStartsAtLineBoundary : buffer[start - 1] === 0x0a;
		this.tailText = buffer.subarray(start).toString("utf-8");
		this.tailBytes = Buffer.byteLength(this.tailText, "utf-8");
	}
	private snapshotText() {
		if (this.tailStartsAtLineBoundary) return this.tailText;
		const i = this.tailText.indexOf("\n");
		return i === -1 ? this.tailText : this.tailText.slice(i + 1);
	}
}

// Environment like pi (resolveSpawnContext): only the session's PI_* values go into the sandbox.
function piEnv(ctx: any): Record<string, string> {
	const env: Record<string, string> = {};
	try {
		const id = ctx?.sessionManager?.getSessionId?.();
		if (id) env.PI_SESSION_ID = id;
		const file = ctx?.sessionManager?.getSessionFile?.();
		if (file) env.PI_SESSION_FILE = file;
	} catch {
		// no session
	}
	if (ctx?.model) {
		env.PI_PROVIDER = ctx.model.provider;
		env.PI_MODEL = ctx.model.id;
	}
	if (ctx?.thinkingLevel) env.PI_REASONING_LEVEL = ctx.thinkingLevel;
	return env;
}

// like pi (bash.js, resolveTimeoutMs); L1
function checkTimeout(timeout: unknown) {
	if (timeout === undefined) return;
	if (typeof timeout !== "number" || !Number.isFinite(timeout) || timeout <= 0) {
		throw new Error("Invalid timeout: must be a finite number of seconds");
	}
	if (timeout * 1000 > MAX_TIMEOUT_MS) throw new Error(`Invalid timeout: maximum is ${MAX_TIMEOUT_MS / 1000} seconds`);
}

// bashCall sends the command to the sandbox and returns the last frame; data goes to onData.
async function bashCall(meta: Meta, command: string, cwd: string, env: Record<string, string>, timeout: number | undefined, signal: AbortSignal | undefined, onData: (b: Buffer) => void): Promise<Frame> {
	let last: Frame | undefined;
	let rejected: string | undefined;
	try {
		const res = await post("/tool/bash", { ...meta, req: { op: "bash", command, cwd, env, timeout: timeout ?? 0 } }, signal, (line) => {
			let f: Frame;
			try {
				f = JSON.parse(line);
			} catch {
				return;
			}
			if (f.data) onData(Buffer.from(f.data, "base64"));
			if (f.done) last = f;
			else if (!f.data && f.error) rejected = f.error;
		});
		if (res.status !== 200) rejected = rejected ?? `HTTP ${res.status}`;
	} catch (e) {
		if (signal?.aborted) throw new Error("aborted");
		throw e;
	}
	if (signal?.aborted) throw new Error("aborted");
	if (rejected) throw new Error(`orchestrator rejected the command: ${rejected}`);
	if (!last) throw new Error("execution sandbox: no response");
	return last;
}

// --- background tasks ---

type BgTask = {
	id: string;
	state: string;
	command: string;
	exit_code?: number;
	error?: string;
	stopped_by?: string;
	started_at: string;
	ended_at?: string;
	log_path: string;
	output_bytes: number;
	output_lines: number;
	session?: string;
};
type BgResponse = { task?: BgTask; output?: string; max?: number; error?: string };

async function bgPost(path: string, body: unknown, signal?: AbortSignal): Promise<BgResponse> {
	const res = await post(path, body, signal);
	let r: BgResponse & { error?: string };
	try {
		r = JSON.parse(res.body);
	} catch {
		throw new ToolError(`orchestrator: unreadable response (${res.status})`);
	}
	if (res.status !== 200) throw new ToolError(`orchestrator rejected the request: ${r.error ?? res.status}`);
	if (r.error) throw new ToolError(r.error);
	return r;
}

export function formatDuration(ms: number): string {
	const s = Math.max(0, Math.round(ms / 1000));
	if (s < 60) return `${s}s`;
	const m = Math.floor(s / 60);
	if (m < 60) return `${m}m ${s % 60}s`;
	return `${Math.floor(m / 60)}h ${m % 60}m`;
}

// bgStatus: one line on the state, in English like pi's messages (L4).
export function bgStatus(t: BgTask, now = Date.now()): string {
	const start = Date.parse(t.started_at);
	const end = t.ended_at ? Date.parse(t.ended_at) : now;
	const took = Number.isFinite(start) ? formatDuration(end - start) : "?";
	switch (t.state) {
		case "running":
			return `${t.id} is running (for ${took})`;
		case "exited":
			return `${t.id} exited with code ${t.exit_code} after ${took}`;
		case "timeout":
			return `${t.id} timed out after ${took}`;
		case "stopped":
			return `${t.id} was stopped${t.stopped_by === "user" ? " by the user" : ""} after ${took}`;
		case "suspended":
			return `${t.id} ended when the chat was suspended (its sandbox is gone)`;
		case "closed":
			return `${t.id} ended when the chat was closed`;
		case "lost":
			return `${t.id} ended with its sandbox${t.error ? ` (${t.error})` : ""}`;
		default:
			return `${t.id} failed${t.error ? `: ${t.error}` : ""}`;
	}
}

async function runBashBackground(toolCallId: string, params: any, signal: AbortSignal | undefined, ctx: any) {
	const { command, timeout } = params;
	checkTimeout(timeout);
	const meta = metaOf("bash", toolCallId, ctx);
	const r = await bgPost("/tool/bg/start", { ...meta, req: { op: "bg", command, cwd: ctx?.cwd || CWD, env: piEnv(ctx), timeout: timeout ?? 0 } }, signal);
	const t = r.task!;
	const who = isChildSession(ctx) ? "The main agent is notified when it ends" : "You will be notified when it ends; do not poll or sleep";
	const text = `Background task ${t.id} started. ${who}. Output so far: bg_output ${t.id} (full log ${t.log_path}); stop it with bg_stop ${t.id}.`;
	return { content: [{ type: "text", text }], details: { background: { id: t.id, logPath: t.log_path } } };
}

/** tail_lines of bg_output: a number (also as text) between 1 and DEFAULT_MAX_LINES, otherwise the default (Review 3, N5: previously NaN). */
export function bgTailLines(v: unknown): number {
	const n = typeof v === "number" ? v : typeof v === "string" && v.trim() !== "" ? Number(v) : Number.NaN;
	if (!Number.isFinite(n)) return DEFAULT_MAX_LINES;
	return Math.max(1, Math.min(Math.floor(n), DEFAULT_MAX_LINES));
}

async function runBgOutput(toolCallId: string, params: any, signal: AbortSignal | undefined, ctx: any) {
	const meta = metaOf("bg_output", toolCallId, ctx);
	const lines = bgTailLines(params?.tail_lines);
	const r = await bgPost("/tool/bg/output", { ...meta, id: String(params.id ?? ""), tailLines: lines }, signal);
	const t = r.task!;
	const out = r.output ?? "";
	const tail = truncateTail(out, { maxLines: lines, maxBytes: DEFAULT_MAX_BYTES });
	let text = bgStatus(t);
	if (!out) {
		text += "\n(no output yet)";
	} else {
		text += `\n\n${tail.content}`;
		const shownFrom = t.output_lines - tail.outputLines + 1;
		if (tail.outputLines < t.output_lines || t.output_bytes > Buffer.byteLength(out)) {
			text += `\n\n[Showing last ${tail.outputLines} lines${shownFrom > 0 ? ` (from line ${shownFrom})` : ""} of ${t.output_lines}. Full output: ${t.log_path}]`;
		}
	}
	return { content: [{ type: "text", text }], details: { background: t } };
}

async function runBgStop(toolCallId: string, params: any, signal: AbortSignal | undefined, ctx: any) {
	const meta = metaOf("bg_stop", toolCallId, ctx);
	const r = await bgPost("/tool/bg/stop", { ...meta, id: String(params.id ?? "") }, signal);
	const t = r.task!;
	return { content: [{ type: "text", text: bgStatus(t) }], details: { background: t } };
}

// runBash is pi's bash tool (createShellToolDefinition.execute) with execution in the
// sandbox and without a file in the pi container (H1). Messages word for word as in pi.
async function runBash(toolCallId: string, params: any, signal: AbortSignal | undefined, onUpdate: any, ctx: any) {
	if (params?.run_in_background === true) return runBashBackground(toolCallId, params, signal, ctx);
	const { command, timeout } = params;
	const meta = metaOf("bash", toolCallId, ctx);
	const cwd = ctx?.cwd || CWD;
	const env = piEnv(ctx);
	const computedPath = spillPath(toolCallId);
	let fullPath: string | undefined = computedPath;
	let spillError: string | undefined;
	const output = new TailOutput();
	let acceptingOutput = true;
	let updateTimer: ReturnType<typeof setTimeout> | undefined;
	let updateDirty = false;
	let lastUpdateAt = 0;
	const emitOutputUpdate = () => {
		if (!onUpdate || !updateDirty) return;
		updateDirty = false;
		lastUpdateAt = Date.now();
		const snapshot = output.snapshot(fullPath);
		onUpdate({
			content: [{ type: "text", text: snapshot.content || "" }],
			details: { truncation: snapshot.truncation.truncated ? snapshot.truncation : undefined, fullOutputPath: snapshot.fullOutputPath },
		});
	};
	const clearUpdateTimer = () => {
		if (updateTimer) {
			clearTimeout(updateTimer);
			updateTimer = undefined;
		}
	};
	const scheduleOutputUpdate = () => {
		if (!onUpdate) return;
		updateDirty = true;
		const delay = BASH_UPDATE_THROTTLE_MS - (Date.now() - lastUpdateAt);
		if (delay <= 0) {
			clearUpdateTimer();
			emitOutputUpdate();
			return;
		}
		updateTimer ??= setTimeout(() => {
			updateTimer = undefined;
			emitOutputUpdate();
		}, delay);
	};
	if (onUpdate) onUpdate({ content: [], details: undefined });
	const handleData = (data: Buffer) => {
		if (!acceptingOutput) return;
		output.append(data);
		scheduleOutputUpdate();
	};
	const finishOutput = () => {
		acceptingOutput = false;
		output.finish();
		clearUpdateTimer();
		emitOutputUpdate();
		return output.snapshot(fullPath);
	};
	const formatOutput = (snapshot: ReturnType<TailOutput["snapshot"]>, emptyText = "(no output)") => {
		const truncation = snapshot.truncation;
		let text = snapshot.content || emptyText;
		let details: any;
		if (truncation.truncated) {
			const where = snapshot.fullOutputPath ?? `(not saved${spillError ? `: ${spillError}` : ""})`;
			details = { truncation, fullOutputPath: snapshot.fullOutputPath };
			const startLine = truncation.totalLines - truncation.outputLines + 1;
			const endLine = truncation.totalLines;
			if (truncation.lastLinePartial) {
				const lastLineSize = formatSize(output.currentLineBytes);
				text += `\n\n[Showing last ${formatSize(truncation.outputBytes)} of line ${endLine} (line is ${lastLineSize}). Full output: ${where}]`;
			} else if (truncation.truncatedBy === "lines") {
				text += `\n\n[Showing lines ${startLine}-${endLine} of ${truncation.totalLines}. Full output: ${where}]`;
			} else {
				text += `\n\n[Showing lines ${startLine}-${endLine} of ${truncation.totalLines} (${formatSize(DEFAULT_MAX_BYTES)} limit). Full output: ${where}]`;
			}
		}
		return { text, details };
	};
	const appendStatus = (text: string, status: string) => `${text ? `${text}\n\n` : ""}${status}`;
	const takePath = (f: Frame | undefined) => {
		fullPath = f?.fullOutputPath || undefined;
		spillError = f?.spillError || undefined;
	};
	try {
		let exitCode: number | null;
		let last: Frame | undefined;
		try {
			checkTimeout(timeout);
			if (signal?.aborted) throw new Error("aborted");
			last = await bashCall(meta, command, cwd, env, timeout, signal, handleData);
			takePath(last);
			if (last.code === "backgrounded" && last.background) {
				// The user turned the command into a background task; it keeps running.
				const snapshot = finishOutput();
				const { text } = formatOutput(snapshot, "");
				const who = isChildSession(ctx) ? "The main agent is notified when it ends" : "You will be notified when it ends; do not poll or sleep";
				const note = `The user moved this command to the background as ${last.background}; it keeps running. ${who}. Later output: bg_output ${last.background}; stop it with bg_stop ${last.background}.`;
				return { content: [{ type: "text", text: appendStatus(text, note) }], details: { background: { id: last.background } } };
			}
			if (last.code === "stopped") throw new Error("stopped");
			if (last.code === "timeout") throw new Error(`timeout:${timeout}`);
			if (last.code === "aborted") throw new Error("aborted");
			if (last.error) throw new Error(last.error);
			exitCode = typeof last.exit === "number" ? last.exit : null;
		} catch (err) {
			if (!last) {
				// Abort or error before the last frame: the file only exists if the sandbox
				// kept it; name what can be computed.
				fullPath = output.isTruncated() ? computedPath : undefined;
			}
			const snapshot = finishOutput();
			const { text } = formatOutput(snapshot, "");
			if (err instanceof Error && err.message === "aborted") throw new Error(appendStatus(text, "Command aborted"));
			if (err instanceof Error && err.message === "stopped") throw new Error(appendStatus(text, "Command stopped by the user"));
			if (err instanceof Error && err.message.startsWith("timeout:")) {
				throw new Error(appendStatus(text, `Command timed out after ${err.message.split(":")[1]} seconds`));
			}
			throw err;
		}
		const snapshot = finishOutput();
		const { text: outputText, details } = formatOutput(snapshot);
		if (exitCode === null) throw new Error(appendStatus(outputText, "Command terminated without an exit code"));
		if (exitCode !== 0) throw new Error(appendStatus(outputText, `Command exited with code ${exitCode}`));
		return { content: [{ type: "text", text: outputText }], details };
	} finally {
		clearUpdateTimer();
	}
}

// --- grep, find, large read ---

// grep like pi's built-in tool (format, limits, notices), but ripgrep runs in the
// execution sandbox. The sandbox returns at most MAX_GREP_LIMIT matches; the notice says
// so instead of suggesting a larger limit (M3).
async function runGrep(meta: Meta, params: any, signal?: AbortSignal, ctxCwd?: string) {
	const searchPath = resolveToCwd(params.path || ".", ctxCwd || CWD);
	const limit = Math.max(1, params.limit ?? 100);
	const effective = Math.min(limit, MAX_GREP_LIMIT);
	let r: any;
	try {
		r = await op(
			meta,
			{
				op: "grep",
				path: searchPath,
				grep: { pattern: params.pattern, glob: params.glob, ignoreCase: !!params.ignoreCase, literal: !!params.literal, context: params.context > 0 ? params.context : 0, limit: effective },
			},
			signal,
		);
	} catch (e) {
		if (signal?.aborted) throw new Error("Operation aborted");
		throw e;
	}
	if (!r.matches) return { content: [{ type: "text", text: "No matches found" }], details: undefined };
	let linesTruncated = false;
	const out = (r.lines ?? []).map((l: any) => {
		const t = truncateLine(String(l.text ?? ""));
		if (t.wasTruncated) linesTruncated = true;
		const sep = l.match ? ":" : "-";
		return `${l.path}${sep}${l.line}${sep} ${t.text}`;
	});
	const truncation = truncateHead(out.join("\n"), { maxLines: Number.MAX_SAFE_INTEGER });
	let output = truncation.content;
	const details: any = {};
	const notices: string[] = [];
	if (r.limitReached) {
		notices.push(limitNotice("matches", effective, MAX_GREP_LIMIT, ", or refine pattern"));
		details.matchLimitReached = effective;
	}
	if (truncation.truncated) {
		notices.push(`${formatSize(DEFAULT_MAX_BYTES)} limit reached`);
		details.truncation = truncation;
	}
	if (linesTruncated) {
		notices.push("Some lines truncated to 500 chars. Use read tool to see full lines");
		details.linesTruncated = true;
	}
	if (notices.length > 0) output += `\n\n[${notices.join(". ")}]`;
	return { content: [{ type: "text", text: output }], details: Object.keys(details).length > 0 ? details : undefined };
}

// Notice when the limit is reached, like pi ("N matches limit reached. Use limit=2N for more, or
// refine pattern"), but without suggesting a limit the sandbox does not deliver (M3).
function limitNotice(what: string, effective: number, max: number, tail: string): string {
	if (effective * 2 <= max) return `${effective} ${what} limit reached. Use limit=${effective * 2} for more${tail}`;
	if (effective < max) return `${effective} ${what} limit reached. Use limit=${max} for more (maximum per call)${tail}`;
	return `${max} ${what} limit reached (maximum per call). Refine pattern or narrow the path`;
}

// like pi (find.js)
function relativizeFindResultPath(resultPath: string, searchPath: string): string {
	const trailing = resultPath.endsWith("/");
	const rel = isAbsolute(resultPath) ? path.relative(searchPath, resultPath) : resultPath;
	return trailing && !rel.endsWith("/") ? `${rel}/` : rel;
}

// find like pi's built-in tool with fd (M2): the sandbox calls fd with the same arguments;
// output, notices and errors as in pi's fd branch.
async function runFind(meta: Meta, params: any, signal?: AbortSignal, ctxCwd?: string) {
	if (signal?.aborted) throw new Error("Operation aborted");
	const searchPath = resolveToCwd(params.path || ".", ctxCwd || CWD);
	const effectiveLimit = params.limit ?? 1000;
	const cap = Math.min(effectiveLimit, MAX_FIND_LIMIT);
	let r: any;
	try {
		r = await op(meta, { op: "glob", path: searchPath, glob: { pattern: params.pattern, limit: cap } }, signal);
	} catch (e) {
		if (signal?.aborted) throw new Error("Operation aborted");
		throw e;
	}
	const relativized: string[] = [];
	for (const raw of (r.paths ?? []) as string[]) {
		const line = raw.replace(/\r$/, "").trim();
		if (line) relativized.push(relativizeFindResultPath(line, searchPath));
	}
	if (relativized.length === 0) return { content: [{ type: "text", text: "No files found matching pattern" }], details: undefined };
	const resultLimitReached = relativized.length >= cap;
	const truncation = truncateHead(relativized.join("\n"), { maxLines: Number.MAX_SAFE_INTEGER });
	let resultOutput = truncation.content;
	const details: any = {};
	const notices: string[] = [];
	if (resultLimitReached) {
		notices.push(limitNotice("results", cap, MAX_FIND_LIMIT, ", or refine pattern"));
		details.resultLimitReached = cap;
	}
	if (truncation.truncated) {
		notices.push(`${formatSize(DEFAULT_MAX_BYTES)} limit reached`);
		details.truncation = truncation;
	}
	if (notices.length > 0) resultOutput += `\n\n[${notices.join(". ")}]`;
	return { content: [{ type: "text", text: resultOutput }], details: Object.keys(details).length > 0 ? details : undefined };
}

// readLarge: text range of a file above MaxFileBytes, like pi's read (text branch), from an
// excerpt instead of the whole file (L4). pi would read the whole file into memory.
async function readLarge(meta: Meta, absolutePath: string, params: any, signal?: AbortSignal) {
	const { path: displayPath, offset, limit } = params;
	const startLine = offset ? Math.max(0, offset - 1) : 0;
	const startLineDisplay = startLine + 1;
	const window = limit !== undefined ? Math.max(1, Math.min(limit, DEFAULT_MAX_LINES + 1)) : DEFAULT_MAX_LINES + 1;
	const r = await op(
		meta,
		{ op: "read_lines", path: absolutePath, lines: { start: startLine, count: window, maxBytes: DEFAULT_MAX_BYTES + 1, select: limit !== undefined ? Math.max(0, limit) : 0 } },
		signal,
	);
	const total: number = r.totalLines;
	if (startLine >= total) throw new Error(`Offset ${offset} is beyond end of file (${total} lines total)`);
	const lines: string[] = (r.lines ?? []).map((b: string) => Buffer.from(b, "base64").toString("utf-8"));
	let selectedContent: string;
	let userLimitedLines: number | undefined;
	if (limit !== undefined) {
		const endLine = Math.min(startLine + limit, total);
		userLimitedLines = endLine - startLine;
		selectedContent = lines.slice(0, userLimitedLines).join("\n");
	} else {
		selectedContent = lines.join("\n");
	}
	// Size of the whole selection, as truncateHead counts it over the whole file in pi.
	const truncation = { ...truncateHead(selectedContent), totalLines: r.selLines - (r.selLastEmpty ? 1 : 0), totalBytes: r.selBytes };
	let outputText: string;
	let details: any;
	if (truncation.firstLineExceedsLimit) {
		outputText = `[Line ${startLineDisplay} is ${formatSize(r.startLineBytes)}, exceeds ${formatSize(DEFAULT_MAX_BYTES)} limit. Use bash: sed -n '${startLineDisplay}p' ${displayPath} | head -c ${DEFAULT_MAX_BYTES}]`;
		details = { truncation };
	} else if (truncation.truncated) {
		const endLineDisplay = startLineDisplay + truncation.outputLines - 1;
		const nextOffset = endLineDisplay + 1;
		outputText = truncation.content;
		if (truncation.truncatedBy === "lines") outputText += `\n\n[Showing lines ${startLineDisplay}-${endLineDisplay} of ${total}. Use offset=${nextOffset} to continue.]`;
		else outputText += `\n\n[Showing lines ${startLineDisplay}-${endLineDisplay} of ${total} (${formatSize(DEFAULT_MAX_BYTES)} limit). Use offset=${nextOffset} to continue.]`;
		details = { truncation };
	} else if (userLimitedLines !== undefined && startLine + userLimitedLines < total) {
		const remaining = total - (startLine + userLimitedLines);
		const nextOffset = startLine + userLimitedLines + 1;
		outputText = `${truncation.content}\n\n[${remaining} more lines in file. Use offset=${nextOffset} to continue.]`;
	} else {
		outputText = truncation.content;
	}
	return { content: [{ type: "text", text: outputText }], details };
}

// --- guard for subagent ---

// Allowed are single subagents (agent + task), read-only management actions and workflows
// via workflowScript, whose script runs in the execution sandbox (remote-worker.mjs).
const SUBAGENT_KEYS = new Set([
	"agent", "task", "action", "topic", "context", "model", "async", "timeoutMs", "maxRuntimeMs", "toolTimeoutMs",
	"maxOutput", "id", "index", "view", "lines", "message", "mode", "agentScope", "capabilities", "includeProgress", "artifacts",
]);
const SUBAGENT_ACTIONS = new Set(["list", "get", "models", "guide", "status", "interrupt", "steer", "resume", "children.list"]);
// Built-in agents without an external runtime (claude-code, codex-exec, cursor-agent start foreign programs).
const SUBAGENT_AGENTS = new Set(["worker", "scout", "reviewer", "oracle", "researcher", "delegate", "evidence-auditor"]);
// With workflowScript: only the script, its arguments and settings for runtime and model. Still
// blocked are workflowScriptPath (file in the pi container), workflow (named workflows), gate and
// acceptance (commands in the pi container), output (paths), cwd, worktree and missions.
const WORKFLOW_KEYS = new Set([
	"workflowScript", "args", "async", "timeoutMs", "maxRuntimeMs", "context", "model", "globalConcurrencyLimit",
	"maxSubagentSpawnsPerRun", "includeProgress", "artifacts",
]);
// Parameters of a run from the script (runs.run, runs.all): like a single subagent.
const RUN_KEYS = new Set(["agent", "task", "model", "context", "timeoutMs", "maxRuntimeMs", "toolTimeoutMs", "maxOutput", "async", "phase", "label"]);

// The copy of pi-subagents in the image (third_party/pi-subagents) takes the worker for workflowScript
// from the module in PI_SUBAGENTS_WORKFLOW_WORKER (fixed to remote-worker.mjs in the pi image)
// and reports it as workflowWorkerModule. Only if that is remote-worker.mjs does the script not
// run in the pi process. Fail-closed: if the export is missing, the module cannot be loaded or it
// reports something else (e.g. node:worker_threads without the variable), workflowScript stays blocked.
const SCRIPTED_WORKFLOW = "/opt/agw/pihome/npm/node_modules/pi-subagents/src/workflows/scripted-workflow.js";
export const REMOTE_WORKER = "/opt/agw/ext/remote-worker.mjs";
export async function workflowRuntimeRedirected(modulePath = SCRIPTED_WORKFLOW): Promise<boolean> {
	try {
		const m = await import(modulePath);
		return m?.workflowWorkerModule === REMOTE_WORKER;
	} catch {
		return false;
	}
}

// Messages in English like pi (L4); they go to the model as the tool result.
export function checkSubagentCall(input: any, opts: { workflowReady?: boolean } = {}): string | undefined {
	if (!input || typeof input !== "object" || Array.isArray(input)) return "invalid parameters";
	if (input.workflowScript !== undefined) {
		if (!opts.workflowReady) return "workflowScript is unavailable here (the workflow runtime is not redirected to the execution sandbox)";
		if (typeof input.workflowScript !== "string" || !input.workflowScript.trim()) return "workflowScript must be a non-empty string";
		for (const k of Object.keys(input)) {
			if (!WORKFLOW_KEYS.has(k)) return `parameter ${k} is blocked with workflowScript`;
		}
		if (input.context === "profile") return "context profile is blocked";
		return undefined;
	}
	for (const k of Object.keys(input)) {
		if (!SUBAGENT_KEYS.has(k)) return `parameter ${k} is blocked`;
	}
	if (input.action !== undefined && !SUBAGENT_ACTIONS.has(input.action)) return `action ${String(input.action)} is blocked`;
	// L2: agent is always checked once set, also next to an action.
	if (input.agent !== undefined && !SUBAGENT_AGENTS.has(input.agent)) return `agent ${String(input.agent)} is not allowed`;
	if (input.context === "profile") return "context profile is blocked";
	return undefined;
}

// checkWorkflowMessage checks a message from the worker (the agent's code in the
// execution sandbox) before pi-subagents sees it. Requests to the host: runs.run (also
// runs.all and runs.lanes) like a single subagent, status and steer; runs.host (a command in
// the pi container) is blocked, as is anything unknown.
export function checkWorkflowMessage(msg: any): string | undefined {
	if (!msg || typeof msg !== "object") return "invalid message";
	if (msg.type !== "call") return undefined;
	switch (msg.method) {
		case "run": {
			const p = msg.args?.params;
			if (!p || typeof p !== "object" || Array.isArray(p)) return "runs.run requires a params object";
			for (const k of Object.keys(p)) {
				if (!RUN_KEYS.has(k)) return `runs.run parameter ${k} is blocked (execution sandbox, E9)`;
			}
			if (!SUBAGENT_AGENTS.has(p.agent)) return `runs.run agent ${String(p.agent)} is not allowed (execution sandbox, E9)`;
			if (p.context === "profile") return "runs.run context profile is blocked";
			return undefined;
		}
		case "status":
		case "steer":
		case "state.get":
		case "state.set":
			return undefined;
		case "host":
			return "runs.host is blocked (it would run a command in the pi container; execution sandbox, E9)";
	}
	return `runs.${String(msg.method)} is blocked (execution sandbox, E9)`;
}

// Approvals for remote-worker.mjs: per approved script the toolCallId of the call. The
// store is global because pi loads the extension again for child sessions in the same process.
type WorkflowRegistry = {
	approved: Map<string, { toolCallId: string; sessionFile: string }[]>;
	claim: (script: string) => { toolCallId: string; sessionFile: string } | undefined;
	checkMessage: (msg: any) => string | undefined;
};
const REGISTRY = Symbol.for("agw.exec-bridge.workflow");
function registry(): WorkflowRegistry {
	const g = globalThis as any;
	if (!g[REGISTRY]) {
		const approved = new Map<string, { toolCallId: string; sessionFile: string }[]>();
		g[REGISTRY] = {
			approved,
			claim: (script: string) => {
				const l = approved.get(script);
				const m = l?.shift();
				if (l && l.length === 0) approved.delete(script);
				return m;
			},
			checkMessage: checkWorkflowMessage,
		} satisfies WorkflowRegistry;
	}
	return g[REGISTRY];
}

// watchdog_diff (agent reviewer) calls git in the pi container (spawnSync). There is no git
// and no shell there (security decision, security review N2); it is blocked anyway so that
// an image with git does not change that.
const BLOCKED_TOOLS: Record<string, string> = {
	watchdog_diff: "watchdog_diff is blocked (it would run git in the pi container; execution sandbox, E9)",
};

// Child sessions of pi-subagents live under <main session>/<run>/run-N/session.jsonl.
function isChildSession(ctx: any): boolean {
	try {
		return /\/run-\d+\/session\.jsonl$/.test(ctx?.sessionManager?.getSessionFile?.() ?? "");
	} catch {
		return false;
	}
}

export default function (pi: ExtensionAPI) {
	// An extension that registers grep, find and ls also enables them in pi. Before E9 the main
	// agent of the variants cli and both had only read, bash, edit and write; to keep the scope of
	// action the same, the orchestrator hides the others via AGW_BRIDGE_HIDE.
	// Subagents keep the tools of their agent definition.
	const hide = new Set((process.env.AGW_BRIDGE_HIDE ?? "").split(",").map((s) => s.trim()).filter(Boolean));
	pi.on("session_start", async (_event: any, ctx: any) => {
		if (hide.size === 0 || isChildSession(ctx)) return;
		const active = pi.getActiveTools();
		if (active.some((n) => hide.has(n))) pi.setActiveTools(active.filter((n) => !hide.has(n)));
	});

	// Checked only at the first workflowScript (child sessions load the extension again and
	// do not need the check), afterwards from the cache.
	let workflowReady: Promise<boolean> | undefined;
	const reg = registry();
	pi.on("tool_call", async (event: any, ctx: any) => {
		if (BLOCKED_TOOLS[event.toolName]) return { block: true, reason: `Blocked (execution sandbox, E9): ${BLOCKED_TOOLS[event.toolName]}.` };
		if (event.toolName !== "subagent") return;
		const ready = event.input?.workflowScript !== undefined ? await (workflowReady ??= workflowRuntimeRedirected()) : false;
		const reason = checkSubagentCall(event.input, { workflowReady: ready });
		if (reason) {
			return {
				block: true,
				reason: `Blocked (execution sandbox, E9): ${reason}. Allowed are single subagents with agent and task (worker, scout, reviewer, oracle, researcher, delegate, evidence-auditor), workflowScript with runs.run/runs.all over these agents, and the actions list, get, status, interrupt, steer and resume.`,
			};
		}
		if (typeof event.input?.workflowScript === "string") {
			const script = event.input.workflowScript;
			const l = reg.approved.get(script) ?? [];
			l.push({ toolCallId: String(event.toolCallId ?? ""), sessionFile: metaOf("subagent", "", ctx).sessionFile });
			reg.approved.set(script, l);
		}
	});

	const localRead = createReadTool(CWD);
	const localWrite = createWriteTool(CWD);
	const localEdit = createEditTool(CWD);
	const localBash = createBashTool(CWD);
	const localGrep = createGrepTool(CWD);
	const localFind = createFindTool(CWD);
	const localLs = createLsTool(CWD);

	pi.registerTool({
		...localRead,
		async execute(id: string, params: any, signal: any, onUpdate: any, ctx: any) {
			const meta = metaOf("read", id, ctx);
			try {
				return await createReadTool(CWD, { operations: readOps(meta, signal) }).execute(id, params, signal, onUpdate, ctx);
			} catch (e) {
				// Text file over 64 MiB: excerpt instead of the whole file (L4).
				if ((e as ToolError)?.code !== "EFBIG" || (e as Error).message.startsWith("File too large to read as image")) throw e;
				if (signal?.aborted) throw new Error("Operation aborted");
				return readLarge(meta, resolveToCwd(params.path, ctx?.cwd || CWD), params, signal);
			}
		},
	} as any);
	pi.registerTool({
		...localWrite,
		async execute(id: string, params: any, signal: any, onUpdate: any, ctx: any) {
			return createWriteTool(CWD, { operations: writeOps(metaOf("write", id, ctx), signal) }).execute(id, params, signal, onUpdate, ctx);
		},
	} as any);
	pi.registerTool({
		...localEdit,
		async execute(id: string, params: any, signal: any, onUpdate: any, ctx: any) {
			return createEditTool(CWD, { operations: editOps(metaOf("edit", id, ctx), signal) }).execute(id, params, signal, onUpdate, ctx);
		},
	} as any);
	// The bridge reimplements bash, find and grep itself (no file in the pi container, fd instead
	// of its own glob search); schema and description stay pi's.
	// bash with the extra parameter run_in_background (schema like pi, TypeBox 1 is JSON Schema).
	const bashParams: any = localBash.parameters;
	pi.registerTool({
		...localBash,
		parameters: {
			...bashParams,
			properties: {
				...bashParams.properties,
				run_in_background: { type: "boolean", description: "Start in background, return at once; you are notified when it ends. For servers, long builds, training." },
			},
		},
		async execute(id: string, params: any, signal: any, onUpdate: any, ctx: any) {
			return runBash(id, params, signal, onUpdate, ctx);
		},
	} as any);
	pi.registerTool({
		name: "bg_output",
		label: "bg_output",
		description: "Status and recent output of a background task (bash with run_in_background).",
		parameters: {
			type: "object",
			required: ["id"],
			properties: {
				id: { type: "string", description: "Task id, e.g. bg-1" },
				tail_lines: { type: "number", description: `Last lines to show (default ${DEFAULT_MAX_LINES})` },
			},
		},
		async execute(id: string, params: any, signal: any, _onUpdate: any, ctx: any) {
			return runBgOutput(id, params, signal, ctx);
		},
	} as any);
	pi.registerTool({
		name: "bg_stop",
		label: "bg_stop",
		description: "Stop a background task and all its processes.",
		parameters: { type: "object", required: ["id"], properties: { id: { type: "string", description: "Task id, e.g. bg-1" } } },
		async execute(id: string, params: any, signal: any, _onUpdate: any, ctx: any) {
			return runBgStop(id, params, signal, ctx);
		},
	} as any);
	pi.registerTool({
		...localLs,
		async execute(id: string, params: any, signal: any, onUpdate: any, ctx: any) {
			return createLsTool(CWD, { operations: lsOps(metaOf("ls", id, ctx), signal) }).execute(id, params, signal, onUpdate, ctx);
		},
	} as any);
	pi.registerTool({
		...localFind,
		async execute(id: string, params: any, signal: any, _onUpdate: any, ctx: any) {
			return runFind(metaOf("find", id, ctx), params, signal, ctx?.cwd);
		},
	} as any);
	pi.registerTool({
		...localGrep,
		async execute(id: string, params: any, signal: any, _onUpdate: any, ctx: any) {
			return runGrep(metaOf("grep", id, ctx), params, signal, ctx?.cwd);
		},
	} as any);

	// User commands with "!" (user_bash) do not exist in RPC mode; if they ever occur, they also
	// run in the execution sandbox.
	pi.on("user_bash", async (_event: any, ctx: any) => ({
		operations: {
			exec: async (command: string, cwd: string, { onData, signal, timeout, env }: any) => {
				checkTimeout(timeout);
				const pe: Record<string, string> = {};
				for (const [k, v] of Object.entries(env ?? {})) if (k.startsWith("PI_") && typeof v === "string") pe[k] = v;
				const last = await bashCall(metaOf("bash", "user_bash", ctx), command, cwd, pe, timeout, signal, onData);
				if (last.code === "timeout") throw new Error(`timeout:${timeout}`);
				if (last.code === "aborted") throw new Error("aborted");
				if (last.error) throw new Error(last.error);
				return { exitCode: typeof last.exit === "number" ? last.exit : null };
			},
		},
	}));
}
