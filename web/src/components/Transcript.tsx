import { Fragment, useLayoutEffect, useMemo, useRef, useState } from "react"
import { BrainIcon, ChevronRightIcon, Loader2Icon, ShieldAlertIcon, ShrinkIcon, PaperclipIcon } from "lucide-react"
import type { Artifact, BackgroundTask, TurnTrigger } from "@/api/types"
import { urls } from "@/api/client"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Markdown } from "@/components/Markdown"
import { ResumeBlock } from "@/components/ResumeBlock"
import { ImagePreview } from "@/components/ImagePreview"
import { SubagentRunView } from "@/components/Subagents"
import { PiNoticeLine, SystemNoteLine } from "@/components/SystemNote"
import { TodoCallGroup } from "@/components/Tasks"
import { ToolCallCard } from "@/components/ToolCallCard"
import { ArtifactAttachments } from "@/components/ArtifactAttachments"
import { subagentHref } from "@/hooks/useHashRoute"
import { useNow } from "@/hooks/useNow"
import { artifactsOfCall } from "@/lib/artifactPreview"
import { isPreviewImage, splitAttachments } from "@/lib/attachments"
import { cacheHitRate, formatPercent } from "@/lib/context"
import type { Evidence } from "@/lib/evidence"
import { formatTime, formatTokens, isUserAbort, formatUsage, formatUsd, hasThinkingText } from "@/lib/format"
import { formatElapsed, formatStepDuration, liveActivity, runStartOf } from "@/lib/runtime"
import { type AssistantItem, awaitingAnswer, type CompactionItem, compactionNotice, type TranscriptState } from "@/lib/stream"
import { type LimitNotice, placeAfter, type RunAssignment, type SubagentRun } from "@/lib/subagents"
import { type MessagePart, splitMessage } from "@/lib/systemnote"
import { groupTodoBlocks, type TodoTimeline, todoTimeline } from "@/lib/tasks"
import { cn } from "@/lib/utils"

/** Subagenten-Läufe, zugeordnet zu den subagent-Aufrufen des Hauptagenten. */
export type TranscriptSubagents = {
  runs: Map<string, SubagentRun>
  assignment: RunAssignment
  proxyIds: Set<string>
  /** Abgleich der Werkzeugaufrufe mit dem Protokoll des Orchestrators (E9). */
  evidence?: Map<string, Evidence>
  /** Der Chat arbeitet nicht; fehlende Belege gelten dann als auffällig. */
  settled?: boolean
}

type Props = {
  transcript: TranscriptState
  /** Für Download-Links der Anhänge. */
  chatId?: string
  subagents?: TranscriptSubagents
  /** Hinweise zu Grenzen (agent_limit, subagent_limit), nach Zeit einsortiert. */
  notices?: LimitNotice[]
  /** Wird unter dem Verlauf angezeigt, etwa offene Bestätigungen. */
  footer?: React.ReactNode
  /** Der Agent arbeitet (für „Denkt …“, solange noch keine Antwort streamt). */
  working?: boolean
  /** Hintergrundaufgaben: Karten von bash mit run_in_background und Meldungen im Verlauf. */
  background?: BackgroundTask[]
  /** Öffnet den Reiter „Hintergrund“, optional mit einer Aufgabe im Blick. */
  onOpenBackground?: (id?: string) => void
  /** Artefakte des Chats; Ergebnisse erscheinen unter dem Werkzeugaufruf, der sie hochgeladen hat. */
  artifacts?: Artifact[]
}

const noTasks: BackgroundTask[] = []

export function Transcript({
  transcript,
  chatId,
  subagents,
  notices,
  footer,
  working,
  background = noTasks,
  onOpenBackground,
  artifacts,
}: Props) {
  const ref = useRef<HTMLDivElement>(null)
  const [stick, setStick] = useState(true)

  const onScroll = () => {
    const el = ref.current
    if (!el) return
    setStick(el.scrollHeight - el.scrollTop - el.clientHeight < 48)
  }

  useLayoutEffect(() => {
    const el = ref.current
    if (el && stick) el.scrollTop = el.scrollHeight
  })

  // Lose Subagenten-Läufe und Grenzhinweise hinter dem passenden Eintrag einsetzen
  const extras = useMemo(() => {
    const m = new Map<number, React.ReactNode[]>()
    const add = (idx: number, node: React.ReactNode) => {
      const l = m.get(idx)
      if (l) l.push(node)
      else m.set(idx, [node])
    }
    if (subagents) {
      for (const [idx, ids] of Object.entries(subagents.assignment.loose)) {
        for (const id of ids) {
          const run = subagents.runs.get(id)
          if (run)
            add(
              Number(idx),
              <SubagentRunView
                key={`run-${id}`}
                run={run}
                proxyIds={subagents.proxyIds}
                href={chatId ? subagentHref(chatId, id) : undefined}
              />,
            )
        }
      }
    }
    for (const n of notices ?? []) add(placeAfter(transcript.items, n.time), <LimitNoticeView key={`notice-${n.id}`} notice={n} />)
    return m
  }, [subagents, notices, transcript.items, chatId])
  // Aufrufe der Aufgabenliste (todo) als kompakte Karten statt roher Werkzeugkarten
  const todo = useMemo(() => todoTimeline(transcript), [transcript])
  // Meldungen des Orchestrators in Nutzernachrichten, zerlegt nach der Herkunft laut Server
  const userParts = useMemo(() => {
    const m = new Map<string, MessagePart[]>()
    for (const it of transcript.items) {
      if (it.kind === "user") m.set(it.key, splitMessage(splitAttachments(it.text).text, it))
    }
    for (const p of transcript.pending) m.set(p.key, splitMessage(splitAttachments(p.text).text, p))
    return m
  }, [transcript.items, transcript.pending])

  return (
    <div ref={ref} onScroll={onScroll} className="min-h-0 flex-1 overflow-y-auto">
      <div className="mx-auto flex max-w-3xl flex-col gap-4 px-4 py-4">
        {transcript.items.length === 0 && transcript.pending.length === 0 && !transcript.resume && (
          <p className="py-8 text-center text-sm text-muted-foreground">Noch keine Nachrichten.</p>
        )}
        {extras.get(-1)}
        {transcript.items.map((item, idx) => (
          <Fragment key={item.key}>
            {item.kind === "user" ? (
              <UserMessage
                text={item.text}
                chatId={chatId}
                parts={userParts.get(item.key)}
                trigger={item.trigger}
                onOpenBackground={onOpenBackground}
              />
            ) : item.kind === "notice" ? (
              <PiNoticeLine text={item.text} customType={item.customType} trigger={item.trigger} />
            ) : item.kind === "compaction" ? (
              <CompactionDivider item={item} />
            ) : item.kind === "resume" ? (
              <ResumeBlock item={item} />
            ) : (
              <AssistantMessage
                item={item}
                transcript={transcript}
                subagents={subagents}
                chatId={chatId}
                todo={todo}
                background={background}
                onOpenBackground={onOpenBackground}
                artifacts={artifacts}
              />
            )}
            {extras.get(idx)}
          </Fragment>
        ))}
        {transcript.pending.map((p) => (
          <UserMessage
            key={p.key}
            text={p.text}
            chatId={chatId}
            state={p.failed ? "failed" : "pending"}
            parts={userParts.get(p.key)}
            onOpenBackground={onOpenBackground}
          />
        ))}
        {transcript.resume && <ResumeBlock key={transcript.resume.key} item={transcript.resume} />}
        {!transcript.compactingKey &&
          transcript.resume?.state !== "running" &&
          (working || (transcript.pending.some((p) => !p.failed) && awaitingAnswer(transcript))) && (
            <ActivityLine transcript={transcript} since={runStartOf(transcript, !!working)} />
          )}
        {footer}
      </div>
    </div>
  )
}

function AssistantMessage({
  item,
  transcript,
  subagents,
  chatId,
  todo,
  background,
  onOpenBackground,
  artifacts,
}: {
  item: AssistantItem
  transcript: TranscriptState
  subagents?: TranscriptSubagents
  chatId?: string
  todo?: TodoTimeline
  background?: BackgroundTask[]
  onOpenBackground?: (id?: string) => void
  artifacts?: Artifact[]
}) {
  // Live-Antworten (ohne seq) haben noch keinen Tarifwert vom Orchestrator; pis Wert ist vorläufig.
  const provisional = item.cost === undefined && item.seq === undefined && !!item.usage?.cost?.total
  const usage = formatUsage(item.usage, item.cost, { provisional })
  const cache = item.usage ? cacheHitRate(item.usage.input, item.usage.cacheRead) : undefined
  const lastIdx = item.blocks.length - 1
  // Erster Block einer Folge von todo-Aufrufen → Indizes der Folge; die übrigen entfallen.
  const todoRuns = useMemo(() => {
    const m = new Map<number, number[]>()
    for (const g of groupTodoBlocks(item.blocks)) if (g.kind === "todo") m.set(g.indices[0], g.indices)
    return m
  }, [item.blocks])
  return (
    <div className="flex max-w-full min-w-0 flex-col gap-2 text-sm">
      {item.blocks.map((b, i) => {
        switch (b.type) {
          case "text":
            return b.text ? (
              <div key={i} className="min-w-0">
                <Markdown
                  text={b.text}
                  chatId={chatId}
                  msgId={item.streaming ? undefined : item.msgKey}
                  streaming={item.streaming}
                />
                {item.streaming && i === lastIdx && <Caret />}
              </div>
            ) : null
          case "thinking":
            return <ThinkingBlock key={i} text={b.thinking} live={item.streaming && i === lastIdx} />
          case "toolCall": {
            if (todo && b.name === "todo" && b.id && todo.calls[b.id]) {
              const run = todoRuns.get(i)
              if (!run) return null
              const calls = run.flatMap((j) => {
                const tb = item.blocks[j]
                const c = tb.type === "toolCall" && tb.id ? todo.calls[tb.id] : undefined
                return c ? [c] : []
              })
              return <TodoCallGroup key={b.id} calls={calls} />
            }
            const runs = b.id && subagents ? (subagents.assignment.byTool[b.id] ?? []) : []
            const card = (
              <>
                <ToolCallCard
                  block={b}
                  exec={b.id ? transcript.tools[b.id] : undefined}
                  evidence={b.id ? subagents?.evidence?.get(b.id) : undefined}
                  settled={subagents?.settled ?? true}
                  background={background}
                  onOpenBackground={onOpenBackground}
                  chatId={chatId}
                />
                {chatId && <ArtifactAttachments chatId={chatId} artifacts={artifactsOfCall(artifacts, b.id)} />}
              </>
            )
            if (runs.length === 0) return <Fragment key={b.id || i}>{card}</Fragment>
            return (
              <div key={b.id || i} className="flex flex-col gap-1.5">
                {card}
                <div className="ml-4 flex flex-col gap-1.5 border-l-2 border-violet-200 pl-3">
                  {runs.map((id) => {
                    const run = subagents!.runs.get(id)
                    return run ? (
                      <SubagentRunView
                        key={id}
                        run={run}
                        proxyIds={subagents!.proxyIds}
                        evidence={subagents!.evidence}
                        settled={subagents!.settled ?? true}
                        href={chatId ? subagentHref(chatId, id) : undefined}
                      />
                    ) : null
                  })}
                </div>
              </div>
            )
          }
          case "image":
            return (
              <div key={i} className="text-xs text-muted-foreground">
                [Bild]
              </div>
            )
          default:
            return null
        }
      })}
      {item.streaming && item.blocks.length === 0 && (
        <div className="text-muted-foreground">
          <Caret />
        </div>
      )}
      {item.errorMessage && !isUserAbort(item) && (
        <div className="rounded border border-red-200 bg-red-50 px-2 py-1 text-xs text-red-800">{item.errorMessage}</div>
      )}
      {!item.streaming &&
        (usage || item.durationMs !== undefined || (item.stopReason && item.stopReason !== "stop" && item.stopReason !== "toolUse")) && (
        <div className="flex flex-wrap items-center gap-x-1 gap-y-0.5 text-[11px] text-muted-foreground/80">
          <span
            title={
              provisional
                ? "Vorläufig: pis Einheitspreis. Der Wert nach Tarif (Spitzen-/Nebenzeit) folgt, sobald die Antwort gespeichert ist."
                : undefined
            }
          >
            {usage}
          </span>
          {cache !== undefined && <span title="Cache-Trefferquote: Cache-Tokens / (Eingabe + Cache)">· Cache {formatPercent(cache * 100)}</span>}
          {item.durationMs !== undefined && (
            <span className="tabular-nums" title="Dauer des ganzen Laufs: von der Nachricht bis zu dieser Antwort">
              {usage ? "· " : ""}
              {formatStepDuration(item.durationMs)}
            </span>
          )}
          {item.peak !== undefined && (
            <span
              className={cn(
                "ml-1 rounded border px-1 py-px text-[10px] font-medium",
                item.peak ? "border-amber-200 bg-amber-50 text-amber-800" : "border-emerald-200 bg-emerald-50 text-emerald-800",
              )}
              title={item.peak ? "Antwort fiel in die Spitzenzeit (voller Preis)" : "Antwort fiel in die Nebenzeit (ermäßigter Preis)"}
            >
              {item.peak ? "Spitzentarif" : "Nebentarif"}
            </span>
          )}
          {isUserAbort(item) && <span> · abgebrochen</span>}
          {item.stopReason === "error" && !isUserAbort(item) && <span> · Fehler</span>}
          {item.stopReason === "length" && <span> · Längengrenze erreicht</span>}
        </div>
      )}
    </div>
  )
}

/**
 * Statuszeile unter dem Verlauf, solange der Agent arbeitet: was er tut und wie lange der Lauf schon dauert
 * („Führt bash aus … 1:05“). Ohne bekannten Start nur die Tätigkeit.
 */
function ActivityLine({ transcript, since }: { transcript: TranscriptState; since?: number }) {
  const now = useNow(1000, since !== undefined)
  return (
    <div className="flex min-w-0 items-center gap-1.5 text-xs text-muted-foreground" role="status">
      <Loader2Icon className="size-3 shrink-0 animate-spin" />
      <span className="truncate">{liveActivity(transcript)} …</span>
      {since !== undefined && <span className="shrink-0 tabular-nums">{formatElapsed(now - since)}</span>}
    </div>
  )
}

function ThinkingBlock({ text, live }: { text: string; live: boolean }) {
  const [open, setOpen] = useState(false)
  if (!hasThinkingText(text)) return null
  return (
    <Collapsible open={open} onOpenChange={setOpen} className="text-xs text-muted-foreground">
      <CollapsibleTrigger className="flex items-center gap-1 hover:text-foreground">
        <ChevronRightIcon className={cn("size-3 transition-transform", open && "rotate-90")} />
        <BrainIcon className="size-3" />
        {live ? "Denkt …" : "Gedanken"}
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="mt-1 border-l-2 pl-3 italic whitespace-pre-wrap">{text}</div>
      </CollapsibleContent>
    </Collapsible>
  )
}

function LimitNoticeView({ notice }: { notice: LimitNotice }) {
  const abort = notice.op === "subagent_limit"
  return (
    <div
      role="status"
      className={cn(
        "flex items-start gap-2 rounded-md border px-3 py-2 text-sm",
        abort ? "border-red-300 bg-red-50 text-red-900" : "border-amber-300 bg-amber-50 text-amber-950",
      )}
    >
      <ShieldAlertIcon className="mt-0.5 size-4 shrink-0" />
      <div className="min-w-0">
        <div className="font-medium">
          {notice.text}
          {notice.count > 1 && <span className="font-normal"> ({notice.count}×)</span>}
        </div>
        <div className="text-xs opacity-80">
          {abort
            ? "Der Orchestrator hat den Durchgang abgebrochen und die Subagenten-Prozesse beendet."
            : "Der LLM-Proxy hat weitere gleichzeitige Modellaufrufe mit HTTP 429 abgewiesen."}{" "}
          {formatTime(new Date(notice.time).toISOString())}
        </div>
      </div>
    </div>
  )
}

function Caret() {
  return <span className="ml-0.5 inline-block h-4 w-1.5 animate-pulse bg-foreground/60 align-text-bottom" />
}

const reasonText: Record<string, string> = { manual: "manuell", threshold: "automatisch", overflow: "automatisch, Überlauf" }

function CompactionDivider({ item }: { item: CompactionItem }) {
  const [open, setOpen] = useState(false)
  const reason = reasonText[item.reason] ?? item.reason
  const failed = item.failed || item.aborted || !!item.errorMessage
  let label: string
  if (item.running) label = `Kontext wird zusammengefasst … (${reason})`
  else if (item.aborted) label = `Kompaktierung abgebrochen (${reason})`
  else if (failed) label = compactionNotice(item.errorMessage)
  else {
    const before = item.tokensBefore !== undefined ? formatTokens(item.tokensBefore) : "?"
    const after = item.tokensAfter !== undefined ? ` → ca. ${formatTokens(item.tokensAfter)}` : ""
    label = `Kontext zusammengefasst: ${before}${after} Tokens (${reason})`
  }
  const cost = item.cost ?? item.usage?.cost?.total
  const hasDetails = !item.running && (!!item.summary || cost !== undefined)
  // „nicht möglich“/„nicht nötig“ ist ein Hinweis, kein Fehler: dezent statt rot
  const benign = failed && !item.aborted && /nicht (möglich|nötig)/.test(label)

  return (
    <Collapsible open={open} onOpenChange={setOpen} className="text-xs">
      {/* Titel am Rahmen, weil deaktivierte Knöpfe in manchen Browsern keinen Tooltip zeigen */}
      <div className="flex items-center gap-2" title={failed && item.errorMessage ? `pi: ${item.errorMessage}` : undefined}>
        <div className="h-px flex-1 bg-border" />
        <CollapsibleTrigger
          disabled={!hasDetails}
          className={cn(
            "flex max-w-[85%] items-center gap-1.5 rounded-full border px-2.5 py-1 text-center",
            benign
              ? "border-amber-200 bg-amber-50 text-amber-800"
              : failed
                ? "border-red-200 bg-red-50 text-red-800"
                : "bg-muted/60 text-muted-foreground",
            hasDetails && "hover:text-foreground",
          )}
        >
          {item.running ? <Loader2Icon className="size-3 shrink-0 animate-spin" /> : <ShrinkIcon className="size-3 shrink-0" />}
          <span className="min-w-0">{label}</span>
          {hasDetails && <ChevronRightIcon className={cn("size-3 shrink-0 transition-transform", open && "rotate-90")} />}
        </CollapsibleTrigger>
        <div className="h-px flex-1 bg-border" />
      </div>
      <CollapsibleContent>
        <div className="mt-2 rounded-md border bg-muted/30 p-3">
          {item.summary && <Markdown text={item.summary} />}
          {cost !== undefined && (
            <p className="mt-2 text-[11px] text-muted-foreground">Kosten der Zusammenfassung: {formatUsd(cost)}</p>
          )}
        </div>
      </CollapsibleContent>
    </Collapsible>
  )
}

/**
 * Nutzernachricht; Anhänge (fester Block am Ende, siehe lib/attachments) als Chips. `pending`: gesendet,
 * von pi noch nicht bestätigt; `failed`: nicht gesendet. `parts`: Zerlegung nach lib/systemnote (Herkunft
 * laut Server); Meldungen des Orchestrators erscheinen als graue Zeile statt in der Blase.
 */
function UserMessage({
  text,
  chatId,
  state,
  parts,
  trigger,
  onOpenBackground,
}: {
  text: string
  chatId?: string
  state?: "pending" | "failed"
  parts?: MessagePart[]
  trigger?: TurnTrigger
  onOpenBackground?: (id?: string) => void
}) {
  const { text: body, files } = splitAttachments(text)
  const hasSystem = !!parts?.some((p) => p.kind !== "user")
  // Ohne Systemanteil bleibt der Text genau wie gesendet (auch Leerzeilen).
  const segments: MessagePart[] = hasSystem ? parts! : body ? [{ kind: "user", text: body }] : []
  const lastUser = segments.reduce((acc, p, i) => (p.kind === "user" ? i : acc), -1)
  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      {segments.map((p, i) =>
        p.kind === "user" ? (
          <div
            key={i}
            className={cn("ml-auto flex max-w-[85%] min-w-0 flex-col items-end gap-1.5", state === "pending" && "opacity-80")}
            aria-busy={state === "pending" || undefined}
          >
            {i === lastUser && <AttachmentChips files={files} chatId={chatId} />}
            <div
              className={cn(
                "max-w-full rounded-2xl bg-primary px-3.5 py-2 text-sm break-words whitespace-pre-wrap text-primary-foreground",
                state === "failed" && "opacity-60 ring-2 ring-red-400 ring-offset-1 ring-offset-background",
              )}
            >
              {p.text}
            </div>
          </div>
        ) : (
          <SystemNoteLine key={i} part={p} trigger={trigger} onOpenBackground={onOpenBackground} />
        ),
      )}
      {lastUser < 0 && files.length > 0 && (
        <div className="ml-auto flex max-w-[85%] flex-col items-end">
          <AttachmentChips files={files} chatId={chatId} />
        </div>
      )}
      {state === "failed" && <span className="ml-auto text-xs text-red-700 dark:text-red-400">Nicht gesendet</span>}
    </div>
  )
}

function AttachmentChips({ files, chatId }: { files: string[]; chatId?: string }) {
  if (files.length === 0) return null
  return (
    <ul className="flex flex-wrap justify-end gap-1.5" aria-label="Anhänge">
      {files.map((f) => (
        <li key={f}>
          {chatId && isPreviewImage(f) ? (
            <ImagePreview
              src={urls.artifact(chatId, f, "input")}
              alt={f}
              label={`/workspace/inputs/${f}`}
              filename={f}
              thumbClassName="size-28 max-h-none rounded-2xl object-cover"
            />
          ) : (
            <a
              href={chatId ? urls.artifact(chatId, f, "input") : undefined}
              className="inline-flex max-w-72 items-center gap-1.5 rounded-md border bg-background px-2 py-1 text-xs hover:bg-muted"
              title={`/workspace/inputs/${f} – herunterladen`}
            >
              <PaperclipIcon className="size-3 shrink-0 text-muted-foreground" />
              <span className="truncate">{f}</span>
            </a>
          )}
        </li>
      ))}
    </ul>
  )
}
