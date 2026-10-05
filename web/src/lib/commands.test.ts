import { describe, expect, it } from "vitest"
import type { Command } from "@/api/types"
import { autoCompactSwitch, filterCommands, isBuiltinCommand, isSlashCommand, renameTitle, slashQuery } from "./commands"

const cmds: Command[] = [
  { name: "compact", description: "Kontext zusammenfassen", source: "builtin", args: "[Anweisungen]" },
  { name: "autocompact", description: "Automatik ein/aus", source: "builtin", args: "on|off" },
  { name: "skill:review", description: "Code prüfen", source: "skill" },
  { name: "commit", description: "Git-Commit vorbereiten", source: "prompt" },
]

describe("slashQuery", () => {
  it.each([
    ["/", ""],
    ["/co", "co"],
    ["/Skill:R", "skill:r"],
  ])("%s → %s", (text, q) => {
    expect(slashQuery(text)).toBe(q)
  })
  it.each(["", "hallo /co", " /co", "/compact jetzt", "/compact\n"])("kein Popover bei %j", (text) => {
    expect(slashQuery(text)).toBeUndefined()
  })
})

describe("filterCommands", () => {
  it("blendet Befehle aus, die nur in pis Terminal wirken (/todos)", () => {
    const list = [
      { name: "todos", source: "extension" },
      { name: "compact", source: "builtin" },
    ] as Command[]
    expect(filterCommands(list, "").map((c) => c.name)).toEqual(["compact"])
    expect(filterCommands(list, "todo").map((c) => c.name)).toEqual([])
  })
  it("zeigt bei leerer Eingabe alle", () => {
    expect(filterCommands(cmds, "").map((c) => c.name)).toEqual(["compact", "autocompact", "skill:review", "commit"])
  })
  it("stellt Präfixtreffer vor Teiltreffer", () => {
    expect(filterCommands(cmds, "comp").map((c) => c.name)).toEqual(["compact", "autocompact"])
    expect(filterCommands(cmds, "co").map((c) => c.name)).toEqual(["compact", "commit", "autocompact"])
  })
  it("sucht auch in der Beschreibung, nachrangig", () => {
    expect(filterCommands(cmds, "prüfen").map((c) => c.name)).toEqual(["skill:review"])
  })
  it("ignoriert Groß- und Kleinschreibung", () => {
    expect(filterCommands(cmds, "SKILL").map((c) => c.name)).toEqual(["skill:review"])
  })
})

describe("isSlashCommand", () => {
  it.each([
    ["/compact", true],
    ["  /compact Fokus", true],
    ["/", false],
    ["Pfad /workspace", false],
    ["//kommentar", false],
  ])("%j → %s", (text, yes) => {
    expect(isSlashCommand(text)).toBe(yes)
  })
})

describe("autoCompactSwitch", () => {
  it.each([
    ["/autocompact on", true],
    ["  /autocompact AUS ", false],
    ["/autocompact ein", true],
    ["/autocompact off", false],
    ["/autocompact", undefined],
    ["/autocompact vielleicht", undefined],
    ["/compact", undefined],
  ])("%j → %s", (text, want) => {
    expect(autoCompactSwitch(text)).toBe(want)
  })
})

describe("isBuiltinCommand", () => {
  it("erkennt nur /compact und /autocompact", () => {
    expect(isBuiltinCommand("/compact")).toBe(true)
    expect(isBuiltinCommand("/compact Fokus")).toBe(true)
    expect(isBuiltinCommand("/autocompact off")).toBe(true)
    expect(isBuiltinCommand("/compactor")).toBe(false)
    expect(isBuiltinCommand("/skill:bericht")).toBe(false)
    expect(isBuiltinCommand("/rename Neuer Name")).toBe(true)
    expect(isBuiltinCommand("/renamed")).toBe(false)
  })
})

describe("renameTitle", () => {
  it("liest den Namen aus /rename", () => {
    expect(renameTitle("/rename  Schwanzbeißen:  Klassen ")).toBe("Schwanzbeißen: Klassen")
    expect(renameTitle("/rename")).toBeUndefined()
    expect(renameTitle("/compact x")).toBeUndefined()
  })
})
