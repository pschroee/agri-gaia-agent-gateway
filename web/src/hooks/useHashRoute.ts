import { useEffect, useState } from "react"

/** `runId`: view of a subagent run within the chat. */
export type Route = { view: "chats"; chatId?: string; runId?: string } | { view: "status" }

const decode = (s: string) => {
  try {
    return decodeURIComponent(s)
  } catch {
    return s
  }
}

export function parseHash(hash: string): Route {
  const parts = hash.replace(/^#\/?/, "").split("/").filter(Boolean)
  if (parts[0] === "status") return { view: "status" }
  if (parts[0] === "chats" && parts[1]) {
    const chatId = decode(parts[1])
    // The run ID can carry "#n"; unencoded, it also ends up in this part.
    if (parts[2] === "subagents" && parts.length > 3) return { view: "chats", chatId, runId: decode(parts.slice(3).join("/")) }
    return { view: "chats", chatId }
  }
  return { view: "chats" }
}

export const chatHref = (id: string) => `#/chats/${encodeURIComponent(id)}`

export const subagentHref = (chatId: string, runId: string) => `${chatHref(chatId)}/subagents/${encodeURIComponent(runId)}`

export function useHashRoute(): Route {
  const [route, setRoute] = useState(() => parseHash(window.location.hash))
  useEffect(() => {
    const onChange = () => setRoute(parseHash(window.location.hash))
    window.addEventListener("hashchange", onChange)
    return () => window.removeEventListener("hashchange", onChange)
  }, [])
  return route
}
