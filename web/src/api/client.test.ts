import { describe, expect, it } from "vitest"
import { urls } from "./client"

// The UI runs under / and behind a proxy under /agent/; all addresses are therefore relative to the document.
describe("urls", () => {
  const all = [
    urls.session("c 1"),
    urls.events("c 1"),
    urls.artifact("c 1", "a b.csv", "output"),
    urls.image("c 1", "/workspace/plot.png", "resp-1"),
  ]
  it("are relative", () => {
    for (const u of all) expect(u.startsWith("api/chats/c%201/"), u).toBe(true)
  })
  it("stay under the path prefix", () => {
    expect(new URL(urls.events("c1"), "https://app.example/agent/?embed=1#/chats/c1").href).toBe(
      "https://app.example/agent/api/chats/c1/events",
    )
    expect(new URL(urls.events("c1"), "http://127.0.0.1:18480/#/chats/c1").href).toBe("http://127.0.0.1:18480/api/chats/c1/events")
  })
})
