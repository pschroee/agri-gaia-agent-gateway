import { FileUpIcon, GlobeIcon } from "lucide-react"
import { useEffect, useMemo, useState } from "react"
import { api } from "@/api/client"
import type { Approval } from "@/api/types"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { formatTime } from "@/lib/format"
import { pendingMenuItems } from "@/lib/pending"
import { cn } from "@/lib/utils"

/**
 * "Pending approvals" badge in the header. A click opens a list; an entry leads to the chat
 * in which the approval is due (the approval card is shown there).
 */
export function PendingApprovalsMenu({ approvals }: { approvals: Approval[] }) {
  const [open, setOpen] = useState(false)
  const [titles, setTitles] = useState<Record<string, string>>({})
  const pendingCount = approvals.length
  const internetCount = approvals.filter((a) => a.kind === "internet_access").length

  // Load chat titles only when opening; the list of approvals only knows IDs.
  useEffect(() => {
    if (!open) return
    let cancelled = false
    api
      .chats()
      .then((cs) => {
        if (!cancelled) setTitles(Object.fromEntries(cs.map((c) => [c.id, c.title])))
      })
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [open])

  const items = useMemo(() => pendingMenuItems(approvals, titles), [approvals, titles])

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        className={cn(
          "ml-auto inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs outline-none focus-visible:ring-2 focus-visible:ring-ring",
          pendingCount > 0 ? "border-amber-300 bg-amber-100 font-medium text-amber-900" : "text-muted-foreground",
        )}
        aria-label={`Pending approvals: ${pendingCount}`}
        title={
          internetCount > 0
            ? `Pending approvals of all chats, ${internetCount} of them request${internetCount === 1 ? "" : "s"} for internet access`
            : "Pending approvals of all chats"
        }
      >
        {internetCount > 0 && <GlobeIcon className="size-3.5 text-sky-700" aria-hidden />}
        <span className="hidden sm:inline">Pending approvals</span>
        <span className="sm:hidden">Pending</span>
        <span className="tabular-nums">{pendingCount}</span>
      </PopoverTrigger>
      <PopoverContent align="end" collisionPadding={12} className="w-[min(24rem,calc(100vw-1.5rem))] p-0">
        <div className="border-b px-3 py-2 text-sm font-medium">
          {pendingCount === 0 ? "No pending approvals" : `${pendingCount} pending approval${pendingCount === 1 ? "" : "s"}`}
        </div>
        {items.length > 0 && (
          <ul className="max-h-[60vh] overflow-y-auto py-1" role="menu">
            {items.map((i) => (
              <li key={i.id} role="none">
                <a
                  role="menuitem"
                  href={i.href}
                  onClick={() => setOpen(false)}
                  className="flex gap-2.5 px-3 py-2 text-left text-sm outline-none hover:bg-muted focus-visible:bg-muted"
                >
                  {i.internet ? (
                    <GlobeIcon className="mt-0.5 size-4 shrink-0 text-sky-700" aria-hidden />
                  ) : (
                    <FileUpIcon className="mt-0.5 size-4 shrink-0 text-amber-700" aria-hidden />
                  )}
                  <span className="min-w-0 flex-1">
                    <span className="flex items-baseline justify-between gap-2">
                      <span className="truncate font-medium">{i.chatTitle}</span>
                      <span className="shrink-0 text-xs text-muted-foreground">{formatTime(i.createdAt)}</span>
                    </span>
                    <span className="block text-xs text-muted-foreground">{i.label}</span>
                    <span className="block truncate text-xs">{i.detail}</span>
                  </span>
                </a>
              </li>
            ))}
          </ul>
        )}
        <a href="#/status" onClick={() => setOpen(false)} className="block border-t px-3 py-2 text-xs text-muted-foreground hover:bg-muted">
          All on the status page
        </a>
      </PopoverContent>
    </Popover>
  )
}
