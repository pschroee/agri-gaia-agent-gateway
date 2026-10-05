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

/** Subagent runs, assigned to the main agent's subagent calls. */
export type TranscriptSubagents = {
  runs: Map<string, SubagentRun>
  assignment: RunAssignment
  proxyIds: Set<string>
  /** Matching of the tool calls against the orchestrator's log (E9). */
  evidence?: Map<string, Evidence>
  /** The chat is not working; missing evidence then counts as suspicious. */
  settled?: boolean
}

type Props = {
  transcript: TranscriptState
  /** For download links of the attachments. */
  chatId?: string
  subagents?: TranscriptSubagents
  /** Notices about limits (agent_limit, subagent_limit), sorted in by time. */
  notices?: LimitNotice[]
  /** Shown below the history, e.g. pending approvals. */
  footer?: React.ReactNode
  /** The agent is working (for "Thinking …" while no response is streaming yet). */
  working?: boolean
  /** Background tasks: cards of bash with run_in_background and notes in the history. */
  background?: BackgroundTask[]
  /** Opens the "Background" tab, optionally with a task in focus. */
  onOpenBackground?: (id?: string) => void
  /** Artifacts of the chat; results appear below the tool call that uploaded them. */
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

  // Insert loose subagent runs and limit notices after the matching entry
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
  // Calls of the task list (todo) as compact cards instead of raw tool cards
  const todo = useMemo(() => todoTimeline(transcript), [transcript])
  // Orchestrator notes in user messages, split by origin according to the server
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
          <p className="py-8 text-center text-sm text-muted-foreground">No messages yet.</p>
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
  // Live responses (without seq) have no tariff value from the orchestrator yet; pi's value is provisional.
  const provisional = item.cost === undefined && item.seq === undefined && !!item.usage?.cost?.total
  const usage = formatUsage(item.usage, item.cost, { provisional })
  const cache = item.usage ? cacheHitRate(item.usage.input, item.usage.cacheRead) : undefined
  const lastIdx = item.blocks.length - 1
  // First block of a sequence of todo calls → indices of the sequence; the others are dropped.
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
                [image]
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
                ? "Provisional: pi's flat price. The value by tariff (peak/off-peak) follows once the response is stored."
                : undefined
            }
          >
            {usage}
          </span>
          {cache !== undefined && <span title="Cache hit rate: cache tokens / (input + cache)">· cache {formatPercent(cache * 100)}</span>}
          {item.durationMs !== undefined && (
            <span className="tabular-nums" title="Duration of the whole run: from the message to this response">
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
              title={item.peak ? "Response fell in peak time (full price)" : "Response fell in off-peak time (reduced price)"}
            >
              {item.peak ? "Peak tariff" : "Off-peak tariff"}
            </span>
          )}
          {isUserAbort(item) && <span> · aborted</span>}
          {item.stopReason === "error" && !isUserAbort(item) && <span> · error</span>}
          {item.stopReason === "length" && <span> · length limit reached</span>}
        </div>
      )}
    </div>
  )
}

/**
 * Status line below the history while the agent is working: what it is doing and how long the run has taken
 * ("Running bash … 1:05"). Without a known start, only the activity.
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
        {live ? "Thinking …" : "Thoughts"}
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
            ? "The orchestrator aborted the turn and ended the subagent processes."
            : "The LLM proxy refused further concurrent model calls with HTTP 429."}{" "}
          {formatTime(new Date(notice.time).toISOString())}
        </div>
      </div>
    </div>
  )
}

function Caret() {
  return <span className="ml-0.5 inline-block h-4 w-1.5 animate-pulse bg-foreground/60 align-text-bottom" />
}

const reasonText: Record<string, string> = { manual: "manual", threshold: "automatic", overflow: "automatic, overflow" }

function CompactionDivider({ item }: { item: CompactionItem }) {
  const [open, setOpen] = useState(false)
  const reason = reasonText[item.reason] ?? item.reason
  const failed = item.failed || item.aborted || !!item.errorMessage
  let label: string
  if (item.running) label = `Summarising context … (${reason})`
  else if (item.aborted) label = `Compaction aborted (${reason})`
  else if (failed) label = compactionNotice(item.errorMessage)
  else {
    const before = item.tokensBefore !== undefined ? formatTokens(item.tokensBefore) : "?"
    const after = item.tokensAfter !== undefined ? ` → approx. ${formatTokens(item.tokensAfter)}` : ""
    label = `Context summarised: ${before}${after} tokens (${reason})`
  }
  const cost = item.cost ?? item.usage?.cost?.total
  const hasDetails = !item.running && (!!item.summary || cost !== undefined)
  // "not possible"/"not needed" is a notice, not an error: subtle instead of red. The label comes from
  // compactionNotice (lib/stream).
  const benign = failed && !item.aborted && /not (possible|needed|necessary)/i.test(label)

  return (
    <Collapsible open={open} onOpenChange={setOpen} className="text-xs">
      {/* title on the frame, because disabled buttons show no tooltip in some browsers */}
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
            <p className="mt-2 text-[11px] text-muted-foreground">Cost of the summary: {formatUsd(cost)}</p>
          )}
        </div>
      </CollapsibleContent>
    </Collapsible>
  )
}

/**
 * User message; attachments (fixed block at the end, see lib/attachments) as chips. `pending`: sent, not yet
 * acknowledged by pi; `failed`: not sent. `parts`: split according to lib/systemnote (origin according to the
 * server); orchestrator notes appear as a grey line instead of in the bubble.
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
  // Without a system part, the text stays exactly as sent (including blank lines).
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
      {state === "failed" && <span className="ml-auto text-xs text-red-700 dark:text-red-400">Not sent</span>}
    </div>
  )
}

function AttachmentChips({ files, chatId }: { files: string[]; chatId?: string }) {
  if (files.length === 0) return null
  return (
    <ul className="flex flex-wrap justify-end gap-1.5" aria-label="Attachments">
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
              title={`/workspace/inputs/${f} – download`}
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
