import { describe, expect, it } from "vitest"
import { reconnectDelay } from "./reconnect"

describe("reconnectDelay", () => {
  it("starts at 2 s and doubles up to at most 30 s", () => {
    expect([0, 1, 2, 3, 4, 5, 10].map(reconnectDelay)).toEqual([2000, 4000, 8000, 16000, 30000, 30000, 30000])
  })
  it("treats negative values like the first attempt", () => {
    expect(reconnectDelay(-1)).toBe(2000)
  })
})
