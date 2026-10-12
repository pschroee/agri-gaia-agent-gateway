import { describe, expect, it } from "vitest"
import type { Approval } from "@/api/types"
import { pendingMenuItems } from "./pending"

describe("pendingMenuItems", () => {
  it("labels plans and names the title", () => {
    const plan: Approval = {
      id: "p", chat_id: "c1", kind: "plan", via: "cli", name: "Plan: Model and rename", size: 2, sha256: "",
      content_type: "application/json", state: "pending", created_at: "2026-10-12T10:00:00Z",
    }
    const [item] = pendingMenuItems([plan], { c1: "Training" })
    expect(item).toMatchObject({ label: "Plan", detail: "Plan: Model and rename", chatTitle: "Training", href: "#/chats/c1" })
  })
})

describe("pendingMenuItems with gateway summaries", () => {
  it("names a platform call by the gateway's summary", () => {
    const call: Approval = {
      id: "w", chat_id: "c1", kind: "platform_write", via: "cli", name: "PATCH /datasets/16", size: 17, sha256: "",
      content_type: "application/json", state: "pending", created_at: "2026-10-12T10:00:00Z",
      summary: 'Rename dataset 16 "a" → "b"',
    }
    const plan: Approval = { ...call, id: "p", kind: "plan", name: "Plan with 2 steps", summary: "Plan with 2 steps\n1. x\n2. y" }
    const items = pendingMenuItems([call, plan], {})
    expect(items[0].detail).toBe('Rename dataset 16 "a" → "b"')
    expect(items[1].detail).toBe("Plan with 2 steps")
  })
})
