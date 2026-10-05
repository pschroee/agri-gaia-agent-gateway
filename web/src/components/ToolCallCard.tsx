import { useState } from "react"
import { ArrowDownToLineIcon, ChevronRightIcon, CircleStopIcon, FileTextIcon, Loader2Icon, TerminalSquareIcon, WrenchIcon } from "lucide-react"
import { toast } from "sonner"
import { api } from "@/api/client"
import type { BackgroundTask } from "@/api/types"
import { EvidenceBadge } from "@/components/badges"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { useNow } from "@/hooks/useNow"
import {
  type BackgroundCall,
  backgroundCall,
  backgroundCallLabel,
  backgroundStatus,
  formatBackgroundRuntime,
  tailLines,
  type Tone,
} from "@/lib/background"
import type { Evidence } from "@/lib/evidence"
import type { ToolCallBlock, ToolExecution } from "@/lib/stream"
import { formatBytes } from "@/lib/format"
import { preparingInfo } from "@/lib/preparing"
import { formatElapsed, formatStepDuration, toolDurationMs } from "@/lib/runtime"
import { type ArgSection, describeToolArgs } from "@/lib/toolargs"
import { cn } from "@/lib/utils"

type CardProps = {
  block: ToolCallBlock
  exec?: ToolExecution
  /** Matching against the orchestrator's log (E9). */
  evidence?: Evidence
  /** The run is finished; only then does missing evidence count as suspicious. */
  settled?: boolean
  /** Background tasks of the chat (for bash with run_in_background, bg_output, bg_stop). */
  background?: BackgroundTask[]
  /** Opens the "Background" tab with this task in focus. */
  onOpenBackground?: (id?: string) => void
  /** Chat of the call; with it, a running bash gets "Stop" and "Move to background". */
  chatId?: string
}

/**
 * Card of a tool call. Background tasks get their own form: bash with run_in_background shows the
 * task's ID, state and run time, bg_output and bg_stop a single line.
 */
export function ToolCallCard(props: CardProps) {
  const { block, exec, background } = props
  const name = block.name || exec?.toolName || ""
  const call =
    background && (block.arguments ?? exec?.args) !== undefined
      ? backgroundCall(name, block.arguments ?? exec?.args, block.id, exec?.result, background)
      : undefined
  // Failed start (e.g. limit reached): the ordinary card shows the error.
  if (call?.kind === "start" && exec?.isError !== true) return <BackgroundStartCard {...props} call={call} />
  if (call && call.kind !== "start") return <BackgroundCallLine {...props} call={call} />
  return <GenericToolCallCard {...props} />
}

function GenericToolCallCard({ block, exec, evidence, settled = true, chatId }: CardProps) {
  const [open, setOpen] = useState(false)
  const running = exec?.running ?? false
  const done = exec !== undefined && !running && exec.result !== undefined
  const error = exec?.isError === true
  const name = block.name || exec?.toolName || "tool"
  // The model is still writing the call (toolcall_delta): the arguments are half-finished JSON.
  const preparing = block.arguments === undefined && block.argsText !== undefined && exec === undefined
  const prep = preparing ? preparingInfo(name, block.argsText ?? "") : undefined
  const view = describeToolArgs(name, block.arguments ?? exec?.args, block.argsText)
  const summary = prep ? (prep.path ?? view.summary) : view.summary
  // Expanded while being prepared, so you see what is emerging; afterwards as chosen.
  const isOpen = preparing || open
  // Duration: live while running, from start and end when finished; after a reload without timestamps
  // the execution time measured by the orchestrator (E9), otherwise nothing.
  const live = running && exec?.startedAt !== undefined
  const now = useNow(1000, live)
  const measured = toolDurationMs(exec, now)
  const fallback = !running && measured === undefined && evidence?.executed && evidence.durationMs > 0 ? evidence.durationMs : undefined
  const took = running ? (measured !== undefined ? formatElapsed(measured) : "") : formatStepDuration(measured ?? fallback)
  const tookTitle = measured === undefined && fallback !== undefined ? "Execution time according to the orchestrator" : "Duration of the tool call"

  return (
    <Collapsible
      open={isOpen}
      onOpenChange={setOpen}
      className={cn("rounded-md border bg-background text-sm", error && "border-red-300")}
    >
      <div className="flex min-w-0 items-center hover:bg-muted/50">
        <CollapsibleTrigger className="flex w-full min-w-0 items-center gap-2 px-3 py-1.5 text-left">
          <ChevronRightIcon className={cn("size-3.5 shrink-0 transition-transform", isOpen && "rotate-90")} />
          <WrenchIcon className="size-3.5 shrink-0 text-muted-foreground" />
          <span className="font-mono font-medium">{name}</span>
          {summary && (
            <span className="min-w-0 truncate font-mono text-xs text-muted-foreground" title={summary}>
              {summary}
            </span>
          )}
          <span className="ml-auto flex shrink-0 items-center gap-1.5">
            <EvidenceBadge evidence={evidence} settled={settled} className="hidden sm:inline-flex" />
            {preparing ? (
              <Badge variant="outline" className="border-violet-200 bg-violet-50 text-violet-800" title="The model is writing this tool call right now">
                <Loader2Icon className="animate-spin" /> preparing · {formatBytes(prep!.bytes)}
                {prep!.lines > 1 ? ` · ${prep!.lines} lines` : ""}
              </Badge>
            ) : running ? (
              <Badge variant="outline" className="border-sky-200 bg-sky-50 text-sky-800">
                <Loader2Icon className="animate-spin" /> running
                {took && <span className="tabular-nums">· {took}</span>}
              </Badge>
            ) : error ? (
              <Badge variant="destructive" title={took ? tookTitle : undefined}>
                error{took && <span className="tabular-nums">· {took}</span>}
              </Badge>
            ) : done ? (
              <Badge variant="outline" className="border-emerald-200 bg-emerald-50 text-emerald-800" title={took ? tookTitle : undefined}>
                done{took && <span className="tabular-nums">· {took}</span>}
              </Badge>
            ) : (
              <Badge variant="outline">requested</Badge>
            )}
          </span>
        </CollapsibleTrigger>
        {running && name === "bash" && chatId && block.id && <ForegroundControls chatId={chatId} toolCallId={block.id} />}
      </div>
      <CollapsibleContent className="space-y-2 border-t px-3 py-2">
        {prep && (
          <Section title={prep.path ? `Writing: ${prep.path}` : "Writing"} text={prep.preview || "…"} />
        )}
        {!prep && view.sections.map((sec, i) => (
          <ArgBlock key={i} section={sec} />
        ))}
        {running && exec?.output !== undefined && <Section title="Output (live)" text={exec.output} />}
        {exec?.result !== undefined && <Section title={error ? "Error" : "Result"} text={exec.result} error={error} />}
        {evidence && <EvidenceDetails evidence={evidence} settled={settled} />}
      </CollapsibleContent>
    </Collapsible>
  )
}

/**
 * Buttons on a running bash: stop (the agent gets "Command stopped by the user" and keeps working)
 * or move to the background (the command keeps running as bg-N; the agent learns about it right
 * away and is notified when it ends).
 */
function ForegroundControls({ chatId, toolCallId }: { chatId: string; toolCallId: string }) {
  const [busy, setBusy] = useState<"stop" | "bg">()
  const act = async (kind: "stop" | "bg") => {
    setBusy(kind)
    try {
      if (kind === "stop") {
        await api.stopTool(chatId, toolCallId)
      } else {
        const t = await api.backgroundTool(chatId, toolCallId)
        toast.success(`Continues as ${t.id} in the background`)
      }
    } catch (e) {
      toast.error(`${kind === "stop" ? "Stopping" : "Moving to the background"} failed: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusy(undefined)
    }
  }
  return (
    <span className="flex shrink-0 items-center gap-0.5 pr-1.5">
      <Button
        size="icon-sm"
        variant="ghost"
        disabled={!!busy}
        title="Move to the background: keeps running, the agent keeps working and is notified when it ends"
        aria-label="Move to the background"
        onClick={() => void act("bg")}
      >
        {busy === "bg" ? <Loader2Icon className="animate-spin" /> : <ArrowDownToLineIcon />}
      </Button>
      <Button
        size="icon-sm"
        variant="ghost"
        disabled={!!busy}
        title="Stop command: the agent learns about it and keeps working"
        aria-label="Stop command"
        onClick={() => void act("stop")}
      >
        {busy === "stop" ? <Loader2Icon className="animate-spin" /> : <CircleStopIcon className="text-red-600" />}
      </Button>
    </span>
  )
}

const toneBadge: Record<Tone, string> = {
  running: "border-sky-200 bg-sky-50 text-sky-800",
  ok: "border-emerald-200 bg-emerald-50 text-emerald-800",
  error: "border-red-200 bg-red-50 text-red-800",
  muted: "",
}

/** ID of a background task as a link to the "Background" tab. */
function BgLink({ id, onOpen, short }: { id: string; onOpen?: (id?: string) => void; short?: boolean }) {
  const text = short ? id : `Background ${id}`
  if (!onOpen) {
    return (
      <Badge variant="outline" className="font-mono">
        {text}
      </Badge>
    )
  }
  return (
    <button
      type="button"
      onClick={() => onOpen(id)}
      className="inline-flex shrink-0 items-center gap-1 rounded-md border border-violet-200 bg-violet-50 px-1.5 py-0.5 text-xs text-violet-800 hover:bg-violet-100 dark:border-violet-800 dark:bg-violet-950 dark:text-violet-200"
      title={`Show ${id} in the Background tab`}
    >
      <TerminalSquareIcon className="size-3" />
      {short ? (
        <span className="font-mono">{id}</span>
      ) : (
        <>
          <span className="hidden sm:inline">Background</span>
          <span className="font-mono">{id}</span>
        </>
      )}
    </button>
  )
}

/** bash with run_in_background: ID, state and run time of the task; after the end, exit code and last lines. */
function BackgroundStartCard({ block, exec, evidence, settled = true, onOpenBackground, call }: CardProps & { call: BackgroundCall }) {
  const [open, setOpen] = useState(false)
  const task = call.task
  const running = task?.state === "running"
  const now = useNow(1000, running)
  const view = describeToolArgs("bash", block.arguments ?? exec?.args, block.argsText)
  const status = task ? backgroundStatus(task) : undefined
  const runtime = task ? formatBackgroundRuntime(task, now) : ""
  const starting = !task && (exec === undefined || exec.running)
  const lines = tailLines(task?.tail, 10)
  return (
    <Collapsible open={open} onOpenChange={setOpen} className="rounded-md border bg-background text-sm">
      <div className="flex min-w-0 items-center gap-1.5 pr-2 hover:bg-muted/50">
        <CollapsibleTrigger className="flex min-w-0 flex-1 items-center gap-2 py-1.5 pl-3 text-left">
          <ChevronRightIcon className={cn("size-3.5 shrink-0 transition-transform", open && "rotate-90")} />
          <WrenchIcon className="size-3.5 shrink-0 text-muted-foreground" />
          <span className="font-mono font-medium">bash</span>
          {view.summary && (
            <span className="min-w-0 truncate font-mono text-xs text-muted-foreground" title={view.summary}>
              {view.summary}
            </span>
          )}
        </CollapsibleTrigger>
        <EvidenceBadge evidence={evidence} settled={settled} className="hidden shrink-0 sm:inline-flex" />
        {call.id ? <BgLink id={call.id} onOpen={onOpenBackground} /> : null}
        {status ? (
          <Badge
            variant="outline"
            className={cn("shrink-0", toneBadge[status.tone])}
            title={task?.state === "running" ? "Running in the background; the agent is notified when it ends" : "State of the background task"}
          >
            {running && <Loader2Icon className="animate-spin" />}
            <span className="hidden sm:inline">{status.label}</span>
            <span className="sm:hidden">{running ? "running" : status.tone === "ok" ? "done" : status.tone === "error" ? "error" : "ended"}</span>
            {runtime && <span className="tabular-nums">· {runtime}</span>}
          </Badge>
        ) : starting ? (
          <Badge variant="outline" className="shrink-0 border-sky-200 bg-sky-50 text-sky-800">
            <Loader2Icon className="animate-spin" /> starting
          </Badge>
        ) : (
          <Badge variant="outline" className="shrink-0">
            started
          </Badge>
        )}
      </div>
      <CollapsibleContent className="space-y-2 border-t px-3 py-2">
        {view.sections.map((sec, i) => (
          <ArgBlock key={i} section={sec} />
        ))}
        {task && (
          <div className="min-w-0">
            <div className="mb-1 text-xs font-medium text-muted-foreground">
              {running ? "Last lines (live)" : "Last lines"}
              {task.output_lines > 0 && ` · ${task.output_lines} lines, ${formatBytes(task.output_bytes)}`}
              {!running && task.exit_code !== undefined && ` · exit ${task.exit_code}`}
              {task.error && !running ? ` · ${task.error}` : ""}
            </div>
            <pre className="max-h-60 overflow-auto rounded bg-muted p-2 font-mono text-xs whitespace-pre-wrap break-all">
              {lines.length ? lines.join("\n") : running ? "No output yet." : "No output."}
            </pre>
            <div className="mt-1 font-mono text-[11px] break-all text-muted-foreground" title="Full output in the execution sandbox">
              {task.log_path}
            </div>
          </div>
        )}
        {exec?.result !== undefined && <Section title="Feedback to the model" text={exec.result} />}
        {evidence && <EvidenceDetails evidence={evidence} settled={settled} />}
      </CollapsibleContent>
    </Collapsible>
  )
}

/** bg_output and bg_stop as one line ("fetched output of bg-3", "stopped bg-3"), expandable. */
function BackgroundCallLine({ exec, evidence, settled = true, onOpenBackground, call }: CardProps & { call: BackgroundCall }) {
  const [open, setOpen] = useState(false)
  const error = exec?.isError === true
  const phase = error ? "error" : exec?.result !== undefined && !exec.running ? "done" : "running"
  const label = backgroundCallLabel(call, phase)
  const Icon = call.kind === "stop" ? CircleStopIcon : FileTextIcon
  return (
    <Collapsible open={open} onOpenChange={setOpen} className="min-w-0 text-xs text-muted-foreground">
      <div className="flex min-w-0 items-center gap-1.5">
        <CollapsibleTrigger className="flex min-w-0 flex-1 items-center gap-1.5 py-0.5 text-left hover:text-foreground">
          <ChevronRightIcon className={cn("size-3 shrink-0 transition-transform", open && "rotate-90")} />
          {phase === "running" ? <Loader2Icon className="size-3 shrink-0 animate-spin" /> : <Icon className="size-3 shrink-0" />}
          <span className={cn("min-w-0 truncate", error && "text-red-700 dark:text-red-400")}>{label}</span>
          <span className="shrink-0 font-mono text-[11px] opacity-70">{call.kind === "stop" ? "bg_stop" : "bg_output"}</span>
        </CollapsibleTrigger>
        {call.id && <BgLink id={call.id} onOpen={onOpenBackground} short />}
      </div>
      <CollapsibleContent className="mt-1 ml-4.5 space-y-1.5">
        {exec?.result !== undefined ? (
          <Section title={error ? "Error" : "Feedback to the model"} text={exec.result} error={error} />
        ) : (
          <p>No feedback yet.</p>
        )}
        {evidence && <EvidenceDetails evidence={evidence} settled={settled} />}
      </CollapsibleContent>
    </Collapsible>
  )
}

export function Section({
  title,
  text,
  error,
  className,
}: {
  title: React.ReactNode
  text: string
  error?: boolean
  /** Additional classes for the text block, e.g. a larger maximum height. */
  className?: string
}) {
  return (
    <div className="min-w-0">
      <div className="mb-1 text-xs font-medium text-muted-foreground">{title}</div>
      <pre
        className={cn(
          "max-h-72 overflow-auto rounded bg-muted p-2 font-mono text-xs whitespace-pre-wrap break-all",
          error && "bg-red-50 text-red-800",
          className,
        )}
      >
        {text || "(empty)"}
      </pre>
    </div>
  )
}

export function ArgBlock({ section, className }: { section: ArgSection; className?: string }) {
  if (section.kind === "path") {
    return (
      <div className="flex min-w-0 items-baseline gap-2">
        <span className="shrink-0 text-xs font-medium text-muted-foreground">{section.label}</span>
        <code className="min-w-0 rounded bg-sky-50 px-1.5 py-0.5 font-mono text-xs font-semibold break-all text-sky-900">
          {section.text}
        </code>
      </div>
    )
  }
  return (
    <div className="min-w-0">
      <div className="mb-1 text-xs font-medium text-muted-foreground">{section.label}</div>
      <pre
        className={cn(
          "max-h-72 overflow-auto rounded p-2 font-mono text-xs",
          section.kind === "code" ? "bg-zinc-900 whitespace-pre text-zinc-100" : "bg-muted whitespace-pre-wrap break-all",
          className,
        )}
      >
        {section.text || "(empty)"}
      </pre>
    </div>
  )
}

/** What the orchestrator logged for this call (E9). */
export function EvidenceDetails({ evidence, settled }: { evidence: Evidence; settled: boolean }) {
  if (evidence.state === "internal") return null
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-[11px] text-muted-foreground">
      <EvidenceBadge evidence={evidence} settled={settled} />
      {evidence.executed ? (
        <span>
          Orchestrator: {evidence.ops.join(", ")} · {evidence.durationMs} ms
          {evidence.exitCode !== undefined ? ` · exit ${evidence.exitCode}` : ""}
          {evidence.error ? ` · ${evidence.error}` : ""}
        </span>
      ) : (
        <span>no execution in the orchestrator's log</span>
      )}
      <span className="font-mono">{evidence.toolCallId}</span>
    </div>
  )
}
