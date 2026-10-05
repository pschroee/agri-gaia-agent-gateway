import { useCallback, useEffect, useMemo, useState } from "react"
import { toast } from "sonner"
import { ApiError, api, urls } from "@/api/client"
import type {
  Approval,
  Artifact,
  BackgroundTask,
  Chat,
  Command,
  LLMCall,
  QueueEntry,
  SocketCall,
  SubagentEntry,
  SubagentRunMeta,
  ToolExecutionRecord,
} from "@/api/types"
import { applyBackgroundEvent } from "@/lib/background"
import { autoHeldText } from "@/lib/queue"
import { parseServerEvent, upsert } from "@/lib/events"
import { reconnectDelay } from "@/lib/reconnect"
import type { LocalQueued } from "@/lib/queue"
import { limitErrorKind, mergeSubagentEntries } from "@/lib/subagents"
import {
  addPending,
  applyCompactionError,
  applyPiEvent,
  applyQueueDelivered,
  applyResumeStep,
  applyUserMeta,
  dropPending,
  failPending,
  compactionNotice,
  emptyTranscript,
  hydrate,
  isCompactionError,
  type TranscriptState,
} from "@/lib/stream"

export type ChatStreamState = {
  chat?: Chat
  transcript: TranscriptState
  artifacts: Artifact[]
  approvals: Approval[]
  socketCalls: SocketCall[]
  subagentEntries: SubagentEntry[]
  /** Name und Zustand je Lauf laut pi-subagents. */
  subagentRuns: SubagentRunMeta[]
  llmCalls: LLMCall[]
  /** Vom Orchestrator ausgeführte Werkzeugoperationen (E9). */
  toolExecs: ToolExecutionRecord[]
  commands: Command[]
  /** Eingereihte Nachrichten laut Server (Warteschlange). */
  queue: QueueEntry[]
  /** Eingereiht, Antwort des Servers steht noch aus. */
  localQueue: LocalQueued[]
  /** Hintergrundaufgaben (bash mit run_in_background). */
  background: BackgroundTask[]
  loadError?: string
  connected: boolean
}

const initial = (): ChatStreamState => ({
  transcript: emptyTranscript(),
  artifacts: [],
  approvals: [],
  socketCalls: [],
  subagentEntries: [],
  subagentRuns: [],
  llmCalls: [],
  toolExecs: [],
  commands: [],
  queue: [],
  localQueue: [],
  background: [],
  connected: false,
})

const artifactKey = (a: Artifact) => `${a.kind}/${a.name}`

/**
 * Lädt einen Chat und hält ihn über SSE aktuell. Nach einem Verbindungsabbruch wird neu verbunden
 * und der Chat danach einmal vollständig neu geladen. Beim Wechsel des Chats die aufrufende
 * Komponente per `key` neu anlegen.
 */
export function useChatStream(chatId: string) {
  const [state, setState] = useState<ChatStreamState>(initial)

  const load = useCallback(async () => {
    try {
      const d = await api.chat(chatId)
      setState((s) => ({
        ...s,
        chat: d.chat,
        transcript: hydrate(s.transcript, d.messages ?? []),
        artifacts: d.artifacts ?? [],
        approvals: d.approvals ?? [],
        socketCalls: d.socket_calls ?? [],
        // Live eingetroffene Einträge behalten, falls die Antwort sie noch nicht kennt; der Server gewinnt
        subagentEntries: mergeSubagentEntries(s.subagentEntries, d.subagent_entries ?? []),
        subagentRuns: (d.subagent_runs ?? []).reduce((acc, r) => upsert(acc, r, (x) => x.run_id), s.subagentRuns),
        queue: d.queue ?? [],
        background: d.background ?? [],
        loadError: undefined,
      }))
    } catch (e) {
      setState((s) => ({ ...s, loadError: e instanceof Error ? e.message : String(e) }))
    }
    try {
      const calls = await api.llmCalls(chatId)
      setState((s) => ({
        ...s,
        llmCalls: (Array.isArray(calls) ? calls : []).reduce((acc, c) => upsert(acc, c, (x) => x.id), s.llmCalls),
      }))
    } catch {
      // Modellaufrufe sind eine Zusatzansicht; ohne sie bleibt der Reiter leer
    }
    try {
      const r = await api.toolExecutions(chatId)
      const list = Array.isArray(r?.executions) ? r.executions : []
      setState((s) => ({ ...s, toolExecs: list.reduce((acc, e) => upsert(acc, e, (x) => x.id), s.toolExecs) }))
    } catch {
      // ohne Protokoll fehlen nur die Belege
    }
  }, [chatId])

  const loadCommands = useCallback(async () => {
    try {
      const commands = await api.commands(chatId)
      setState((s) => ({ ...s, commands: Array.isArray(commands) ? commands : [] }))
    } catch {
      // Befehlsliste ist optional; ohne sie gibt es nur kein Popover
    }
  }, [chatId])

  useEffect(() => {
    let es: EventSource | null = null
    let retry: ReturnType<typeof setTimeout> | undefined
    let stopped = false
    let reconnecting = false
    let attempt = 0

    // setState geschieht erst nach dem Abruf, nicht synchron im Effekt
    // oxlint-disable-next-line react/set-state-in-effect
    void load()
    void loadCommands()

    const connect = () => {
      if (stopped) return
      es = new EventSource(urls.events(chatId))
      es.onopen = () => {
        attempt = 0
        setState((s) => ({ ...s, connected: true }))
        if (reconnecting) {
          reconnecting = false
          void load()
        }
      }
      es.onmessage = (msg: MessageEvent<string>) => {
        const ev = parseServerEvent(msg.data)
        if (!ev) return
        switch (ev.kind) {
          case "pi": {
            // Empfangszeit außerhalb des Updaters, damit ein doppelter Aufruf (StrictMode) dieselbe Zeit sieht
            const received = Date.now()
            setState((s) => ({ ...s, transcript: applyPiEvent(s.transcript, ev.data, received) }))
            // Danach einmal neu laden: Kosten und Tarif je Antwort (cost/peak) und gespeicherte
            // Kompaktierungen gibt es nur in der Historie, nicht in den Live-Ereignissen.
            if (ev.data.type === "agent_settled" || ev.data.type === "compaction_end") {
              void load()
              if (ev.data.type === "agent_settled") void loadCommands()
            }
            break
          }
          case "chat":
            if (ev.data.id === chatId) setState((s) => ({ ...s, chat: ev.data }))
            break
          case "approval":
            setState((s) => ({ ...s, approvals: upsert(s.approvals, ev.data, (a) => a.id) }))
            break
          case "artifact":
            setState((s) => ({ ...s, artifacts: upsert(s.artifacts, ev.data, artifactKey) }))
            break
          case "socket_call":
            setState((s) => ({ ...s, socketCalls: upsert(s.socketCalls, ev.data, (c) => c.id) }))
            break
          case "llm_call":
            setState((s) => ({ ...s, llmCalls: upsert(s.llmCalls, ev.data, (c) => c.id) }))
            break
          case "subagent":
            setState((s) => ({ ...s, subagentEntries: mergeSubagentEntries(s.subagentEntries, [ev.data]) }))
            break
          case "subagent_run":
            setState((s) => ({ ...s, subagentRuns: upsert(s.subagentRuns, ev.data, (r) => r.run_id) }))
            break
          case "tool_execution":
            setState((s) => ({ ...s, toolExecs: upsert(s.toolExecs, ev.data, (e) => e.id) }))
            break
          case "background":
            setState((s) => ({ ...s, background: applyBackgroundEvent(s.background, ev.data) }))
            break
          case "user_meta":
            setState((s) => ({ ...s, transcript: applyUserMeta(s.transcript, ev.data) }))
            break
          case "auto_held":
            toast.warning(autoHeldText(ev.data))
            break
          case "resume":
            setState((s) => ({ ...s, transcript: applyResumeStep(s.transcript, ev.data) }))
            break
          case "queue": {
            const q = ev.data
            setState((s) => ({
              ...s,
              queue: Array.isArray(q.entries) ? q.entries : [],
              transcript:
                q.change === "delivered" && q.text
                  ? applyQueueDelivered(s.transcript, `queue-${q.ids?.[0] ?? Date.now()}`, q.text, q)
                  : q.change === "restored" && q.ids?.length
                    ? // eingeschleust, aber von pi nicht eingefügt (Abbruch): steht wieder in der Warteschlange
                      dropPending(s.transcript, `queue-${q.ids[0]}`)
                    : s.transcript,
            }))
            break
          }
          case "error": {
            const message = ev.data?.message
            if (limitErrorKind(message)) {
              // Der Hinweis steht im Verlauf (aus dem Protokolleintrag agent_limit/subagent_limit);
              // neu laden, falls das Ereignis socket_call verpasst wurde.
              toast.warning(message)
              void load()
            } else if (isCompactionError(message)) {
              // Der Hinweis steht als Trennlinie im Verlauf; der Toast macht nur darauf aufmerksam.
              setState((s) => ({ ...s, transcript: applyCompactionError(s.transcript, message!) }))
              toast.warning(compactionNotice(message))
            } else if (message?.startsWith("Arbeitsbereich nicht gesichert")) {
              // Über der Größengrenze: Hinweis, kein Fehler; der Stand steht im Seitenreiter.
              toast.warning(message)
            } else {
              toast.error(message ?? "Unbekannter Fehler")
            }
            break
          }
        }
      }
      es.onerror = () => {
        es?.close()
        es = null
        reconnecting = true
        setState((s) => ({ ...s, connected: false }))
        if (stopped) return
        // Gibt es den Chat nicht mehr (etwa nach ./dev.sh reset), nicht endlos neu verbinden.
        api.chat(chatId).then(
          () => {
            if (!stopped) retry = setTimeout(connect, reconnectDelay(attempt++))
          },
          (e: unknown) => {
            if (e instanceof ApiError && e.status === 404) {
              stopped = true
              setState((s) => ({ ...s, chat: undefined, loadError: "Diesen Chat gibt es nicht mehr." }))
              return
            }
            if (!stopped) retry = setTimeout(connect, reconnectDelay(attempt++))
          },
        )
      }
    }
    connect()

    return () => {
      stopped = true
      if (retry) clearTimeout(retry)
      es?.close()
    }
  }, [chatId, load, loadCommands])

  const setChat = useCallback((chat: Chat) => setState((s) => ({ ...s, chat })), [])
  const mergeApproval = useCallback(
    (a: Approval) => setState((s) => ({ ...s, approvals: upsert(s.approvals, a, (x) => x.id) })),
    [],
  )
  const mergeArtifacts = useCallback(
    (list: Artifact[]) =>
      setState((s) => ({ ...s, artifacts: list.reduce((acc, a) => upsert(acc, a, artifactKey), s.artifacts) })),
    [],
  )

  // Gesendete Nachrichten und Warteschlange (optimistische Anzeige, siehe lib/stream, lib/queue)
  const outbox = useMemo(
    () => ({
      addPending: (key: string, text: string) =>
        setState((s) => ({ ...s, transcript: addPending(s.transcript, key, text) })),
      failPending: (key: string) => setState((s) => ({ ...s, transcript: failPending(s.transcript, key) })),
      dropPending: (key: string) => setState((s) => ({ ...s, transcript: dropPending(s.transcript, key) })),
      addLocalQueued: (l: LocalQueued) => setState((s) => ({ ...s, localQueue: [...s.localQueue, l] })),
      dropLocalQueued: (key: string) =>
        setState((s) => ({ ...s, localQueue: s.localQueue.filter((l) => l.key !== key) })),
      setQueue: (queue: QueueEntry[]) => setState((s) => ({ ...s, queue })),
    }),
    [],
  )

  return { ...state, reload: load, setChat, mergeApproval, mergeArtifacts, outbox }
}
