// Task list of the agent (tool todo): list, progress in the chat header, card in the history.
import { useState } from "react"
import { CheckIcon, ChevronDownIcon, ChevronRightIcon, CircleIcon, ListTodoIcon, Loader2Icon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import {
  statusLabel,
  type Task,
  taskCountLabel,
  taskCounts,
  type TodoCall,
  todoGroupSummary,
  visibleTasks,
} from "@/lib/tasks"
import { cn } from "@/lib/utils"

function StatusIcon({ status, className }: { status: Task["status"]; className?: string }) {
  const cls = cn("size-3.5 shrink-0", className)
  if (status === "completed")
    return (
      <span className={cn(cls, "inline-flex items-center justify-center rounded-full bg-emerald-600 text-white")}>
        <CheckIcon className="size-2.5" strokeWidth={3} />
      </span>
    )
  if (status === "in_progress") return <Loader2Icon className={cn(cls, "animate-spin text-sky-600")} />
  return <CircleIcon className={cn(cls, "text-muted-foreground")} />
}

/** List of tasks; `detailed` shows description, dependencies and owner. */
export function TaskList({ tasks, detailed, className }: { tasks: Task[]; detailed?: boolean; className?: string }) {
  const list = visibleTasks(tasks)
  if (list.length === 0) return <p className="text-xs text-muted-foreground">No tasks.</p>
  return (
    <ul className={cn("flex min-w-0 flex-col gap-1", className)}>
      {list.map((t) => (
        <li key={t.id} className="flex min-w-0 items-start gap-2" title={statusLabel[t.status]}>
          <StatusIcon status={t.status} className="mt-0.5" />
          <div className="min-w-0 flex-1">
            <div
              className={cn(
                "break-words",
                t.status === "completed" && "text-muted-foreground line-through",
                t.status === "in_progress" && "font-medium",
              )}
            >
              <span className="mr-1 text-muted-foreground">#{t.id}</span>
              {t.subject}
            </div>
            {t.status === "in_progress" && t.activeForm && (
              <div className="text-xs break-words text-sky-700 dark:text-sky-400">{t.activeForm} …</div>
            )}
            {detailed && t.description && (
              <div className="mt-0.5 text-xs break-words whitespace-pre-wrap text-muted-foreground">{t.description}</div>
            )}
            {detailed && (t.blockedBy?.length || t.owner) && (
              <div className="mt-0.5 text-xs text-muted-foreground">
                {t.blockedBy?.length ? `waiting for ${t.blockedBy.map((id) => `#${id}`).join(", ")}` : ""}
                {t.blockedBy?.length && t.owner ? " · " : ""}
                {t.owner ? `Owner: ${t.owner}` : ""}
              </div>
            )}
          </div>
        </li>
      ))}
    </ul>
  )
}

/** Small progress ring (done / total). */
function ProgressRing({ done, total, className }: { done: number; total: number; className?: string }) {
  const r = 6
  const c = 2 * Math.PI * r
  const frac = total > 0 ? done / total : 0
  return (
    <svg viewBox="0 0 16 16" className={cn("size-3.5 shrink-0 -rotate-90", className)} aria-hidden>
      <circle cx="8" cy="8" r={r} fill="none" strokeWidth="2.5" className="stroke-emerald-600/20" />
      <circle
        cx="8"
        cy="8"
        r={r}
        fill="none"
        strokeWidth="2.5"
        strokeLinecap="round"
        strokeDasharray={`${c * frac} ${c}`}
        className="stroke-emerald-600"
      />
    </svg>
  )
}

/** Progress "3/7 tasks" in the chat header; the list opens only on click. */
export function TasksMenu({ tasks }: { tasks: Task[] }) {
  const c = taskCounts(tasks)
  if (c.total === 0) return null
  const label = taskCountLabel(c)
  return (
    <Popover>
      <PopoverTrigger
        className="inline-flex shrink-0 items-center gap-1 rounded-md border border-emerald-200 bg-emerald-50/60 px-2 py-0.5 text-xs font-medium text-emerald-900 hover:bg-emerald-100 data-[state=open]:bg-emerald-100 dark:border-emerald-900 dark:bg-emerald-950/40 dark:text-emerald-200"
        aria-label={`${label}${c.inProgress ? `, ${c.inProgress} in progress` : ""}: open list`}
      >
        {c.inProgress > 0 ? <Loader2Icon className="size-3.5 animate-spin text-sky-600" /> : <ProgressRing done={c.completed} total={c.total} />}
        {label}
        <ChevronDownIcon className="size-3.5" />
      </PopoverTrigger>
      <PopoverContent align="start" collisionPadding={12} className="max-h-[70vh] w-80 max-w-[calc(100vw-1.5rem)] overflow-y-auto">
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          <ProgressRing done={c.completed} total={c.total} />
          Agent tasks · {c.completed} of {c.total} done
        </div>
        <TaskList tasks={tasks} />
      </PopoverContent>
    </Popover>
  )
}

/** Content of the "Tasks" side tab. */
export function TasksPanel({ tasks }: { tasks: Task[] }) {
  const c = taskCounts(tasks)
  if (c.total === 0)
    return (
      <p className="text-xs text-muted-foreground">
        The agent has not created a task list. For work with several steps it keeps one with the tool todo.
      </p>
    )
  return (
    <div className="flex flex-col gap-3 text-sm">
      <div className="flex items-center gap-2 text-xs text-muted-foreground">
        <ProgressRing done={c.completed} total={c.total} className="size-4" />
        {c.completed} of {c.total} done
        {c.inProgress ? ` · ${c.inProgress} in progress` : ""}
        {c.pending ? ` · ${c.pending} open` : ""}
      </div>
      <TaskList tasks={tasks} detailed />
    </div>
  )
}

/** Compact card for consecutive todo calls in the history, with an expandable list. */
export function TodoCallGroup({ calls }: { calls: TodoCall[] }) {
  const [open, setOpen] = useState(false)
  if (calls.length === 0) return null
  const last = calls[calls.length - 1]
  const running = calls.some((c) => c.running)
  const failed = calls.filter((c) => c.error).length
  const summary = running ? "Updating tasks …" : todoGroupSummary(calls.map((c) => c.action), last.after)
  const changes = calls.map((c) => c.change)
  return (
    <Collapsible open={open} onOpenChange={setOpen} className={cn("rounded-md border bg-background text-sm", failed && "border-amber-300")}>
      <CollapsibleTrigger className="flex w-full min-w-0 items-center gap-2 px-3 py-1.5 text-left hover:bg-muted/50">
        <ChevronRightIcon className={cn("size-3.5 shrink-0 transition-transform", open && "rotate-90")} />
        <ListTodoIcon className="size-3.5 shrink-0 text-emerald-700" />
        <span className="min-w-0 truncate" title={changes.join("\n")}>
          {summary}
        </span>
        <span className="ml-auto shrink-0">
          {running ? (
            <Loader2Icon className="size-3.5 animate-spin text-sky-600" />
          ) : failed ? (
            <Badge variant="outline" className="border-amber-300 bg-amber-50 text-amber-800">
              {failed} refused
            </Badge>
          ) : null}
        </span>
      </CollapsibleTrigger>
      <CollapsibleContent className="space-y-2 border-t px-3 py-2">
        <ul className="flex flex-col gap-0.5 text-xs text-muted-foreground">
          {changes.map((ch, i) => (
            <li key={i} className={cn("break-words", calls[i].error && "text-amber-800")}>
              {ch}
            </li>
          ))}
        </ul>
        <TaskList tasks={last.after} />
      </CollapsibleContent>
    </Collapsible>
  )
}
