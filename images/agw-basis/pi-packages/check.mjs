// Check while building the pi image (images/agw-basis/Dockerfile): /opt/agw/pihome/npm holds exactly the versions
// of package.json (and thus of the lockfile), the two copies from third_party are in place at the paths the gateway
// loads (internal/worker/worker.go), and tsx works although the install ran with --ignore-scripts. Any deviation
// aborts the build. The Dockerfile removes this file afterwards.
import { existsSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { join } from "node:path";

const ROOT = "/opt/agw/pihome/npm";

function fail(msg) {
	console.error(`pi-packages: ${msg}`);
	process.exit(1);
}
const json = (p) => JSON.parse(readFileSync(p, "utf8"));

const deps = json(join(ROOT, "package.json")).dependencies;
for (const [name, version] of Object.entries(deps)) {
	if (/^[\^~]/.test(version)) fail(`${name} ${version} is not pinned exactly`);
	const got = json(join(ROOT, "node_modules", name, "package.json")).version;
	if (got !== version) fail(`${name} ${got} installed, expected ${version}`);
}
for (const name of ["searxng-suite", "pi-intercom"]) {
	if (Object.keys(deps).some((d) => d.endsWith(name))) fail(`${name} must come from third_party, not from npm`);
}

const copies = [
	["@blazer2k/searxng-suite", "0.2.3", "src/index.ts"],
	["pi-intercom", "0.15.0", "index.ts"],
];
for (const [name, version, entry] of copies) {
	const dir = join(ROOT, "node_modules", name);
	const got = json(join(dir, "package.json")).version;
	if (got !== version) fail(`${name} ${got} in place, expected ${version}`);
	if (!existsSync(join(dir, entry))) fail(`${name}: ${entry} missing`);
}
if (!existsSync(join(ROOT, "node_modules/@juicesharp/rpiv-todo/index.ts"))) fail("rpiv-todo: index.ts missing");

// esbuild finds its platform package without a postinstall script; tsx needs it to load the .ts extensions.
const require = createRequire(join(ROOT, "package.json"));
const out = require("esbuild").transformSync("let a = 1", { loader: "ts" }).code;
if (!out.includes("let a = 1")) fail(`esbuild transformSync returned ${JSON.stringify(out)}`);

console.log(`pi-packages: ${Object.entries(deps).map(([n, v]) => `${n}@${v}`).join(", ")}; copies ${copies.map((c) => `${c[0]}@${c[1]}`).join(", ")}`);
