// agw.2: Mit PI_SUBAGENTS_OUTPUT_INLINE=1 sollen Kinder ihr Ergebnis in der Antwort zurückgeben,
// statt in einen Pfad im Container von pi zu schreiben, den ihre Werkzeuge nicht sehen.
import { test } from "node:test";
import assert from "node:assert/strict";
import { injectSingleOutputInstruction } from "../src/runs/shared/single-output.js";

const path = "/agent/sessions/subagent-artifacts/outputs/wf/daten.md";

test("ohne Variable: Anweisung, in den Pfad zu schreiben (wie das Original)", () => {
	delete process.env.PI_SUBAGENTS_OUTPUT_INLINE;
	const t = injectSingleOutputInstruction("Aufgabe", path, { tools: ["bash", "write"] });
	assert.match(t, /Write your findings to exactly this path/);
});

test("mit PI_SUBAGENTS_OUTPUT_INLINE=1: Ergebnis in der Antwort, die Laufzeit speichert", () => {
	process.env.PI_SUBAGENTS_OUTPUT_INLINE = "1";
	try {
		const t = injectSingleOutputInstruction("Aufgabe", path, { tools: ["bash", "write"] });
		assert.doesNotMatch(t, /Write your findings/);
		assert.match(t, /Return the complete artifact in your final response/);
		assert.match(t, /The runtime will persist it to exactly this path/);
	} finally {
		delete process.env.PI_SUBAGENTS_OUTPUT_INLINE;
	}
});
