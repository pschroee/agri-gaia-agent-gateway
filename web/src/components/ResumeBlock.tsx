import { useState } from "react"
import { CheckIcon, ChevronRightIcon, CircleIcon, Loader2Icon, RotateCcwIcon, TriangleAlertIcon, XIcon } from "lucide-react"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { formatMs } from "@/lib/format"
import { resumeSummary, stepDetail, stepLabel } from "@/lib/resume"
import type { ResumeItem, ResumeStepView } from "@/lib/stream"
import { cn } from "@/lib/utils"

/**
 * Resuming an idle chat in the history: while it runs (or fails), a block with the steps live;
 * afterwards a line that can be expanded again.
 */
export function ResumeBlock({ item }: { item: ResumeItem }) {
  const [open, setOpen] = useState(false)
  if (item.state === "done") {
    return (
      <Collapsible open={open} onOpenChange={setOpen} className="text-xs">
        <div className="flex items-center gap-2">
          <div className="h-px flex-1 bg-border" />
          <CollapsibleTrigger className="flex max-w-[85%] min-w-0 items-center gap-1.5 rounded-full border bg-muted/60 px-2.5 py-1 text-muted-foreground hover:text-foreground">
            <RotateCcwIcon className="size-3 shrink-0" />
            <span className="min-w-0 truncate">{resumeSummary(item)}</span>
            <ChevronRightIcon className={cn("size-3 shrink-0 transition-transform", open && "rotate-90")} />
          </CollapsibleTrigger>
          <div className="h-px flex-1 bg-border" />
        </div>
        <CollapsibleContent>
          <div className="mx-auto mt-2 max-w-md rounded-md border bg-muted/30 px-3 py-2">
            <Steps steps={item.steps} />
          </div>
        </CollapsibleContent>
      </Collapsible>
    )
  }
  const failed = item.state === "failed"
  return (
    <div
      role="status"
      aria-live="polite"
      className={cn(
        "mx-auto w-full max-w-md rounded-lg border px-3 py-2.5 text-xs",
        failed ? "border-red-200 bg-red-50 text-red-900 dark:border-red-900 dark:bg-red-950/40 dark:text-red-200" : "bg-muted/40",
      )}
    >
      <div className="flex min-w-0 items-center gap-2 text-sm font-medium">
        {failed ? (
          <XIcon className="size-4 shrink-0" />
        ) : (
          <Loader2Icon className="size-4 shrink-0 animate-spin text-muted-foreground" />
        )}
        <span className="min-w-0 break-words">{resumeSummary(item)}</span>
      </div>
      {!failed && (
        <p className="mt-0.5 text-muted-foreground">The chat was idle; the sandbox is being rebuilt.</p>
      )}
      <div className="mt-2">
        <Steps steps={item.steps} />
      </div>
      {failed && (
        <p className="mt-2">
          The message was not sent and is not lost: it is back in the input field and can be sent
          again.
        </p>
      )}
    </div>
  )
}

function Steps({ steps }: { steps: ResumeStepView[] }) {
  return (
    <ol className="flex flex-col gap-1">
      {steps.map((s) => {
        const detail = stepDetail(s)
        const finished = s.status !== "pending" && s.status !== "running"
        return (
          <li
            key={s.phase}
            className={cn("flex min-w-0 items-start gap-2", s.status === "pending" && "text-muted-foreground/70")}
            title={s.phase === "acquire" && s.detail ? `Slot ${s.detail}` : undefined}
          >
            <StepIcon status={s.status} />
            <span className="min-w-0 flex-1 break-words">
              {stepLabel(s.phase)}
              {detail && (
                <span className={cn(s.status === "done" ? "text-muted-foreground" : "font-medium")}> · {detail}</span>
              )}
            </span>
            {finished && s.ms !== undefined && (
              <span className="shrink-0 text-muted-foreground tabular-nums">{formatMs(s.ms)}</span>
            )}
          </li>
        )
      })}
    </ol>
  )
}

function StepIcon({ status }: { status: ResumeStepView["status"] }) {
  const cls = "mt-px size-3.5 shrink-0"
  switch (status) {
    case "running":
      return <Loader2Icon className={cn(cls, "animate-spin text-sky-600")} aria-label="running" />
    case "done":
      return <CheckIcon className={cn(cls, "text-emerald-600")} aria-label="done" />
    case "warning":
      return <TriangleAlertIcon className={cn(cls, "text-amber-600")} aria-label="with warning" />
    case "error":
      return <XIcon className={cn(cls, "text-red-600")} aria-label="failed" />
    default:
      return <CircleIcon className={cn(cls, "text-muted-foreground/40")} aria-label="pending" />
  }
}
