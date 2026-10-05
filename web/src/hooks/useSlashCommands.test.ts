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
      { value: "deepseek/deepseek-flash", label: "DeepSeek V4.1 Flash · 1 Mio. Tokens" },
      { value: "deepseek/deepseek-v4-pro", label: "DeepSeek V4 Pro" },
    ],
  },
  { name: "effort", source: "builtin", options: ["off", "low", "high", "max"].map((value) => ({ value })) },
]

describe("slashItems", () => {
  it("bei „/mo“ die Befehle, bei „/model “ die Modelle", () => {
    expect(slashItems(cmds, "/mo").items.map((i) => i.command.name)).toEqual(["model"])
    const r = slashItems(cmds, "/model ")
    expect(r.argsOf?.name).toBe("model")
    expect(r.items.map((i) => i.option?.value)).toEqual(["deepseek/deepseek-flash", "deepseek/deepseek-v4-pro"])
  })
  it("filtert Argumente nach Wert und Beschriftung", () => {
    expect(slashItems(cmds, "/model pro").items.map((i) => i.option?.value)).toEqual(["deepseek/deepseek-v4-pro"])
    expect(slashItems(cmds, "/effort h").items.map((i) => i.option?.value)).toEqual(["high"])
  })
  it("schließt, sobald ein Wert vollständig ist (Enter sendet dann)", () => {
    expect(slashItems(cmds, "/effort high").items).toEqual([])
  })
  it("ohne Vorschläge für Befehle ohne Optionen und bei mehreren Wörtern", () => {
    expect(slashItems(cmds, "/compact Fokus").items).toEqual([])
    expect(slashItems(cmds, "/effort high jetzt").items).toEqual([])
  })
})

describe("Hilfen für /model und /effort", () => {
  it("slashArg und filterOptions", () => {
    expect(slashArg("/Model  deep")).toEqual({ name: "model", query: "deep" })
    expect(slashArg("/model")).toBeUndefined()
    expect(filterOptions([{ value: "a", label: "Hoch" }, { value: "hoch" }], "hoch").map((o) => o.value)).toEqual(["hoch", "a"])
  })
  it("sind eingebaute Befehle", () => {
    expect(isBuiltinCommand("/model deepseek/deepseek-flash")).toBe(true)
    expect(isBuiltinCommand("/effort high")).toBe(true)
    expect(isBuiltinCommand("/models")).toBe(false)
  })
  it("withLiveOptions: Stufen und aktueller Wert aus dem Chat", () => {
    const live = withLiveOptions(cmds, { model: "deepseek/deepseek-v4-pro", thinking_level: "low", thinking_levels: ["off", "low", "high"] })
    expect(live.find((c) => c.name === "model")?.options?.find((o) => o.current)?.value).toBe("deepseek/deepseek-v4-pro")
    expect(live.find((c) => c.name === "effort")?.options).toEqual([
      { value: "off", label: "aus", current: false },
      { value: "low", label: "niedrig", current: true },
      { value: "high", label: "hoch", current: false },
    ])
  })
})
