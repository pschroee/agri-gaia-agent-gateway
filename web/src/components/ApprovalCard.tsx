import { useState } from "react"
import { CheckIcon, GlobeIcon, ServerIcon, ShieldAlertIcon, XIcon } from "lucide-react"
import { toast } from "sonner"
import { api } from "@/api/client"
import type { Approval } from "@/api/types"
import { AgentOrigin } from "@/components/AgentOrigin"
import { ApprovalStateBadge } from "@/components/badges"
import { Button } from "@/components/ui/button"
import { formatBytes, formatTime, shortHash } from "@/lib/format"
import { isSubagentSession, type SubagentRun } from "@/lib/subagents"
import { cn } from "@/lib/utils"

type Props = {
  approval: Approval
  onDecided?: (a: Approval) => void
  /** Kurzform ohne Vorschau, etwa für die Statusseite. */
  compact?: boolean
  chatLabel?: React.ReactNode
  /** Läufe des Chats, um einen fragenden Subagenten beim Namen zu nennen. */
  runs?: SubagentRun[]
}

export function ApprovalCard({ approval: a, onDecided, compact, chatLabel, runs }: Props) {
  const [busy, setBusy] = useState(false)
  const pending = a.state === "pending"
  const who = isSubagentSession(a.session) ? "Ein Subagent" : "Der Agent"
  const origin = <AgentOrigin chatId={a.chat_id} session={a.session} runs={runs} />

  const decide = async (approve: boolean) => {
    setBusy(true)
    try {
      onDecided?.(await api.decide(a.id, approve))
    } catch (e) {
      toast.error(`Entscheidung fehlgeschlagen: ${e instanceof Error ? e.message : String(e)}`)
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
          <span className="font-medium">{who} bittet um Internetzugang</span>
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
          <span className="font-mono uppercase">{a.via}</span> · {pending ? "angefragt" : "entschieden"}{" "}
          {formatTime(pending ? a.created_at : (a.decided_at ?? a.created_at))}
        </div>
        {pending && (
          <div className="mt-3 flex flex-wrap gap-2">
            <Button size="sm" disabled={busy} onClick={() => void decide(true)}>
              <GlobeIcon /> Internet freigeben
            </Button>
            <Button size="sm" variant="destructive" disabled={busy} onClick={() => void decide(false)}>
              <XIcon /> Ablehnen
            </Button>
          </div>
        )}
      </div>
    )
  }

  if (a.kind === "platform_write") {
    return (
      <div
        className={cn(
          "rounded-lg border p-3 text-sm",
          pending ? "border-violet-400 bg-violet-50 shadow-sm ring-1 ring-violet-300" : "bg-muted/40",
        )}
      >
        <div className="flex flex-wrap items-center gap-2">
          <ServerIcon className={cn("size-4", pending ? "text-violet-600" : "text-muted-foreground")} />
          <span className="font-medium">{who} möchte auf der Agri-Gaia-Plattform schreiben</span>
          <ApprovalStateBadge state={a.state} />
          {origin}
          {chatLabel}
        </div>
        <div className="mt-2 font-mono text-xs break-all">{a.name}</div>
        {!compact && a.preview && a.preview !== a.name && (
          <pre className="mt-2 max-h-64 overflow-auto rounded border bg-background p-2 font-mono text-xs whitespace-pre-wrap">
            {a.preview.slice(a.name.length).trim()}
          </pre>
        )}
        <div className="mt-2 text-xs text-muted-foreground">
          <span className="font-mono uppercase">{a.via}</span> · {pending ? "angefragt" : "entschieden"}{" "}
          {formatTime(pending ? a.created_at : (a.decided_at ?? a.created_at))}
        </div>
        {pending && (
          <div className="mt-3 flex flex-wrap gap-2">
            <Button size="sm" disabled={busy} onClick={() => void decide(true)}>
              <CheckIcon /> Ausführen
            </Button>
            <Button size="sm" variant="destructive" disabled={busy} onClick={() => void decide(false)}>
              <XIcon /> Ablehnen
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
        <span className="font-medium">{who} möchte ein Artefakt hochladen</span>
        <ApprovalStateBadge state={a.state} />
        {origin}
        {chatLabel}
      </div>
      <dl className="mt-2 grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-0.5 text-xs">
        <dt className="text-muted-foreground">Name</dt>
        <dd className="font-mono break-all">{a.name}</dd>
        <dt className="text-muted-foreground">Größe</dt>
        <dd>
          {formatBytes(a.size)} · {a.content_type}
        </dd>
        <dt className="text-muted-foreground">sha256</dt>
        <dd className="font-mono" title={a.sha256}>
          {shortHash(a.sha256, 16)}
        </dd>
        <dt className="text-muted-foreground">Weg</dt>
        <dd className="font-mono uppercase">{a.via}</dd>
        <dt className="text-muted-foreground">{pending ? "Angefragt" : "Entschieden"}</dt>
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
            <CheckIcon /> Bestätigen
          </Button>
          <Button size="sm" variant="destructive" disabled={busy} onClick={() => void decide(false)}>
            <XIcon /> Ablehnen
          </Button>
        </div>
      )}
    </div>
  )
}
