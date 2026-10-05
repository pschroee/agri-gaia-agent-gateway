import { describe, expect, it } from "vitest"
import { imageSource, messageImageKey, sandboxImagePath } from "./images"

describe("sandboxImagePath", () => {
  it("nimmt lokale Pfade unter /workspace, /tmp und /home/agent, relative ab /workspace", () => {
    expect(sandboxImagePath("/workspace/plot.png")).toBe("/workspace/plot.png")
    expect(sandboxImagePath("plot.png")).toBe("/workspace/plot.png")
    expect(sandboxImagePath("./out/a.png")).toBe("/workspace/out/a.png")
    expect(sandboxImagePath("/tmp/x.gif")).toBe("/tmp/x.gif")
    expect(sandboxImagePath("/home/agent/b.webp")).toBe("/home/agent/b.webp")
    expect(sandboxImagePath("/workspace/a/../b.png")).toBe("/workspace/b.png")
    expect(sandboxImagePath("file:///workspace/plot.png")).toBe("/workspace/plot.png")
    expect(sandboxImagePath("/workspace//doppelt//x.png")).toBe("/workspace/doppelt/x.png")
  })
  it("löst Prozentkodierung auf (react-markdown kodiert Leerzeichen und Umlaute)", () => {
    expect(sandboxImagePath("mein%20bild.png")).toBe("/workspace/mein bild.png")
    expect(sandboxImagePath("/workspace/%C3%A4.png")).toBe("/workspace/ä.png")
    expect(sandboxImagePath("/workspace/%E0%A4%A.png")).toBe("/workspace/%E0%A4%A.png")
  })
  it("weist fremde Adressen, andere Orte und Ausbrüche ab", () => {
    for (const src of [
      "",
      "https://angreifer.example/p.png",
      "http://x/y.png",
      "//angreifer.example/p.png",
      "data:image/png;base64,AAAA",
      "javascript:alert(1)",
      "file://angreifer/x.png",
      "/etc/passwd",
      "/agent/config/models.json",
      "/workspace",
      "/workspace/",
      "../etc/passwd",
      "/workspace/../../etc/passwd",
      "/workspacex/a.png",
      "/workspace/a\u0000.png",
      "a".repeat(1100),
    ]) {
      expect(sandboxImagePath(src), src).toBeUndefined()
    }
  })
})

describe("imageSource", () => {
  const ctx = { chatId: "c 1", msgId: "resp-1" }
  it("löst lokale Pfade auf den Endpunkt des Orchestrators auf", () => {
    expect(imageSource("plot.png", ctx)).toEqual({
      kind: "sandbox",
      path: "/workspace/plot.png",
      url: "/api/chats/c%201/images?path=%2Fworkspace%2Fplot.png&msg=resp-1",
    })
  })
  it("wartet mit lokalen Bildern, bis die Antwort fertig ist (Kennung fehlt)", () => {
    expect(imageSource("/workspace/plot.png", { chatId: "c" })).toEqual({ kind: "pending", path: "/workspace/plot.png" })
    expect(imageSource("/workspace/plot.png")).toEqual({ kind: "pending", path: "/workspace/plot.png" })
  })
  it("zeigt data:-Bilder nur als PNG, JPEG, GIF oder WebP in base64", () => {
    const png = "data:image/png;base64,iVBORw0KGgo="
    expect(imageSource(png, ctx)).toEqual({ kind: "data", url: png })
    expect(imageSource("data:image/webp;base64,UklGRg==")).toEqual({ kind: "data", url: "data:image/webp;base64,UklGRg==" })
    expect(imageSource("data:image/svg+xml;base64,PHN2Zz4=", ctx).kind).toBe("blocked")
    expect(imageSource("data:text/html;base64,PGh0bWw+", ctx).kind).toBe("blocked")
    expect(imageSource("data:image/png,<svg>", ctx).kind).toBe("blocked")
  })
  it("lädt fremde Adressen nie (Markdown-Image-Exfiltration)", () => {
    expect(imageSource("https://angreifer.example/p?d=geheim", ctx)).toEqual({
      kind: "blocked",
      src: "https://angreifer.example/p?d=geheim",
    })
    expect(imageSource("/etc/passwd", ctx).kind).toBe("blocked")
    expect(imageSource(undefined, ctx)).toEqual({ kind: "blocked", src: "" })
  })
})

describe("messageImageKey", () => {
  it("nimmt responseId, sonst ts-<timestamp> (wie der Orchestrator)", () => {
    expect(messageImageKey({ responseId: "chatcmpl-1:a_b.c", timestamp: 5 })).toBe("chatcmpl-1:a_b.c")
    expect(messageImageKey({ timestamp: 1700000000000 })).toBe("ts-1700000000000")
    expect(messageImageKey({ responseId: "böse/../id", timestamp: 5 })).toBe("ts-5")
    expect(messageImageKey({})).toBeUndefined()
    expect(messageImageKey({ timestamp: 0 })).toBeUndefined()
  })
})
