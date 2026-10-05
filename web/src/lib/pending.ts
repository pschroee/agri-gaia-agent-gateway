import type { Approval } from "@/api/types"
import { formatBytes } from "./format"

export type PendingItem = {
  id: string
  chatId: string
  chatTitle: string
  label: string
  detail: string
  createdAt: string
  href: string
  internet: boolean
}

const MAX_DETAIL = 80

/** Entries of the "Pending approvals" drop-down: only pending ones, oldest first. */
export function pendingMenuItems(approvals: Approval[], titles: Record<string, string>): PendingItem[] {
  return approvals
    .filter((a) => a.state === "pending")
    .sort((a, b) => a.created_at.localeCompare(b.created_at))
    .map((a) => {
      const internet = a.kind === "internet_access"
      const plattform = a.kind === "platform_write"
      let detail = internet || plattform ? a.name : `${a.name} · ${formatBytes(a.size)}`
      if (detail.length > MAX_DETAIL) detail = detail.slice(0, MAX_DETAIL - 1) + "…"
      return {
        id: a.id,
        chatId: a.chat_id,
        chatTitle: titles[a.chat_id] ?? `Chat ${a.chat_id.slice(0, 8)}`,
        label: internet ? "Internet access" : plattform ? "Platform call" : "Upload artifact",
        detail,
        createdAt: a.created_at,
        href: `#/chats/${a.chat_id}`,
        internet,
      }
    })
}
