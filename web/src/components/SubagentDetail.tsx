// Own view of a subagent run: full width of the chat area, in the style of the main history.
import { useLayoutEffect, useMemo, useRef, useState } from "react"
import { BotIcon, ChevronRightIcon, ChevronsDownUpIcon, ChevronsUpDownIcon, WrenchIcon } from "lucide-react"
import type { Artifact, LLMCall, SubagentEntry } from "@/api/types"
import { ArtifactAttachments } from "@/components/ArtifactAttachments"
import { Markdown } from "@/components/Markdown"
import { Provenance, RunStatusBadge } from "@/components/Subagents"
import { TaskList } from "@/components/Tasks"
import { EvidenceBadge } from "@/components/badges"
import { ArgBlock, EvidenceDetails, Section } from "@/components/ToolCallCard"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { chatHref, subagentHref } from "@/hooks/useHashRoute"
import { useNow } from "@/hooks/useNow"
import { SubagentBar } from "@/components/SubagentOverview"
import { useAgentTree } from "@/hooks/useAgentTree"
import { artifactsOfCall } from "@/lib/artifactPreview"
import { formatTime, formatTokens, formatUsd } from "@/lib/format"
import { formatSpan, runDuration, runMetrics, runTitle } from "@/lib/subagent-overview"
import { taskCountLabel, taskCounts, tasksFromSubagentEntries } from "@/lib/tasks"
import {
  baseRunId,
  isEntryConfirmed,
  pairRunEntries,
  parseArguments,
  runConfirmation,
  runStatus,
  shortRunId,
  type RunStatus,
  type SubagentRun,
} from "@/lib/subagents"
import type { Evidence } from "@/lib/evidence"
import { describeToolArgs } from "@/lib/toolargs"
import { cn } from "@/lib/utils"

type Props = {
  chatId: string
  chatTitle: string
  chatRunning: boolean
  runId: string
  runs: SubagentRun[]
  proxyIds: Set<string>
  llmCalls: LLMCall[]
  /** Matching against the orchestrator's log (E9). */
  evidence?: Map<string, Evidence>
  /** Artifacts of the chat; shown below the tool call that uploaded them. */
  artifacts?: Artifact[]
  /** Below the history, e.g. pending approvals. */
  footer?: React.ReactNode
}

const iso = (ms: number) => new Date(ms).toISOString()

export function SubagentDetail({ chatId, chatTitle, chatRunning, runId, runs, proxyIds, llmCalls, evidence, artifacts, footer }: Props) {
  const run = runs.find((r) => r.runId === runId)
  const root = useAgentTree({ chatTitle, chatRunning, runs, llmCalls })

  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col">
      <SubagentBar chatId={chatId} root={root} runId={runId} runTitle={run ? runTitle(run) : `Run ${shortRunId(runId)}`} />
      {run ? (
        <RunBody key={runId} run={run} runs={runs} chatId={chatId} chatRunning={chatRunning} proxyIds={proxyIds} llmCalls={llmCalls} evidence={evidence} artifacts={artifacts} />
      ) : (
        <div className="flex flex-1 flex-col items-center justify-center gap-2 p-6 text-center text-sm text-muted-foreground">
          <p>
            There are no entries (yet) for <span className="font-mono">{runId}</span> in this chat.
          </p>
          <a href={chatHref(chatId)} className="text-sky-700 hover:underline">
            Back to the chat
          </a>
        </div>
      )}
      {footer}
      <p className="border-t bg-muted/30 px-3 py-2.5 text-center text-xs text-muted-foreground">
        One-off task of a subagent: it takes no follow-up questions; this is the full history.{" "}
        <a href={chatHref(chatId)} className="text-sky-700 hover:underline">
          To the main agent
        </a>
      </p>
    </div>
  )
}

function RunBody({
  run,
  runs,
  chatId,
  chatRunning,
  proxyIds,
  llmCalls,
  evidence,
  artifacts,
}: {
  run: SubagentRun
  runs: SubagentRun[]
  chatId: string
  chatRunning: boolean
  proxyIds: Set<string>
  llmCalls: LLMCall[]
  evidence?: Map<string, Evidence>
  artifacts?: Artifact[]
}) {
  const ref = useRef<HTMLDivElement>(null)
  const [stick, setStick] = useState(true)
  // Counter per click on "expand/collapse all"; the cards take it as their initial state.
  const [expand, setExpand] = useState<{ open: boolean; n: number }>({ open: false, n: 0 })
  const now = useNow(5000)
  const status = runStatus(run, { chatRunning, now })
  const items = useMemo(() => pairRunEntries(run.entries), [run.entries])
  const conf = runConfirmation(run, proxyIds)
  const metrics = useMemo(() => runMetrics(run, llmCalls), [run, llmCalls])
  const base = baseRunId(run.runId)
  const siblings = runs.filter((r) => baseRunId(r.runId) === base)
  const lastOpenTool = items.findLastIndex((i) => i.type === "tool" && !i.result)
  // The run's own task list, if it called the tool todo
  const tasks = useMemo(() => tasksFromSubagentEntries(run.entries), [run.entries])

  const onScroll = () => {
    const el = ref.current
    if (el) setStick(el.scrollHeight - el.scrollTop - el.clientHeight < 48)
  }
  useLayoutEffect(() => {
    const el = ref.current
    if (el && stick) el.scrollTop = el.scrollHeight
  })

  return (
    <div ref={ref} onScroll={onScroll} className="min-h-0 flex-1 overflow-y-auto">
      <div className="mx-auto flex max-w-3xl min-w-0 flex-col gap-4 px-3 py-4 sm:px-4">
        <section className="rounded-md border border-violet-200 bg-violet-50/40 px-3 py-2 text-sm">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <BotIcon className="size-4 shrink-0 text-violet-700" />
            <span className="font-semibold">{run.agent || "Subagent"}</span>
            <span className="min-w-0 font-mono text-xs break-all text-muted-foreground">{run.runId}</span>
            <RunStatusBadge status={status} className="ml-auto" />
          </div>
          <dl className="mt-1.5 grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-0.5 text-xs sm:grid-cols-[auto_minmax(0,1fr)_auto_minmax(0,1fr)]">
            <dt className="text-muted-foreground">Start</dt>
            <dd>{formatTime(iso(run.start))}</dd>
            <dt className="text-muted-foreground">Last activity</dt>
            <dd>{formatTime(iso(run.end))}</dd>
            <dt className="text-muted-foreground">Duration</dt>
            <dd className="tabular-nums">{formatSpan(runDuration(run, status, now))}</dd>
            <dt className="text-muted-foreground">Calls</dt>
            <dd className="tabular-nums">
              {run.toolCalls}
              {run.errors > 0 && <span className="text-red-700"> · {run.errors} error{run.errors === 1 ? "" : "s"}</span>}
            </dd>
            <dt className="text-muted-foreground">Tokens</dt>
            <dd className="tabular-nums" title="Model calls recorded at the proxy whose response appears in this run">
              {metrics.tokens === undefined
                ? "–"
                : `${formatTokens(metrics.input)} in · ${formatTokens(metrics.output)} out · ${formatUsd(metrics.cost)}`}
            </dd>
            <dt className="text-muted-foreground">Confirmed at proxy</dt>
            <dd className="tabular-nums">
              {conf.confirmed} of {conf.total} entries
            </dd>
          </dl>
          <p className="mt-1.5 text-[11px] text-muted-foreground">
            The entries come from the subagent's session file in the pi container; "confirmed at proxy" means the
            corresponding model response is recorded outside the sandbox. Tool calls show "confirmed" when the
            orchestrator executed them itself. The status is derived from the entries.
          </p>
          {siblings.length > 1 && (
            <div className="mt-2 flex flex-wrap items-center gap-1.5 text-xs">
              <span className="text-muted-foreground">Parallel runs:</span>
              {siblings.map((r) => (
                <a
                  key={r.runId}
                  href={subagentHref(chatId, r.runId)}
                  aria-current={r.runId === run.runId ? "page" : undefined}
                  className={cn(
                    "rounded-md border bg-background px-1.5 py-0.5 font-mono hover:bg-violet-100",
                    r.runId === run.runId && "border-violet-400 bg-violet-100 font-semibold",
                  )}
                  title={`${r.agent || "Subagent"} · ${r.runId}`}
                >
                  {r.agent ? `${r.agent} ` : ""}
                  {shortRunId(r.runId)}
                </a>
              ))}
            </div>
          )}
        </section>

        {tasks && taskCounts(tasks).total > 0 && (
          <section className="rounded-md border px-3 py-2 text-xs" aria-label="Subagent tasks">
            <div className="mb-1 font-medium text-muted-foreground">Subagent tasks · {taskCountLabel(taskCounts(tasks))}</div>
            <TaskList tasks={tasks} />
          </section>
        )}

        {items.some((i) => i.type === "tool") && (
          <div className="-mb-2 flex justify-end">
            <Button size="sm" variant="ghost" onClick={() => setExpand((e) => ({ open: !e.open, n: e.n + 1 }))}>
              {expand.open ? <ChevronsDownUpIcon /> : <ChevronsUpDownIcon />}
              {expand.open ? "Collapse all" : "Expand all"}
            </Button>
          </div>
        )}

        {items.map((item, i) => {
          switch (item.type) {
            case "task":
              return <TaskBubble key={item.entry.entry_id} entry={item.entry} proxyIds={proxyIds} />
            case "text":
              return <TextEntry key={item.entry.entry_id} entry={item.entry} proxyIds={proxyIds} />
            case "tool":
              return (
                <RunToolCard
                  key={`${(item.call ?? item.result)!.entry_id}-${expand.n}`}
                  call={item.call}
                  result={item.result}
                  proxyIds={proxyIds}
                  defaultOpen={expand.open}
                  pending={!item.result && i === lastOpenTool && (status === "running" || status === "idle")}
                  evidence={evidence}
                  settled={!chatRunning}
                  chatId={chatId}
                  artifacts={artifactsOfCall(artifacts, item.call?.payload?.id)}
                />
              )
            default:
              return null
          }
        })}
        {items.length === 0 && <p className="py-8 text-center text-sm text-muted-foreground">No entries yet.</p>}
        <RunFooter status={status} />
      </div>
    </div>
  )
}

function RunFooter({ status }: { status: RunStatus }) {
  if (status !== "stopped") return null
  return (
    <p className="text-center text-xs text-muted-foreground">
      The run ends without a text response. The chat is no longer working; it may have been aborted.
    </p>
  )
}

function TaskBubble({ entry, proxyIds }: { entry: SubagentEntry; proxyIds: Set<string> }) {
  return (
    <div className="ml-auto flex max-w-[90%] min-w-0 flex-col items-end gap-1 sm:max-w-[85%]">
      <div className="flex flex-wrap items-center justify-end gap-x-2 text-[11px] text-muted-foreground">
        <span>Task from the main agent</span>
        <Provenance confirmed={isEntryConfirmed(entry, proxyIds)} />
      </div>
      <div className="max-w-full rounded-lg bg-primary px-3 py-2 text-sm break-words whitespace-pre-wrap text-primary-foreground">
        {entry.payload?.text || "(empty)"}
      </div>
    </div>
  )
}

function TextEntry({ entry, proxyIds }: { entry: SubagentEntry; proxyIds: Set<string> }) {
  return (
    <div className="min-w-0 text-sm">
      <Markdown text={entry.payload?.text ?? ""} />
      <div className="mt-1 flex flex-wrap items-center gap-x-2 text-[11px] text-muted-foreground/80">
        <span>{formatTime(entry.created_at)}</span>
        <Provenance confirmed={isEntryConfirmed(entry, proxyIds)} />
      </div>
    </div>
  )
}

/** Tool call of a subagent with its result, in the style of ToolCallCard. */
function RunToolCard({
  call,
  result,
  proxyIds,
  defaultOpen,
  pending,
  evidence,
  settled,
  chatId,
  artifacts,
}: {
  chatId: string
  artifacts: Artifact[]
  call?: SubagentEntry
  result?: SubagentEntry
  proxyIds: Set<string>
  evidence?: Map<string, Evidence>
  settled: boolean
  defaultOpen: boolean
  /** Call without a result that is probably running right now. */
  pending: boolean
}) {
  const [open, setOpen] = useState(defaultOpen)
  const name = call?.payload?.name || result?.payload?.name || "tool"
  const view = describeToolArgs(name, parseArguments(call?.payload?.arguments))
  const error = result?.payload?.is_error === true
  const callConfirmed = call ? isEntryConfirmed(call, proxyIds) : undefined
  const ev = call?.payload?.id ? evidence?.get(call.payload.id) : undefined
  const resultConfirmed = result ? isEntryConfirmed(result, proxyIds) : undefined

  return (
    <div className="min-w-0">
      <Collapsible
        open={open}
        onOpenChange={setOpen}
        className={cn("min-w-0 rounded-md border bg-background text-sm", error && "border-red-300")}
      >
        <CollapsibleTrigger className="flex w-full min-w-0 items-center gap-2 px-3 py-1.5 text-left hover:bg-muted/50">
          <ChevronRightIcon className={cn("size-3.5 shrink-0 transition-transform", open && "rotate-90")} />
          <WrenchIcon className="size-3.5 shrink-0 text-muted-foreground" />
          <span className="shrink-0 font-mono font-medium">{name}</span>
          {view.summary && (
            <span className="min-w-0 truncate font-mono text-xs text-muted-foreground" title={view.summary}>
              {view.summary}
            </span>
          )}
          <span className="ml-auto flex shrink-0 items-center gap-2">
            {ev ? (
              <EvidenceBadge evidence={ev} settled={settled} className="hidden sm:inline-flex" />
            ) : (
              callConfirmed !== undefined && <Provenance confirmed={callConfirmed} className="hidden sm:inline-flex" />
            )}
            {error ? (
              <Badge variant="destructive">error</Badge>
            ) : result ? (
              <Badge variant="outline" className="border-emerald-200 bg-emerald-50 text-emerald-800">
                done
              </Badge>
            ) : pending ? (
              <Badge variant="outline" className="border-sky-200 bg-sky-50 text-sky-800">
                running
              </Badge>
            ) : (
              <Badge variant="outline">no result</Badge>
            )}
          </span>
        </CollapsibleTrigger>
        <CollapsibleContent className="min-w-0 space-y-2 border-t px-3 py-2">
          {call ? (
            <>
              <div className="flex flex-wrap items-center gap-x-2 text-[11px] text-muted-foreground">
                <span>Call {formatTime(call.created_at)}</span>
                <Provenance confirmed={callConfirmed!} />
              </div>
              {view.sections.map((s, i) => (
                <ArgBlock key={i} section={s} className="max-h-[28rem]" />
              ))}
            </>
          ) : (
            <p className="text-xs text-muted-foreground">The corresponding call is missing from the session file.</p>
          )}
          {result ? (
            <Section
              title={
                <span className="flex flex-wrap items-center gap-x-2">
                  <span className={cn(error && "text-red-700")}>{error ? "Error" : "Result"}</span>
                  <span className="font-normal">{formatTime(result.created_at)}</span>
                  <Provenance confirmed={resultConfirmed!} />
                </span>
              }
              text={result.payload?.text ?? ""}
              error={error}
              className="max-h-[28rem]"
            />
          ) : (
            <p className="text-xs text-muted-foreground">{pending ? "No result yet." : "No result in the session file."}</p>
          )}
          {ev && <EvidenceDetails evidence={ev} settled={settled} />}
        </CollapsibleContent>
      </Collapsible>
      <ArtifactAttachments chatId={chatId} artifacts={artifacts} />
    </div>
  )
}
