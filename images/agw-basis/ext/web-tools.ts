// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

// pi extension: loads pi-searxng-suite (unchanged) and gives its tools web_search and web_extract
// descriptions that say what they are for, how to use them well and where their limits are
// (issue #44). The suite's own descriptions are one line each ("Search the web using SearxNG").
// The descriptions reach every binding and every subagent alike, because this extension is loaded
// in place of the suite everywhere (piArgs and defaultSubagentOnlyExtensions in worker.go).
//
// Only the texts change: name, parameters, execute and rendering stay the suite's. pi keeps the
// first registration per tool name, so the texts are set while the suite registers, through a
// proxy of the extension API, not by registering the tools a second time.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import suite from "/opt/agw/pihome/npm/node_modules/pi-searxng-suite/index.ts";

export const SEARCH_DESCRIPTION =
	"Search the web through our own SearXNG metasearch. Returns up to 10 results, each with title, URL and a " +
	"snippet of at most 1000 characters. Use it to find sources and get an overview; then read the relevant " +
	"pages with web_extract, because snippets are short and can be outdated: do not cite figures from a snippet " +
	"alone. Write short keyword queries (about 3 to 6 words, not a full question) in the language the best sources " +
	"are likely written in (English for international or technical topics, the local language for local rules, " +
	"news or prices), and run several short searches with different keywords instead of one long one. Add a year " +
	"or 'latest' for current facts. Only available while internet is on."

export const SEARCH_QUERY = "Keywords, e.g. 'tail biting pigs early detection' (not a full sentence)"

export const SEARCH_CATEGORY =
	"Optional search category (pick one): general (default), news (current events), science (papers), " +
	"it (software, programming), files, images, videos, social media"

export const EXTRACT_DESCRIPTION =
	"Fetch one public web page or file by URL and return its content as text: HTML as Markdown with title and " +
	"metadata, PDF as text per page, plain text as is, images attached. Use it to read a page found with " +
	"web_search or a URL the user gave, and take facts and figures from here. The text is cut after 100000 " +
	"characters (marked '[Content truncated at 100000 characters]'); HTML over 2 MB and PDFs or images over " +
	"50 MB are refused. Only http and https to public hosts. Never put passwords, tokens or API keys into a " +
	"URL. If you have bash, use curl instead to save a file into /workspace or to call a JSON API. Only " +
	"available while internet is on."

export const EXTRACT_URL = "Full http(s) URL of the page or file, e.g. from a web_search result"

export const CITE_GUIDELINE =
	"Name the web sources you used in your answer with their URL, as Markdown links with a meaningful title ([Title](https://...)), never as bare URLs; if sources disagree, say so and name both."

type Tool = Parameters<ExtensionAPI["registerTool"]>[0]

/** Gives a tool of pi-searxng-suite our texts; other tools pass unchanged. */
export function describe(tool: Tool): Tool {
	const params = tool.parameters as { properties?: Record<string, { description?: string }> }
	const props = params?.properties ?? {}
	// In place: the schema objects are created fresh at registration, and a copy could lose
	// TypeBox's non-enumerable markers.
	const set = (name: string, description: string) => {
		if (props[name]) props[name].description = description
	}
	const guidelines = [...(tool.promptGuidelines ?? []), CITE_GUIDELINE]
	if (tool.name === "web_search") {
		set("query", SEARCH_QUERY)
		set("category", SEARCH_CATEGORY)
		return { ...tool, description: SEARCH_DESCRIPTION, promptGuidelines: guidelines }
	}
	if (tool.name === "web_extract") {
		set("url", EXTRACT_URL)
		return { ...tool, description: EXTRACT_DESCRIPTION, promptGuidelines: guidelines }
	}
	return tool
}

export default function (pi: ExtensionAPI) {
	const api = new Proxy(pi, {
		get(target, key) {
			if (key === "registerTool") return (tool: Tool) => target.registerTool(describe(tool))
			const v = Reflect.get(target, key)
			return typeof v === "function" ? v.bind(target) : v
		},
	})
	suite(api)
}
