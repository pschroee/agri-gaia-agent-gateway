import { Loader2Icon, ShrinkIcon } from "lucide-react"
import type { Chat, ContextUsage } from "@/api/types"
import { Button } from "@/components/ui/button"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
import { cacheHitRate, type ContextLevel, describeContext, formatPercent } from "@/lib/context"
import { formatTime } from "@/lib/format"
import { cn } from "@/lib/utils"

const ringColor: Record<ContextLevel, string> = {
  neutral: "text-foreground/70",
  warn: "text-amber-500",
  danger: "text-red-600",
}
const textColor: Record<ContextLevel, string> = {
  neutral: "text-muted-foreground",
  warn: "text-amber-700",
  danger: "text-red-700",
}

/** Kleiner Ring mit dem Anteil des genutzten Kontextfensters. */
export function ContextRing({ ratio, level, className }: { ratio: number; level: ContextLevel; className?: string }) {
  const r = 7
  const c = 2 * Math.PI * r
  return (
    <svg viewBox="0 0 18 18" className={cn("size-4 -rotate-90", className)} aria-hidden>
      <circle cx="9" cy="9" r={r} fill="none" strokeWidth="2.5" className="stroke-muted" />
      <circle
        cx="9"
        cy="9"
        r={r}
        fill="none"
        strokeWidth="2.5"
        strokeLinecap="round"
        stroke="currentColor"
        strokeDasharray={`${c * ratio} ${c}`}
        className={ringColor[level]}
      />
    </svg>
  )
}

/** Kompakte Anzeige für Chatliste und Statusseite. */
export function ContextBadge({ context, className }: { context?: ContextUsage; className?: string }) {
  if (!context) return null
  const d = describeContext(context)
  return (
    <span
      className={cn("inline-flex items-center gap-1 text-xs tabular-nums", textColor[d.level], className)}
      title={`Kontext: ${d.measured ? `${d.used} (${d.percent})` : d.used}`}
    >
      <ContextRing ratio={d.ratio} level={d.level} className="size-3.5" />
      {d.percent}
    </span>
  )
}

const pct = (v: number | undefined) => (v === undefined ? "–" : formatPercent(v * 100))

type Props = {
  chat: Chat
  busy?: string
  onAutoCompact: (enabled: boolean) => void
  onCompact: () => void
}

/** Kontextanzeige im Chatkopf; die Einzelheiten öffnen sich erst beim Klicken. */
export function ContextIndicator({ chat, busy, onAutoCompact, onCompact }: Props) {
  const ctx = chat.context
  const d = ctx ? describeContext(ctx) : undefined
  const cache = cacheHitRate(chat.tokens?.input, chat.tokens?.cache_read)
  const level = d?.level ?? "neutral"

  return (
    <Popover>
      <PopoverTrigger asChild>
        <button
          type="button"
          className={cn(
            "inline-flex items-center gap-1.5 rounded-md border px-2 py-1 text-xs tabular-nums hover:bg-muted",
            textColor[level],
          )}
          aria-label="Kontextauslastung"
        >
          <ContextRing ratio={d?.ratio ?? 0} level={level} />
          <span>{d ? d.percent : "–"}</span>
          <span className="hidden text-muted-foreground sm:inline">Kontext</span>
        </button>
      </PopoverTrigger>
      <PopoverContent align="end" collisionPadding={12} className="w-72 max-w-[calc(100vw-1.5rem)] text-xs">
        <div className="mb-2 flex items-center gap-2">
          <ContextRing ratio={d?.ratio ?? 0} level={level} className="size-6" />
          <div>
            <div className="text-sm font-semibold">Kontext {d?.measured ? d.percent : ""}</div>
            <div className="text-muted-foreground">{d ? d.used : "noch nicht gemessen"}</div>
          </div>
        </div>
        <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1">
          {d?.measured && (
            <>
              <dt className="text-muted-foreground">Verbleibend</dt>
              <dd>{d.remaining}</dd>
            </>
          )}
          <dt className="text-muted-foreground">Auto-Kompaktierung</dt>
          <dd>{chat.auto_compact ? `an, ab ${d?.threshold ?? "?"}` : "aus"}</dd>
          <dt className="text-muted-foreground">Kompaktierungen</dt>
          <dd>{chat.compactions ?? 0}</dd>
          <dt className="text-muted-foreground">Cache-Treffer</dt>
          <dd>{pct(cache)}</dd>
          {ctx && (
            <>
              <dt className="text-muted-foreground">Stand</dt>
              <dd>{formatTime(ctx.updated_at)}</dd>
            </>
          )}
        </dl>
        <div className="mt-3 flex items-center justify-between gap-2 border-t pt-2">
          <Label htmlFor={`autocompact-${chat.id}`} className="text-xs">
            Auto-Kompaktierung
          </Label>
          <Switch
            id={`autocompact-${chat.id}`}
            checked={chat.auto_compact ?? false}
            disabled={busy === "Auto-Kompaktierung"}
            onCheckedChange={onAutoCompact}
          />
        </div>
        <Button
          size="sm"
          variant="outline"
          className="mt-2 w-full"
          disabled={chat.running || busy === "Kompaktieren"}
          title={chat.running ? "Erst möglich, wenn pi nicht arbeitet" : undefined}
          onClick={onCompact}
        >
          {busy === "Kompaktieren" ? <Loader2Icon className="animate-spin" /> : <ShrinkIcon />} Jetzt kompaktieren
        </Button>
      </PopoverContent>
    </Popover>
  )
}
