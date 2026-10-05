import { useEffect, useMemo, useState } from "react"
import { CodeIcon, Loader2Icon, NetworkIcon, TriangleAlertIcon } from "lucide-react"
import { ImagePreview } from "@/components/ImagePreview"
import { useDarkMode } from "@/hooks/useDarkMode"
import { autoRender, diagramView, isLargeDiagram, type MermaidOutcome, mermaidRenderer, svgDataUrl } from "@/lib/mermaid"
import { cn } from "@/lib/utils"

type Props = {
  code: string
  /** Der Block ist vollständig (Antwort fertig oder Codeblock geschlossen); vorher wird nichts gezeichnet. */
  ready: boolean
  /** Position in der Nachricht (ab 0); ab MERMAID_AUTO_MAX wird erst auf Klick gezeichnet. */
  index?: number
}

/**
 * Mermaid-Codeblock einer Antwort als Diagramm. Die Bibliothek lädt erst beim ersten Diagramm (lib/mermaid);
 * das SVG erscheint als <img> in der Bild-Großansicht (ImagePreview), also ohne Skript und ohne Nachladen.
 */
export function MermaidDiagram({ code, ready, index = 0 }: Props) {
  const dark = useDarkMode()
  const theme = dark ? "dark" : "light"
  const [result, setResult] = useState<{ key: string; outcome: MermaidOutcome }>()
  const [showSource, setShowSource] = useState(false)
  const [clicked, setClicked] = useState(false)
  const key = `${theme}\n${code}`
  // Große Diagramme und alle ab dem sechsten einer Nachricht erst auf Klick (Review 3, N4).
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
          alt="Mermaid-Diagramm"
          label="Mermaid-Diagramm"
          filename="diagramm.svg"
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
            title="Das Zeichnen großer oder vieler Diagramme hält den Browser auf; deshalb erst auf Klick."
          >
            <NetworkIcon className="size-3" />
            {large ? "Großes Diagramm, zum Zeichnen klicken" : "Weiteres Diagramm, zum Zeichnen klicken"}
          </button>
        )}
        {view === "loading" && (
          <span className="inline-flex items-center gap-1">
            <Loader2Icon className="size-3 animate-spin" /> Diagramm wird gezeichnet …
          </span>
        )}
        {view === "error" && outcome && !outcome.ok && (
          <span className="inline-flex min-w-0 items-center gap-1 text-amber-700 dark:text-amber-400" title={outcome.error}>
            <TriangleAlertIcon className="size-3 shrink-0" />
            <span className="truncate">Diagramm konnte nicht gerendert werden</span>
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
            {showSource ? "Diagramm" : "Quelltext"}
          </button>
        )}
      </div>
    </div>
  )
}
