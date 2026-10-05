import { describe, expect, it } from "vitest"
import type { Command } from "@/api/types"
import { autoCompactSwitch, filterCommands, isBuiltinCommand, isSlashCommand, renameTitle, slashQuery } from "./commands"

const cmds: Command[] = [
  { name: "compact", description: "Summarize context", source: "builtin", args: "[instructions]" },
  { name: "autocompact", description: "Automatic on/off", source: "builtin", args: "on|off" },
  { name: "skill:review", description: "Review code", source: "skill" },
  { name: "commit", description: "Prepare git commit", source: "prompt" },
]

describe("slashQuery", () => {
  it.each([
    ["/", ""],
    ["/co", "co"],
    ["/Skill:R", "skill:r"],
  ])("%s → %s", (text, q) => {
    expect(slashQuery(text)).toBe(q)
  })
  it.each(["", "hello /co", " /co", "/compact now", "/compact\n"])("no popover for %j", (text) => {
    expect(slashQuery(text)).toBeUndefined()
  })
})

describe("filterCommands", () => {
  it("hides commands that only work in pi's terminal (/todos)", () => {
    const list = [
      { name: "todos", source: "extension" },
      { name: "compact", source: "builtin" },
    ] as Command[]
    expect(filterCommands(list, "").map((c) => c.name)).toEqual(["compact"])
    expect(filterCommands(list, "todo").map((c) => c.name)).toEqual([])
  })
  it("shows all for empty input", () => {
    expect(filterCommands(cmds, "").map((c) => c.name)).toEqual(["compact", "autocompact", "skill:review", "commit"])
  })
  it("puts prefix matches before partial matches", () => {
    expect(filterCommands(cmds, "comp").map((c) => c.name)).toEqual(["compact", "autocompact"])
    expect(filterCommands(cmds, "co").map((c) => c.name)).toEqual(["compact", "commit", "autocompact"])
  })
  it("also searches the description, with lower priority", () => {
    expect(filterCommands(cmds, "review c").map((c) => c.name)).toEqual(["skill:review"])
  })
  it("ignores case", () => {
    expect(filterCommands(cmds, "SKILL").map((c) => c.name)).toEqual(["skill:review"])
  })
})

describe("isSlashCommand", () => {
  it.each([
    ["/compact", true],
    ["  /compact focus", true],
    ["/", false],
    ["path /workspace", false],
    ["//comment", false],
  ])("%j → %s", (text, yes) => {
    expect(isSlashCommand(text)).toBe(yes)
  })
})

describe("autoCompactSwitch", () => {
  it.each([
    ["/autocompact on", true],
    ["  /autocompact OFF ", false],
    ["/autocompact true", true],
    ["/autocompact aus", undefined],
    ["/autocompact off", false],
    ["/autocompact", undefined],
    ["/autocompact maybe", undefined],
    ["/compact", undefined],
  ])("%j → %s", (text, want) => {
    expect(autoCompactSwitch(text)).toBe(want)
  })
})

describe("isBuiltinCommand", () => {
  it("recognises only the built-in commands", () => {
    expect(isBuiltinCommand("/compact")).toBe(true)
    expect(isBuiltinCommand("/compact focus")).toBe(true)
    expect(isBuiltinCommand("/autocompact off")).toBe(true)
    expect(isBuiltinCommand("/compactor")).toBe(false)
    expect(isBuiltinCommand("/skill:report")).toBe(false)
    expect(isBuiltinCommand("/rename New name")).toBe(true)
    expect(isBuiltinCommand("/renamed")).toBe(false)
  })
})

describe("renameTitle", () => {
  it("reads the name from /rename", () => {
    expect(renameTitle("/rename  Tail biting:  classes ")).toBe("Tail biting: classes")
    expect(renameTitle("/rename")).toBeUndefined()
    expect(renameTitle("/compact x")).toBeUndefined()
  })
})
