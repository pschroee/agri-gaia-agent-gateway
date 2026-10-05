// agw.2: with PI_SUBAGENTS_OUTPUT_INLINE=1 children are to return their result in the response
// instead of writing to a path in the pi container that their tools cannot see.
import { test } from "node:test";
import assert from "node:assert/strict";
import { injectSingleOutputInstruction } from "../src/runs/shared/single-output.js";

const path = "/agent/sessions/subagent-artifacts/outputs/wf/data.md";

test("without the variable: instruction to write to the path (like the original)", () => {
	delete process.env.PI_SUBAGENTS_OUTPUT_INLINE;
	const t = injectSingleOutputInstruction("Task", path, { tools: ["bash", "write"] });
	assert.match(t, /Write your findings to exactly this path/);
});

test("with PI_SUBAGENTS_OUTPUT_INLINE=1: result in the response, the runtime stores it", () => {
	process.env.PI_SUBAGENTS_OUTPUT_INLINE = "1";
	try {
		const t = injectSingleOutputInstruction("Task", path, { tools: ["bash", "write"] });
		assert.doesNotMatch(t, /Write your findings/);
		assert.match(t, /Return the complete artifact in your final response/);
		assert.match(t, /The runtime will persist it to exactly this path/);
	} finally {
		delete process.env.PI_SUBAGENTS_OUTPUT_INLINE;
	}
});
