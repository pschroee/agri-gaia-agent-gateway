import { useState } from "react"
import { CheckIcon, GlobeIcon, ServerIcon, ShieldAlertIcon, XIcon } from "lucide-react"
import { toast } from "sonner"
import { api } from "@/api/client"
import type { Approval } from "@/api/types"
import { AgentOrigin } from "@/components/AgentOrigin"
import { ApprovalStateBadge } from "@/components/badges"
import { Button } from "@/components/ui/button"
import { approvalEffect, EFFECT_BADGE, EFFECT_LABEL } from "@/lib/effect"
import { formatBytes, formatTime, shortHash } from "@/lib/format"
import { isSubagentSession, type SubagentRun } from "@/lib/subagents"
import { summaryParts, technicalPreview } from "@/lib/summary"
import { cn } from "@/lib/utils"

type Props = {
  approval: Approval
  onDecided?: (a: Approval) => void
  /** Short form without preview, e.g. for the status page. */
  compact?: boolean
  chatLabel?: React.ReactNode
  /** Runs of the chat, to name an asking subagent. */
  runs?: SubagentRun[]
}

/** The gateway's summary (issue #72): what will happen, as the main text of a card. */
function GatewaySummary({ summary, list }: { summary?: string; list?: boolean }) {
  const parts = summaryParts(summary)
  if (!parts) return null
  return (
    <div className="mt-2" data-testid="gateway-summary">
      <div className="font-medium break-words">{parts.head}</div>
      {parts.details.length > 0 && (
        <ul className={cn("mt-1 space-y-0.5 text-xs break-words", list ? "list-none" : "list-disc pl-4")}>
          {parts.details.map((d, i) => (
            <li key={i}>{d}</li>
          ))}
        </ul>
      )}
      <div className="mt-1 text-[11px] text-muted-foreground">Written by the gateway from the call and the platform</div>
    </div>
  )
}

/** Technical details (method, path, body), collapsed below the summary; open when there is no summary. */
function Technical({ text, open, danger }: { text?: string; open: boolean; danger?: boolean }) {
  if (!text) return null
  return (
    <details className="mt-2" open={open}>
      <summary className="cursor-pointer text-xs text-muted-foreground">Technical details</summary>
      <pre
        className={cn(
          "mt-1 overflow-auto rounded border bg-background p-2 font-mono text-xs whitespace-pre-wrap",
          !danger && "max-h-64",
        )}
      >
        {text}
      </pre>
    </details>
  )
}

export function ApprovalCard({ approval: a, onDecided, compact, chatLabel, runs }: Props) {
  const [busy, setBusy] = useState(false)
  const pending = a.state === "pending"
  const who = isSubagentSession(a.session) ? "A subagent" : "The agent"
  const origin = <AgentOrigin chatId={a.chat_id} session={a.session} runs={runs} />

  const decide = async (approve: boolean) => {
    setBusy(true)
    try {
      onDecided?.(await api.decide(a.id, approve))
    } catch (e) {
      toast.error(`Decision failed: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setBusy(false)
    }
  }

  if (a.kind === "internet_access") {
    return (
      <div
        className={cn(
          "rounded-lg border p-3 text-sm",
          pending ? "border-sky-400 bg-sky-50 shadow-sm ring-1 ring-sky-300" : "bg-muted/40",
        )}
      >
        <div className="flex flex-wrap items-center gap-2">
          <GlobeIcon className={cn("size-4", pending ? "text-sky-600" : "text-muted-foreground")} />
          <span className="font-medium">{who} asks for internet access</span>
          <ApprovalStateBadge state={a.state} />
          {origin}
          {chatLabel}
        </div>
        {a.name && (
          <blockquote className="mt-2 border-l-2 border-sky-300 pl-3 text-sm break-words whitespace-pre-wrap italic">
            {a.name}
          </blockquote>
        )}
        <div className="mt-2 text-xs text-muted-foreground">
          <span className="font-mono uppercase">{a.via}</span> · {pending ? "requested" : "decided"}{" "}
          {formatTime(pending ? a.created_at : (a.decided_at ?? a.created_at))}
        </div>
        {pending && (
          <div className="mt-3 flex flex-wrap gap-2">
            <Button size="sm" disabled={busy} onClick={() => void decide(true)}>
              <GlobeIcon /> Allow internet
            </Button>
            <Button size="sm" variant="destructive" disabled={busy} onClick={() => void decide(false)}>
              <XIcon /> Reject
            </Button>
          </div>
        )}
      </div>
    )
  }

  if (a.kind === "platform_write") {
    const effect = approvalEffect(a)
    // An irreversible call stands out in red while it waits, and its preview is shown in full.
    const danger = pending && effect === "irreversible"
    return (
      <div
        data-effect={effect}
        className={cn(
          "rounded-lg border p-3 text-sm",
          danger
            ? "border-red-500 bg-red-50 shadow-sm ring-1 ring-red-400"
            : pending
              ? "border-violet-400 bg-violet-50 shadow-sm ring-1 ring-violet-300"
              : "bg-muted/40",
        )}
      >
        <div className="flex flex-wrap items-center gap-2">
          <ServerIcon
            className={cn("size-4", danger ? "text-red-600" : pending ? "text-violet-600" : "text-muted-foreground")}
          />
          <span className="font-medium">{who} wants to write to the Agri-Gaia platform</span>
          {effect && (
            <span className={cn("rounded px-1.5 py-0.5 text-xs font-medium ring-1", EFFECT_BADGE[effect])}>
              {EFFECT_LABEL[effect]}
            </span>
          )}
          <ApprovalStateBadge state={a.state} />
          {origin}
          {chatLabel}
        </div>
        {!compact && <GatewaySummary summary={a.summary} />}
        <div className="mt-2 font-mono text-xs break-all">{a.name}</div>
        {danger && (
          <div className="mt-2 font-medium text-red-700">
            This cannot be undone. Check every parameter before you execute it.
          </div>
        )}
        {!compact && (
          <Technical text={technicalPreview(a)} open={!a.summary || effect === "irreversible"} danger={effect === "irreversible"} />
        )}
        <div className="mt-2 text-xs text-muted-foreground">
          <span className="font-mono uppercase">{a.via}</span> · {pending ? "requested" : "decided"}{" "}
          {formatTime(pending ? a.created_at : (a.decided_at ?? a.created_at))}
        </div>
        {pending && (
          <div className="mt-3 flex flex-wrap gap-2">
            <Button size="sm" disabled={busy} onClick={() => void decide(true)}>
              <CheckIcon /> Execute
            </Button>
            <Button size="sm" variant="destructive" disabled={busy} onClick={() => void decide(false)}>
              <XIcon /> Reject
            </Button>
          </div>
        )}
      </div>
    )
  }

  if (a.kind === "plan") {
    return (
      <div
        className={cn(
          "rounded-lg border p-3 text-sm",
          pending ? "border-violet-400 bg-violet-50 shadow-sm ring-1 ring-violet-300" : "bg-muted/40",
        )}
      >
        <div className="flex flex-wrap items-center gap-2">
          <ServerIcon className={cn("size-4", pending ? "text-violet-600" : "text-muted-foreground")} />
          <span className="font-medium">{who} proposes a plan</span>
          <ApprovalStateBadge state={a.state} />
          {origin}
          {chatLabel}
        </div>
        {a.summary ? <GatewaySummary summary={a.summary} list /> : <div className="mt-2 font-medium">{a.name}</div>}
        {!compact && <Technical text={a.preview} open={!a.summary} />}
        <div className="mt-2 text-xs text-muted-foreground">
          <span className="font-mono uppercase">{a.via}</span> · {pending ? "requested" : "decided"}{" "}
          {formatTime(pending ? a.created_at : (a.decided_at ?? a.created_at))}
        </div>
        {pending && (
          <div className="mt-3 flex flex-wrap gap-2">
            <Button size="sm" disabled={busy} onClick={() => void decide(true)}>
              <CheckIcon /> Approve plan
            </Button>
            <Button size="sm" variant="destructive" disabled={busy} onClick={() => void decide(false)}>
              <XIcon /> Reject
            </Button>
          </div>
        )}
      </div>
    )
  }

  return (
    <div
      className={cn(
        "rounded-lg border p-3 text-sm",
        pending ? "border-amber-400 bg-amber-50 shadow-sm ring-1 ring-amber-300" : "bg-muted/40",
      )}
    >
      <div className="flex flex-wrap items-center gap-2">
        <ShieldAlertIcon className={cn("size-4", pending ? "text-amber-600" : "text-muted-foreground")} />
        <span className="font-medium">{who} wants to upload an artifact</span>
        <ApprovalStateBadge state={a.state} />
        {origin}
        {chatLabel}
      </div>
      <dl className="mt-2 grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-0.5 text-xs">
        <dt className="text-muted-foreground">Name</dt>
        <dd className="font-mono break-all">{a.name}</dd>
        <dt className="text-muted-foreground">Size</dt>
        <dd>
          {formatBytes(a.size)} · {a.content_type}
        </dd>
        <dt className="text-muted-foreground">sha256</dt>
        <dd className="font-mono" title={a.sha256}>
          {shortHash(a.sha256, 16)}
        </dd>
        <dt className="text-muted-foreground">Via</dt>
        <dd className="font-mono uppercase">{a.via}</dd>
        <dt className="text-muted-foreground">{pending ? "Requested" : "Decided"}</dt>
        <dd>{formatTime(pending ? a.created_at : (a.decided_at ?? a.created_at))}</dd>
      </dl>
      {!compact && a.preview !== undefined && a.preview !== "" && (
        <pre className="mt-2 max-h-48 overflow-auto rounded border bg-background p-2 font-mono text-xs whitespace-pre-wrap">
          {a.preview}
        </pre>
      )}
      {pending && (
        <div className="mt-3 flex gap-2">
          <Button size="sm" disabled={busy} onClick={() => void decide(true)}>
            <CheckIcon /> Approve
          </Button>
          <Button size="sm" variant="destructive" disabled={busy} onClick={() => void decide(false)}>
            <XIcon /> Reject
          </Button>
        </div>
      )}
    </div>
  )
}
