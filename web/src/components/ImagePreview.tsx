import { useState } from "react"
import { DownloadIcon, ImageOffIcon } from "lucide-react"
import { Dialog, DialogContent, DialogDescription, DialogTitle, DialogTrigger } from "@/components/ui/dialog"
import { cn } from "@/lib/utils"

type Props = {
  src: string
  alt?: string
  /** Pfad oder Dateiname: Tooltip, Unterschrift in der Großansicht und Hinweis bei Ladefehlern. */
  label?: string
  /** Dateiname für „Herunterladen“. */
  filename?: string
  /** Klassen des Vorschaubilds (Standard: höchstens 320 px hoch). */
  thumbClassName?: string
}

/**
 * Bild als kleine Vorschau; ein Klick öffnet die Großansicht mit Download. Nur für Adressen, die die UI laden
 * darf (Endpunkt des Orchestrators, eigene Anhänge, data:-Rasterbilder), siehe lib/images.
 */
export function ImagePreview({ src, alt, label, filename, thumbClassName }: Props) {
  const [failed, setFailed] = useState(false)
  if (failed) {
    return (
      <span
        className="not-prose inline-flex max-w-full items-center gap-1 rounded border bg-muted px-1.5 py-0.5 text-xs text-muted-foreground"
        title={label}
      >
        <ImageOffIcon className="size-3 shrink-0" />
        <span className="truncate">Bild nicht verfügbar{label ? ` · ${label}` : ""}</span>
      </span>
    )
  }
  const name = filename ?? label?.split("/").pop() ?? "bild"
  return (
    <Dialog>
      <DialogTrigger asChild>
        <button
          type="button"
          className="not-prose my-1 inline-block max-w-full cursor-zoom-in rounded-lg align-top focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          title={label ? `${label} – vergrößern` : "vergrößern"}
        >
          <img
            src={src}
            alt={alt ?? ""}
            loading="lazy"
            onError={() => setFailed(true)}
            className={cn("block max-h-80 max-w-full rounded-lg border bg-white object-contain hover:opacity-95", thumbClassName)}
          />
        </button>
      </DialogTrigger>
      <DialogContent className="flex max-h-[95vh] w-auto max-w-[95vw] flex-col items-center gap-2 p-3 sm:max-w-[95vw]">
        <img src={src} alt={alt ?? ""} className="max-h-[80vh] max-w-full rounded object-contain" />
        <div className="flex w-full min-w-0 items-center gap-3 text-xs">
          <div className="min-w-0 flex-1">
            <DialogTitle className="truncate text-sm">{alt || name}</DialogTitle>
            {/* zweite Zeile nur, wenn sie mehr sagt als der Titel (etwa den Pfad) */}
            {(label ?? name) !== (alt || name) ? (
              <DialogDescription className="truncate text-xs">{label ?? name}</DialogDescription>
            ) : (
              <DialogDescription className="sr-only">{name}</DialogDescription>
            )}
          </div>
          <a
            href={src}
            download={name}
            className="inline-flex shrink-0 items-center gap-1 rounded-md border px-2 py-1 hover:bg-muted"
          >
            <DownloadIcon className="size-3" />
            Herunterladen
          </a>
        </div>
      </DialogContent>
    </Dialog>
  )
}
