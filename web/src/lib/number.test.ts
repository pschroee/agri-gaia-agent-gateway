import { describe, expect, it } from "vitest"
import { clampInt } from "./number"

describe("clampInt", () => {
  it.each([
    [3, 0, 5, 3],
    [-1, 0, 5, 0],
    [9, 0, 5, 5],
    [2.6, 0, 5, 3],
    [Number.NaN, 0, 5, 0],
  ])("%d in [%d, %d] → %d", (v, min, max, out) => {
    expect(clampInt(v, min, max)).toBe(out)
  })
  it("reads input text", () => {
    expect(clampInt("4", 0, 5)).toBe(4)
    expect(clampInt("", 1, 5)).toBe(1)
    expect(clampInt("abc", 1, 5)).toBe(1)
  })
})
