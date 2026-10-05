import { describe, expect, it } from "vitest"
import { browserLanguage, validLanguage } from "./language"

describe("validLanguage", () => {
  it("nimmt Angaben nach BCP 47", () => {
    expect(validLanguage("en-US")).toBe("en-US")
    expect(validLanguage(" de ")).toBe("de")
    expect(validLanguage("zh-Hant-TW")).toBe("zh-Hant-TW")
    expect(validLanguage("es-419")).toBe("es-419")
  })
  it("weist Ungültiges ab", () => {
    for (const t of ["", "en_US", "en--US", "-en", "en-", "1en", "en US", "a".repeat(36), undefined, null]) {
      expect(validLanguage(t)).toBeUndefined()
    }
  })
})

describe("browserLanguage", () => {
  it("liest navigator.language", () => {
    expect(browserLanguage({ language: "fr-CA" })).toBe("fr-CA")
    expect(browserLanguage({})).toBeUndefined()
  })
})
