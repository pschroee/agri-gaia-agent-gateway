import { describe, expect, it } from "vitest"
import { parseServerEvent, upsert } from "./events"

describe("parseServerEvent", () => {
  it("liest kind und data", () => {
    expect(parseServerEvent('{"kind":"pi","data":{"type":"agent_start"}}')).toEqual({
      kind: "pi",
      data: { type: "agent_start" },
    })
  })
  it("verwirft Unlesbares", () => {
    expect(parseServerEvent("kein json")).toBeNull()
    expect(parseServerEvent('{"data":1}')).toBeNull()
  })
})

describe("upsert", () => {
  it("ersetzt vorhandene Einträge und hängt neue an", () => {
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
