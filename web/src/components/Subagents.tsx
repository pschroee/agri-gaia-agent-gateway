import { useState } from "react"
import { BotIcon, CheckIcon, ChevronRightIcon, Loader2Icon, MaximizeIcon, WrenchIcon } from "lucide-react"
import type { SubagentEntry } from "@/api/types"
import { Markdown } from "@/components/Markdown"
import { ArgBlock } from "@/components/ToolCallCard"
import { Badge } from "@/components/ui/badge"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { EvidenceBadge } from "@/components/badges"
import type { Evidence } from "@/lib/evidence"
import { formatTime } from "@/lib/format"
import {
  clipText,
  isEntryConfirmed,
  parseArguments,
  runLabel,
  runStatusLabel,
  type RunStatus,
  type SubagentRun,
} from "@/lib/subagents"
import { describeToolArgs } from "@/lib/toolargs"
import { cn } from "@/lib/utils"

const SANDBOX_HINT =
  "This information comes from the subagent's session file in the pi container. Since E9 the agent can no longer reach it; still, it is only confirmed by the proxy or the orchestrator's log"

/** Provenance of an entry: confirmed at the proxy or only from the sandbox. */
export function Provenance({ confirmed, className }: { confirmed: boolean; className?: string }) {
  if (confirmed) {
    return (
      <span
        className={cn("inline-flex items-center gap-0.5 text-[10px] text-emerald-700", className)}
        title="The corresponding model response is recorded at the LLM proxy (outside the sandbox)"
      >
        <CheckIcon className="size-3" /> confirmed at proxy
      </span>
    )
  }
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span tabIndex={0} className={cn("cursor-help text-[10px] text-muted-foreground/80 italic", className)}>
          sandbox only
        </span>
      </TooltipTrigger>
      <TooltipContent>{SANDBOX_HINT}</TooltipContent>
    </Tooltip>
  )
}

function EntryHead({
  label,
  entry,
  proxyIds,
  evidence,
}: {
  label: React.ReactNode
  entry: SubagentEntry
  proxyIds: Set<string>
  /** Evidence from the orchestrator's log (E9); replaces the provenance for tool calls. */
  evidence?: { ev?: Evidence; settled: boolean }
}) {
  return (
    <div className="mb-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs">
      <span className="font-medium text-muted-foreground">{label}</span>
      {evidence?.ev ? (
        <EvidenceBadge evidence={evidence.ev} settled={evidence.settled} className="ml-auto" />
      ) : (
        <Provenance confirmed={isEntryConfirmed(entry, proxyIds)} className="ml-auto" />
      )}
    </div>
  )
}

function ResultText({ text, error }: { text: string; error: boolean }) {
  const [full, setFull] = useState(false)
  const c = clipText(text)
  return (
    <div>
      <pre
        className={cn(
          "max-h-72 overflow-auto rounded bg-muted p-2 font-mono text-xs break-all whitespace-pre-wrap",
          error && "bg-red-50 text-red-800",
        )}
      >
        {(full ? text : c.text) || "(empty)"}
      </pre>
      {c.clipped && (
        <button type="button" className="mt-0.5 text-[11px] text-sky-700 hover:underline" onClick={() => setFull((v) => !v)}>
          {full ? "show less" : "show all"}
        </button>
      )}
    </div>
  )
}

function Entry({
  entry,
  proxyIds,
  evidence,
  settled,
}: {
  entry: SubagentEntry
  proxyIds: Set<string>
  evidence?: Map<string, Evidence>
  settled: boolean
}) {
  const p = entry.payload ?? {}
  const ev = entry.kind === "tool_call" && p.id ? evidence?.get(p.id) : undefined
  switch (entry.kind) {
    case "task":
      return (
        <div>
          <EntryHead label="Task" entry={entry} proxyIds={proxyIds} />
          <div className="rounded border-l-2 border-violet-300 bg-violet-50/60 px-2 py-1 text-xs break-words whitespace-pre-wrap">
            {p.text || "(empty)"}
          </div>
        </div>
      )
    case "tool_call": {
      const name = p.name || "tool"
      const view = describeToolArgs(name, parseArguments(p.arguments))
      return (
        <div>
          <EntryHead
            label={
              <span className="inline-flex items-center gap-1">
                <WrenchIcon className="size-3" /> Tool call <span className="font-mono text-foreground">{name}</span>
              </span>
            }
            entry={entry}
            proxyIds={proxyIds}
            evidence={ev ? { ev, settled } : undefined}
          />
          <div className="space-y-1.5">
            {view.sections.map((s, i) => (
              <ArgBlock key={i} section={s} />
            ))}
          </div>
        </div>
      )
    }
    case "tool_result": {
      const error = p.is_error === true
      return (
        <div>
          <EntryHead
            label={
              <span className={cn(error && "text-red-700")}>
                {error ? "Error" : "Result"}
                {p.name && <span className="ml-1 font-mono">{p.name}</span>}
              </span>
            }
            entry={entry}
            proxyIds={proxyIds}
          />
          <ResultText text={p.text ?? ""} error={error} />
        </div>
      )
    }
    case "text":
      return (
        <div>
          <EntryHead label="Response" entry={entry} proxyIds={proxyIds} />
          <Markdown text={p.text ?? ""} className="text-xs" />
        </div>
      )
    default:
      return null
  }
}

type RunProps = {
  run: SubagentRun
  proxyIds: Set<string>
  /** Matching against the orchestrator's log (E9). */
  evidence?: Map<string, Evidence>
  settled?: boolean
  defaultOpen?: boolean
  className?: string
  /** Link to the run's own view. */
  href?: string
}

/** Expandable group of a subagent run with task, tool calls, results and text. */
export function SubagentRunView({ run, proxyIds, evidence, settled = true, defaultOpen = false, className, href }: RunProps) {
  const [open, setOpen] = useState(defaultOpen)
  const confirmed = run.entries.filter((e) => isEntryConfirmed(e, proxyIds)).length
  return (
    <Collapsible
      open={open}
      onOpenChange={setOpen}
      className={cn("rounded-md border border-violet-200 bg-violet-50/30 text-sm", run.errors > 0 && "border-red-200", className)}
    >
      <div className="flex min-w-0 items-center">
        <CollapsibleTrigger className="flex min-w-0 flex-1 items-center gap-2 px-3 py-1.5 text-left hover:bg-violet-50">
          <ChevronRightIcon className={cn("size-3.5 shrink-0 transition-transform", open && "rotate-90")} />
          <BotIcon className="size-3.5 shrink-0 text-violet-700" />
          <span className="min-w-0 truncate font-medium sm:shrink-0" title={run.runId}>
            {runLabel(run)}
          </span>
          {!open && run.task && (
            <span className="hidden min-w-0 truncate text-xs text-muted-foreground sm:inline" title={run.task}>
              {run.task}
            </span>
          )}
          <span className="ml-auto shrink-0 text-[11px] text-muted-foreground tabular-nums">
            {run.toolCalls}
            <span className="hidden sm:inline"> call{run.toolCalls === 1 ? "" : "s"}</span>
            {run.errors > 0 && <span className="text-red-700"> · {run.errors} error{run.errors === 1 ? "" : "s"}</span>}
          </span>
        </CollapsibleTrigger>
        {href && (
          <a
            href={href}
            className="mr-1.5 inline-flex shrink-0 items-center gap-1 rounded-md border border-violet-300 bg-background px-2 py-0.5 text-xs font-medium text-violet-800 hover:bg-violet-100"
            title="Open run in its own view"
            aria-label={`Open ${runLabel(run)}`}
          >
            <MaximizeIcon className="size-3" /> Open
          </a>
        )}
      </div>
      <CollapsibleContent className="space-y-2.5 border-t border-violet-200 px-3 py-2">
        <p className="text-[11px] text-muted-foreground">
          {formatTime(new Date(run.start).toISOString())} · {run.entries.length} entries, {confirmed} of them confirmed at the proxy.
          The entries come from the subagent's session file; tool calls are confirmed through the orchestrator's
          log.
        </p>
        {run.entries.map((e) => (
          <Entry key={e.entry_id} entry={e} proxyIds={proxyIds} evidence={evidence} settled={settled} />
        ))}
      </CollapsibleContent>
    </Collapsible>
  )
}

const statusTone: Record<RunStatus, string> = {
  running: "border-sky-200 bg-sky-50 text-sky-800",
  idle: "border-amber-200 bg-amber-50 text-amber-900",
  done: "border-emerald-200 bg-emerald-50 text-emerald-800",
  stopped: "border-border bg-muted text-muted-foreground",
}

const statusHint: Record<RunStatus, string> = {
  running: "The chat is working, and the run delivered entries recently",
  idle: "The chat is working, but the run has delivered nothing for a while (e.g. a long command)",
  done: "The run ends with a text response",
  stopped: "The chat is no longer working, and the run ends without a text response",
}

export function RunStatusBadge({ status, className }: { status: RunStatus; className?: string }) {
  return (
    <Badge variant="outline" className={cn(statusTone[status], className)} title={`${statusHint[status]} (derived from the entries)`}>
      {status === "running" && <Loader2Icon className="animate-spin" />}
      {runStatusLabel(status)}
    </Badge>
  )
}
