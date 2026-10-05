import { BrainIcon, CpuIcon, LoaderCircleIcon } from "lucide-react"
import type { Chat, Command } from "@/api/types"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import type { useModelSwitch } from "@/hooks/useModelSwitch"
import { effortLabels } from "@/lib/commands"

type ModelSwitch = ReturnType<typeof useModelSwitch>

const num = (n: number) => n.toLocaleString("de-DE")

/** Rückfrage, wenn der Kontext nicht ins neue Modell passt: erst kompaktieren, dann wechseln. */
export function ContextTooLargeDialog({ sw, modelName }: { sw: ModelSwitch; modelName: (id: string) => string }) {
  const { tooLarge, busy } = sw
  return (
    <Dialog open={!!tooLarge} onOpenChange={(o) => !o && sw.cancel()}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Kontext passt nicht in {tooLarge ? modelName(tooLarge.model) : ""}</DialogTitle>
          <DialogDescription>
            {tooLarge &&
              `Der bisherige Verlauf umfasst ${num(tooLarge.tokens)} Tokens. ${modelName(tooLarge.model)} verarbeitet ` +
                `höchstens ${num(tooLarge.limit)} (Kontextfenster ${num(tooLarge.window)}). Soll der Verlauf erst ` +
                "kompaktiert werden? Danach geht es mit dem neuen Modell weiter."}
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" onClick={sw.cancel}>
            Abbrechen
          </Button>
          <Button disabled={busy} onClick={() => void sw.compactAndSwitch()}>
            {busy && <LoaderCircleIcon className="animate-spin" />} Kompaktieren und wechseln
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

/** Modell und Denkstufe unten im Eingabefeld. Werte aus den Vorschlägen von /model und /effort. */
export function ModelEffortPicker({
  chat,
  commands,
  disabled,
  modelName,
  onModel,
  onEffort,
}: {
  chat: Chat
  commands: Command[]
  disabled: boolean
  modelName: (id: string) => string
  onModel: (model: string) => void
  onEffort: (level: string) => void
}) {
  const models = commands.find((c) => c.name === "model")?.options ?? [{ value: chat.model }]
  const levels = commands.find((c) => c.name === "effort")?.options ?? []
  const hint = disabled ? "Erst nach der laufenden Antwort" : undefined
  const trigger = "h-8 max-w-44 gap-1 rounded-full border-0 bg-transparent px-2 text-xs text-muted-foreground shadow-none hover:bg-muted"
  return (
    <div className="flex min-w-0 items-center">
      <Select value={chat.model} onValueChange={onModel} disabled={disabled || !!chat.pending_model}>
        <SelectTrigger size="sm" className={trigger} title={hint ?? "Modell (/model)"} aria-label="Modell">
          <CpuIcon className="size-3.5" />
          <SelectValue>{modelName(chat.model)}</SelectValue>
        </SelectTrigger>
        <SelectContent>
          {models.map((o) => (
            <SelectItem key={o.value} value={o.value}>
              {o.label ?? o.value}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {levels.length > 1 && (
        <Select value={chat.thinking_level ?? ""} onValueChange={onEffort} disabled={disabled}>
          <SelectTrigger size="sm" className={trigger} title={hint ?? "Denkstufe (/effort)"} aria-label="Denkstufe">
            <BrainIcon className="size-3.5" />
            <SelectValue placeholder="Denkstufe">
              {chat.thinking_level ? (effortLabels[chat.thinking_level] ?? chat.thinking_level) : "Denkstufe"}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            {levels.map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {o.label ?? o.value}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      )}
      {chat.pending_model && (
        <span className="truncate text-xs text-muted-foreground">
          wechselt nach dem Kompaktieren zu {modelName(chat.pending_model)}
        </span>
      )}
    </div>
  )
}
