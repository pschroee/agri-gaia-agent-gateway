import { GlobeIcon, ShieldAlertIcon } from "lucide-react"
import type { Chat } from "@/api/types"
import { ChatStateBadge, RunningIndicator, VariantBadge } from "@/components/badges"
import { ContextBadge } from "@/components/ContextMeter"
import { chatHref } from "@/hooks/useHashRoute"
import { subagentLimitLabel } from "@/lib/subagents"
import { cn } from "@/lib/utils"

type Props = {
  chats: Chat[]
  selectedId?: string
  modelName: (id: string) => string
  /** Beginn des laufenden Durchgangs je Chat (ms), sofern bekannt; sonst „Läuft“ ohne Zeit. */
  runSince?: (c: Chat) => number | undefined
}

export function ChatList({ chats, selectedId, modelName, runSince }: Props) {
  if (chats.length === 0) return <p className="p-3 text-sm text-muted-foreground">Noch keine Chats.</p>
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
              <span className="truncate font-medium">{c.title || "Ohne Titel"}</span>
              {c.internet && <GlobeIcon className="size-3.5 shrink-0 text-sky-600" aria-label="Internetzugang an" />}
              {c.pending_approvals > 0 && (
                <ShieldAlertIcon className="size-3.5 shrink-0 text-amber-600" aria-label="offene Bestätigung" />
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
                <span
                  className={cn("ml-auto shrink-0 tabular-nums", (c.subagents ?? 0) > c.max_subagents && "text-red-700")}
                  title="gestartete / erlaubte Subagenten"
                >
                  {subagentLimitLabel(c)}
                </span>
              )}
            </div>
          </a>
        </li>
      ))}
    </ul>
  )
}
