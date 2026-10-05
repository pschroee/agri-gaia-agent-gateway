import { describe, expect, it } from "vitest"
import { chatHref, parseHash, subagentHref } from "./useHashRoute"

describe("parseHash", () => {
  it("recognises chat list, chat and status", () => {
    expect(parseHash("")).toEqual({ view: "chats" })
    expect(parseHash("#/chats")).toEqual({ view: "chats" })
    expect(parseHash("#/chats/c1")).toEqual({ view: "chats", chatId: "c1" })
    expect(parseHash("#/status")).toEqual({ view: "status" })
  })

  it("recognises the view of a subagent run with an encoded run ID", () => {
    expect(parseHash("#/chats/c1/subagents/abc123%232")).toEqual({ view: "chats", chatId: "c1", runId: "abc123#2" })
  })

  it("tolerates an unencoded # in the run ID", () => {
    expect(parseHash("#/chats/c1/subagents/abc123#2")).toEqual({ view: "chats", chatId: "c1", runId: "abc123#2" })
  })

  it("ignores \"subagents\" without a run ID", () => {
    expect(parseHash("#/chats/c1/subagents")).toEqual({ view: "chats", chatId: "c1" })
  })

  it("does not break on broken encoding", () => {
    expect(parseHash("#/chats/c1/subagents/%E0%A4%A")).toEqual({ view: "chats", chatId: "c1", runId: "%E0%A4%A" })
  })
})

describe("links", () => {
  it("encode the run ID and can be read back", () => {
    const href = subagentHref("c 1", "abc#2")
    expect(href).toBe("#/chats/c%201/subagents/abc%232")
    expect(parseHash(href)).toEqual({ view: "chats", chatId: "c 1", runId: "abc#2" })
    expect(parseHash(chatHref("c/1"))).toEqual({ view: "chats", chatId: "c/1" })
  })
})
