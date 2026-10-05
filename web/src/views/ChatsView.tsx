import { useCallback, useState } from "react"
import { api } from "@/api/client"
import type { Chat } from "@/api/types"
import { cn } from "@/lib/utils"
import { ChatList } from "@/components/ChatList"
import { ChatPanel } from "@/components/ChatPanel"
import { NewChatDialog } from "@/components/NewChatDialog"
import { chatHref } from "@/hooks/useHashRoute"
import { modelName, useMeta } from "@/hooks/useMeta"
import { usePolling } from "@/hooks/usePolling"
import { chatRunSince } from "@/lib/runtime"

type Props = { chatId?: string; runId?: string; meta: ReturnType<typeof useMeta>; embed?: boolean }

export function ChatsView({ chatId, runId, meta, embed }: Props) {
  const chats = usePolling(api.chats, 3000)
  // Start of the run in the open chat (from its live events), for the run time in the list
  const [openRun, setOpenRun] = useState<{ id: string; since?: number }>()
  const onRunSince = useCallback((since: number | undefined) => chatId && setOpenRun({ id: chatId, since }), [chatId])
  const runSince = (c: Chat) => chatRunSince(c) ?? (c.running && openRun?.id === c.id ? openRun.since : undefined)
  const name = (id: string) => modelName(meta.models, id)

  const onCreated = (c: Chat) => {
    void chats.reload()
    window.location.hash = chatHref(c.id)
  }

  if (embed) {
    // Side panel of the platform (~420 px): chat list as a select, below it the chat at full width.
    const list = chats.data ?? []
    return (
      <div className="flex min-h-0 min-w-0 flex-1 flex-col">
        <div className="flex items-center gap-2 border-b p-2">
          <select
            className="h-8 min-w-0 flex-1 rounded-md border bg-background px-2 text-sm"
            aria-label="Choose chat"
            value={chatId ?? ""}
            onChange={(e) => (window.location.hash = e.target.value ? chatHref(e.target.value) : "#/chats")}
          >
            <option value="">{list.length ? "Choose chat …" : "No chats yet"}</option>
            {chatId && !list.some((c) => c.id === chatId) && <option value={chatId}>(current chat)</option>}
            {list.map((c) => (
              <option key={c.id} value={c.id}>
                {(c.pending_approvals > 0 ? "● " : "") + (c.title || "Untitled")}
              </option>
            ))}
          </select>
          <NewChatDialog meta={meta} onCreated={onCreated} compact />
        </div>
        {chats.error && <p className="px-3 pt-2 text-xs text-red-700">Chats unreachable: {chats.error}</p>}
        {chatId ? (
          <ChatPanel
            key={chatId}
            chatId={chatId}
            runId={runId}
            config={meta.config}
            modelName={name}
            onChanged={() => void chats.reload()}
            onRunSince={onRunSince}
            embed
          />
        ) : (
          <div className="flex flex-1 items-center justify-center p-6 text-center text-sm text-muted-foreground">
            Choose a chat above or create a new one.
          </div>
        )}
      </div>
    )
  }

  return (
    <div className="flex min-h-0 min-w-0 flex-1">
      {/* Below md: either the list (no chat chosen) or the chat with a back button */}
      <aside
        className={cn(
          "min-w-0 flex-col md:flex md:w-72 md:shrink-0 md:border-r",
          chatId ? "hidden" : "flex w-full",
        )}
      >
        <div className="border-b p-2">
          <NewChatDialog meta={meta} onCreated={onCreated} />
        </div>
        {chats.error && <p className="px-3 pt-2 text-xs text-red-700">Chats unreachable: {chats.error}</p>}
        <div className="min-h-0 flex-1 overflow-y-auto">
          <ChatList chats={chats.data ?? []} selectedId={chatId} modelName={name} runSince={runSince} />
        </div>
      </aside>
      {chatId ? (
        <ChatPanel
          key={chatId}
          chatId={chatId}
          runId={runId}
          config={meta.config}
          modelName={name}
          onChanged={() => void chats.reload()}
          onRunSince={onRunSince}
        />
      ) : (
        <div className="hidden flex-1 items-center justify-center p-6 text-sm text-muted-foreground md:flex">
          Choose a chat on the left or create a new one.
        </div>
      )}
    </div>
  )
}
