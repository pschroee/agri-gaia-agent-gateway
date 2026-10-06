import { GlobeIcon, ShieldAlertIcon } from "lucide-react"
import type { Chat } from "@/api/types"
import { ChatStateBadge, RunningIndicator, VariantBadge } from "@/components/badges"
import { ContextBadge } from "@/components/ContextMeter"
import { chatHref } from "@/hooks/useHashRoute"
import { subagentsRunningLabel, subagentsRunningTitle } from "@/lib/subagents"
import { cn } from "@/lib/utils"

type Props = {
  chats: Chat[]
  selectedId?: string
  modelName: (id: string) => string
  /** Start of the running turn per chat (ms), if known; otherwise "Running" without a time. */
  runSince?: (c: Chat) => number | undefined
}

export function ChatList({ chats, selectedId, modelName, runSince }: Props) {
  if (chats.length === 0) return <p className="p-3 text-sm text-muted-foreground">No chats yet.</p>
  return (
    <ul className="flex flex-col gap-1 p-2">
      {chats.map((c) => (
        <li key={c.id}>
          <a
            href={chatHref(c.id)}
            className={cn(
              "block rounded-md border border-transparent px-2.5 py-2 text-sm hover:bg-muted",
              c.id === selectedId && "border-border bg-muted",
            )}
          >
            <div className="flex items-center gap-1.5">
              <span className="truncate font-medium">{c.title || "Untitled"}</span>
              {c.internet && <GlobeIcon className="size-3.5 shrink-0 text-sky-600" aria-label="Internet access on" />}
              {c.pending_approvals > 0 && (
                <ShieldAlertIcon className="size-3.5 shrink-0 text-amber-600" aria-label="pending approval" />
              )}
            </div>
            <div className="mt-1 flex flex-wrap items-center gap-1.5">
              <ChatStateBadge state={c.state} resuming={c.resuming} />
              <VariantBadge variant={c.variant} />
              {c.running && <RunningIndicator since={runSince?.(c)} />}
              <ContextBadge context={c.context} className="ml-auto" />
            </div>
            <div className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
              <span className="min-w-0 truncate">{modelName(c.model)}</span>
              {c.max_subagents !== undefined && (
                <span className="ml-auto shrink-0 tabular-nums" title={subagentsRunningTitle(c)}>
                  {subagentsRunningLabel(c)}
                </span>
              )}
            </div>
          </a>
        </li>
      ))}
    </ul>
  )
}
