// Overview of the subagents: dropdown in the chat header, switcher in the breadcrumbs of the detail view and
// tree ("main agent" with the runs below it) in the side tab.
import { useState } from "react"
import { ArrowLeftIcon, BotIcon, ChevronDownIcon, ChevronRightIcon, ChevronsUpDownIcon } from "lucide-react"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { chatHref, subagentHref } from "@/hooks/useHashRoute"
import { useAgentTree, type AgentTreeInput } from "@/hooks/useAgentTree"
import { formatUsd } from "@/lib/format"
import {
  containsRun,
  flattenAgentTree,
  formatSpan,
  formatTokensShort,
  groupAgentNodes,
  statusCountsLabel,
  subagentCountLabel,
  type AgentNode,
  type RunMetrics,
  type StatusCounts,
} from "@/lib/subagent-overview"
import { runStatusLabel, type RunStatus } from "@/lib/subagents"
import { cn } from "@/lib/utils"

const dotTone: Record<RunStatus, string> = {
  running: "bg-sky-500",
  idle: "bg-amber-500",
  done: "bg-emerald-500",
  stopped: "bg-muted-foreground/50",
}

export function StatusDot({ status, className }: { status: RunStatus; className?: string }) {
  return (
    <span className={cn("relative inline-flex size-2 shrink-0", className)} aria-hidden>
      {status === "running" && <span className="absolute inset-0 animate-ping rounded-full bg-sky-400 opacity-60" />}
      <span className={cn("relative inline-flex size-2 rounded-full", dotTone[status])} />
    </span>
  )
}

function metricsTitle(m: RunMetrics): string {
  const parts = [`${m.toolCalls} tool call${m.toolCalls === 1 ? "" : "s"}`]
  if (m.errors) parts.push(`${m.errors} error${m.errors === 1 ? "" : "s"}`)
  if (m.tokens !== undefined) {
    const calls = `${m.llmCalls} model call${m.llmCalls === 1 ? "" : "s"} at the proxy`
    parts.push(`${m.input} in · ${m.output} out · ${m.cacheRead} from cache (${calls})`)
  }
  if (m.cost !== undefined) parts.push(formatUsd(m.cost))
  return parts.join(" · ")
}

/** Short form: tokens if matched at the proxy, otherwise the number of tool calls. */
function metricsShort(m: RunMetrics): string {
  if (m.tokens !== undefined) return `${formatTokensShort(m.tokens)} tokens`
  return `${m.toolCalls} call${m.toolCalls === 1 ? "" : "s"}`
}

function MenuRows({ chatId, root, activeRunId, withMain }: { chatId: string; root: AgentNode; activeRunId?: string; withMain?: boolean }) {
  return (
    <>
      {withMain && (
        <DropdownMenuItem asChild className="items-start gap-2 py-1.5">
          <a
            href={chatHref(chatId)}
            aria-current={!activeRunId ? "page" : undefined}
            className={cn(!activeRunId && "bg-accent/60")}
          >
            <BotIcon className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
            <span className="min-w-0 flex-1">
              <span className="block truncate font-medium" title={root.title}>
                Main agent
              </span>
              <span className="block truncate text-xs text-muted-foreground">
                {root.title} · {runStatusLabel(root.status)}
              </span>
            </span>
            <ChevronRightIcon className="mt-1.5 size-3.5 text-muted-foreground" />
          </a>
        </DropdownMenuItem>
      )}
      {flattenAgentTree(root).map(({ node: n, depth }) => (
        <DropdownMenuItem key={n.id} asChild className="items-start gap-2 py-1.5">
          <a
            href={subagentHref(chatId, n.runId!)}
            aria-current={n.runId === activeRunId ? "page" : undefined}
            className={cn(n.runId === activeRunId && "bg-accent/60")}
            style={depth ? { paddingLeft: `${0.375 + depth * 0.875}rem` } : undefined}
          >
            <StatusDot status={n.status} className="mt-1.5" />
            <span className="min-w-0 flex-1">
              <span className="block truncate font-medium" title={n.title}>
                {n.title}
              </span>
              <span className="block truncate text-xs text-muted-foreground">
                {n.subtitle} · {runStatusLabel(n.status)}
                {n.metrics.errors > 0 && <span className="text-red-700"> · {n.metrics.errors} error{n.metrics.errors === 1 ? "" : "s"}</span>}
              </span>
            </span>
            <span className="shrink-0 text-right text-[11px] leading-4 text-muted-foreground tabular-nums" title={metricsTitle(n.metrics)}>
              <span className="block">{metricsShort(n.metrics)}</span>
              <span className="block">{formatSpan(n.durationMs)}</span>
            </span>
            <ChevronRightIcon className="mt-1.5 size-3.5 text-muted-foreground" />
          </a>
        </DropdownMenuItem>
      ))}
    </>
  )
}

const menuContent = "w-[26rem] max-w-[calc(100vw-1.5rem)]"

/** Chat header: "2 subagents ⌄" with the list of runs; nothing without runs. */
type MenuProps = { chatId: string; tree: AgentTreeInput; activeRunId?: string }

export function SubagentsMenu({ chatId, tree, activeRunId }: MenuProps) {
  const root = useAgentTree(tree)
  const count = flattenAgentTree(root).length
  if (count === 0) return null
  const running = flattenAgentTree(root).filter((x) => x.node.status === "running").length
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        className="inline-flex shrink-0 items-center gap-1 rounded-md border border-violet-200 bg-violet-50/60 px-2 py-0.5 text-xs font-medium text-violet-900 hover:bg-violet-100 data-[state=open]:bg-violet-100"
        aria-label={`${subagentCountLabel(count)}${running ? `, ${running} running` : ""}: open list`}
      >
        {running > 0 && <StatusDot status="running" />}
        {subagentCountLabel(count)}
        <ChevronDownIcon className="size-3.5" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" collisionPadding={12} className={menuContent}>
        <DropdownMenuLabel className="text-xs text-muted-foreground">Subagents of this chat</DropdownMenuLabel>
        <MenuRows chatId={chatId} root={root} activeRunId={activeRunId} />
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

/** Breadcrumbs of the detail view: the run's title as a switcher to the other runs. */
export function SubagentSwitcher({
  chatId,
  root,
  runId,
  title,
}: {
  chatId: string
  root: AgentNode
  /** Open subagent; without it, the main agent is open. */
  runId?: string
  title: string
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        className="inline-flex max-w-full min-w-0 items-center gap-1 rounded-md px-1.5 py-0.5 font-medium hover:bg-muted data-[state=open]:bg-muted"
        aria-label={`${title}: switch to another subagent`}
        title={title}
      >
        <span className="min-w-0 truncate">{title}</span>
        <ChevronsUpDownIcon className="size-3.5 shrink-0 text-muted-foreground" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" collisionPadding={12} className={menuContent}>
        <DropdownMenuLabel className="text-xs text-muted-foreground">Switch to the main agent or a subagent</DropdownMenuLabel>
        <MenuRows chatId={chatId} root={root} activeRunId={runId} withMain />
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

type TreeProps = {
  chatId: string
  tree: AgentTreeInput
  /** Run of the currently open detail view. */
  activeRunId?: string
  /** After a click on a link, e.g. to close the side sheet. */
  onNavigate?: () => void
}

/** "Subagents" side tab: card of the main agent, below it the runs connected by dashed lines. */
export function AgentTaskTree({ chatId, tree, activeRunId, onNavigate }: TreeProps) {
  const root = useAgentTree(tree)
  const count = flattenAgentTree(root).length
  return (
    <nav aria-label="Subagent runs" className="flex min-w-0 flex-col text-sm">
      <a
        href={chatHref(chatId)}
        onClick={onNavigate}
        aria-current={!activeRunId ? "page" : undefined}
        title="To the main agent's history"
        className={cn(
          "block min-w-0 rounded-lg border bg-background px-2.5 py-1.5 hover:bg-muted/60",
          !activeRunId && "border-foreground/20 bg-muted/50",
        )}
      >
        {/* Same layout as the subagent cards: dot and title, below it who and in which state. */}
        <div className="flex min-w-0 items-start gap-1.5">
          <StatusDot status={root.status} className="mt-1.5" />
          <span className="line-clamp-2 min-w-0 flex-1 text-xs font-medium break-words" title={root.title}>
            {root.title}
          </span>
        </div>
        <div className="mt-0.5 flex items-center gap-1 pl-3.5 text-[11px] text-muted-foreground">
          <BotIcon className="size-3 shrink-0" />
          <span>Main agent</span>
          <span aria-hidden>·</span>
          <span className={cn(root.status === "running" && "text-sky-700")}>
            {root.status === "running" ? "running" : "waiting for you"}
          </span>
        </div>
      </a>
      {root.children.length === 0 ? (
        <p className="mt-2 pl-4 text-xs text-muted-foreground">
          No subagents yet. Entries appear about every 2 s once a subagent is working.
        </p>
      ) : (
        <NodeList nodes={root.children} chatId={chatId} activeRunId={activeRunId} onNavigate={onNavigate} />
      )}
      {count > 0 && (
        <p className="mt-3 text-[11px] text-muted-foreground">
          {count} run{count === 1 ? "" : "s"}. Contents come from the sandbox; tokens and cost come from the proxy and
          are matched via the response ID. Name and status are reported by pi-subagents.
        </p>
      )}
    </nav>
  )
}

type ListProps = { nodes: AgentNode[]; chatId: string; activeRunId?: string; onNavigate?: () => void }

/** One level of the tree: vertical dashed line, a horizontal connector per card. */
function NodeList(props: ListProps) {
  const g = groupAgentNodes(props.nodes)
  return (
    <ul className="ml-3 flex min-w-0 flex-col gap-2 border-l-2 border-dashed border-violet-300 pt-2 pl-3">
      {g.type === "nodes" ? (
        g.nodes.map((n) => <NodeItem key={n.id} node={n} {...props} />)
      ) : (
        <GroupItem {...props} counts={g.counts} />
      )}
    </ul>
  )
}

const connector =
  "relative before:absolute before:top-4 before:-left-3 before:w-3 before:border-t-2 before:border-dashed before:border-violet-300"

function NodeItem({ node, ...props }: ListProps & { node: AgentNode }) {
  return (
    <li className={cn("min-w-0", connector)}>
      <NodeCard
        node={node}
        chatId={props.chatId}
        active={!!node.runId && node.runId === props.activeRunId}
        onNavigate={props.onNavigate}
      />
      {node.children.length > 0 && <NodeList {...props} nodes={node.children} />}
    </li>
  )
}

function GroupItem({ counts, ...props }: ListProps & { counts: StatusCounts }) {
  const label = statusCountsLabel(counts)
  const status: RunStatus = counts.running ? "running" : counts.idle ? "idle" : counts.stopped ? "stopped" : "done"
  const hasActive = containsRun(props.nodes, props.activeRunId)
  const [open, setOpen] = useState(hasActive)
  // When a run of the group is opened (e.g. through the menu in the header), the group expands.
  const [prev, setPrev] = useState(props.activeRunId)
  if (prev !== props.activeRunId) {
    setPrev(props.activeRunId)
    if (hasActive) setOpen(true)
  }
  return (
    <li className={cn("min-w-0", connector)}>
      <Collapsible open={open} onOpenChange={setOpen}>
        <CollapsibleTrigger
          className={cn(
            "flex w-full min-w-0 items-center gap-1.5 rounded-lg border border-violet-200 bg-violet-50/40 px-2.5 py-1.5 text-left text-xs hover:bg-violet-50",
            hasActive && !open && "ring-2 ring-violet-400",
          )}
          aria-label={`${props.nodes.length} runs: ${label}. ${open ? "Collapse" : "Expand"}`}
        >
          <ChevronRightIcon className={cn("size-3.5 shrink-0 transition-transform", open && "rotate-90")} />
          <StatusDot status={status} />
          <span className="min-w-0 flex-1 truncate font-medium">{label}</span>
          <span className="shrink-0 text-muted-foreground tabular-nums">{props.nodes.length} runs</span>
        </CollapsibleTrigger>
        <CollapsibleContent>
          <ul className="ml-3 flex min-w-0 flex-col gap-2 border-l-2 border-dashed border-violet-300 pt-2 pl-3">
            {props.nodes.map((n) => (
              <NodeItem key={n.id} node={n} {...props} />
            ))}
          </ul>
        </CollapsibleContent>
      </Collapsible>
    </li>
  )
}

function NodeCard({
  node: n,
  chatId,
  active,
  onNavigate,
}: {
  node: AgentNode
  chatId: string
  active: boolean
  onNavigate?: () => void
}) {
  const m = n.metrics
  const body = (
    <>
      <div className="flex min-w-0 items-start gap-1.5">
        <StatusDot status={n.status} className="mt-1.5" />
        <span className="line-clamp-2 min-w-0 flex-1 text-xs font-medium break-words" title={n.title}>
          {n.title}
        </span>
      </div>
      <div className="mt-0.5 truncate pl-3.5 text-[11px] text-muted-foreground">{n.subtitle}</div>
      <div className="mt-0.5 flex flex-wrap gap-x-2 pl-3.5 text-[11px] text-muted-foreground tabular-nums" title={metricsTitle(m)}>
        <span className={cn(n.status === "running" && "text-sky-700", n.status === "done" && "text-emerald-700")}>
          {runStatusLabel(n.status)}
        </span>
        <span>{metricsShort(m)}</span>
        {m.cost !== undefined && <span>{formatUsd(m.cost)}</span>}
        <span>{formatSpan(n.durationMs)}</span>
        {m.errors > 0 && <span className="text-red-700">{m.errors} error{m.errors === 1 ? "" : "s"}</span>}
      </div>
    </>
  )
  const cls = cn(
    "block min-w-0 rounded-lg border border-violet-200 bg-violet-50/30 px-2.5 py-1.5",
    m.errors > 0 && "border-red-200",
    active && "border-violet-400 bg-violet-100/70 ring-2 ring-violet-400",
  )
  if (!n.runId) return <div className={cls}>{body}</div>
  return (
    <a
      href={subagentHref(chatId, n.runId)}
      onClick={onNavigate}
      aria-current={active ? "page" : undefined}
      className={cn(cls, "hover:bg-violet-50")}
    >
      {body}
    </a>
  )
}

/**
 * Bar "Chat / Subagent ⌄" above the history once there are subagents: in the main history "Chat ⌄", in a
 * subagent's view "Chat / Subagent ⌄". The switcher leads to the main agent and to every run.
 */
export function SubagentBar({ chatId, root, runId, runTitle }: { chatId: string; root: AgentNode; runId?: string; runTitle?: string }) {
  const title = root.title || "Untitled"
  return (
    <nav aria-label="Breadcrumbs" className="flex min-w-0 items-center gap-1 border-b bg-muted/30 px-2 py-1.5 text-sm sm:px-3">
      {runId && (
        <Button size="icon-sm" variant="ghost" asChild>
          <a href={chatHref(chatId)} title="Back to the chat" aria-label="Back to the chat">
            <ArrowLeftIcon />
          </a>
        </Button>
      )}
      <ol className="flex min-w-0 flex-1 items-center gap-1">
        {runId ? (
          <>
            <li className="min-w-0 max-w-[40%] shrink-0">
              <a
                href={chatHref(chatId)}
                className="block truncate text-muted-foreground hover:text-foreground hover:underline"
                title={`Back to the chat "${title}"`}
              >
                {title}
              </a>
            </li>
            <li aria-hidden className="shrink-0 text-muted-foreground">
              /
            </li>
            <li aria-current="page" className="min-w-0 flex-1">
              <SubagentSwitcher chatId={chatId} root={root} runId={runId} title={runTitle ?? runId} />
            </li>
          </>
        ) : (
          <li aria-current="page" className="min-w-0 flex-1">
            <SubagentSwitcher chatId={chatId} root={root} title={title} />
          </li>
        )}
      </ol>
    </nav>
  )
}
