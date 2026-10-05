import { describe, expect, it } from "vitest"
import { splitAttachments, withAttachments } from "./attachments"

describe("splitAttachments", () => {
  it("splits the attachment block from the text", () => {
    expect(splitAttachments("Have a look\n\n[Attachments in /workspace/inputs/]\n- data.csv\n- image 1.png")).toEqual({
      text: "Have a look",
      files: ["data.csv", "image 1.png"],
    })
  })
  it("leaves text without a block unchanged", () => {
    expect(splitAttachments("just text\n- no attachment")).toEqual({ text: "just text\n- no attachment", files: [] })
  })
  it("recognises the block only at the end, not in the middle of the text", () => {
    const t = "[Attachments in /workspace/inputs/]\n- a.csv\n\nand more text afterwards"
    expect(splitAttachments(t)).toEqual({ text: t, files: [] })
  })
  it("does not show \"See attachments.\" as text when only attachments were sent", () => {
    expect(splitAttachments("See attachments.\n\n[Attachments in /workspace/inputs/]\n- a.csv")).toEqual({ text: "", files: ["a.csv"] })
  })
})

import { composerButtons, isPreviewImage } from "./attachments"

describe("isPreviewImage", () => {
  it("recognises raster images by extension, also in upper case", () => {
    for (const n of ["a.png", "b.JPG", "c.jpeg", "d.webp", "e.gif", "f.avif"]) expect(isPreviewImage(n)).toBe(true)
  })
  it("does not show SVG and other files as an image", () => {
    for (const n of ["a.svg", "b.pdf", "png", "c.png.txt", ""]) expect(isPreviewImage(n)).toBe(false)
  })
})

describe("composerButtons", () => {
  it("without a run: Send, disabled without content", () => {
    expect(composerButtons({ running: false, hasContent: false })).toEqual({ stop: false, send: true, sendEnabled: false })
    expect(composerButtons({ running: false, hasContent: true })).toEqual({ stop: false, send: true, sendEnabled: true })
  })
  it("during a run: Stop instead of Send, with content both (enqueue the message)", () => {
    expect(composerButtons({ running: true, hasContent: false })).toEqual({ stop: true, send: false, sendEnabled: false })
    expect(composerButtons({ running: true, hasContent: true })).toEqual({ stop: true, send: true, sendEnabled: true })
  })
})

describe("withAttachments", () => {
  it("builds the same text as the orchestrator and can be split again", () => {
    expect(withAttachments("Look", [])).toBe("Look")
    const t = withAttachments(" Look ", ["a.csv", "b.png"])
    expect(t).toBe("Look\n\n[Attachments in /workspace/inputs/]\n- a.csv\n- b.png")
    expect(splitAttachments(t)).toEqual({ text: "Look", files: ["a.csv", "b.png"] })
    expect(splitAttachments(withAttachments("", ["a.csv"]))).toEqual({ text: "", files: ["a.csv"] })
    expect(withAttachments("", ["a.csv"])).toBe("See attachments.\n\n[Attachments in /workspace/inputs/]\n- a.csv")
  })
})
