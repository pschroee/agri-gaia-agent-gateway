import { GlobeIcon } from "lucide-react"
import { api } from "@/api/client"
import type { Approval, Chat, Pool, VariantId } from "@/api/types"
import { ApprovalCard } from "@/components/ApprovalCard"
import { ContextBadge } from "@/components/ContextMeter"
import { ChatStateBadge, RunningIndicator, SlotStateBadge, VariantBadge } from "@/components/badges"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { chatHref } from "@/hooks/useHashRoute"
import { modelName, type useMeta } from "@/hooks/useMeta"
import { useNow } from "@/hooks/useNow"
import { usePolling } from "@/hooks/usePolling"
import { formatActivity, formatDuration, formatTime, formatTokens, formatUsd } from "@/lib/format"
import { subagentsRunningTitle } from "@/lib/subagents"

type Props = {
  meta: ReturnType<typeof useMeta>
  approvals: Approval[]
  onApprovalsChanged: () => void
}

export function StatusView({ meta, approvals, onApprovalsChanged }: Props) {
  const pool = usePolling(api.pool, 1000)
  const chats = usePolling(api.chats, 2000)
  const now = useNow(1000)
  const activeChats = (chats.data ?? []).filter((c) => c.state === "active")
  const chatTitle = (id: string) => chats.data?.find((c) => c.id === id)?.title
  const internetCount = approvals.filter((a) => a.kind === "internet_access").length

  return (
    <div className="min-h-0 flex-1 overflow-y-auto">
      <div className="mx-auto flex max-w-7xl flex-col gap-6 p-4">
        {pool.error && <p className="text-sm text-red-700">Pool unreachable: {pool.error}</p>}
        {pool.data && <Kennzahlen pool={pool.data} />}

        <section>
          <h2 className="mb-2 text-sm font-semibold">Pool slots</h2>
          <div className="rounded-lg border">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Slot</TableHead>
                  <TableHead>Variant</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Internet</TableHead>
                  <TableHead>Container</TableHead>
                  <TableHead>Image</TableHead>
                  <TableHead>Since</TableHead>
                  <TableHead>Chat</TableHead>
                  <TableHead>Activity</TableHead>
                  <TableHead>Duration</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(pool.data?.slots ?? []).length === 0 && (
                  <TableRow>
                    <TableCell colSpan={10} className="text-center text-muted-foreground">
                      No slots.
                    </TableCell>
                  </TableRow>
                )}
                {(pool.data?.slots ?? []).map((s) => (
                  <TableRow key={s.id}>
                    <TableCell className="font-mono">{s.id}</TableCell>
                    <TableCell>
                      <VariantBadge variant={s.variant} />
                    </TableCell>
                    <TableCell>
                      <SlotStateBadge state={s.state} />
                    </TableCell>
                    <TableCell>
                      {s.state !== "assigned" ? (
                        <span className="text-muted-foreground">–</span>
                      ) : s.internet ? (
                        <span className="inline-flex items-center gap-1 text-sky-700">
                          <GlobeIcon className="size-3.5" /> on
                        </span>
                      ) : (
                        <span className="text-muted-foreground">off</span>
                      )}
                    </TableCell>
                    <TableCell>
                      <div className="font-mono text-xs">{s.container_name}</div>
                      <div className="font-mono text-xs text-muted-foreground">{s.container_id.slice(0, 12)}</div>
                    </TableCell>
                    <TableCell className="max-w-48 truncate font-mono text-xs" title={s.image}>
                      {s.image}
                    </TableCell>
                    <TableCell className="text-xs">
                      {formatTime(s.assigned_at ?? s.created_at)}
                      <div className="text-muted-foreground">{formatDuration(s.assigned_at ?? s.created_at, now)}</div>
                    </TableCell>
                    <TableCell className="max-w-48">
                      {s.chat_id ? (
                        <a className="block truncate text-sky-700 hover:underline" href={chatHref(s.chat_id)}>
                          {s.chat_title || chatTitle(s.chat_id) || s.chat_id.slice(0, 8)}
                        </a>
                      ) : (
                        <span className="text-muted-foreground">–</span>
                      )}
                    </TableCell>
                    <TableCell>{s.state === "assigned" ? formatActivity(s.activity) : "–"}</TableCell>
                    <TableCell className="text-xs">
                      {s.state === "assigned" && s.activity ? formatDuration(s.activity.since, now) : "–"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        </section>

        <section>
          <h2 className="mb-2 text-sm font-semibold">Active chats ({activeChats.length})</h2>
          <ActiveChats chats={activeChats} modelName={(id) => modelName(meta.models, id)} />
        </section>

        <section>
          <h2 className="mb-2 text-sm font-semibold">
            Pending approvals ({approvals.length}
            {internetCount > 0 && `, ${internetCount} of them internet access`})
          </h2>
          {approvals.length === 0 ? (
            <p className="text-sm text-muted-foreground">No pending approvals.</p>
          ) : (
            <div className="grid gap-3 md:grid-cols-2">
              {approvals.map((a) => (
                <ApprovalCard
                  key={a.id}
                  approval={a}
                  onDecided={onApprovalsChanged}
                  chatLabel={
                    <a className="ml-auto text-xs text-sky-700 hover:underline" href={chatHref(a.chat_id)}>
                      {chatTitle(a.chat_id) || a.chat_id.slice(0, 8)}
                    </a>
                  }
                />
              ))}
            </div>
          )}
        </section>
      </div>
    </div>
  )
}

function Kennzahlen({ pool }: { pool: Pool }) {
  const variants = Array.from(
    new Set([...(Object.keys(pool.targets) as VariantId[]), ...pool.slots.map((s) => s.variant)]),
  )
  return (
    <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
      {variants.map((v) => {
        const slots = pool.slots.filter((s) => s.variant === v)
        const count = (st: string) => slots.filter((s) => s.state === st).length
        return (
          <Card key={v} size="sm">
            <CardHeader>
              <CardTitle className="flex items-center gap-2 text-sm">
                Variant <VariantBadge variant={v} />
              </CardTitle>
            </CardHeader>
            <CardContent className="grid grid-cols-4 gap-2 text-center">
              <Stat label="Target" value={pool.targets[v] ?? 0} />
              <Stat label="idle" value={count("idle")} className="text-emerald-700" />
              <Stat label="assigned" value={count("assigned")} className="text-violet-700" />
              <Stat label="starting" value={count("starting")} className="text-sky-700" />
            </CardContent>
          </Card>
        )
      })}
      {pool.totals && (
        <Card size="sm">
          <CardHeader>
            <CardTitle className="text-sm">Total</CardTitle>
          </CardHeader>
          <CardContent className="grid grid-cols-3 gap-2 text-center">
            <Stat label="Cost" value={formatUsd(pool.totals.cost)} />
            <Stat label="Tokens" value={formatTokens(pool.totals.tokens?.total)} />
            <Stat label="active chats" value={pool.totals.chats_active} />
          </CardContent>
        </Card>
      )}
    </div>
  )
}

function Stat({ label, value, className }: { label: string; value: React.ReactNode; className?: string }) {
  return (
    <div>
      <div className={`text-lg font-semibold tabular-nums ${className ?? ""}`}>{value}</div>
      <div className="text-xs text-muted-foreground">{label}</div>
    </div>
  )
}

function ActiveChats({ chats, modelName }: { chats: Chat[]; modelName: (id: string) => string }) {
  if (chats.length === 0) return <p className="text-sm text-muted-foreground">No active chats.</p>
  return (
    <div className="rounded-lg border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Title</TableHead>
            <TableHead>State</TableHead>
            <TableHead>Variant</TableHead>
            <TableHead>Model</TableHead>
            <TableHead>Slot</TableHead>
            <TableHead className="text-right">Tokens</TableHead>
            <TableHead className="text-right">Cost</TableHead>
            <TableHead>Context</TableHead>
            <TableHead title="running now / at most at the same time">Subagents</TableHead>
            <TableHead className="text-right">Model calls</TableHead>
            <TableHead>Pending</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {chats.map((c) => (
            <TableRow key={c.id}>
              <TableCell>
                <a className="text-sky-700 hover:underline" href={chatHref(c.id)}>
                  {c.title || "Untitled"}
                </a>
                {c.internet && <GlobeIcon className="ml-1 inline size-3.5 text-sky-600" />}
              </TableCell>
              <TableCell>
                <div className="flex items-center gap-2">
                  <ChatStateBadge state={c.state} resuming={c.resuming} />
                  {c.running && <RunningIndicator />}
                </div>
              </TableCell>
              <TableCell>
                <VariantBadge variant={c.variant} />
              </TableCell>
              <TableCell className="text-xs">{modelName(c.model)}</TableCell>
              <TableCell className="font-mono text-xs">{c.slot_id ?? "–"}</TableCell>
              <TableCell className="text-right tabular-nums">{formatTokens(c.tokens?.total)}</TableCell>
              <TableCell
                className="text-right tabular-nums"
                title={c.cost_other ? `of which outside the main responses: ${formatUsd(c.cost_other)}` : undefined}
              >
                {formatUsd(c.cost)}
              </TableCell>
              <TableCell>{c.context ? <ContextBadge context={c.context} /> : "–"}</TableCell>
              <TableCell className="tabular-nums" title={subagentsRunningTitle(c)}>
                {c.max_subagents !== undefined ? `${c.subagents_running ?? 0}/${c.max_subagents}` : "–"}
              </TableCell>
              <TableCell className="text-right tabular-nums">{c.llm_calls ?? "–"}</TableCell>
              <TableCell>{c.pending_approvals > 0 ? c.pending_approvals : "–"}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}
