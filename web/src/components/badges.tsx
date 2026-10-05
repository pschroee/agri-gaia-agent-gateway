import { Loader2Icon, ShieldAlertIcon, ShieldCheckIcon, ShieldIcon } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import type { ApprovalState, ChatState, SlotState, VariantId } from "@/api/types"
import { useNow } from "@/hooks/useNow"
import { displayState, type Evidence, evidenceLabel, sessionLabel } from "@/lib/evidence"
import { formatElapsed } from "@/lib/runtime"
import { cn } from "@/lib/utils"

const tone = {
  green: "bg-emerald-100 text-emerald-800 border-emerald-200",
  amber: "bg-amber-100 text-amber-900 border-amber-200",
  blue: "bg-sky-100 text-sky-800 border-sky-200",
  gray: "bg-muted text-muted-foreground border-border",
  red: "bg-red-100 text-red-800 border-red-200",
  violet: "bg-violet-100 text-violet-800 border-violet-200",
} as const

type Tone = keyof typeof tone

function ToneBadge({ t, children, className }: { t: Tone; children: React.ReactNode; className?: string }) {
  return (
    <Badge variant="outline" className={cn(tone[t], className)}>
      {children}
    </Badge>
  )
}

/** State of the chat: "active" or, while resuming in a fresh sandbox, "resuming".
 * An idle chat gets no badge; it resumes by itself with the next message. */
export function ChatStateBadge({ state, resuming }: { state: ChatState; resuming?: boolean }) {
  if (resuming) {
    return (
      <ToneBadge t="blue">
        <Loader2Icon className="animate-spin" aria-hidden />
        resuming
      </ToneBadge>
    )
  }
  if (state !== "active") return null
  return <ToneBadge t="green">active</ToneBadge>
}

const slotStates: Record<SlotState, [string, Tone]> = {
  starting: ["starting", "blue"],
  idle: ["idle", "green"],
  assigned: ["assigned", "violet"],
  stopping: ["stopping", "gray"],
}
export function SlotStateBadge({ state }: { state: SlotState }) {
  const [label, t] = slotStates[state] ?? [state, "gray"]
  return <ToneBadge t={t}>{label}</ToneBadge>
}

const approvalStates: Record<ApprovalState, [string, Tone]> = {
  pending: ["pending", "amber"],
  approved: ["approved", "green"],
  rejected: ["rejected", "red"],
  expired: ["expired", "gray"],
}
export function ApprovalStateBadge({ state }: { state: ApprovalState }) {
  const [label, t] = approvalStates[state] ?? [state, "gray"]
  return <ToneBadge t={t}>{label}</ToneBadge>
}

export function VariantBadge({ variant }: { variant: VariantId | string }) {
  return (
    <Badge variant="outline" className="font-mono uppercase">
      {variant}
    </Badge>
  )
}

/** "Running"; with `since` (start of the run, ms) the duration counts up live, otherwise no time. */
export function RunningIndicator({ className, since }: { className?: string; since?: number }) {
  const now = useNow(1000, since !== undefined)
  return (
    <span
      className={cn("inline-flex items-center gap-1 text-xs whitespace-nowrap text-sky-700", className)}
      title={since !== undefined ? "Duration of the running turn" : undefined}
    >
      <span className="relative flex size-2">
        <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-sky-400 opacity-75" />
        <span className="relative inline-flex size-2 rounded-full bg-sky-500" />
      </span>
      Running
      {since !== undefined && <span className="tabular-nums"> · {formatElapsed(now - since)}</span>}
    </span>
  )
}

/**
 * Evidence of a tool execution (E9): "confirmed" means executed by the orchestrator and requested
 * at the proxy. Suspicious cases (not executed, not requested, mismatching) are red; harmless
 * causes (response aborted, refused by pi) grey. Tools without execution in the sandbox and
 * matching still in progress show nothing.
 */
export function EvidenceBadge({ evidence, settled, className }: { evidence?: Evidence; settled: boolean; className?: string }) {
  const state = displayState(evidence, { settled })
  if (!state || state === "internal" || state === "pending") return null
  const { label, tone: t, title } = evidenceLabel(state)
  const ops = evidence && evidence.ops.length > 0 ? ` (${evidence.ops.join(", ")}; ${sessionLabel(evidence.session)})` : ""
  const reason = evidence?.reason ? ` – according to the session: ${evidence.reason}` : ""
  return (
    <Badge
      variant="outline"
      title={title + ops + reason}
      className={cn(
        "gap-1",
        t === "ok"
          ? "border-emerald-200 bg-emerald-50/60 text-emerald-800"
          : t === "muted"
            ? "border-border bg-muted/60 text-muted-foreground"
            : "border-red-300 bg-red-50 text-red-800",
        className,
      )}
    >
      {t === "ok" ? <ShieldCheckIcon /> : t === "muted" ? <ShieldIcon /> : <ShieldAlertIcon />}
      {label}
    </Badge>
  )
}
