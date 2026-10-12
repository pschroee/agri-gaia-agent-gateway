// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

import { describe, expect, it } from "vitest"
import type { Approval } from "@/api/types"
import { approvalEffect, isEffect } from "@/lib/effect"

const base: Approval = {
  id: "1",
  chat_id: "c",
  kind: "platform_write",
  via: "cli",
  name: "DELETE /datasets/9",
  size: 0,
  sha256: "",
  content_type: "application/json",
  state: "pending",
  created_at: "2026-10-12T10:00:00Z",
}

describe("effect", () => {
  it("accepts only the four classes", () => {
    for (const v of ["read", "write", "compute", "irreversible"]) expect(isEffect(v)).toBe(true)
    for (const v of ["", "delete", "Read", undefined, null, 3]) expect(isEffect(v)).toBe(false)
  })

  it("reads the class of platform approvals", () => {
    expect(approvalEffect({ ...base, effect: "irreversible" })).toBe("irreversible")
    expect(approvalEffect(base)).toBeUndefined()
    expect(approvalEffect({ ...base, effect: "delete" as Approval["effect"] })).toBeUndefined()
  })

  it("has no class for internet access", () => {
    expect(approvalEffect({ ...base, kind: "internet_access", effect: "write" })).toBeUndefined()
  })
})
