import { useState } from "react"
import { DownloadIcon, ImageOffIcon } from "lucide-react"
import { Dialog, DialogContent, DialogDescription, DialogTitle, DialogTrigger } from "@/components/ui/dialog"
import { cn } from "@/lib/utils"

type Props = {
  src: string
  alt?: string
  /** Path or file name: tooltip, caption in the large view and hint on load errors. */
  label?: string
  /** File name for "Download". */
  filename?: string
  /** Classes of the thumbnail (default: at most 320 px high). */
  thumbClassName?: string
}

/**
 * Image as a small thumbnail; a click opens the large view with download. Only for addresses the UI may load
 * (orchestrator endpoint, own attachments, data: raster images), see lib/images.
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
        <span className="truncate">Image not available{label ? ` · ${label}` : ""}</span>
      </span>
    )
  }
  const name = filename ?? label?.split("/").pop() ?? "image"
  return (
    <Dialog>
      <DialogTrigger asChild>
        <button
          type="button"
          className="not-prose my-1 inline-block max-w-full cursor-zoom-in rounded-lg align-top focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          title={label ? `${label} – enlarge` : "enlarge"}
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
            {/* second line only if it says more than the title (e.g. the path) */}
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
            Download
          </a>
        </div>
      </DialogContent>
    </Dialog>
  )
}
