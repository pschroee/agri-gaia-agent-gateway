import { describe, expect, it } from "vitest"
import { readFlag, writeFlag } from "./prefs"

const memory = () => {
  const m = new Map<string, string>()
  return { getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => void m.set(k, v) }
}

const locked = {
  getItem: () => {
    throw new Error("gesperrt")
  },
  setItem: () => {
    throw new Error("gesperrt")
  },
}

describe("readFlag/writeFlag", () => {
  it("liefert den Standard, solange nichts gespeichert ist", () => {
    const s = memory()
    expect(readFlag("x", true, s)).toBe(true)
    expect(readFlag("x", false, s)).toBe(false)
  })

  it("merkt sich den Wert", () => {
    const s = memory()
    writeFlag("x", false, s)
    expect(readFlag("x", true, s)).toBe(false)
    writeFlag("x", true, s)
    expect(readFlag("x", false, s)).toBe(true)
  })

  it("übersteht einen gesperrten oder fehlenden Speicher", () => {
    expect(() => writeFlag("x", false, locked)).not.toThrow()
    expect(readFlag("x", true, locked)).toBe(true)
    expect(readFlag("x", true, undefined)).toBe(true)
    expect(() => writeFlag("x", true, undefined)).not.toThrow()
  })
})
