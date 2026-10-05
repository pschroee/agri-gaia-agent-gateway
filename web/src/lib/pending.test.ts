import { describe, expect, it } from "vitest"
import type { Approval } from "@/api/types"
import { pendingMenuItems } from "./pending"

const base: Approval = {
  id: "a", chat_id: "c1", kind: "artifact_upload", via: "cli", name: "ergebnis.csv", size: 2048,
  sha256: "x", content_type: "text/csv", state: "pending", created_at: "2026-09-29T12:00:00Z",
}

describe("pendingMenuItems", () => {
  it("ordnet älteste zuerst, nennt Chat-Titel und Art", () => {
    const items = pendingMenuItems(
      [
        { ...base, id: "b", chat_id: "c2", kind: "internet_access", name: "pip install pandas", size: 0, created_at: "2026-09-29T12:05:00Z" },
        base,
      ],
      { c1: "Auswertung", c2: "Web-Abruf" },
    )
    expect(items.map((i) => i.id)).toEqual(["a", "b"])
    expect(items[0]).toMatchObject({ chatTitle: "Auswertung", label: "Artefakt hochladen", detail: "ergebnis.csv · 2,0 KiB", href: "#/chats/c1" })
    expect(items[1]).toMatchObject({ chatTitle: "Web-Abruf", label: "Internetzugang", detail: "pip install pandas", href: "#/chats/c2", internet: true })
  })
  it("zeigt ohne bekannten Titel die gekürzte Chat-Kennung und kürzt lange Begründungen", () => {
    const long = "x".repeat(200)
    const [i] = pendingMenuItems([{ ...base, chat_id: "12345678-aaaa", kind: "internet_access", name: long, size: 0 }], {})
    expect(i.chatTitle).toBe("Chat 12345678")
    expect(i.detail.length).toBeLessThanOrEqual(81)
    expect(i.detail.endsWith("…")).toBe(true)
  })
  it("ignoriert entschiedene Bestätigungen", () => {
    expect(pendingMenuItems([{ ...base, state: "approved" }], {})).toEqual([])
  })
})
