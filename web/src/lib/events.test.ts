import { describe, expect, it } from "vitest"
import { parseServerEvent, upsert } from "./events"

describe("parseServerEvent", () => {
  it("reads kind and data", () => {
    expect(parseServerEvent('{"kind":"pi","data":{"type":"agent_start"}}')).toEqual({
      kind: "pi",
      data: { type: "agent_start" },
    })
  })
  it("discards unreadable input", () => {
    expect(parseServerEvent("no json")).toBeNull()
    expect(parseServerEvent('{"data":1}')).toBeNull()
  })
})

describe("upsert", () => {
  it("replaces existing entries and appends new ones", () => {
    const list = [
      { id: "a", v: 1 },
      { id: "b", v: 1 },
    ]
    expect(upsert(list, { id: "b", v: 2 }, (x) => x.id)).toEqual([
      { id: "a", v: 1 },
      { id: "b", v: 2 },
    ])
    expect(upsert(list, { id: "c", v: 1 }, (x) => x.id)).toHaveLength(3)
  })
})
