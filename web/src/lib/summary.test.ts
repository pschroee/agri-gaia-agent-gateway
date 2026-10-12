// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

import { describe, expect, it } from "vitest"
import type { Approval } from "@/api/types"
import { summaryParts, technicalPreview } from "./summary"

const base: Approval = {
  id: "a", chat_id: "c", kind: "platform_write", via: "cli", name: "PATCH /datasets/16", size: 17, sha256: "",
  content_type: "application/json", state: "pending", created_at: "2026-10-12T10:00:00Z",
}

describe("summaryParts", () => {
  it("splits the gateway's summary into the action and its details", () => {
    expect(summaryParts('Rename model 3 "a" → "b"\nKept as they are: format\n')).toEqual({
      head: 'Rename model 3 "a" → "b"',
      details: ["Kept as they are: format"],
    })
  })
  it("has nothing for older entries", () => {
    expect(summaryParts(undefined)).toBeUndefined()
    expect(summaryParts("")).toBeUndefined()
  })
})

describe("technicalPreview", () => {
  it("drops the repeated name", () => {
    expect(technicalPreview({ ...base, preview: 'PATCH /datasets/16\nEffect: write\n\n{"name":"b"}' })).toBe('Effect: write\n\n{"name":"b"}')
    expect(technicalPreview({ ...base, preview: "PATCH /datasets/16" })).toBeUndefined()
  })
})
