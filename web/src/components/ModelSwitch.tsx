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

const num = (n: number) => n.toLocaleString("en-US")

/** Prompt when the context does not fit the new model: compact first, then switch. */
export function ContextTooLargeDialog({ sw, modelName }: { sw: ModelSwitch; modelName: (id: string) => string }) {
  const { tooLarge, busy } = sw
  return (
    <Dialog open={!!tooLarge} onOpenChange={(o) => !o && sw.cancel()}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Context does not fit {tooLarge ? modelName(tooLarge.model) : ""}</DialogTitle>
          <DialogDescription>
            {tooLarge &&
              `The history so far has ${num(tooLarge.tokens)} tokens. ${modelName(tooLarge.model)} processes ` +
                `at most ${num(tooLarge.limit)} (context window ${num(tooLarge.window)}). Compact the history ` +
                "first? Then the chat continues with the new model."}
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" onClick={sw.cancel}>
            Cancel
          </Button>
          <Button disabled={busy} onClick={() => void sw.compactAndSwitch()}>
            {busy && <LoaderCircleIcon className="animate-spin" />} Compact and switch
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

/** Model and thinking level at the bottom of the input field. Values from the suggestions of /model and /effort. */
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
  const hint = disabled ? "Only after the running response" : undefined
  const trigger = "h-8 max-w-44 gap-1 rounded-full border-0 bg-transparent px-2 text-xs text-muted-foreground shadow-none hover:bg-muted"
  return (
    <div className="flex min-w-0 items-center">
      <Select value={chat.model} onValueChange={onModel} disabled={disabled || !!chat.pending_model}>
        <SelectTrigger size="sm" className={trigger} title={hint ?? "Model (/model)"} aria-label="Model">
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
          <SelectTrigger size="sm" className={trigger} title={hint ?? "Thinking level (/effort)"} aria-label="Thinking level">
            <BrainIcon className="size-3.5" />
            <SelectValue placeholder="Thinking level">
              {chat.thinking_level ? (effortLabels[chat.thinking_level] ?? chat.thinking_level) : "Thinking level"}
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
          switches to {modelName(chat.pending_model)} after compaction
        </span>
      )}
    </div>
  )
}
