import { useEffect, useMemo, useState } from "react"
import { CodeIcon, Loader2Icon, NetworkIcon, TriangleAlertIcon } from "lucide-react"
import { ImagePreview } from "@/components/ImagePreview"
import { useDarkMode } from "@/hooks/useDarkMode"
import { autoRender, diagramView, isLargeDiagram, type MermaidOutcome, mermaidRenderer, svgDataUrl } from "@/lib/mermaid"
import { cn } from "@/lib/utils"

type Props = {
  code: string
  /** The block is complete (response finished or code block closed); nothing is rendered before that. */
  ready: boolean
  /** Position in the message (from 0); from MERMAID_AUTO_MAX on, rendering waits for a click. */
  index?: number
}

/**
 * Mermaid code block of a response as a diagram. The library loads only with the first diagram (lib/mermaid);
 * the SVG appears as an <img> in the large image view (ImagePreview), so without scripts and without fetching.
 */
export function MermaidDiagram({ code, ready, index = 0 }: Props) {
  const dark = useDarkMode()
  const theme = dark ? "dark" : "light"
  const [result, setResult] = useState<{ key: string; outcome: MermaidOutcome }>()
  const [showSource, setShowSource] = useState(false)
  const [clicked, setClicked] = useState(false)
  const key = `${theme}\n${code}`
  // Large diagrams and all from the sixth in a message on only after a click (Review 3, N4).
  const large = useMemo(() => isLargeDiagram(code), [code])
  const deferred = !clicked && !autoRender({ index, large })

  useEffect(() => {
    if (!ready || deferred) return
    let alive = true
    void mermaidRenderer.render(code, theme).then((outcome) => {
      if (alive) setResult({ key, outcome })
    })
    return () => {
      alive = false
    }
  }, [ready, deferred, code, theme, key])

  const outcome = result?.key === key ? result.outcome : undefined
  const view = diagramView({ ready, outcome, showSource, deferred })

  return (
    <div className="not-prose my-3 min-w-0">
      {view === "diagram" && outcome?.ok ? (
        <ImagePreview
          src={svgDataUrl(outcome.svg)}
          alt="Mermaid diagram"
          label="Mermaid diagram"
          filename="diagram.svg"
          thumbClassName="max-h-[28rem] bg-background p-2"
        />
      ) : (
        <pre className="max-h-96 overflow-auto rounded-md bg-zinc-900 p-3 font-mono text-xs whitespace-pre text-zinc-100">
          <code>{code}</code>
        </pre>
      )}
      <div className="mt-1 flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground">
        {view === "deferred" && (
          <button
            type="button"
            onClick={() => setClicked(true)}
            className="inline-flex items-center gap-1 rounded px-1 hover:bg-muted hover:text-foreground"
            title="Rendering large or many diagrams stalls the browser, so only on click."
          >
            <NetworkIcon className="size-3" />
            {large ? "Large diagram, click to render" : "Another diagram, click to render"}
          </button>
        )}
        {view === "loading" && (
          <span className="inline-flex items-center gap-1">
            <Loader2Icon className="size-3 animate-spin" /> Rendering diagram …
          </span>
        )}
        {view === "error" && outcome && !outcome.ok && (
          <span className="inline-flex min-w-0 items-center gap-1 text-amber-700 dark:text-amber-400" title={outcome.error}>
            <TriangleAlertIcon className="size-3 shrink-0" />
            <span className="truncate">Diagram could not be rendered</span>
          </span>
        )}
        {outcome?.ok && (
          <button
            type="button"
            onClick={() => setShowSource((v) => !v)}
            className={cn("inline-flex items-center gap-1 rounded px-1 hover:bg-muted hover:text-foreground")}
            aria-pressed={showSource}
          >
            {showSource ? <NetworkIcon className="size-3" /> : <CodeIcon className="size-3" />}
            {showSource ? "Diagram" : "Source"}
          </button>
        )}
      </div>
    </div>
  )
}
