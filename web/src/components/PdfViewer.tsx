import { useEffect, useRef, useState } from "react"
import { MinusIcon, PlusIcon, ScanIcon } from "lucide-react"
import { GlobalWorkerOptions, getDocument, type PDFDocumentProxy } from "pdfjs-dist"
import workerUrl from "pdfjs-dist/build/pdf.worker.min.mjs?url"
import { Button } from "@/components/ui/button"

// Worker von derselben Herkunft (CSP: script-src 'self'); pdf.js 6 kommt ohne eval aus.
GlobalWorkerOptions.workerSrc = workerUrl

const ZOOM_STEPS = [0.5, 0.67, 0.8, 0.9, 1, 1.1, 1.25, 1.5, 1.75, 2, 2.5, 3]

/**
 * PDF-Betrachter mit pdf.js: Seiten als Canvas untereinander auf hellem Grund, scrollbar, mit Zoom.
 * Wird erst beim Öffnen eines PDFs geladen (lazy, eigener Chunk). Skripte und Formulare im PDF
 * laufen nicht; gezeichnet wird nur.
 */
export default function PdfViewer({ data, name }: { data: ArrayBuffer; name: string }) {
  const [doc, setDoc] = useState<PDFDocumentProxy>()
  const [error, setError] = useState<string>()
  const [zoom, setZoom] = useState(1) // 1 = Seitenbreite
  const [width, setWidth] = useState(0)
  const [page, setPage] = useState(1)
  const scroller = useRef<HTMLDivElement>(null)

  useEffect(() => {
    let cancelled = false
    const task = getDocument({ data: new Uint8Array(data.slice(0)) })
    task.promise.then(
      (d) => !cancelled && setDoc(d),
      (e: unknown) => !cancelled && setError(e instanceof Error ? e.message : String(e)),
    )
    return () => {
      cancelled = true
      void task.destroy()
    }
  }, [data])

  // Breite des Bereichs für „Seitenbreite“; folgt Größenänderungen des Dialogs.
  useEffect(() => {
    const el = scroller.current
    if (!el) return
    const ro = new ResizeObserver(() => setWidth(el.clientWidth))
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  // Aktuelle Seite: die, deren Mitte der oberen Hälfte des Bereichs am nächsten liegt.
  const onScroll = () => {
    const el = scroller.current
    if (!el) return
    const probe = el.scrollTop + el.clientHeight / 3
    let current = 1
    for (const child of Array.from(el.querySelectorAll<HTMLElement>("[data-page]"))) {
      if (child.offsetTop <= probe) current = Number(child.dataset.page)
    }
    setPage(current)
  }

  const step = (dir: 1 | -1) => {
    const i = ZOOM_STEPS.findIndex((z) => z >= zoom - 1e-6)
    const next = ZOOM_STEPS[Math.min(ZOOM_STEPS.length - 1, Math.max(0, (i < 0 ? ZOOM_STEPS.length - 1 : i) + dir))]
    setZoom(next)
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex items-center gap-1 border-b bg-background px-3 py-1.5 text-xs text-muted-foreground">
        <span className="tabular-nums">{doc ? `Seite ${page} von ${doc.numPages}` : "Lade …"}</span>
        <div className="ml-auto flex items-center gap-0.5">
          <Button size="icon-sm" variant="ghost" onClick={() => step(-1)} disabled={zoom <= ZOOM_STEPS[0]} aria-label="Verkleinern">
            <MinusIcon />
          </Button>
          <span className="w-12 text-center tabular-nums">{Math.round(zoom * 100)} %</span>
          <Button size="icon-sm" variant="ghost" onClick={() => step(1)} disabled={zoom >= ZOOM_STEPS[ZOOM_STEPS.length - 1]} aria-label="Vergrößern">
            <PlusIcon />
          </Button>
          <Button size="sm" variant="ghost" className="h-7 px-2 text-xs" onClick={() => setZoom(1)} title="An Seitenbreite anpassen">
            <ScanIcon className="size-3.5" /> Seitenbreite
          </Button>
        </div>
      </div>
      <div ref={scroller} onScroll={onScroll} className="min-h-0 flex-1 overflow-auto bg-muted/60 dark:bg-neutral-900">
        {error ? (
          <p className="p-6 text-sm text-red-700">PDF nicht lesbar: {error}</p>
        ) : doc && width > 0 ? (
          <div className="flex flex-col items-center gap-4 px-4 py-5">
            {Array.from({ length: doc.numPages }, (_, i) => (
              <PdfPage key={i} doc={doc} number={i + 1} fitWidth={Math.min(width - 32, 1100)} zoom={zoom} name={name} />
            ))}
          </div>
        ) : null}
      </div>
    </div>
  )
}

/** Eine Seite als Canvas; gezeichnet in Bildschirmauflösung (devicePixelRatio). */
function PdfPage({ doc, number, fitWidth, zoom, name }: { doc: PDFDocumentProxy; number: number; fitWidth: number; zoom: number; name: string }) {
  const canvas = useRef<HTMLCanvasElement>(null)
  const [size, setSize] = useState<{ w: number; h: number }>()
  useEffect(() => {
    let cancelled = false
    let render: { cancel: () => void } | undefined
    void doc.getPage(number).then((p) => {
      if (cancelled || !canvas.current) return
      const base = p.getViewport({ scale: 1 })
      const scale = (fitWidth / base.width) * zoom
      const vp = p.getViewport({ scale })
      const dpr = window.devicePixelRatio || 1
      const c = canvas.current
      c.width = Math.floor(vp.width * dpr)
      c.height = Math.floor(vp.height * dpr)
      setSize({ w: vp.width, h: vp.height })
      const task = p.render({ canvas: c, viewport: vp, transform: dpr !== 1 ? [dpr, 0, 0, dpr, 0, 0] : undefined })
      render = task
      task.promise.catch(() => {}) // abgebrochen beim Zoomen
    })
    return () => {
      cancelled = true
      render?.cancel()
    }
  }, [doc, number, fitWidth, zoom])
  return (
    <div data-page={number} className="shrink-0 bg-white shadow-md ring-1 ring-black/5" style={size ? { width: size.w, height: size.h } : { width: fitWidth * zoom, aspectRatio: "1 / 1.414" }}>
      <canvas ref={canvas} aria-label={`${name}, Seite ${number}`} style={size ? { width: size.w, height: size.h } : undefined} className="block" />
    </div>
  )
}
