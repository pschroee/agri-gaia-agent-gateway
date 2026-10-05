import { createContext, memo, useContext, useMemo } from "react"
import ReactMarkdown, { type Components, defaultUrlTransform, type ExtraProps, type UrlTransform } from "react-markdown"
import remarkGfm from "remark-gfm"
import { ImagePreview } from "@/components/ImagePreview"
import { MermaidDiagram } from "@/components/MermaidDiagram"
import { imageSource } from "@/lib/images"
import { isMermaidClass, mermaidFenceStarts, openFenceStart } from "@/lib/mermaid"
import { cn } from "@/lib/utils"

/** Beim Streamen: Anfang des noch offenen Codeblocks am Textende (lib/mermaid, openFenceStart). */
const OpenFence = createContext<number | undefined>(undefined)
/** Anfänge der Mermaid-Blöcke der Nachricht: Position eines Diagramms (höchstens fünf von selbst). */
const MermaidStarts = createContext<number[]>([])

type HastNode = NonNullable<ExtraProps["node"]>
const textOf = (n: HastNode["children"][number]): string =>
  n.type === "text" ? n.value : "children" in n ? n.children.map(textOf).join("") : ""

/**
 * Codeblöcke mit Sprache mermaid werden als Diagramm gezeichnet, aber erst, wenn der Block vollständig ist:
 * Solange die Antwort streamt und der Block am Textende noch offen ist, bleibt es beim Quelltext.
 */
function Pre({ node, ...props }: React.ComponentProps<"pre"> & ExtraProps) {
  const open = useContext(OpenFence)
  const starts = useContext(MermaidStarts)
  const code = node?.children.find((c) => c.type === "element" && c.tagName === "code")
  if (code && code.type === "element" && isMermaidClass(code.properties.className)) {
    const start = node?.position?.start.offset
    const ready = open === undefined || start === undefined || start < open
    const index = start === undefined ? 0 : Math.max(0, starts.filter((s) => s <= start).length - 1)
    return <MermaidDiagram code={textOf(code).replace(/\n$/, "")} ready={ready} index={index} />
  }
  return <pre {...props} />
}

function Placeholder({ text, title }: { text: string; title?: string }) {
  return (
    <span className="rounded bg-muted px-1 text-xs text-muted-foreground" title={title}>
      [{text}]
    </span>
  )
}

/**
 * Bilder: Fremde Adressen werden nie geladen, nur als Text gezeigt. Über die Bildadresse könnten Daten aus der
 * Sandbox zu einem fremden Server gelangen, ohne dass der Nutzer Internet bestätigt hat
 * (Markdown-Image-Exfiltration). Lokale Pfade aus der Sandbox lädt der Orchestrator (lib/images), und nur,
 * wenn Chat und fertige Antwort bekannt sind; data:-Rasterbilder bleiben im Browser.
 */
function makeComponents(chatId?: string, msgId?: string): Components {
  return {
    img: ({ src, alt }) => {
      const raw = typeof src === "string" ? src : ""
      const s = imageSource(raw, chatId ? { chatId, msgId } : undefined)
      switch (s.kind) {
        case "data":
          return <ImagePreview src={s.url} alt={alt} label={alt || "eingebettetes Bild"} />
        case "sandbox":
          return <ImagePreview src={s.url} alt={alt} label={s.path} />
        case "pending":
          if (chatId) {
            const text = `Bild${alt ? `: ${alt}` : ""} · ${s.path}`
            return <Placeholder text={text} title="erscheint, sobald die Antwort fertig ist" />
          }
          break
      }
      return <Placeholder text={`Bild nicht geladen${alt ? `: ${alt}` : ""} · ${raw}`} title={raw || undefined} />
    },
    pre: Pre,
    a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noreferrer noopener" />,
    table: ({ node: _node, ...props }) => (
      <div className="my-3 overflow-x-auto">
        <table {...props} className="my-0" />
      </div>
    ),
  }
}

const defaultComponents = makeComponents()

// react-markdown verwirft data:-Adressen; eingebettete Rasterbilder sollen aber erscheinen (die Prüfung
// macht imageSource). Alles andere wie gewohnt.
const urlTransform: UrlTransform = (url, key) =>
  key === "src" && /^data:image\//i.test(url) ? url : defaultUrlTransform(url)

const plugins = [remarkGfm]

type Props = {
  text: string
  className?: string
  /** Chat, aus dessen Sandbox lokale Bildpfade geladen werden; ohne ihn bleiben sie Platzhalter. */
  chatId?: string
  /** Kennung der fertigen Antwort (AssistantItem.msgKey); fehlt beim Streamen. */
  msgId?: string
  /** Die Antwort wird noch gestreamt: ein offener Mermaid-Block am Ende bleibt Quelltext. */
  streaming?: boolean
}

/**
 * Rendert Markdown der Assistenten-Antworten (GFM). Rohes HTML wird verworfen (`skipHtml`),
 * halbfertiges Markdown beim Streamen ist unkritisch: der Parser schließt offene Blöcke selbst.
 */
export const Markdown = memo(function Markdown({ text, className, chatId, msgId, streaming }: Props) {
  const components = useMemo(() => (chatId ? makeComponents(chatId, msgId) : defaultComponents), [chatId, msgId])
  const open = streaming ? openFenceStart(text) : undefined
  const starts = useMemo(() => mermaidFenceStarts(text), [text])
  return (
    <OpenFence.Provider value={open}>
      <MermaidStarts.Provider value={starts}>
      <div
        className={cn(
          "prose prose-sm max-w-none min-w-0 break-words text-foreground prose-neutral [&>*:first-child]:mt-0 [&>*:last-child]:mb-0",
          "prose-headings:mt-4 prose-headings:mb-2 prose-headings:font-semibold prose-p:my-2 prose-p:leading-relaxed",
          "prose-ul:my-2 prose-ol:my-2 prose-li:my-0.5 prose-a:text-sky-700",
          // Code-Blöcke
          "prose-pre:my-3 prose-pre:overflow-x-auto prose-pre:rounded-md prose-pre:bg-zinc-900 prose-pre:p-3 prose-pre:text-xs prose-pre:text-zinc-100",
          // Inline-Code ohne die Backticks der Typografie-Vorgabe
          "prose-code:font-mono prose-code:font-normal prose-code:before:content-none prose-code:after:content-none",
          "[&_:not(pre)>code]:rounded [&_:not(pre)>code]:bg-muted [&_:not(pre)>code]:px-1 [&_:not(pre)>code]:py-0.5 [&_:not(pre)>code]:text-[0.85em] [&_:not(pre)>code]:break-all",
          "prose-th:border prose-th:bg-muted/60 prose-th:px-2 prose-th:py-1 prose-td:border prose-td:px-2 prose-td:py-1",
          className,
        )}
      >
        <ReactMarkdown remarkPlugins={plugins} components={components} urlTransform={urlTransform} skipHtml>
          {text}
        </ReactMarkdown>
      </div>
      </MermaidStarts.Provider>
    </OpenFence.Provider>
  )
})
