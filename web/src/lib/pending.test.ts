import { describe, expect, it } from "vitest"
import type { Approval } from "@/api/types"
import { pendingMenuItems } from "./pending"

const base: Approval = {
  id: "a", chat_id: "c1", kind: "artifact_upload", via: "cli", name: "result.csv", size: 2048,
  sha256: "x", content_type: "text/csv", state: "pending", created_at: "2026-09-29T12:00:00Z",
}

describe("pendingMenuItems", () => {
  it("sorts oldest first, names chat title and kind", () => {
    const items = pendingMenuItems(
      [
        { ...base, id: "b", chat_id: "c2", kind: "internet_access", name: "pip install pandas", size: 0, created_at: "2026-09-29T12:05:00Z" },
        base,
      ],
      { c1: "Analysis", c2: "Web fetch" },
    )
    expect(items.map((i) => i.id)).toEqual(["a", "b"])
    expect(items[0]).toMatchObject({ chatTitle: "Analysis", label: "Upload artifact", detail: "result.csv · 2.0 KiB", href: "#/chats/c1" })
    expect(items[1]).toMatchObject({ chatTitle: "Web fetch", label: "Internet access", detail: "pip install pandas", href: "#/chats/c2", internet: true })
  })
  it("shows the shortened chat ID without a known title and shortens long reasons", () => {
    const long = "x".repeat(200)
    const [i] = pendingMenuItems([{ ...base, chat_id: "12345678-aaaa", kind: "internet_access", name: long, size: 0 }], {})
    expect(i.chatTitle).toBe("Chat 12345678")
    expect(i.detail.length).toBeLessThanOrEqual(81)
    expect(i.detail.endsWith("…")).toBe(true)
  })
  it("ignores decided approvals", () => {
    expect(pendingMenuItems([{ ...base, state: "approved" }], {})).toEqual([])
  })
})
