import { describe, expect, it } from "vitest"
import { imageSource, messageImageKey, sandboxImagePath } from "./images"

describe("sandboxImagePath", () => {
  it("accepts local paths under /workspace, /tmp and /home/agent, relative ones from /workspace", () => {
    expect(sandboxImagePath("/workspace/plot.png")).toBe("/workspace/plot.png")
    expect(sandboxImagePath("plot.png")).toBe("/workspace/plot.png")
    expect(sandboxImagePath("./out/a.png")).toBe("/workspace/out/a.png")
    expect(sandboxImagePath("/tmp/x.gif")).toBe("/tmp/x.gif")
    expect(sandboxImagePath("/home/agent/b.webp")).toBe("/home/agent/b.webp")
    expect(sandboxImagePath("/workspace/a/../b.png")).toBe("/workspace/b.png")
    expect(sandboxImagePath("file:///workspace/plot.png")).toBe("/workspace/plot.png")
    expect(sandboxImagePath("/workspace//double//x.png")).toBe("/workspace/double/x.png")
  })
  it("decodes percent encoding (react-markdown encodes spaces and non-ASCII letters)", () => {
    expect(sandboxImagePath("my%20image.png")).toBe("/workspace/my image.png")
    expect(sandboxImagePath("/workspace/%C3%A4.png")).toBe("/workspace/ä.png")
    expect(sandboxImagePath("/workspace/%E0%A4%A.png")).toBe("/workspace/%E0%A4%A.png")
  })
  it("rejects foreign addresses, other locations and escapes", () => {
    for (const src of [
      "",
      "https://attacker.example/p.png",
      "http://x/y.png",
      "//attacker.example/p.png",
      "data:image/png;base64,AAAA",
      "javascript:alert(1)",
      "file://attacker/x.png",
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
  it("resolves local paths to the orchestrator's endpoint", () => {
    expect(imageSource("plot.png", ctx)).toEqual({
      kind: "sandbox",
      path: "/workspace/plot.png",
      url: "api/chats/c%201/images?path=%2Fworkspace%2Fplot.png&msg=resp-1",
    })
  })
  it("holds local images back until the response is finished (ID missing)", () => {
    expect(imageSource("/workspace/plot.png", { chatId: "c" })).toEqual({ kind: "pending", path: "/workspace/plot.png" })
    expect(imageSource("/workspace/plot.png")).toEqual({ kind: "pending", path: "/workspace/plot.png" })
  })
  it("shows data: images only as PNG, JPEG, GIF or WebP in base64", () => {
    const png = "data:image/png;base64,iVBORw0KGgo="
    expect(imageSource(png, ctx)).toEqual({ kind: "data", url: png })
    expect(imageSource("data:image/webp;base64,UklGRg==")).toEqual({ kind: "data", url: "data:image/webp;base64,UklGRg==" })
    expect(imageSource("data:image/svg+xml;base64,PHN2Zz4=", ctx).kind).toBe("blocked")
    expect(imageSource("data:text/html;base64,PGh0bWw+", ctx).kind).toBe("blocked")
    expect(imageSource("data:image/png,<svg>", ctx).kind).toBe("blocked")
  })
  it("never loads foreign addresses (Markdown image exfiltration)", () => {
    expect(imageSource("https://attacker.example/p?d=secret", ctx)).toEqual({
      kind: "blocked",
      src: "https://attacker.example/p?d=secret",
    })
    expect(imageSource("/etc/passwd", ctx).kind).toBe("blocked")
    expect(imageSource(undefined, ctx)).toEqual({ kind: "blocked", src: "" })
  })
})

describe("messageImageKey", () => {
  it("takes responseId, otherwise ts-<timestamp> (like the orchestrator)", () => {
    expect(messageImageKey({ responseId: "chatcmpl-1:a_b.c", timestamp: 5 })).toBe("chatcmpl-1:a_b.c")
    expect(messageImageKey({ timestamp: 1700000000000 })).toBe("ts-1700000000000")
    expect(messageImageKey({ responseId: "evil/../id", timestamp: 5 })).toBe("ts-5")
    expect(messageImageKey({})).toBeUndefined()
    expect(messageImageKey({ timestamp: 0 })).toBeUndefined()
  })
})
