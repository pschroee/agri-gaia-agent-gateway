import { describe, expect, it } from "vitest"
import { chatHref, parseHash, subagentHref } from "./useHashRoute"

describe("parseHash", () => {
  it("erkennt Chatliste, Chat und Status", () => {
    expect(parseHash("")).toEqual({ view: "chats" })
    expect(parseHash("#/chats")).toEqual({ view: "chats" })
    expect(parseHash("#/chats/c1")).toEqual({ view: "chats", chatId: "c1" })
    expect(parseHash("#/status")).toEqual({ view: "status" })
  })

  it("erkennt die Ansicht eines Subagenten-Laufs mit kodierter Laufkennung", () => {
    expect(parseHash("#/chats/c1/subagents/abc123%232")).toEqual({ view: "chats", chatId: "c1", runId: "abc123#2" })
  })

  it("verträgt ein unkodiertes # in der Laufkennung", () => {
    expect(parseHash("#/chats/c1/subagents/abc123#2")).toEqual({ view: "chats", chatId: "c1", runId: "abc123#2" })
  })

  it("ignoriert „subagents“ ohne Laufkennung", () => {
    expect(parseHash("#/chats/c1/subagents")).toEqual({ view: "chats", chatId: "c1" })
  })

  it("bricht bei kaputter Kodierung nicht ab", () => {
    expect(parseHash("#/chats/c1/subagents/%E0%A4%A")).toEqual({ view: "chats", chatId: "c1", runId: "%E0%A4%A" })
  })
})

describe("Links", () => {
  it("kodieren die Laufkennung und lassen sich zurücklesen", () => {
    const href = subagentHref("c 1", "abc#2")
    expect(href).toBe("#/chats/c%201/subagents/abc%232")
    expect(parseHash(href)).toEqual({ view: "chats", chatId: "c 1", runId: "abc#2" })
    expect(parseHash(chatHref("c/1"))).toEqual({ view: "chats", chatId: "c/1" })
  })
})
