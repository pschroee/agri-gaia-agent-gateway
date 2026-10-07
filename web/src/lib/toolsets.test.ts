// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

import { describe, expect, it } from "vitest"
import type { Config, Variant } from "@/api/types"
import { activeToolsets, bindingsLabel } from "./toolsets"

const cliApi: Variant = { id: "cli,api", label: "Command line + REST API", bindings: ["cli", "api"], tools: ["bash", "platform_http"] }
const config = { internet_default: false, approval_timeout_s: 1, artifact_max_mb: 1, idle_timeout_s: 1 } satisfies Config

describe("activeToolsets", () => {
  it("prefers the config", () => {
    expect(activeToolsets({ ...config, toolsets: cliApi }, [{ id: "cli", label: "x", tools: [], active: true }])).toBe(cliApi)
  })
  it("falls back to the active variant", () => {
    expect(activeToolsets(config, [{ id: "cli", label: "x", tools: [] }, { ...cliApi, active: true }])?.id).toBe("cli,api")
  })
  it("is undefined for an older gateway", () => {
    expect(activeToolsets(config, [{ id: "cli", label: "x", tools: [] }])).toBeUndefined()
    expect(activeToolsets(undefined, [])).toBeUndefined()
  })
})

describe("bindingsLabel", () => {
  it("joins the bindings", () => {
    expect(bindingsLabel("cli")).toBe("CLI")
    expect(bindingsLabel("cli,api")).toBe("CLI + API")
    expect(bindingsLabel("cli,mcp,api")).toBe("CLI + MCP + API")
  })
  it("maps the older id both", () => {
    expect(bindingsLabel("both")).toBe("CLI + MCP")
  })
})
