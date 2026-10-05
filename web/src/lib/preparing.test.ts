import { describe, expect, it } from "vitest"
import { preparingInfo } from "./preparing"

describe("preparingInfo", () => {
  it("liest Pfad und halbfertigen Inhalt eines write-Aufrufs", () => {
    const partial = '{"path":"/workspace/bericht.md","content":"# Titel\\n\\nZeile mit \\"Zitat\\"\\nnoch nicht fer'
    const i = preparingInfo("write", partial)
    expect(i.path).toBe("/workspace/bericht.md")
    expect(i.preview).toBe('# Titel\n\nZeile mit "Zitat"\nnoch nicht fer')
    expect(i.lines).toBe(4)
    expect(i.bytes).toBe(partial.length)
  })
  it("zeigt bei bash den halbfertigen Befehl", () => {
    const i = preparingInfo("bash", '{"command":"python3 -c \\"print(1)')
    expect(i.preview).toBe('python3 -c "print(1)')
  })
  it("zeigt bei unbekannten Werkzeugen das Ende des Roh-JSON und kürzt lange Vorschauen am Anfang", () => {
    const long = '{"x":"' + "a".repeat(5000)
    const i = preparingInfo("mcp_upload_artifact", long)
    expect(i.preview.length).toBeLessThanOrEqual(1201)
    expect(i.preview.startsWith("…")).toBe(true)
    expect(i.path).toBeUndefined()
  })
  it("verträgt leere und kaputte Anfänge", () => {
    expect(preparingInfo("write", "").preview).toBe("")
    expect(preparingInfo("write", '{"pa').path).toBeUndefined()
    expect(preparingInfo("edit", '{"path":"a.txt","edits":[{"oldText":"x","newText":"y').path).toBe("a.txt")
  })
})
