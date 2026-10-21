// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// Unit tests of web-gate.ts (issue #175): which of web_search and web_extract the chat's grants put on offer.
// Runs with node --test in the test image agw-parity (TestBridgeParity).
import assert from "node:assert/strict";
import { test } from "node:test";
import { nextActive, wantedFrom } from "../ext/web-gate.ts";

const REGISTERED = ["read", "web_search", "web_extract"];
const BOTH = { web_search: true, web_extract: true };
const NONE = { web_search: false, web_extract: false };

test("wantedFrom reads the answer of GET /tool/internet per tool", () => {
	assert.deepEqual(wantedFrom({ enabled: true, search: true, extract: true }), BOTH);
	assert.deepEqual(wantedFrom({ enabled: true, search: false, extract: true }), { web_search: false, web_extract: true });
	assert.deepEqual(wantedFrom({ enabled: true, search: false, extract: false }), NONE);
	assert.deepEqual(wantedFrom({ enabled: false, search: true, extract: true }), NONE);
	// An answer from before issue #175 only knows "enabled": both tools.
	assert.deepEqual(wantedFrom({ enabled: true }), BOTH);
	assert.deepEqual(wantedFrom({}), NONE);
	assert.deepEqual(wantedFrom(null), NONE);
});

test("a search grant shows both tools", () => {
	assert.deepEqual(nextActive(["read"], REGISTERED, BOTH, true), ["read", "web_search", "web_extract"]);
});

test("a domains grant shows web_extract only", () => {
	assert.deepEqual(nextActive(["read"], REGISTERED, { web_search: false, web_extract: true }, true), ["read", "web_extract"]);
});

test("no grant hides the tools, but only where removing is allowed", () => {
	const active = ["read", "web_search", "web_extract"];
	assert.deepEqual(nextActive(active, REGISTERED, NONE, true), ["read"]);
	assert.equal(nextActive(active, REGISTERED, NONE, false), undefined);
});

test("a grant that ends for web_search keeps web_extract", () => {
	const active = ["read", "web_search", "web_extract"];
	assert.deepEqual(nextActive(active, REGISTERED, { web_search: false, web_extract: true }, true), ["read", "web_extract"]);
	assert.equal(nextActive(active, REGISTERED, { web_search: false, web_extract: true }, false), undefined);
});

test("nothing to change gives undefined; unregistered tools are not added", () => {
	assert.equal(nextActive(["read", "web_search", "web_extract"], REGISTERED, BOTH, true), undefined);
	assert.equal(nextActive(["read"], REGISTERED, NONE, true), undefined);
	assert.deepEqual(nextActive(["read"], ["read", "web_extract"], BOTH, true), ["read", "web_extract"]);
});
