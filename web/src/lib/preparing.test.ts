import { describe, expect, it } from "vitest"
import { preparingInfo } from "./preparing"

describe("preparingInfo", () => {
  it("reads path and half-finished content of a write call", () => {
    const partial = '{"path":"/workspace/report.md","content":"# Title\\n\\nLine with \\"quote\\"\\nnot yet fin'
    const i = preparingInfo("write", partial)
    expect(i.path).toBe("/workspace/report.md")
    expect(i.preview).toBe('# Title\n\nLine with "quote"\nnot yet fin')
    expect(i.lines).toBe(4)
    expect(i.bytes).toBe(partial.length)
  })
  it("shows the half-finished command for bash", () => {
    const i = preparingInfo("bash", '{"command":"python3 -c \\"print(1)')
    expect(i.preview).toBe('python3 -c "print(1)')
  })
  it("shows the end of the raw JSON for unknown tools and shortens long previews at the start", () => {
    const long = '{"x":"' + "a".repeat(5000)
    const i = preparingInfo("mcp_upload_artifact", long)
    expect(i.preview.length).toBeLessThanOrEqual(1201)
    expect(i.preview.startsWith("…")).toBe(true)
    expect(i.path).toBeUndefined()
  })
  it("tolerates empty and broken beginnings", () => {
    expect(preparingInfo("write", "").preview).toBe("")
    expect(preparingInfo("write", '{"pa').path).toBeUndefined()
    expect(preparingInfo("edit", '{"path":"a.txt","edits":[{"oldText":"x","newText":"y').path).toBe("a.txt")
  })
})
