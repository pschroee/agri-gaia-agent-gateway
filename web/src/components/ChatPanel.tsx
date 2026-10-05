import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import {
  ArrowLeftIcon,
  BellIcon,
  ArrowUpIcon,
  EllipsisIcon,
  FileIcon,
  LoaderCircleIcon,
  PlusIcon,
  BotIcon,
  FileTextIcon,
  FolderOpenIcon,
  Maximize2Icon,
  Minimize2Icon,
  GlobeIcon,
  ListOrderedIcon,
  MoonIcon,
  PaperclipIcon,
  SendHorizontalIcon,
  XIcon,
  SquareIcon,
  UploadIcon,
  WifiOffIcon,
} from "lucide-react"
import { toast } from "sonner"
import { ApiError, api, urls } from "@/api/client"
import type { Artifact, Chat, Command, Config } from "@/api/types"
import { ApprovalCard } from "@/components/ApprovalCard"
import { BackgroundCounter } from "@/components/BackgroundTasks"
import { ChatSidePanel } from "@/components/ChatSidePanel"
import { SubagentDetail } from "@/components/SubagentDetail"
import { SubagentBar, SubagentsMenu } from "@/components/SubagentOverview"
import { TasksMenu } from "@/components/Tasks"
import { ContextIndicator } from "@/components/ContextMeter"
import { SlashCommandPopover } from "@/components/SlashCommandPopover"
import { ContextTooLargeDialog, ModelEffortPicker } from "@/components/ModelSwitch"
import { ChatStateBadge, RunningIndicator, VariantBadge } from "@/components/badges"
import { NumberStepper } from "@/components/NumberStepper"
import { Transcript, type TranscriptSubagents } from "@/components/Transcript"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Label } from "@/components/ui/label"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { Switch } from "@/components/ui/switch"
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle, SheetTrigger } from "@/components/ui/sheet"
import { Textarea } from "@/components/ui/textarea"
import { useChatStream } from "@/hooks/useChatStream"
import { useFlag } from "@/hooks/useFlag"
import { useAgentTree } from "@/hooks/useAgentTree"
import { useModelSwitch } from "@/hooks/useModelSwitch"
import { useSlashCommands } from "@/hooks/useSlashCommands"
import { composerButtons, isPreviewImage, withAttachments } from "@/lib/attachments"
import { autoCompactSwitch, isBuiltinCommand, isSlashCommand, renameTitle, withLiveOptions } from "@/lib/commands"
import { reconcile, rejectionsFrom } from "@/lib/evidence"
import { formatBytes, formatTokens, formatUsd } from "@/lib/format"
import { answerCostSum, costSplit } from "@/lib/llmcalls"
import { contextTooLarge } from "@/lib/modelswitch"
import { expectQueued, holdReasonText, queuePreview, queueRows, type QueueRow } from "@/lib/queue"
import { runningCount } from "@/lib/background"
import { chatRunSince } from "@/lib/runtime"
import { assignRuns, groupRuns, limitNotices, subagentLimitLabel } from "@/lib/subagents"
import { todoTimeline } from "@/lib/tasks"
import { cn } from "@/lib/utils"

type Props = {
  chatId: string
  /** Offene Detailansicht eines Subagenten-Laufs; sonst der Hauptverlauf. */
  runId?: string
  config?: Config
  modelName: (id: string) => string
  onChanged: () => void
  /** Meldet den Beginn des laufenden Durchgangs (ms) für die Chatliste; undefined, wenn nichts läuft. */
  onRunSince?: (since: number | undefined) => void
}

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e))

type Outbox = ReturnType<typeof useChatStream>["outbox"]

export function ChatPanel({ chatId, runId, config, modelName, onChanged, onRunSince }: Props) {
  const s = useChatStream(chatId)
  const { chat } = s
  const runSince = chatRunSince(chat, s.transcript)
  useEffect(() => onRunSince?.(runSince), [onRunSince, runSince])
  const [dragging, setDragging] = useState(false)
  const dragDepth = useRef(0)
  const [uploading, setUploading] = useState(false)
  const [staged, setStaged] = useState<Artifact[]>([])
  const [sheetOpen, setSheetOpen] = useState(false)
  const [sideOpen, setSideOpen] = useFlag("seitenleiste", true)
  // Zähler im Chatkopf und Karten im Verlauf öffnen den Reiter „Hintergrund“ (n ändert sich je Klick,
  // id: Aufgabe im Blick, fresh: bis das Seitenblatt wieder zu ist, siehe ChatSidePanel).
  const [bgFocus, setBgFocus] = useState<{ n: number; id?: string; fresh: boolean }>({ n: 0, fresh: false })
  const openBackground = useCallback(
    (id?: string) => {
      setSideOpen(true)
      const narrow = !window.matchMedia("(min-width: 1024px)").matches
      if (narrow) setSheetOpen(true)
      setBgFocus((f) => ({ n: f.n + 1, id, fresh: narrow }))
    },
    [setSideOpen],
  )

  const runs = useMemo(() => groupRuns(s.subagentEntries, s.subagentRuns), [s.subagentEntries, s.subagentRuns])
  const agentRoot = useAgentTree({ chatTitle: chat?.title ?? "", chatRunning: !!chat?.running, runs, llmCalls: s.llmCalls })
  const proxyIds = useMemo(
    () => new Set(s.llmCalls.map((c) => c.response_id).filter((id): id is string => !!id)),
    [s.llmCalls],
  )
  // Abgleich angefordert (Proxy) ↔ ausgeführt (Orchestrator) je toolCallId (E9)
  const rejections = useMemo(() => rejectionsFrom(s.transcript.tools, s.subagentEntries), [s.transcript.tools, s.subagentEntries])
  const evidence = useMemo(
    () => reconcile(s.llmCalls, s.toolExecs, { executedTools: config?.executed_tools, rejections }),
    [s.llmCalls, s.toolExecs, config?.executed_tools, rejections],
  )
  const settled = !(chat?.running ?? false)
  const subagents = useMemo<TranscriptSubagents>(
    () => ({
      runs: new Map(runs.map((r) => [r.runId, r])),
      assignment: assignRuns(s.transcript.items, runs),
      proxyIds,
      evidence,
      settled,
    }),
    [runs, s.transcript.items, proxyIds, evidence, settled],
  )
  const notices = useMemo(() => limitNotices(s.socketCalls), [s.socketCalls])
  const answersCost = useMemo(() => answerCostSum(s.transcript.items), [s.transcript.items])
  const tasks = useMemo(() => todoTimeline(s.transcript).tasks, [s.transcript])
  const queue = useMemo(() => queueRows(s.queue, s.localQueue), [s.queue, s.localQueue])

  const upload = async (files: File[]) => {
    if (!chat || files.length === 0) return
    const maxMb = config?.artifact_max_mb
    if (maxMb) {
      const tooBig = files.filter((f) => f.size > maxMb * 1024 * 1024)
      if (tooBig.length) {
        toast.error(`Zu groß (höchstens ${maxMb} MB je Datei): ${tooBig.map((f) => f.name).join(", ")}`)
        files = files.filter((f) => !tooBig.includes(f))
        if (files.length === 0) return
      }
    }
    setUploading(true)
    try {
      const list = await api.uploadFiles(chat.id, files)
      s.mergeArtifacts(list)
      // Hochgeladene Dateien hängen an der Nachricht, die gerade geschrieben wird.
      setStaged((prev) => [...prev.filter((p) => !list.some((a) => a.name === p.name)), ...list])
    } catch (e) {
      toast.error(`Hochladen fehlgeschlagen: ${errText(e)}`)
    } finally {
      setUploading(false)
    }
  }

  const dropHandlers = {
    onDragEnter: (e: React.DragEvent) => {
      if (!e.dataTransfer.types.includes("Files")) return
      dragDepth.current++
      setDragging(true)
    },
    onDragLeave: () => {
      dragDepth.current = Math.max(0, dragDepth.current - 1)
      if (dragDepth.current === 0) setDragging(false)
    },
    onDragOver: (e: React.DragEvent) => {
      if (e.dataTransfer.types.includes("Files")) e.preventDefault()
    },
    onDrop: (e: React.DragEvent) => {
      e.preventDefault()
      dragDepth.current = 0
      setDragging(false)
      void upload(Array.from(e.dataTransfer.files))
    },
  }

  if (!chat) {
    return (
      <div className="flex flex-1 items-center justify-center p-6 text-sm text-muted-foreground">
        {s.loadError ? `Chat konnte nicht geladen werden: ${s.loadError}` : "Lade Chat …"}
      </div>
    )
  }

  const pending = s.approvals.filter((a) => a.state === "pending")

  return (
    <div className="relative flex min-h-0 min-w-0 flex-1" {...dropHandlers}>
      {dragging && (
        <div className="pointer-events-none absolute inset-2 z-20 flex flex-col items-center justify-center gap-2 rounded-xl border-2 border-dashed border-sky-400 bg-sky-50/90 text-sky-800">
          <UploadIcon className="size-8" />
          <p className="font-medium">Dateien hier ablegen</p>
          <p className="text-sm">Sie liegen danach in der Sandbox unter /workspace/inputs/</p>
        </div>
      )}
      <div className="flex min-h-0 min-w-0 flex-1 flex-col">
        <ChatHeader
          chat={chat}
          modelName={modelName}
          config={config}
          answersCost={answersCost}
          connected={s.connected}
          runSince={runSince}
          subagentsMenu={
            <SubagentsMenu
              chatId={chat.id}
              activeRunId={runId}
              tree={{ chatTitle: chat.title, chatRunning: chat.running, runs, llmCalls: s.llmCalls }}
            />
          }
          tasksMenu={
            <>
              <BackgroundCounter
                count={runningCount(s.background)}
                onOpen={() => openBackground()}
              />
              <TasksMenu tasks={tasks} />
            </>
          }
          onChat={(c) => {
            s.setChat(c)
            onChanged()
          }}
          sidePanel={
            <>
            <Button
              size="icon-sm"
              variant="ghost"
              className="hidden lg:inline-flex"
              title={sideOpen ? "Chat vergrößern (Seitenleiste ausblenden)" : "Chat verkleinern (Seitenleiste einblenden)"}
              aria-label={sideOpen ? "Seitenleiste ausblenden" : "Seitenleiste einblenden"}
              aria-pressed={!sideOpen}
              onClick={() => setSideOpen(!sideOpen)}
            >
              {sideOpen ? <Maximize2Icon /> : <Minimize2Icon />}
            </Button>
            <Sheet
              open={sheetOpen}
              onOpenChange={(o) => {
                setSheetOpen(o)
                if (!o) setBgFocus((f) => (f.fresh ? { ...f, fresh: false } : f))
              }}
            >
              <SheetTrigger asChild>
                <Button
                  size="sm"
                  variant="outline"
                  className="lg:hidden"
                  title="Artefakte, Socket-Protokoll, Subagenten und Modellaufrufe"
                  aria-label={`Artefakte (${s.artifacts.length}), Socket-Protokoll (${s.socketCalls.length}), Subagenten (${runs.length}) und Modellaufrufe (${s.llmCalls.length}) öffnen`}
                >
                  <FolderOpenIcon />
                  <span className="hidden sm:inline">Artefakte & Protokolle</span>
                  <span className="sm:hidden" aria-hidden>
                    {s.artifacts.length}/{s.socketCalls.length}
                  </span>
                </Button>
              </SheetTrigger>
              <SheetContent side="right" className="w-[90vw] gap-0 p-0 sm:max-w-sm">
                <SheetHeader className="border-b pr-10">
                  <SheetTitle>Artefakte & Protokolle</SheetTitle>
                  <SheetDescription className="sr-only">
                    Dateien, Socket-Aufrufe, Subagenten und Modellaufrufe dieses Chats
                  </SheetDescription>
                </SheetHeader>
                <div className="min-h-0 flex-1">
                  <ChatSidePanel
                    chatId={chat.id}
                    chatTitle={chat.title}
                    chatRunning={chat.running}
                    activeRunId={runId}
                    onNavigate={() => {
                      setSheetOpen(false)
                      setBgFocus((f) => (f.fresh ? { ...f, fresh: false } : f))
                    }}
                    artifacts={s.artifacts}
                    socketCalls={s.socketCalls}
                    delegation={chat.delegation}
                    runs={runs}
                    llmCalls={s.llmCalls}
                    evidence={evidence}
                    modelName={modelName}
                    workspace={chat.workspace}
                    tasks={tasks}
                    background={s.background}
                    backgroundFocus={bgFocus.n}
                    backgroundFocusId={bgFocus.id}
                    backgroundFocusFresh={bgFocus.fresh}
                  />
                </div>
              </SheetContent>
            </Sheet>
            </>
          }
        />
        {runId ? (
          <SubagentDetail
            chatId={chat.id}
            chatTitle={chat.title}
            chatRunning={chat.running}
            runId={runId}
            runs={runs}
            proxyIds={proxyIds}
            llmCalls={s.llmCalls}
            evidence={evidence}
            artifacts={s.artifacts}
            footer={
              // Offene Bestätigungen gelten dem ganzen Chat; auch hier entscheidbar, damit eine Frage
              // (etwa eines Subagenten) nicht unbemerkt wartet.
              pending.length > 0 && (
                <div className="flex max-h-[45vh] flex-col gap-2 overflow-y-auto border-t bg-background p-3">
                  {pending.map((a) => (
                    <ApprovalCard key={a.id} approval={a} onDecided={s.mergeApproval} runs={runs} />
                  ))}
                </div>
              )
            }
          />
        ) : (
          <>
            {runs.length > 0 && <SubagentBar chatId={chat.id} root={agentRoot} />}
            <Transcript
              transcript={s.transcript}
              chatId={chat.id}
              subagents={subagents}
              notices={notices}
              working={chat.running}
              background={s.background}
              onOpenBackground={openBackground}
              artifacts={s.artifacts}
              footer={
                pending.length > 0 && (
                  <div className="flex flex-col gap-2">
                    {pending.map((a) => (
                      <ApprovalCard key={a.id} approval={a} onDecided={s.mergeApproval} runs={runs} />
                    ))}
                  </div>
                )
              }
            />
            <Composer
              chat={chat}
              config={config}
              modelName={modelName}
              commands={s.commands}
              uploading={uploading}
              onUpload={upload}
              staged={staged}
              onUnstage={(name) => setStaged((prev) => prev.filter((a) => a.name !== name))}
              onSent={() => setStaged([])}
              onRestoreStaged={(list) =>
                setStaged((prev) => [...list.filter((a) => !prev.some((p) => p.name === a.name)), ...prev])
              }
              onChat={(c) => {
                s.setChat(c)
                onChanged()
              }}
              onActivity={onChanged}
              queue={queue}
              outbox={s.outbox}
            />
          </>
        )}
      </div>
      <aside className={cn("hidden w-80 shrink-0 border-l xl:w-96", sideOpen && "lg:block")}>
        <ChatSidePanel
          chatId={chat.id}
          chatTitle={chat.title}
          chatRunning={chat.running}
          activeRunId={runId}
          artifacts={s.artifacts}
          socketCalls={s.socketCalls}
                    delegation={chat.delegation}
          runs={runs}
          llmCalls={s.llmCalls}
          evidence={evidence}
          modelName={modelName}
          workspace={chat.workspace}
          tasks={tasks}
          background={s.background}
          backgroundFocus={bgFocus.n}
          backgroundFocusId={bgFocus.id}
        />
      </aside>
    </div>
  )
}

function ChatHeader({
  chat,
  modelName,
  config,
  answersCost,
  connected,
  runSince,
  subagentsMenu,
  tasksMenu,
  onChat,
  sidePanel,
}: {
  chat: Chat
  modelName: (id: string) => string
  config?: Config
  answersCost: number
  connected: boolean
  /** Beginn des laufenden Durchgangs (ms) für die Laufzeit neben „Läuft“. */
  runSince?: number
  /** Klappmenü der Subagenten neben dem Titel (nur mit Läufen sichtbar). */
  subagentsMenu: React.ReactNode
  /** Fortschritt der Aufgabenliste (nur mit Aufgaben sichtbar). */
  tasksMenu: React.ReactNode
  onChat: (c: Chat) => void
  sidePanel: React.ReactNode
}) {
  const [busy, setBusy] = useState<string>()
  const run = async (label: string, fn: () => Promise<Chat>) => {
    setBusy(label)
    try {
      onChat(await fn())
    } catch (e) {
      toast.error(`${label} fehlgeschlagen: ${errText(e)}`)
    } finally {
      setBusy(undefined)
    }
  }
  const compact = async () => {
    setBusy("Kompaktieren")
    try {
      await api.runCommand(chat.id, "/compact")
    } catch (e) {
      toast.error(
        e instanceof ApiError && e.status === 409
          ? "pi arbeitet gerade; kompaktieren geht erst danach."
          : `Kompaktieren fehlgeschlagen: ${errText(e)}`,
      )
    } finally {
      setBusy(undefined)
    }
  }

  return (
    <header className="border-b px-3 py-2.5 sm:px-4">
      <div className="flex flex-wrap items-center gap-2">
        <Button size="icon-sm" variant="ghost" className="md:hidden" asChild>
          <a href="#/chats" title="Zurück zur Chatliste" aria-label="Zurück zur Chatliste">
            <ArrowLeftIcon />
          </a>
        </Button>
        <h2 className="min-w-0 flex-1 basis-48 truncate text-base font-semibold sm:flex-none sm:basis-auto">{chat.title || "Ohne Titel"}</h2>
        {subagentsMenu}
        {tasksMenu}
        <ChatStateBadge state={chat.state} resuming={chat.resuming} />
        <VariantBadge variant={chat.variant} />
        {chat.running && <RunningIndicator since={runSince} />}
        {!connected && (
          <span className="inline-flex items-center gap-1 text-xs text-amber-700">
            <WifiOffIcon className="size-3.5" /> <span className="hidden sm:inline">Live-Verbindung getrennt, verbinde neu …</span>
            <span className="sm:hidden">getrennt</span>
          </span>
        )}
        <div className="ml-auto flex flex-wrap items-center gap-1.5">
          <ContextIndicator
            chat={chat}
            busy={busy}
            onAutoCompact={(v) => void run("Auto-Kompaktierung", () => api.setAutoCompact(chat.id, v))}
            onCompact={() => void compact()}
          />
          {sidePanel}
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button size="icon-sm" variant="ghost" disabled={!!busy} title="Weitere Aktionen" aria-label="Weitere Aktionen">
                <EllipsisIcon />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-60">
              {chat.state === "active" && (
                <DropdownMenuItem onSelect={() => void run("Ruhen lassen", () => api.suspend(chat.id))}>
                  <MoonIcon />
                  <div>
                    <div>Ruhen lassen</div>
                    <div className="text-xs text-muted-foreground">Sandbox freigeben, später fortsetzen</div>
                  </div>
                </DropdownMenuItem>
              )}
              <DropdownMenuItem asChild>
                <a href={urls.session(chat.id)} target="_blank" rel="noreferrer">
                  <FileTextIcon /> Sitzung als JSONL öffnen
                </a>
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>
      <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
        <span className="text-foreground">{modelName(chat.model)}</span>
        <span className="font-mono" title="Platz im Warm-Pool">
          {chat.slot_id ?? "kein Platz"}
        </span>
        <span
          title={`${formatTokens(chat.tokens?.input)} ein · ${formatTokens(chat.tokens?.output)} aus · ${formatTokens(chat.tokens?.cache_read)} aus dem Cache`}
        >
          <span className="text-foreground">{formatTokens(chat.tokens?.total)}</span> Tokens
        </span>
        <CostInfo chat={chat} answersCost={answersCost} />
        <SubagentLimit
          chat={chat}
          limit={config?.max_subagents_limit}
          busy={busy === "Subagenten-Grenze"}
          onChange={(max) => void run("Subagenten-Grenze", () => api.setMaxSubagents(chat.id, max))}
        />
        <span
          className={cn("inline-flex items-center gap-1.5 sm:ml-auto", chat.internet && "text-sky-700")}
          title={
            chat.internet
              ? "Internetzugang an. Sprachmodell und Orchestrator sind immer erreichbar."
              : "Internetzugang aus. Sprachmodell und Orchestrator sind trotzdem erreichbar; der Agent kann Internet erbitten."
          }
        >
          <GlobeIcon className="size-3" aria-hidden />
          <Label htmlFor={`internet-${chat.id}`} className="text-xs font-normal text-inherit">
            Internet
          </Label>
          <Switch
            id={`internet-${chat.id}`}
            size="sm"
            checked={chat.internet}
            disabled={busy === "Internet"}
            onCheckedChange={(v) => void run("Internet", () => api.setInternet(chat.id, v))}
          />
        </span>
      </div>
    </header>
  )
}

/** Gesamtkosten; die Aufteilung öffnet sich beim Klicken. */
function CostInfo({ chat, answersCost }: { chat: Chat; answersCost: number }) {
  const c = costSplit(chat)
  return (
    <Popover>
      <PopoverTrigger asChild>
        <button
          type="button"
          className="rounded underline decoration-dotted underline-offset-2 hover:text-foreground"
          aria-label="Kosten im Einzelnen"
        >
          Kosten: <span className="font-medium text-foreground">{formatUsd(c.total)}</span>
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" collisionPadding={12} className="w-80 max-w-[calc(100vw-1.5rem)] text-xs">
        <div className="mb-2 text-sm font-semibold">Kosten {formatUsd(c.total)}</div>
        <dl className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-3 gap-y-1 tabular-nums">
          <dt className="text-muted-foreground">Hauptantworten</dt>
          <dd className="text-right">{formatUsd(c.main)}</dd>
          <dt className="text-muted-foreground">davon außerhalb der Hauptantworten (Subagenten, Kompaktierung)</dt>
          <dd className="text-right">{formatUsd(c.other)}</dd>
          <dt className="text-muted-foreground">Modellaufrufe am Proxy</dt>
          <dd className="text-right">{c.calls}</dd>
          <dt className="text-muted-foreground">Summe der Antworten im Verlauf</dt>
          <dd className="text-right">{formatUsd(answersCost)}</dd>
        </dl>
        <p className="mt-2 border-t pt-2 text-muted-foreground">
          Die Gesamtkosten stammen vom LLM-Proxy und umfassen jeden Modellaufruf des Chats, auch die von Subagenten und
          Kompaktierungen. Die Kosten je Antwort im Verlauf zählen nur die Antworten der Hauptsitzung; ihre Summe kann
          deshalb kleiner sein.
        </p>
      </PopoverContent>
    </Popover>
  )
}

/** „Subagenten x / y“ mit Stepper zum Ändern der Grenze; bei beendetem Chat gesperrt. */
function SubagentLimit({
  chat,
  limit,
  busy,
  onChange,
}: {
  chat: Chat
  limit?: number
  busy: boolean
  onChange: (max: number) => void
}) {
  const max = chat.max_subagents ?? 0
  const used = chat.subagents ?? 0
  const over = used > max
  return (
    <Popover>
      <PopoverTrigger asChild>
        <button
          type="button"
          className={cn(
            "inline-flex items-center gap-1 rounded underline decoration-dotted underline-offset-2 hover:text-foreground",
            over && "font-medium text-red-700",
          )}
          aria-label={`${subagentLimitLabel(chat)}, Grenze ändern`}
        >
          <BotIcon className="size-3.5" /> {subagentLimitLabel(chat)}
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" collisionPadding={12} className="w-72 max-w-[calc(100vw-1.5rem)] text-xs">
        <div className="text-sm font-semibold">Grenze für Subagenten</div>
        <p className="mt-1 text-muted-foreground">
          Bisher {used} gestartet, erlaubt {max}. Hart durchgesetzt am Proxy (höchstens {max + 1} gleichzeitige
          Modellaufrufe) und durch Abbruch, sobald mehr Subagenten starten.
        </p>
        <div className="mt-2 flex items-center justify-between gap-2">
          <Label htmlFor={`maxsub-${chat.id}`} className="text-xs">
            Max. Subagenten
          </Label>
          <NumberStepper
            id={`maxsub-${chat.id}`}
            value={max}
            min={0}
            max={Math.max(limit ?? max, max)}
            disabled={busy}
            onChange={onChange}
            aria-label="Max. Subagenten"
          />
        </div>
        {limit !== undefined && <p className="mt-2 text-muted-foreground">Höchstens {limit}. Wirkt sofort.</p>}
      </PopoverContent>
    </Popover>
  )
}

function Composer({
  chat,
  config,
  modelName,
  commands,
  uploading,
  onUpload,
  staged,
  onUnstage,
  onSent,
  onRestoreStaged,
  onChat,
  onActivity,
  queue,
  outbox,
}: {
  chat: Chat
  config?: Config
  modelName: (id: string) => string
  commands: Command[]
  uploading: boolean
  onUpload: (files: File[]) => Promise<void>
  staged: Artifact[]
  onUnstage: (name: string) => void
  onSent: () => void
  /** Anhänge nach einem gescheiterten Senden wieder ins Eingabefeld legen. */
  onRestoreStaged: (list: Artifact[]) => void
  onChat: (c: Chat) => void
  /** Etwas hat sich am Chat geändert (Chatliste neu laden). */
  onActivity: () => void
  queue: QueueRow[]
  outbox: Outbox
}) {
  const [text, setText] = useState("")
  const inFlight = useRef(0)
  const fileRef = useRef<HTMLInputElement>(null)
  const busy = chat.running || !!chat.resuming

  const liveCommands = useMemo(() => withLiveOptions(commands, chat), [commands, chat])
  const slash = useSlashCommands(liveCommands, text, setText)
  const modelSwitch = useModelSwitch(chat, onChat, modelName)
  const [stopping, setStopping] = useState(false)
  const buttons = composerButtons({
    running: chat.running,
    hasContent: text.trim() !== "" || staged.length > 0,
  })

  const stop = async () => {
    setStopping(true)
    try {
      onChat(await api.abort(chat.id))
    } catch (e) {
      toast.error(`Abbrechen fehlgeschlagen: ${errText(e)}`)
    } finally {
      setStopping(false)
    }
  }

  // Senden zeigt die Nachricht sofort: im Verlauf, oder in der Warteschlange über dem Eingabefeld,
  // wenn der Agent gerade arbeitet. Die Antwort des Servers entscheidet endgültig (queued).
  const send = async () => {
    const t = text.trim()
    const slashCmd = isSlashCommand(t)
    if (!t && staged.length === 0) return
    const attachments = slashCmd ? [] : staged
    const names = attachments.map((a) => a.name)
    const shown = slashCmd && isBuiltinCommand(t) ? undefined : withAttachments(t, names)
    const guessQueued = expectQueued(chat, inFlight.current)
    const resuming = chat.state === "dormant" && !guessQueued
    const key = `send-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`
    if (shown !== undefined) {
      if (guessQueued) outbox.addLocalQueued({ key, text: t, attachments: names })
      else outbox.addPending(key, shown)
    }
    setText("")
    if (!slashCmd) onSent()
    if (resuming) {
      onChat({ ...chat, resuming: true })
    }
    inFlight.current++
    try {
      const r = slashCmd ? await api.runCommand(chat.id, t) : await api.sendMessage(chat.id, t, names)
      if (shown !== undefined) {
        outbox.dropLocalQueued(key)
        if (r.queued) outbox.dropPending(key) // steht in der Warteschlange (SSE „queue“)
        else if (guessQueued) outbox.addPending(key, shown) // doch sofort gesendet
      }
      // /compact meldet sich über die Trennlinie im Verlauf; /autocompact hat sonst keine sichtbare Rückmeldung
      const ac = autoCompactSwitch(t)
      if (ac !== undefined) toast.success(`Auto-Kompaktierung ${ac ? "an" : "aus"}`)
      if (/^\/(model|effort)\s/i.test(t)) toast.success(/^\/model/i.test(t) ? "Modell gewechselt" : "Denkstufe gesetzt")
      const renamed = renameTitle(t)
      if (renamed !== undefined) {
        onChat({ ...chat, title: renamed })
        toast.success("Chat umbenannt")
      }
    } catch (e) {
      if (shown !== undefined) {
        outbox.dropLocalQueued(key)
        outbox.failPending(key)
      }
      const tooLarge = contextTooLarge(e)
      // Nichts geht verloren: Text und Anhänge kommen zurück ins Eingabefeld. Bei zu vollem Kontext
      // übernimmt der Dialog (erst kompaktieren, dann wechseln).
      if (!tooLarge) setText((cur) => (cur.trim() ? `${t}\n\n${cur}` : t))
      if (!slashCmd && attachments.length > 0) onRestoreStaged(attachments)
      if (tooLarge) {
        modelSwitch.showTooLarge(tooLarge)
      } else if (slashCmd && isBuiltinCommand(t) && e instanceof ApiError && e.status !== 409) {
        toast.error(`Befehl fehlgeschlagen: ${errText(e)}`)
      } else if (e instanceof ApiError && e.status === 409 && slashCmd) {
        toast.error(`Befehl gerade nicht möglich: ${errText(e)}`)
      } else if (!resuming) {
        // Beim Fortsetzen steht der Grund im Block im Verlauf
        toast.error(`Senden fehlgeschlagen: ${errText(e)}`)
      }
    } finally {
      inFlight.current--
      onActivity()
    }
  }

  return (
    <div className="bg-background px-3 pt-1 pb-3 sm:px-4">
      <div className="mx-auto flex max-w-3xl flex-col gap-2">
        {chat.state === "dormant" && !chat.resuming && queue.length === 0 && (
          <Alert>
            <AlertDescription>Der Chat ruht. Eine Nachricht setzt ihn in einer frischen Sandbox fort.</AlertDescription>
          </Alert>
        )}
        {queue.length > 0 && (
          <QueueList chat={chat} rows={queue} busy={busy} onActivity={onActivity} />
        )}
        <SlashCommandPopover state={slash}>
          <div
            className={cn(
              "flex flex-col gap-2 rounded-3xl border bg-muted/40 px-3 pt-3 pb-2 shadow-xs transition-colors",
              "focus-within:border-ring/60 focus-within:bg-background",
            )}
          >
            {staged.length > 0 && (
              <ul className="flex flex-wrap gap-2 px-1" aria-label="Anhänge der Nachricht">
                {staged.map((a) => (
                  <StagedTile key={a.name} chatId={chat.id} artifact={a} onRemove={() => onUnstage(a.name)} />
                ))}
              </ul>
            )}
            <Textarea
              value={text}
              rows={1}
              aria-label="Nachricht"
              placeholder={busy ? "Weitere Nachricht einreihen …" : "Nachricht an den Agenten … („/“ für Befehle)"}
              onChange={(e) => setText(e.target.value)}
              onKeyDown={(e) => {
                if (slash.onKeyDown(e)) return
                if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
                  e.preventDefault()
                  void send()
                }
              }}
              className="max-h-60 min-h-10 resize-none border-0 bg-transparent px-1 py-1 text-sm shadow-none focus-visible:ring-0 dark:bg-transparent"
            />
            <div className="flex items-center gap-2">
              <input
                ref={fileRef}
                type="file"
                multiple
                hidden
                onChange={(e) => {
                  const files = Array.from(e.target.files ?? [])
                  e.target.value = ""
                  void onUpload(files)
                }}
              />
              <Button
                size="icon"
                variant="ghost"
                className="rounded-full"
                disabled={uploading}
                title="Dateien anhängen (liegen unter /workspace/inputs/)"
                aria-label="Dateien für den Agenten hochladen"
                onClick={() => fileRef.current?.click()}
              >
                {uploading ? <LoaderCircleIcon className="animate-spin" /> : <PlusIcon />}
              </Button>
              {uploading && <span className="text-xs text-muted-foreground">Lade hoch …</span>}
              <ModelEffortPicker
                chat={chat}
                commands={liveCommands}
                disabled={busy || modelSwitch.busy}
                modelName={modelName}
                onModel={(m) => void modelSwitch.setModel(m)}
                onEffort={(l) => void modelSwitch.setEffort(l)}
              />
              <div className="ml-auto flex items-center gap-2">
                {buttons.send && (
                  <Button
                    size="icon"
                    className="rounded-full"
                    variant={buttons.stop ? "outline" : "default"}
                    disabled={!buttons.sendEnabled}
                    title={busy ? "Nachricht einreihen (geht an der nächsten passenden Stelle an den Agenten, spätestens nach dem laufenden Werkzeug)" : "Senden (Enter)"}
                    aria-label={busy ? "Nachricht einreihen" : "Senden"}
                    onClick={() => void send()}
                  >
                    <ArrowUpIcon />
                  </Button>
                )}
                {buttons.stop && (
                  <Button
                    size="icon"
                    className="rounded-full"
                    disabled={stopping}
                    title="Laufende Antwort abbrechen (Eingereihtes bleibt stehen)"
                    aria-label="Laufende Antwort abbrechen"
                    onClick={() => void stop()}
                  >
                    <SquareIcon className="fill-current" />
                  </Button>
                )}
              </div>
            </div>
          </div>
        </SlashCommandPopover>
        <ContextTooLargeDialog sw={modelSwitch} modelName={modelName} />
        <p className="hidden text-center text-xs text-muted-foreground sm:block">
          Enter sendet, Umschalt+Enter neue Zeile · Dateien auch hierher ziehen
          {config?.artifact_max_mb ? ` (höchstens ${config.artifact_max_mb} MB)` : ""}
        </p>
      </div>
    </div>
  )
}

/**
 * Eingereihte Nachrichten über dem Eingabefeld (wie in Claude Code): gekürzt, Anhänge als Chips,
 * mit X zu entfernen, solange sie nicht übergeben sind. Nach einem Abbruch oder bei ruhendem Chat
 * gehen sie erst mit der nächsten Nachricht mit, oder sofort über „Jetzt senden“.
 */
function QueueList({ chat, rows, busy, onActivity }: { chat: Chat; rows: QueueRow[]; busy: boolean; onActivity: () => void }) {
  const [flushing, setFlushing] = useState(false)
  const held = !!chat.queue_held && !busy
  const remove = async (id: string) => {
    try {
      await api.unqueue(chat.id, id)
    } catch (e) {
      toast.error(
        e instanceof ApiError && e.status === 409 ? "Schon an den Agenten übergeben." : `Entfernen fehlgeschlagen: ${errText(e)}`,
      )
    }
  }
  const flush = async () => {
    setFlushing(true)
    try {
      await api.flushQueue(chat.id)
    } catch (e) {
      toast.error(`Senden fehlgeschlagen: ${errText(e)}`)
    } finally {
      setFlushing(false)
      onActivity()
    }
  }
  return (
    <section aria-label="Eingereihte Nachrichten" className="rounded-2xl border bg-muted/30 px-3 py-2 text-xs">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-muted-foreground">
        <ListOrderedIcon className="size-3.5 shrink-0" />
        <span className="font-medium text-foreground">Eingereiht ({rows.length})</span>
        <span className="min-w-0">
          {held
            ? `· ${holdReasonText(chat.hold_reason) ? `${holdReasonText(chat.hold_reason)}, ` : ""}geht mit der nächsten Nachricht mit`
            : "· geht an der nächsten passenden Stelle an den Agenten (nach dem laufenden Werkzeug)"}
        </span>
        {held && (
          <Button size="xs" variant="outline" className="ml-auto" disabled={flushing} onClick={() => void flush()}>
            {flushing ? <LoaderCircleIcon className="animate-spin" /> : <SendHorizontalIcon />}
            Jetzt senden
          </Button>
        )}
      </div>
      <ul className="mt-1.5 flex flex-col gap-1">
        {rows.map((r) => (
          <li
            key={r.key}
            className={cn(
              "flex min-w-0 items-center gap-2 rounded-lg px-2 py-1",
              r.system ? "border border-dashed bg-muted/40 text-muted-foreground" : "bg-background/70",
            )}
          >
            {r.system && (
              <span
                className="inline-flex shrink-0 items-center gap-1 text-[11px] font-medium"
                title="Meldung des Orchestrators an den Agenten (kind „system“), nicht vom Nutzer"
              >
                <BellIcon className="size-3.5" />
                <span>Systemhinweis</span>
              </span>
            )}
            <span className="min-w-0 flex-1 truncate" title={r.text}>
              {(r.system && r.label) || queuePreview(r.text) || <span className="text-muted-foreground italic">nur Anhänge</span>}
            </span>
            {r.attachments.length > 0 && (
              <span
                className="inline-flex max-w-32 shrink-0 items-center gap-1 rounded border bg-background px-1.5 py-0.5 text-[11px] text-muted-foreground"
                title={r.attachments.join(", ")}
              >
                <PaperclipIcon className="size-3 shrink-0" />
                <span className="truncate">{r.attachments.length === 1 ? r.attachments[0] : `${r.attachments.length} Dateien`}</span>
              </span>
            )}
            {r.id ? (
              <button
                type="button"
                className="shrink-0 rounded-full p-0.5 text-muted-foreground hover:bg-muted hover:text-foreground"
                aria-label={r.system ? "Systemhinweis entfernen" : "Eingereihte Nachricht entfernen"}
                title={
                  r.system
                    ? "Entfernen (noch nicht übergeben); der Agent erfährt dann nicht von dieser Meldung"
                    : "Entfernen (noch nicht übergeben)"
                }
                onClick={() => void remove(r.id!)}
              >
                <XIcon className="size-3.5" />
              </button>
            ) : (
              <LoaderCircleIcon className="size-3.5 shrink-0 animate-spin text-muted-foreground" aria-label="wird eingereiht" />
            )}
          </li>
        ))}
      </ul>
    </section>
  )
}

/** Anhang im Eingabefeld: Bilder als Vorschau, andere Dateien als Kachel mit Name und Größe. */
function StagedTile({ chatId, artifact: a, onRemove }: { chatId: string; artifact: Artifact; onRemove: () => void }) {
  const image = isPreviewImage(a.name)
  return (
    <li className="group relative" title={`liegt in der Sandbox unter /workspace/inputs/${a.name}`}>
      {image ? (
        <img
          src={urls.artifact(chatId, a.name, "input")}
          alt={a.name}
          className="size-16 rounded-xl border object-cover"
        />
      ) : (
        <div className="flex h-16 w-52 max-w-full items-center gap-2 rounded-xl border bg-background px-2.5">
          <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary text-primary-foreground">
            <FileIcon className="size-4" />
          </span>
          <span className="min-w-0 text-xs">
            <span className="block truncate font-medium">{a.name}</span>
            <span className="text-muted-foreground">{formatBytes(a.size)}</span>
          </span>
        </div>
      )}
      <button
        type="button"
        className="absolute -top-1.5 -right-1.5 rounded-full border bg-background p-0.5 text-muted-foreground shadow-xs hover:text-foreground"
        aria-label={`Anhang ${a.name} entfernen`}
        title="Nicht an diese Nachricht hängen (die Datei bleibt unter Eingaben)"
        onClick={onRemove}
      >
        <XIcon className="size-3" />
      </button>
    </li>
  )
}
