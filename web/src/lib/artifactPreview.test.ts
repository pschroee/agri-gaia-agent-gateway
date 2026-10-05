import { describe, expect, it } from "vitest"
import type { Artifact } from "@/api/types"
import { artifactsOfCall, previewKind } from "./artifactPreview"

describe("previewKind", () => {
  it("image, PDF or file by type and extension", () => {
    expect(previewKind({ name: "plot.png", content_type: "image/png" })).toBe("image")
    expect(previewKind({ name: "photo.JPG", content_type: "image/jpeg" })).toBe("image")
    expect(previewKind({ name: "report.pdf", content_type: "application/pdf" })).toBe("pdf")
    expect(previewKind({ name: "page.html", content_type: "image/png" })).toBe("file")
    expect(previewKind({ name: "x.pdf", content_type: "text/html" })).toBe("file")
    expect(previewKind({ name: "logo.svg", content_type: "image/svg+xml" })).toBe("file")
    expect(previewKind({ name: "data.csv", content_type: "text/csv; charset=utf-8" })).toBe("file")
  })
})

describe("artifactsOfCall", () => {
  const a = (name: string, id: string, kind: "input" | "output" = "output", at = "2026-09-30T10:00:00Z"): Artifact => ({
    chat_id: "c", kind, name, size: 1, sha256: "", content_type: "text/plain", created_at: at, via: "cli", tool_call_id: id,
  })
  it("only results of this call", () => {
    const list = [a("b", "t1", "output", "2026-09-30T10:00:02Z"), a("a", "t1"), a("c", "t2"), a("d", "t1", "input")]
    expect(artifactsOfCall(list, "t1").map((x) => x.name)).toEqual(["a", "b"])
    expect(artifactsOfCall(list, undefined)).toEqual([])
  })
})
