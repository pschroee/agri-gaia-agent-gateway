import { describe, expect, it } from "vitest"
import { splitAttachments, withAttachments } from "./attachments"

describe("splitAttachments", () => {
  it("trennt den Anhang-Block vom Text", () => {
    expect(splitAttachments("Schau mal\n\n[Anhänge unter /workspace/inputs/]\n- daten.csv\n- bild 1.png")).toEqual({
      text: "Schau mal",
      files: ["daten.csv", "bild 1.png"],
    })
  })
  it("lässt Text ohne Block unverändert", () => {
    expect(splitAttachments("nur Text\n- kein Anhang")).toEqual({ text: "nur Text\n- kein Anhang", files: [] })
  })
  it("erkennt den Block nur am Ende, nicht mitten im Text", () => {
    const t = "[Anhänge unter /workspace/inputs/]\n- a.csv\n\nund danach noch Text"
    expect(splitAttachments(t)).toEqual({ text: t, files: [] })
  })
  it("zeigt „Siehe Anhänge.“ nicht als Text, wenn nur Anhänge gesendet wurden", () => {
    expect(splitAttachments("Siehe Anhänge.\n\n[Anhänge unter /workspace/inputs/]\n- a.csv")).toEqual({ text: "", files: ["a.csv"] })
  })
})

import { composerButtons, isPreviewImage } from "./attachments"

describe("isPreviewImage", () => {
  it("erkennt Rasterbilder an der Endung, auch groß geschrieben", () => {
    for (const n of ["a.png", "b.JPG", "c.jpeg", "d.webp", "e.gif", "f.avif"]) expect(isPreviewImage(n)).toBe(true)
  })
  it("zeigt SVG und andere Dateien nicht als Bild", () => {
    for (const n of ["a.svg", "b.pdf", "png", "c.png.txt", ""]) expect(isPreviewImage(n)).toBe(false)
  })
})

describe("composerButtons", () => {
  it("ohne Lauf: Senden, gesperrt ohne Inhalt", () => {
    expect(composerButtons({ running: false, hasContent: false })).toEqual({ stop: false, send: true, sendEnabled: false })
    expect(composerButtons({ running: false, hasContent: true })).toEqual({ stop: false, send: true, sendEnabled: true })
  })
  it("während eines Laufs: Stopp statt Senden, mit Inhalt beide (Nachricht einreihen)", () => {
    expect(composerButtons({ running: true, hasContent: false })).toEqual({ stop: true, send: false, sendEnabled: false })
    expect(composerButtons({ running: true, hasContent: true })).toEqual({ stop: true, send: true, sendEnabled: true })
  })
})

describe("withAttachments", () => {
  it("baut denselben Text wie der Orchestrator und lässt sich wieder zerlegen", () => {
    expect(withAttachments("Schau", [])).toBe("Schau")
    const t = withAttachments(" Schau ", ["a.csv", "b.png"])
    expect(t).toBe("Schau\n\n[Anhänge unter /workspace/inputs/]\n- a.csv\n- b.png")
    expect(splitAttachments(t)).toEqual({ text: "Schau", files: ["a.csv", "b.png"] })
    expect(splitAttachments(withAttachments("", ["a.csv"]))).toEqual({ text: "", files: ["a.csv"] })
  })
})
