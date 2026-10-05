import { describe, expect, it } from "vitest"
import { LOGIN_RETRY_MS, claimSilentLogin, clearSilentLogin, isEmbed, silentLoginUrl } from "./auth"

function memStore() {
  const m = new Map<string, string>()
  return { getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => void m.set(k, v), removeItem: (k: string) => void m.delete(k) }
}

describe("auth", () => {
  it("baut das Ziel mit Rücksprung", () => {
    expect(silentLoginUrl("/oidc/login", { pathname: "/", search: "?embed=1", hash: "#/chats/x" })).toBe(
      "/oidc/login?prompt=none&return=%2F%3Fembed%3D1%23%2Fchats%2Fx",
    )
  })
  it("versucht nur einmal je Frist", () => {
    const s = memStore()
    expect(claimSilentLogin(s, 1000)).toBe(true)
    expect(claimSilentLogin(s, 2000)).toBe(false)
    expect(claimSilentLogin(s, 1000 + LOGIN_RETRY_MS)).toBe(true)
    clearSilentLogin(s)
    expect(claimSilentLogin(s, 1000 + LOGIN_RETRY_MS + 1)).toBe(true)
  })
  it("ohne Speicher keine Navigation", () => {
    const broken = { getItem: () => { throw new Error("x") }, setItem: () => {}, removeItem: () => {} }
    expect(claimSilentLogin(broken, 1)).toBe(false)
  })
  it("erkennt die eingebettete Ansicht", () => {
    expect(isEmbed("?embed=1")).toBe(true)
    expect(isEmbed("?embed=0")).toBe(false)
    expect(isEmbed("")).toBe(false)
  })
})
