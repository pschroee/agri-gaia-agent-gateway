import { describe, expect, it } from "vitest"
import { reconnectDelay } from "./reconnect"

describe("reconnectDelay", () => {
  it("beginnt bei 2 s und verdoppelt bis höchstens 30 s", () => {
    expect([0, 1, 2, 3, 4, 5, 10].map(reconnectDelay)).toEqual([2000, 4000, 8000, 16000, 30000, 30000, 30000])
  })
  it("behandelt negative Werte wie den ersten Versuch", () => {
    expect(reconnectDelay(-1)).toBe(2000)
  })
})
