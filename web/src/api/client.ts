import { claimSilentLogin, isLoginPath, silentLoginUrl } from "@/lib/auth"
import type {
  Approval,
  Artifact,
  ArtifactKind,
  BackgroundTask,
  Chat,
  ChatDetail,
  Command,
  Config,
  CreateChatRequest,
  LLMCall,
  Me,
  Model,
  Pool,
  SendResult,
  ToolExecutionsResponse,
  Variant,
} from "./types"

export class ApiError extends Error {
  readonly status: number
  /** Maschinenlesbarer Grund, etwa „context_too_large“ beim Modellwechsel. */
  readonly code?: string
  readonly details?: unknown
  /** Bei 401 im oidc-Modus: Pfad der Anmeldung (/oidc/login, unter einem Präfix etwa /agent/oidc/login). */
  readonly login?: string
  constructor(status: number, message: string, code?: string, details?: unknown, login?: string) {
    super(message)
    this.name = "ApiError"
    this.status = status
    this.code = code
    this.details = details
    this.login = login
  }
}

/** Anmeldung über die Plattform: einmal still anmelden (prompt=none), danach „nicht angemeldet“ zeigen. */
function silentLogin(loginPath: string) {
  if (!isLoginPath(loginPath)) return
  let store: Storage | undefined
  try {
    store = window.sessionStorage
  } catch {
    store = undefined
  }
  if (claimSilentLogin(store, Date.now())) window.location.assign(silentLoginUrl(loginPath, window.location))
}

/** Details zu „context_too_large“: Der Kontext passt nicht in das gewünschte Modell. */
export type ContextTooLarge = { model: string; tokens: number; window: number; limit: number }

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: {
      Accept: "application/json",
      ...(init?.body && !(init.body instanceof FormData) ? { "Content-Type": "application/json" } : {}),
      ...init?.headers,
    },
  })
  if (!res.ok) {
    let message = `${res.status} ${res.statusText}`
    let code: string | undefined
    let details: unknown
    let login: string | undefined
    try {
      const body = (await res.json()) as { error?: string; code?: string; details?: unknown; login?: string }
      if (body?.error) message = body.error
      code = body?.code
      details = body?.details
      login = body?.login
    } catch {
      // Antwort ohne JSON-Körper
    }
    if (res.status === 401 && login) silentLogin(login)
    throw new ApiError(res.status, message, code, details, login)
  }
  return (await res.json()) as T
}

const post = <T>(path: string, body?: unknown) =>
  request<T>(path, { method: "POST", body: body === undefined ? undefined : JSON.stringify(body) })

const enc = encodeURIComponent

export const api = {
  me: () => request<Me>("api/me"),
  /** Sitzung am Orchestrator beenden (nur oidc-Modus). */
  logout: async () => {
    await fetch("oidc/logout", { method: "POST" })
  },
  config: () => request<Config>("api/config"),
  models: () => request<Model[]>("api/models"),
  variants: () => request<Variant[]>("api/variants"),
  pool: () => request<Pool>("api/pool"),
  chats: () => request<Chat[]>("api/chats"),
  chat: (id: string) => request<ChatDetail>(`api/chats/${enc(id)}`),
  createChat: (req: CreateChatRequest) => post<Chat>("api/chats", req),
  /** Modell wechseln; 409 „context_too_large“, wenn der Kontext nicht passt (dann compactFirst). */
  setModel: (id: string, model: string, compactFirst = false) =>
    post<Chat>(`api/chats/${enc(id)}/model`, { model, compact_first: compactFirst }),
  setEffort: (id: string, level: string) => post<Chat>(`api/chats/${enc(id)}/effort`, { level }),
  /** Laufenden bash-Befehl stoppen; der Agent bekommt „Command stopped by the user“. */
  stopTool: (id: string, toolCallId: string) => post<{ ok: boolean }>(`api/chats/${enc(id)}/tools/${enc(toolCallId)}/stop`),
  /** Laufenden bash-Befehl in eine Hintergrundaufgabe umwandeln; er läuft weiter. */
  backgroundTool: (id: string, toolCallId: string) =>
    post<BackgroundTask>(`api/chats/${enc(id)}/tools/${enc(toolCallId)}/background`),
  sendMessage: (id: string, text: string, attachments?: string[]) =>
    post<SendResult>(
      `api/chats/${enc(id)}/messages`,
      attachments && attachments.length > 0 ? { text, attachments } : { text },
    ),
  /** Eingereihte Nachricht entfernen; 409, wenn schon übergeben. */
  unqueue: (id: string, entry: string) =>
    request<{ ok: boolean }>(`api/chats/${enc(id)}/queue/${enc(entry)}`, { method: "DELETE" }),
  /** Zurückgehaltene Nachrichten jetzt übergeben; 409, wenn der Agent arbeitet. */
  flushQueue: (id: string) => post<SendResult>(`api/chats/${enc(id)}/queue/send`),
  abort: (id: string) => post<Chat>(`api/chats/${enc(id)}/abort`),
  suspend: (id: string) => post<Chat>(`api/chats/${enc(id)}/suspend`),
  setInternet: (id: string, enabled: boolean) => post<Chat>(`api/chats/${enc(id)}/internet`, { enabled }),
  setMaxSubagents: (id: string, max: number) => post<Chat>(`api/chats/${enc(id)}/subagents`, { max }),
  llmCalls: (id: string) => request<LLMCall[]>(`api/chats/${enc(id)}/llm_calls`),
  toolExecutions: (id: string) => request<ToolExecutionsResponse>(`api/chats/${enc(id)}/tool_executions`),
  background: (id: string) => request<BackgroundTask[]>(`api/chats/${enc(id)}/background`),
  /** Laufende Hintergrundaufgabe beenden; 409, wenn sie nicht (mehr) läuft. */
  stopBackground: (id: string, bg: string) => post<BackgroundTask>(`api/chats/${enc(id)}/background/${enc(bg)}/stop`),
  setAutoCompact: (id: string, enabled: boolean) => post<Chat>(`api/chats/${enc(id)}/autocompact`, { enabled }),
  commands: (id: string) => request<Command[]>(`api/chats/${enc(id)}/commands`),
  runCommand: (id: string, command: string) =>
    post<SendResult>(`api/chats/${enc(id)}/commands`, { command }),
  uploadFiles: (id: string, files: File[]) => {
    const form = new FormData()
    for (const f of files) form.append("file", f, f.name)
    return request<Artifact[]>(`api/chats/${enc(id)}/files`, { method: "POST", body: form })
  },
  pendingApprovals: () => request<Approval[]>("api/approvals?state=pending"),
  decide: (id: string, approve: boolean) => post<Approval>(`api/approvals/${enc(id)}`, { approve }),
}

export const urls = {
  session: (id: string) => `api/chats/${enc(id)}/session`,
  events: (id: string) => `api/chats/${enc(id)}/events`,
  artifact: (id: string, name: string, kind: ArtifactKind) =>
    `api/chats/${enc(id)}/artifacts/${enc(name)}?kind=${kind}`,
  /** Anzeige-Bild einer Antwort (Pfad in der Sandbox, Kennung der Antwort), siehe lib/images. */
  image: (id: string, path: string, msg: string) => `api/chats/${enc(id)}/images?path=${enc(path)}&msg=${enc(msg)}`,
}
