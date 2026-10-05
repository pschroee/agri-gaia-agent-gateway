import { useState } from "react"
import { BotIcon, GlobeIcon, KeyRoundIcon, PlusIcon, ShrinkIcon } from "lucide-react"
import { ApiError, api } from "@/api/client"
import type { Chat, Model, Pricing, VariantId } from "@/api/types"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog"
import { NumberStepper } from "@/components/NumberStepper"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import { Textarea } from "@/components/ui/textarea"
import type { Meta } from "@/hooks/useMeta"
import { DEFAULT_DELEGATION_HOURS, DELEGATION_TEMPLATES, delegationFrom } from "@/lib/delegationTemplates"
import { formatPrice, modelPriceSource, sourceLabel } from "@/lib/format"
import { formatPeakWindows } from "@/lib/tariff"
import { cn } from "@/lib/utils"

type Props = { meta: Meta & { reload: () => Promise<void> }; onCreated: (chat: Chat) => void; compact?: boolean }

export function NewChatDialog({ meta, onCreated, compact }: Props) {
  const { reload } = meta
  const [open, setOpen] = useState(false)
  const [modelChoice, setModel] = useState("")
  const [variantChoice, setVariant] = useState<VariantId | "">("")
  const [title, setTitle] = useState("")
  const [message, setMessage] = useState("")
  const [internet, setInternet] = useState(false)
  // undefined: Vorgabe des Servers (config.auto_compact_default)
  const [autoCompactChoice, setAutoCompact] = useState<boolean>()
  // undefined: Vorgabe des Servers (config.max_subagents_default)
  const [maxSubChoice, setMaxSub] = useState<number>()
  const [templateId, setTemplateId] = useState("none")
  const [hours, setHours] = useState(DEFAULT_DELEGATION_HOURS)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  const template = DELEGATION_TEMPLATES.find((t) => t.id === templateId)

  // Voreinstellungen ableiten, solange nichts gewählt ist
  const model = modelChoice || (meta.models.find((m) => m.default) ?? meta.models[0])?.id || ""
  const variant = variantChoice || meta.variants[0]?.id || ""

  const onOpenChange = (next: boolean) => {
    setOpen(next)
    if (!next) return
    setError(undefined)
    setInternet(meta.config?.internet_default ?? false)
    setAutoCompact(undefined)
    setMaxSub(undefined)
    if (meta.models.length === 0) void reload()
  }

  const autoCompact = autoCompactChoice ?? meta.config?.auto_compact_default ?? true
  const maxSubLimit = meta.config?.max_subagents_limit
  const maxSub = maxSubChoice ?? meta.config?.max_subagents_default ?? 0
  const selected = meta.models.find((m) => m.id === model)
  const selectedVariant = meta.variants.find((v) => v.id === variant)

  const submit = async () => {
    setBusy(true)
    setError(undefined)
    try {
      const chat = await api.createChat({
        model: model || undefined,
        variant: variant || undefined,
        title: title.trim() || undefined,
        message: message.trim() || undefined,
        internet,
        auto_compact: autoCompactChoice ?? meta.config?.auto_compact_default,
        max_subagents: maxSubChoice ?? meta.config?.max_subagents_default,
        delegation: delegationFrom(template, hours),
      })
      setOpen(false)
      setTitle("")
      setMessage("")
      onCreated(chat)
    } catch (e) {
      if (e instanceof ApiError && e.status === 503) {
        setError("Kein freier Platz im Pool, bitte kurz warten.")
      } else {
        setError(e instanceof Error ? e.message : String(e))
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogTrigger asChild>
        <Button size="sm" className={compact ? "shrink-0" : "w-full"} title="Neuer Chat">
          <PlusIcon /> {compact ? "Neu" : "Neuer Chat"}
        </Button>
      </DialogTrigger>
      <DialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto sm:max-w-lg [&>*]:min-w-0">
        <DialogHeader>
          <DialogTitle>Neuer Chat</DialogTitle>
          <DialogDescription>Der Chat bekommt einen freien Platz aus dem Pool der gewählten Variante.</DialogDescription>
        </DialogHeader>
        <div className="grid gap-4 [&>*]:min-w-0">
          <div className="grid gap-1.5">
            <Label>Modell</Label>
            <Select value={model} onValueChange={setModel}>
              <SelectTrigger className="w-full min-w-0">
                <SelectValue placeholder="Modell wählen" />
              </SelectTrigger>
              <SelectContent>
                {meta.models.map((m) => (
                  <SelectItem key={m.id} value={m.id}>
                    {m.name} <span className="text-muted-foreground">({m.id})</span>
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {selected?.pricing && <PricingInfo model={selected} />}
          </div>
          <div className="grid gap-1.5">
            <Label>Variante</Label>
            <Select value={variant} onValueChange={(v) => setVariant(v as VariantId)}>
              <SelectTrigger className="w-full min-w-0">
                <SelectValue placeholder="Variante wählen" />
              </SelectTrigger>
              <SelectContent>
                {meta.variants.map((v) => (
                  <SelectItem key={v.id} value={v.id}>
                    {v.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {selectedVariant && selectedVariant.tools.length > 0 && (
              <p className="text-xs text-muted-foreground">Werkzeuge: {selectedVariant.tools.join(", ")}</p>
            )}
          </div>
          <div className="grid gap-1.5 rounded-md border px-3 py-2">
            <Label className="flex items-center gap-1.5">
              <KeyRoundIcon className="size-3.5" /> Rechte des Agenten (Delegation)
            </Label>
            <Select value={templateId} onValueChange={setTemplateId}>
              <SelectTrigger className="w-full min-w-0" aria-label="Vorlage für die Delegation">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {DELEGATION_TEMPLATES.map((t) => (
                  <SelectItem key={t.id} value={t.id}>
                    {t.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {template && <p className="text-xs text-muted-foreground">{template.description}</p>}
            {template?.rules && (
              <div className="flex items-center justify-between gap-3">
                <p className="text-xs text-muted-foreground">Gültig für Stunden; Übergriffe werden abgewiesen.</p>
                <NumberStepper id="new-hours" value={hours} min={1} max={72} onChange={setHours} aria-label="Gültigkeit in Stunden" />
              </div>
            )}
          </div>
          <div className="flex items-start justify-between gap-3 rounded-md border px-3 py-2">
            <div>
              <Label htmlFor="new-internet" className="flex items-center gap-1.5">
                <GlobeIcon className="size-3.5" /> Internetzugang
              </Label>
              <p className="text-xs text-muted-foreground">Sprachmodell und Orchestrator bleiben immer erreichbar.</p>
            </div>
            <Switch id="new-internet" checked={internet} onCheckedChange={setInternet} />
          </div>
          <div className="flex items-start justify-between gap-3 rounded-md border px-3 py-2">
            <div>
              <Label htmlFor="new-autocompact" className="flex items-center gap-1.5">
                <ShrinkIcon className="size-3.5" /> Auto-Kompaktierung
              </Label>
              <p className="text-xs text-muted-foreground">
                Fasst den Verlauf zusammen, wenn das Kontextfenster knapp wird. Später im Chat umschaltbar.
              </p>
            </div>
            <Switch id="new-autocompact" checked={autoCompact} onCheckedChange={setAutoCompact} />
          </div>
          <div className="flex items-start justify-between gap-3 rounded-md border px-3 py-2">
            <div>
              <Label htmlFor="new-maxsub" className="flex items-center gap-1.5">
                <BotIcon className="size-3.5" /> Max. Subagenten
              </Label>
              <p className="text-xs text-muted-foreground">
                Hart durchgesetzt am Proxy und durch Abbruch.
                {maxSubLimit !== undefined && ` 0 bis ${maxSubLimit}.`} Später im Chat änderbar.
              </p>
            </div>
            <NumberStepper
              id="new-maxsub"
              value={maxSub}
              min={0}
              max={Math.max(maxSubLimit ?? maxSub, maxSub)}
              onChange={setMaxSub}
              aria-label="Max. Subagenten"
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="new-title">Titel (optional)</Label>
            <Input id="new-title" value={title} onChange={(e) => setTitle(e.target.value)} />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="new-message">Erste Nachricht (optional)</Label>
            <Textarea id="new-message" rows={4} value={message} onChange={(e) => setMessage(e.target.value)} />
          </div>
          {error && (
            <Alert variant="destructive">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>
            Abbrechen
          </Button>
          <Button disabled={busy} onClick={() => void submit()}>
            {busy ? "Wird angelegt …" : "Chat anlegen"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

const factorFmt = new Intl.NumberFormat("de-DE", { maximumFractionDigits: 2 })

function priceLine(p: Pricing, f = 1) {
  return `Eingabe ${formatPrice(p.input * f)} · Ausgabe ${formatPrice(p.output * f)} · Cache lesen ${formatPrice(
    p.cache_read * f,
  )} · Cache schreiben ${formatPrice(p.cache_write * f)}`
}

/** Preise je 1 Mio. Tokens, bei einem Tarif mit Spitzenzeiten als Spitzen- und Nebentarif. */
function PricingInfo({ model }: { model: Model }) {
  const p = model.pricing
  if (!p) return null
  const t = model.tariff
  const src = modelPriceSource(model)
  const srcText = sourceLabel(src)
  const windows = t ? formatPeakWindows(t.peak_windows_utc) : ""
  return (
    <div className="rounded-md bg-muted/60 px-2.5 py-1.5 text-xs text-muted-foreground">
      <div className="font-medium text-foreground">Preise je 1 Mio. Tokens</div>
      {t ? (
        <>
          <div className="mt-0.5">
            <span className={cn(model.peak_now === true && "font-medium text-foreground")}>Spitzentarif:</span>{" "}
            {priceLine(p)}
          </div>
          <div className="mt-0.5">
            <span className={cn(model.peak_now === false && "font-medium text-foreground")}>
              Nebentarif (× {factorFmt.format(t.offpeak_factor)}):
            </span>{" "}
            {priceLine(p, t.offpeak_factor)}
          </div>
          {windows && <div className="mt-0.5">Spitzenzeiten (Ortszeit): {windows}</div>}
          {model.peak_now !== undefined && (
            <div
              className={cn(
                "mt-1 inline-block rounded border px-1.5 py-px font-medium",
                model.peak_now ? "border-amber-200 bg-amber-50 text-amber-800" : "border-emerald-200 bg-emerald-50 text-emerald-800",
              )}
            >
              Gerade gilt der {model.peak_now ? "Spitzentarif" : "Nebentarif"}.
            </div>
          )}
          {t.note && <div className="mt-1 break-words italic">{t.note}</div>}
        </>
      ) : (
        <div className="mt-0.5">{priceLine(p)}</div>
      )}
      {p.note && <div className="mt-0.5 break-words italic">{p.note}</div>}
      {src.href && (
        <div className="mt-0.5">
          <a className="underline underline-offset-2 hover:text-foreground" href={src.href} target="_blank" rel="noreferrer" title={src.href}>
            {srcText}
          </a>
        </div>
      )}
    </div>
  )
}
