import { createContext, memo, useContext, useMemo } from "react"
import ReactMarkdown, { type Components, defaultUrlTransform, type ExtraProps, type UrlTransform } from "react-markdown"
import remarkGfm from "remark-gfm"
import { ImagePreview } from "@/components/ImagePreview"
import { MermaidDiagram } from "@/components/MermaidDiagram"
import { imageSource } from "@/lib/images"
import { isMermaidClass, mermaidFenceStarts, openFenceStart } from "@/lib/mermaid"
import { cn } from "@/lib/utils"

/** While streaming: start of the still open code block at the end of the text (lib/mermaid, openFenceStart). */
const OpenFence = createContext<number | undefined>(undefined)
/** Starts of the Mermaid blocks of the message: position of a diagram (at most five on their own). */
const MermaidStarts = createContext<number[]>([])

type HastNode = NonNullable<ExtraProps["node"]>
const textOf = (n: HastNode["children"][number]): string =>
  n.type === "text" ? n.value : "children" in n ? n.children.map(textOf).join("") : ""

/**
 * Code blocks with language mermaid are rendered as diagrams, but only once the block is complete:
 * while the response streams and the block at the end of the text is still open, the source stays.
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
 * Images: foreign addresses are never loaded, only shown as text. Through the image address, data from the
 * sandbox could reach a foreign server without the user having confirmed internet access
 * (Markdown image exfiltration). Local paths from the sandbox are loaded by the orchestrator (lib/images), and
 * only when chat and finished response are known; data: raster images stay in the browser.
 */
function makeComponents(chatId?: string, msgId?: string): Components {
  return {
    img: ({ src, alt }) => {
      const raw = typeof src === "string" ? src : ""
      const s = imageSource(raw, chatId ? { chatId, msgId } : undefined)
      switch (s.kind) {
        case "data":
          return <ImagePreview src={s.url} alt={alt} label={alt || "embedded image"} />
        case "sandbox":
          return <ImagePreview src={s.url} alt={alt} label={s.path} />
        case "pending":
          if (chatId) {
            const text = `Image${alt ? `: ${alt}` : ""} · ${s.path}`
            return <Placeholder text={text} title="appears once the response is finished" />
          }
          break
      }
      return <Placeholder text={`Image not loaded${alt ? `: ${alt}` : ""} · ${raw}`} title={raw || undefined} />
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

// react-markdown discards data: addresses; embedded raster images should appear, though (imageSource does
// the check). Everything else as usual.
const urlTransform: UrlTransform = (url, key) =>
  key === "src" && /^data:image\//i.test(url) ? url : defaultUrlTransform(url)

const plugins = [remarkGfm]

type Props = {
  text: string
  className?: string
  /** Chat from whose sandbox local image paths are loaded; without it they stay placeholders. */
  chatId?: string
  /** ID of the finished response (AssistantItem.msgKey); missing while streaming. */
  msgId?: string
  /** The response is still streaming: an open Mermaid block at the end stays source. */
  streaming?: boolean
}

/**
 * Renders the Markdown of assistant responses (GFM). Raw HTML is discarded (`skipHtml`);
 * half-finished Markdown while streaming is harmless: the parser closes open blocks itself.
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
          // code blocks
          "prose-pre:my-3 prose-pre:overflow-x-auto prose-pre:rounded-md prose-pre:bg-zinc-900 prose-pre:p-3 prose-pre:text-xs prose-pre:text-zinc-100",
          // inline code without the backticks of the typography preset
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
