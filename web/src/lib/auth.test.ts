import { describe, expect, it } from "vitest"
import { LOGIN_RETRY_MS, claimSilentLogin, clearSilentLogin, isEmbed, isLoginPath, silentLoginUrl } from "./auth"

function memStore() {
  const m = new Map<string, string>()
  return { getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => void m.set(k, v), removeItem: (k: string) => void m.delete(k) }
}

describe("auth", () => {
  it("builds the target with return address", () => {
    expect(silentLoginUrl("/oidc/login", { pathname: "/", search: "?embed=1", hash: "#/chats/x" })).toBe(
      "/oidc/login?prompt=none&return=%2F%3Fembed%3D1%23%2Fchats%2Fx",
    )
  })
  it("keeps the path in the return address under a path prefix", () => {
    expect(silentLoginUrl("/agent/oidc/login", { pathname: "/agent/", search: "?embed=1", hash: "#/chats/x" })).toBe(
      "/agent/oidc/login?prompt=none&return=%2Fagent%2F%3Fembed%3D1%23%2Fchats%2Fx",
    )
  })
  it("recognises our own login, also relative and under a prefix", () => {
    for (const p of ["oidc/login", "/oidc/login", "/agent/oidc/login", "/a/b/oidc/login"]) expect(isLoginPath(p), p).toBe(true)
    for (const p of ["", "//evil.example/oidc/login", "https://evil.example/oidc/login", "javascript:oidc/login", "/oidc/login?x=1", "/../oidc/login", "/api/me"])
      expect(isLoginPath(p), p).toBe(false)
  })
  it("tries only once per period", () => {
    const s = memStore()
    expect(claimSilentLogin(s, 1000)).toBe(true)
    expect(claimSilentLogin(s, 2000)).toBe(false)
    expect(claimSilentLogin(s, 1000 + LOGIN_RETRY_MS)).toBe(true)
    clearSilentLogin(s)
    expect(claimSilentLogin(s, 1000 + LOGIN_RETRY_MS + 1)).toBe(true)
  })
  it("no navigation without storage", () => {
    const broken = { getItem: () => { throw new Error("x") }, setItem: () => {}, removeItem: () => {} }
    expect(claimSilentLogin(broken, 1)).toBe(false)
  })
  it("recognises the embedded view", () => {
    expect(isEmbed("?embed=1")).toBe(true)
    expect(isEmbed("?embed=0")).toBe(false)
    expect(isEmbed("")).toBe(false)
  })
})
