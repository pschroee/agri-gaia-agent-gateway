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
