import { describe, expect, it } from "vitest"
import type { Command } from "@/api/types"
import { slashItems } from "./useSlashCommands"
import { filterOptions, isBuiltinCommand, slashArg, withLiveOptions } from "@/lib/commands"

const cmds: Command[] = [
  { name: "compact", source: "builtin" },
  {
    name: "model",
    source: "builtin",
    options: [
      { value: "deepseek/deepseek-flash", label: "DeepSeek V4.1 Flash · 1M tokens" },
      { value: "deepseek/deepseek-v4-pro", label: "DeepSeek V4 Pro" },
    ],
  },
  { name: "effort", source: "builtin", options: ["off", "low", "high", "max"].map((value) => ({ value })) },
]

describe("slashItems", () => {
  it("the commands for \"/mo\", the models for \"/model \"", () => {
    expect(slashItems(cmds, "/mo").items.map((i) => i.command.name)).toEqual(["model"])
    const r = slashItems(cmds, "/model ")
    expect(r.argsOf?.name).toBe("model")
    expect(r.items.map((i) => i.option?.value)).toEqual(["deepseek/deepseek-flash", "deepseek/deepseek-v4-pro"])
  })
  it("filters arguments by value and label", () => {
    expect(slashItems(cmds, "/model pro").items.map((i) => i.option?.value)).toEqual(["deepseek/deepseek-v4-pro"])
    expect(slashItems(cmds, "/effort h").items.map((i) => i.option?.value)).toEqual(["high"])
  })
  it("closes as soon as a value is complete (Enter then sends)", () => {
    expect(slashItems(cmds, "/effort high").items).toEqual([])
  })
  it("no suggestions for commands without options and for several words", () => {
    expect(slashItems(cmds, "/compact focus").items).toEqual([])
    expect(slashItems(cmds, "/effort high now").items).toEqual([])
  })
})

describe("helpers for /model and /effort", () => {
  it("slashArg and filterOptions", () => {
    expect(slashArg("/Model  deep")).toEqual({ name: "model", query: "deep" })
    expect(slashArg("/model")).toBeUndefined()
    expect(filterOptions([{ value: "a", label: "High" }, { value: "high" }], "high").map((o) => o.value)).toEqual(["high", "a"])
  })
  it("are built-in commands", () => {
    expect(isBuiltinCommand("/model deepseek/deepseek-flash")).toBe(true)
    expect(isBuiltinCommand("/effort high")).toBe(true)
    expect(isBuiltinCommand("/models")).toBe(false)
  })
  it("withLiveOptions: levels and current value from the chat", () => {
    const live = withLiveOptions(cmds, { model: "deepseek/deepseek-v4-pro", thinking_level: "low", thinking_levels: ["off", "low", "high"] })
    expect(live.find((c) => c.name === "model")?.options?.find((o) => o.current)?.value).toBe("deepseek/deepseek-v4-pro")
    expect(live.find((c) => c.name === "effort")?.options).toEqual([
      { value: "off", label: "off", current: false },
      { value: "low", label: "low", current: true },
      { value: "high", label: "high", current: false },
    ])
  })
})
