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

type Props = { chatId?: string; runId?: string; meta: ReturnType<typeof useMeta> }

export function ChatsView({ chatId, runId, meta }: Props) {
  const chats = usePolling(api.chats, 3000)
  // Beginn des Laufs im geöffneten Chat (aus dessen Live-Ereignissen), für die Laufzeit in der Liste
  const [openRun, setOpenRun] = useState<{ id: string; since?: number }>()
  const onRunSince = useCallback((since: number | undefined) => chatId && setOpenRun({ id: chatId, since }), [chatId])
  const runSince = (c: Chat) => chatRunSince(c) ?? (c.running && openRun?.id === c.id ? openRun.since : undefined)
  const name = (id: string) => modelName(meta.models, id)

  return (
    <div className="flex min-h-0 min-w-0 flex-1">
      {/* Unter md: entweder Liste (kein Chat gewählt) oder Chat mit Zurück-Knopf */}
      <aside
        className={cn(
          "min-w-0 flex-col md:flex md:w-72 md:shrink-0 md:border-r",
          chatId ? "hidden" : "flex w-full",
        )}
      >
        <div className="border-b p-2">
          <NewChatDialog
            meta={meta}
            onCreated={(c) => {
              void chats.reload()
              window.location.hash = chatHref(c.id)
            }}
          />
        </div>
        {chats.error && <p className="px-3 pt-2 text-xs text-red-700">Chats nicht erreichbar: {chats.error}</p>}
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
          Links einen Chat wählen oder einen neuen anlegen.
        </div>
      )}
    </div>
  )
}
