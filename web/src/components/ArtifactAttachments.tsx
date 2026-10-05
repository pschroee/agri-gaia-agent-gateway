import { lazy, Suspense, useEffect, useState } from "react"
import { DownloadIcon, FileIcon, FileTextIcon, LoaderCircleIcon } from "lucide-react"
import { urls } from "@/api/client"
import type { Artifact } from "@/api/types"
import { ImagePreview } from "@/components/ImagePreview"
import { Dialog, DialogContent, DialogDescription, DialogTitle, DialogTrigger } from "@/components/ui/dialog"
import { formatBytes } from "@/lib/format"
import { previewKind } from "@/lib/artifactPreview"

// pdf.js erst beim ersten Öffnen eines PDFs laden (eigener Chunk).
const PdfViewer = lazy(() => import("@/components/PdfViewer"))

/**
 * Ergebnisse eines Werkzeugaufrufs im Verlauf, direkt unter dem Aufruf, der sie hochgeladen hat:
 * Bilder als Vorschau (Klick vergrößert), PDFs mit Vorschau per Klick, alles mit Download.
 */
export function ArtifactAttachments({ chatId, artifacts }: { chatId: string; artifacts: Artifact[] }) {
  if (artifacts.length === 0) return null
  return (
    <div className="mt-1.5 flex flex-col gap-1.5" aria-label="Hochgeladene Ergebnisse">
      {artifacts.map((a) => (
        <ArtifactItem key={a.name} chatId={chatId} artifact={a} />
      ))}
    </div>
  )
}

function ArtifactItem({ chatId, artifact: a }: { chatId: string; artifact: Artifact }) {
  const url = urls.artifact(chatId, a.name, a.kind)
  const kind = previewKind(a)
  const meta = (
    <div className="flex min-w-0 items-center gap-2 text-xs text-muted-foreground">
      <span className="min-w-0 truncate font-mono text-foreground" title={a.name}>
        {a.name}
      </span>
      <span className="shrink-0 tabular-nums">{formatBytes(a.size)}</span>
      <span className="ml-auto" />
      <DownloadLink url={url} name={a.name} />
    </div>
  )
  if (kind === "image") {
    return (
      <div className="rounded-md border bg-background p-2">
        <ImagePreview src={url} alt={a.name} label={a.name} filename={a.name} thumbClassName="max-h-64" />
        {meta}
      </div>
    )
  }
  if (kind === "pdf") return <PdfPreview url={url} name={a.name} size={a.size} />
  return (
    <div className="flex items-center gap-2 rounded-md border bg-background px-3 py-2">
      <FileIcon className="size-4 shrink-0 text-muted-foreground" aria-hidden />
      <div className="min-w-0 flex-1">{meta}</div>
    </div>
  )
}

function DownloadLink({ url, name }: { url: string; name: string }) {
  return (
    <a
      href={url}
      download={name}
      className="inline-flex shrink-0 items-center gap-1 rounded-md px-1.5 py-0.5 text-xs text-sky-700 hover:bg-muted hover:underline"
      title={`${name} herunterladen`}
    >
      <DownloadIcon className="size-3.5" /> Herunterladen
    </a>
  )
}

function Loading() {
  return (
    <div className="m-auto flex items-center text-sm text-muted-foreground">
      <LoaderCircleIcon className="mr-2 size-4 animate-spin" /> Lade …
    </div>
  )
}

/**
 * PDF-Vorschau im Dialog: Klick auf die Karte öffnet das PDF groß (eigener Betrachter mit pdf.js,
 * scrollbar, mit Zoom), mit Download oben rechts wie in der Großansicht der Bilder. Der Orchestrator
 * liefert Artefakte nur zum Herunterladen aus (attachment); die UI lädt die Datei selbst und zeichnet
 * sie mit pdf.js auf Canvas, so dass kein Inhalt des Agenten als Seite dieser Anwendung geöffnet wird.
 */
function PdfPreview({ url, name, size }: { url: string; name: string; size: number }) {
  const [open, setOpen] = useState(false)
  const [data, setData] = useState<ArrayBuffer>()
  const [error, setError] = useState<string>()
  useEffect(() => {
    if (!open) return
    let cancelled = false
    fetch(url, { credentials: "same-origin" })
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`)
        return r.arrayBuffer()
      })
      .then((b) => !cancelled && setData(b))
      .catch((e: unknown) => !cancelled && setError(e instanceof Error ? e.message : String(e)))
    return () => {
      cancelled = true
      setData(undefined)
    }
  }, [open, url])
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (o) setError(undefined)
        setOpen(o)
      }}
    >
      <div className="flex items-center gap-2 rounded-md border bg-background pr-2 hover:bg-muted/40">
        <DialogTrigger asChild>
          <button
            type="button"
            className="flex min-w-0 flex-1 cursor-zoom-in items-center gap-2 px-3 py-2 text-left text-xs"
            title={`${name} ansehen`}
          >
            <FileTextIcon className="size-5 shrink-0 text-red-700" aria-hidden />
            <span className="min-w-0 truncate font-mono text-foreground">{name}</span>
            <span className="shrink-0 text-muted-foreground tabular-nums">{formatBytes(size)}</span>
            <span className="shrink-0 text-muted-foreground">· PDF ansehen</span>
          </button>
        </DialogTrigger>
        <DownloadLink url={url} name={name} />
      </div>
      <DialogContent className="flex h-[92vh] w-[95vw] max-w-[95vw] flex-col gap-0 overflow-hidden p-0 sm:max-w-5xl">
        <div className="flex min-w-0 items-center gap-3 border-b bg-background py-2.5 pr-12 pl-4">
          <FileTextIcon className="size-5 shrink-0 text-red-700" aria-hidden />
          <div className="min-w-0 flex-1">
            <DialogTitle className="truncate text-sm">{name}</DialogTitle>
            <DialogDescription className="text-xs">{formatBytes(size)} · PDF</DialogDescription>
          </div>
          <a
            href={url}
            download={name}
            className="inline-flex shrink-0 items-center gap-1 rounded-md border px-2 py-1 text-xs hover:bg-muted"
          >
            <DownloadIcon className="size-3" />
            Herunterladen
          </a>
        </div>
        {error ? (
          <p className="m-auto text-sm text-red-700">PDF nicht geladen: {error}</p>
        ) : data ? (
          <Suspense fallback={<Loading />}>
            <PdfViewer data={data} name={name} />
          </Suspense>
        ) : (
          <Loading />
        )}
      </DialogContent>
    </Dialog>
  )
}
