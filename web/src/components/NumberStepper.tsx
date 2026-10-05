import { useState } from "react"
import { MinusIcon, PlusIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { clampInt } from "@/lib/number"

type Props = {
  id?: string
  value: number
  min: number
  max: number
  disabled?: boolean
  onChange: (v: number) => void
  "aria-label"?: string
}

/** Number field with minus/plus; input is clamped to [min, max] once the field loses focus. */
export function NumberStepper({ id, value, min, max, disabled, onChange, "aria-label": ariaLabel }: Props) {
  const [draft, setDraft] = useState<string>()
  const commit = (v: number | string) => {
    setDraft(undefined)
    const n = clampInt(v, min, max)
    if (n !== value) onChange(n)
  }
  return (
    <div className="inline-flex items-center gap-1">
      <Button
        type="button"
        size="icon-sm"
        variant="outline"
        disabled={disabled || value <= min}
        aria-label="less"
        onClick={() => commit(value - 1)}
      >
        <MinusIcon />
      </Button>
      <Input
        id={id}
        type="number"
        inputMode="numeric"
        min={min}
        max={max}
        step={1}
        disabled={disabled}
        aria-label={ariaLabel}
        className="h-8 w-14 text-center tabular-nums"
        value={draft ?? String(value)}
        onChange={(e) => setDraft(e.target.value)}
        onBlur={() => draft !== undefined && commit(draft)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && draft !== undefined) {
            e.preventDefault()
            commit(draft)
          }
        }}
      />
      <Button
        type="button"
        size="icon-sm"
        variant="outline"
        disabled={disabled || value >= max}
        aria-label="more"
        onClick={() => commit(value + 1)}
      >
        <PlusIcon />
      </Button>
    </div>
  )
}
