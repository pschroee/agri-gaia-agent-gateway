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
import { useNow } from "@/hooks/useNow"
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
import { countViolations } from "@/lib/delegationTemplates"
import { chatRunSince } from "@/lib/runtime"
import { assignRuns, groupRuns, limitNotices, subagentLimitLabel } from "@/lib/subagents"
import { todoTimeline } from "@/lib/tasks"
import { cn } from "@/lib/utils"

type Props = {
  chatId: string
  /** Open detail view of a subagent run; otherwise the main history. */
  runId?: string
  config?: Config
  modelName: (id: string) => string
  onChanged: () => void
  /** Reports the start of the running turn (ms) for the chat list; undefined when nothing is running. */
  onRunSince?: (since: number | undefined) => void
  /** Embedded in the platform's side panel: without back button (the chat list is a select). */
  embed?: boolean
}

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e))

type Outbox = ReturnType<typeof useChatStream>["outbox"]

export function ChatPanel({ chatId, runId, config, modelName, onChanged, onRunSince, embed }: Props) {
  const s = useChatStream(chatId)
  const { chat } = s
  const runSince = chatRunSince(chat, s.transcript)
  useEffect(() => onRunSince?.(runSince), [onRunSince, runSince])
  const [dragging, setDragging] = useState(false)
  const dragDepth = useRef(0)
  const [uploading, setUploading] = useState(false)
  const [staged, setStaged] = useState<Artifact[]>([])
  const [sheetOpen, setSheetOpen] = useState(false)
  const [sideOpen, setSideOpen] = useFlag("sidebar", true)
  // The counter in the chat header and cards in the history open the "Background" tab (n changes with each
  // click, id: task in focus, fresh: until the side sheet is closed again, see ChatSidePanel).
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
  // Matching requested (proxy) ↔ executed (orchestrator) per toolCallId (E9)
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
        toast.error(`Too large (at most ${maxMb} MB per file): ${tooBig.map((f) => f.name).join(", ")}`)
        files = files.filter((f) => !tooBig.includes(f))
        if (files.length === 0) return
      }
    }
    setUploading(true)
    try {
      const list = await api.uploadFiles(chat.id, files)
      s.mergeArtifacts(list)
      // Uploaded files are attached to the message currently being written.
      setStaged((prev) => [...prev.filter((p) => !list.some((a) => a.name === p.name)), ...list])
    } catch (e) {
      toast.error(`Upload failed: ${errText(e)}`)
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
        {s.loadError ? `Chat could not be loaded: ${s.loadError}` : "Loading chat …"}
      </div>
    )
  }

  const pending = s.approvals.filter((a) => a.state === "pending")

  return (
    <div className="relative flex min-h-0 min-w-0 flex-1" {...dropHandlers}>
      {dragging && (
        <div className="pointer-events-none absolute inset-2 z-20 flex flex-col items-center justify-center gap-2 rounded-xl border-2 border-dashed border-sky-400 bg-sky-50/90 text-sky-800">
          <UploadIcon className="size-8" />
          <p className="font-medium">Drop files here</p>
          <p className="text-sm">They will be in the sandbox under /workspace/inputs/</p>
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
          embed={embed}
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
              title={sideOpen ? "Enlarge chat (hide sidebar)" : "Shrink chat (show sidebar)"}
              aria-label={sideOpen ? "Hide sidebar" : "Show sidebar"}
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
                  title="Artifacts, socket log, subagents and model calls"
                  aria-label={`Open artifacts (${s.artifacts.length}), socket log (${s.socketCalls.length}), subagents (${runs.length}) and model calls (${s.llmCalls.length})`}
                >
                  <FolderOpenIcon />
                  <span className="hidden sm:inline">Artifacts & logs</span>
                  <span className="sm:hidden" aria-hidden>
                    {s.artifacts.length}/{s.socketCalls.length}
                  </span>
                </Button>
              </SheetTrigger>
              <SheetContent side="right" className="w-[90vw] gap-0 p-0 sm:max-w-sm">
                <SheetHeader className="border-b pr-10">
                  <SheetTitle>Artifacts & logs</SheetTitle>
                  <SheetDescription className="sr-only">
                    Files, socket calls, subagents and model calls of this chat
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
              // Pending approvals apply to the whole chat; they can be decided here too, so that a question
              // (e.g. from a subagent) does not wait unnoticed.
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
            <DelegationStrip chat={chat} violations={countViolations(s.socketCalls)} onOpen={() => setSheetOpen(true)} />
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

/** Delegation and violations at a glance, also in the narrow view (details in the side sheet). */
function DelegationStrip({ chat, violations, onOpen }: { chat: Chat; violations: number; onOpen: () => void }) {
  const d = chat.delegation
  const now = useNow(30_000, !!d?.expires_at)
  if (!d && violations === 0) return null
  const expired = d?.expires_at ? new Date(d.expires_at).getTime() < now : false
  return (
    <button
      type="button"
      onClick={onOpen}
      className={cn(
        "flex w-full flex-wrap items-center gap-x-2 border-b px-3 py-1 text-left text-xs lg:pointer-events-none",
        violations > 0 ? "border-red-200 bg-red-50 text-red-800" : "bg-muted/40 text-muted-foreground",
      )}
      title="Open delegation and socket log"
    >
      {d ? (
        <span>
          Delegation: {d.rules.length} {d.rules.length === 1 ? "rule" : "rules"}
          {d.expires_at && (expired ? ", expired" : `, until ${new Date(d.expires_at).toLocaleString("en-US", { dateStyle: "short", timeStyle: "short", hourCycle: "h23" })}`)}
          {d.enforce === false && ", logged only"}
        </span>
      ) : (
        <span>No delegation</span>
      )}
      {violations > 0 && (
        <span className="font-medium">
          · {violations} {violations === 1 ? "violation" : "violations"}
          {d?.enforce === false ? " logged" : " refused"}
        </span>
      )}
    </button>
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
  embed,
}: {
  embed?: boolean
  chat: Chat
  modelName: (id: string) => string
  config?: Config
  answersCost: number
  connected: boolean
  /** Start of the running turn (ms) for the run time next to "Running". */
  runSince?: number
  /** Subagent dropdown next to the title (only visible with runs). */
  subagentsMenu: React.ReactNode
  /** Progress of the task list (only visible with tasks). */
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
      toast.error(`${label} failed: ${errText(e)}`)
    } finally {
      setBusy(undefined)
    }
  }
  const compact = async () => {
    setBusy("Compact")
    try {
      await api.runCommand(chat.id, "/compact")
    } catch (e) {
      toast.error(
        e instanceof ApiError && e.status === 409
          ? "pi is working right now; compaction is only possible afterwards."
          : `Compaction failed: ${errText(e)}`,
      )
    } finally {
      setBusy(undefined)
    }
  }

  return (
    <header className="border-b px-3 py-2.5 sm:px-4">
      <div className="flex flex-wrap items-center gap-2">
        <Button size="icon-sm" variant="ghost" className={embed ? "hidden" : "md:hidden"} asChild>
          <a href="#/chats" title="Back to the chat list" aria-label="Back to the chat list">
            <ArrowLeftIcon />
          </a>
        </Button>
        <h2 className="min-w-0 flex-1 basis-48 truncate text-base font-semibold sm:flex-none sm:basis-auto">{chat.title || "Untitled"}</h2>
        {subagentsMenu}
        {tasksMenu}
        <ChatStateBadge state={chat.state} resuming={chat.resuming} />
        <VariantBadge variant={chat.variant} />
        {chat.running && <RunningIndicator since={runSince} />}
        {!connected && (
          <span className="inline-flex items-center gap-1 text-xs text-amber-700">
            <WifiOffIcon className="size-3.5" /> <span className="hidden sm:inline">Live connection lost, reconnecting …</span>
            <span className="sm:hidden">disconnected</span>
          </span>
        )}
        <div className="ml-auto flex flex-wrap items-center gap-1.5">
          <ContextIndicator
            chat={chat}
            busy={busy}
            onAutoCompact={(v) => void run("Auto-compaction", () => api.setAutoCompact(chat.id, v))}
            onCompact={() => void compact()}
          />
          {sidePanel}
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button size="icon-sm" variant="ghost" disabled={!!busy} title="More actions" aria-label="More actions">
                <EllipsisIcon />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-60">
              {chat.state === "active" && (
                <DropdownMenuItem onSelect={() => void run("Idle", () => api.suspend(chat.id))}>
                  <MoonIcon />
                  <div>
                    <div>Let idle</div>
                    <div className="text-xs text-muted-foreground">Release the sandbox, resume later</div>
                  </div>
                </DropdownMenuItem>
              )}
              <DropdownMenuItem asChild>
                <a href={urls.session(chat.id)} target="_blank" rel="noreferrer">
                  <FileTextIcon /> Open session as JSONL
                </a>
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>
      <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
        <span className="text-foreground">{modelName(chat.model)}</span>
        <span className="font-mono" title="Slot in the warm pool">
          {chat.slot_id ?? "no slot"}
        </span>
        <span
          title={`${formatTokens(chat.tokens?.input)} in · ${formatTokens(chat.tokens?.output)} out · ${formatTokens(chat.tokens?.cache_read)} from cache`}
        >
          <span className="text-foreground">{formatTokens(chat.tokens?.total)}</span> tokens
        </span>
        <CostInfo chat={chat} answersCost={answersCost} />
        <SubagentLimit
          chat={chat}
          limit={config?.max_subagents_limit}
          busy={busy === "Subagent limit"}
          onChange={(max) => void run("Subagent limit", () => api.setMaxSubagents(chat.id, max))}
        />
        <span
          className={cn("inline-flex items-center gap-1.5 sm:ml-auto", chat.internet && "text-sky-700")}
          title={
            chat.internet
              ? "Internet access on. The language model and the orchestrator are always reachable."
              : "Internet access off. The language model and the orchestrator are still reachable; the agent can request internet."
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

/** Total cost; the breakdown opens on click. */
function CostInfo({ chat, answersCost }: { chat: Chat; answersCost: number }) {
  const c = costSplit(chat)
  return (
    <Popover>
      <PopoverTrigger asChild>
        <button
          type="button"
          className="rounded underline decoration-dotted underline-offset-2 hover:text-foreground"
          aria-label="Cost breakdown"
        >
          Cost: <span className="font-medium text-foreground">{formatUsd(c.total)}</span>
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" collisionPadding={12} className="w-80 max-w-[calc(100vw-1.5rem)] text-xs">
        <div className="mb-2 text-sm font-semibold">Cost {formatUsd(c.total)}</div>
        <dl className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-3 gap-y-1 tabular-nums">
          <dt className="text-muted-foreground">Main responses</dt>
          <dd className="text-right">{formatUsd(c.main)}</dd>
          <dt className="text-muted-foreground">of which outside the main responses (subagents, compaction)</dt>
          <dd className="text-right">{formatUsd(c.other)}</dd>
          <dt className="text-muted-foreground">Model calls at the proxy</dt>
          <dd className="text-right">{c.calls}</dd>
          <dt className="text-muted-foreground">Sum of the responses in the history</dt>
          <dd className="text-right">{formatUsd(answersCost)}</dd>
        </dl>
        <p className="mt-2 border-t pt-2 text-muted-foreground">
          The total cost comes from the LLM proxy and covers every model call of the chat, including those of subagents
          and compactions. The cost per response in the history counts only the responses of the main session; their
          sum can therefore be smaller.
        </p>
      </PopoverContent>
    </Popover>
  )
}

/** "Subagents x / y" with a stepper to change the limit; locked for an ended chat. */
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
          aria-label={`${subagentLimitLabel(chat)}, change limit`}
        >
          <BotIcon className="size-3.5" /> {subagentLimitLabel(chat)}
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" collisionPadding={12} className="w-72 max-w-[calc(100vw-1.5rem)] text-xs">
        <div className="text-sm font-semibold">Subagent limit</div>
        <p className="mt-1 text-muted-foreground">
          {used} started so far, {max} allowed. Enforced strictly at the proxy (at most {max + 1} concurrent model
          calls) and by aborting as soon as more subagents start.
        </p>
        <div className="mt-2 flex items-center justify-between gap-2">
          <Label htmlFor={`maxsub-${chat.id}`} className="text-xs">
            Max. subagents
          </Label>
          <NumberStepper
            id={`maxsub-${chat.id}`}
            value={max}
            min={0}
            max={Math.max(limit ?? max, max)}
            disabled={busy}
            onChange={onChange}
            aria-label="Max. subagents"
          />
        </div>
        {limit !== undefined && <p className="mt-2 text-muted-foreground">At most {limit}. Takes effect immediately.</p>}
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
  /** Put attachments back into the input field after a failed send. */
  onRestoreStaged: (list: Artifact[]) => void
  onChat: (c: Chat) => void
  /** Something about the chat changed (reload the chat list). */
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
      toast.error(`Abort failed: ${errText(e)}`)
    } finally {
      setStopping(false)
    }
  }

  // Sending shows the message right away: in the history, or in the queue above the input field when
  // the agent is working. The server's response decides finally (queued).
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
        if (r.queued) outbox.dropPending(key) // is in the queue (SSE "queue")
        else if (guessQueued) outbox.addPending(key, shown) // sent right away after all
      }
      // /compact reports through the divider in the history; /autocompact has no visible feedback otherwise
      const ac = autoCompactSwitch(t)
      if (ac !== undefined) toast.success(`Auto-compaction ${ac ? "on" : "off"}`)
      if (/^\/(model|effort)\s/i.test(t)) toast.success(/^\/model/i.test(t) ? "Model switched" : "Thinking level set")
      const renamed = renameTitle(t)
      if (renamed !== undefined) {
        onChat({ ...chat, title: renamed })
        toast.success("Chat renamed")
      }
    } catch (e) {
      if (shown !== undefined) {
        outbox.dropLocalQueued(key)
        outbox.failPending(key)
      }
      const tooLarge = contextTooLarge(e)
      // Nothing is lost: text and attachments go back into the input field. If the context is too full,
      // the dialog takes over (compact first, then switch).
      if (!tooLarge) setText((cur) => (cur.trim() ? `${t}\n\n${cur}` : t))
      if (!slashCmd && attachments.length > 0) onRestoreStaged(attachments)
      if (tooLarge) {
        modelSwitch.showTooLarge(tooLarge)
      } else if (slashCmd && isBuiltinCommand(t) && e instanceof ApiError && e.status !== 409) {
        toast.error(`Command failed: ${errText(e)}`)
      } else if (e instanceof ApiError && e.status === 409 && slashCmd) {
        toast.error(`Command not possible right now: ${errText(e)}`)
      } else if (!resuming) {
        // When resuming, the reason is shown in the block in the history
        toast.error(`Sending failed: ${errText(e)}`)
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
            <AlertDescription>The chat is idle. A message resumes it in a fresh sandbox.</AlertDescription>
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
              <ul className="flex flex-wrap gap-2 px-1" aria-label="Message attachments">
                {staged.map((a) => (
                  <StagedTile key={a.name} chatId={chat.id} artifact={a} onRemove={() => onUnstage(a.name)} />
                ))}
              </ul>
            )}
            <Textarea
              value={text}
              rows={1}
              aria-label="Message"
              placeholder={busy ? "Queue another message …" : "Message to the agent … (type / for commands)"}
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
                title="Attach files (placed under /workspace/inputs/)"
                aria-label="Upload files for the agent"
                onClick={() => fileRef.current?.click()}
              >
                {uploading ? <LoaderCircleIcon className="animate-spin" /> : <PlusIcon />}
              </Button>
              {uploading && <span className="text-xs text-muted-foreground">Uploading …</span>}
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
                    title={busy ? "Queue message (goes to the agent at the next suitable point, at the latest after the running tool)" : "Send (Enter)"}
                    aria-label={busy ? "Queue message" : "Send"}
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
                    title="Abort the running response (queued messages stay)"
                    aria-label="Abort the running response"
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
          Enter sends, Shift+Enter for a new line · you can also drag files here
          {config?.artifact_max_mb ? ` (at most ${config.artifact_max_mb} MB)` : ""}
        </p>
      </div>
    </div>
  )
}

/**
 * Queued messages above the input field (as in Claude Code): shortened, attachments as chips,
 * removable with X as long as they have not been handed over. After an abort or with an idle chat
 * they go along only with the next message, or right away via "Send now".
 */
function QueueList({ chat, rows, busy, onActivity }: { chat: Chat; rows: QueueRow[]; busy: boolean; onActivity: () => void }) {
  const [flushing, setFlushing] = useState(false)
  const held = !!chat.queue_held && !busy
  const remove = async (id: string) => {
    try {
      await api.unqueue(chat.id, id)
    } catch (e) {
      toast.error(
        e instanceof ApiError && e.status === 409 ? "Already handed to the agent." : `Removing failed: ${errText(e)}`,
      )
    }
  }
  const flush = async () => {
    setFlushing(true)
    try {
      await api.flushQueue(chat.id)
    } catch (e) {
      toast.error(`Sending failed: ${errText(e)}`)
    } finally {
      setFlushing(false)
      onActivity()
    }
  }
  return (
    <section aria-label="Queued messages" className="rounded-2xl border bg-muted/30 px-3 py-2 text-xs">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-muted-foreground">
        <ListOrderedIcon className="size-3.5 shrink-0" />
        <span className="font-medium text-foreground">Queued ({rows.length})</span>
        <span className="min-w-0">
          {held
            ? `· ${holdReasonText(chat.hold_reason) ? `${holdReasonText(chat.hold_reason)}, ` : ""}goes along with the next message`
            : "· goes to the agent at the next suitable point (after the running tool)"}
        </span>
        {held && (
          <Button size="xs" variant="outline" className="ml-auto" disabled={flushing} onClick={() => void flush()}>
            {flushing ? <LoaderCircleIcon className="animate-spin" /> : <SendHorizontalIcon />}
            Send now
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
                title="Orchestrator note to the agent (kind 'system'), not from the user"
              >
                <BellIcon className="size-3.5" />
                <span>System note</span>
              </span>
            )}
            <span className="min-w-0 flex-1 truncate" title={r.text}>
              {(r.system && r.label) || queuePreview(r.text) || <span className="text-muted-foreground italic">attachments only</span>}
            </span>
            {r.attachments.length > 0 && (
              <span
                className="inline-flex max-w-32 shrink-0 items-center gap-1 rounded border bg-background px-1.5 py-0.5 text-[11px] text-muted-foreground"
                title={r.attachments.join(", ")}
              >
                <PaperclipIcon className="size-3 shrink-0" />
                <span className="truncate">{r.attachments.length === 1 ? r.attachments[0] : `${r.attachments.length} files`}</span>
              </span>
            )}
            {r.id ? (
              <button
                type="button"
                className="shrink-0 rounded-full p-0.5 text-muted-foreground hover:bg-muted hover:text-foreground"
                aria-label={r.system ? "Remove system note" : "Remove queued message"}
                title={
                  r.system
                    ? "Remove (not handed over yet); the agent will then not learn about this note"
                    : "Remove (not handed over yet)"
                }
                onClick={() => void remove(r.id!)}
              >
                <XIcon className="size-3.5" />
              </button>
            ) : (
              <LoaderCircleIcon className="size-3.5 shrink-0 animate-spin text-muted-foreground" aria-label="queueing" />
            )}
          </li>
        ))}
      </ul>
    </section>
  )
}

/** Attachment in the input field: images as thumbnails, other files as a tile with name and size. */
function StagedTile({ chatId, artifact: a, onRemove }: { chatId: string; artifact: Artifact; onRemove: () => void }) {
  const image = isPreviewImage(a.name)
  return (
    <li className="group relative" title={`in the sandbox under /workspace/inputs/${a.name}`}>
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
        aria-label={`Remove attachment ${a.name}`}
        title="Do not attach to this message (the file stays under inputs)"
        onClick={onRemove}
      >
        <XIcon className="size-3" />
      </button>
    </li>
  )
}
