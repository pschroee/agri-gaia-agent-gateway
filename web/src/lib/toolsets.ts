// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

import type { Config, Variant } from "@/api/types"

/**
 * The bindings every new chat gets (AGW_TOOLSETS, issue #29). Newer gateways report them in
 * `GET /api/config`; for an older one the active entry of `GET /api/variants` is used, if any.
 */
export function activeToolsets(config: Config | undefined, variants: Variant[]): Variant | undefined {
  return config?.toolsets ?? variants.find((v) => v.active)
}

/** The bindings of a variant key as shown in the UI: "cli,api" → "CLI + API"; "both" is cli,mcp. */
export function bindingsLabel(id: string): string {
  const key = id === "both" ? "cli,mcp" : id
  return key
    .split(",")
    .map((b) => b.trim().toUpperCase())
    .filter(Boolean)
    .join(" + ")
}
