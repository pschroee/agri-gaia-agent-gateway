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
  /** Machine-readable reason, e.g. "context_too_large" when switching models. */
  readonly code?: string
  readonly details?: unknown
  /** On 401 in oidc mode: login path (/oidc/login, under a prefix e.g. /agent/oidc/login). */
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

/** Login through the platform: log in silently once (prompt=none), afterwards show "not logged in". */
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

/** Details of "context_too_large": the context does not fit into the requested model. */
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
      // response without a JSON body
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
  /** End the session at the orchestrator (oidc mode only). */
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
  /** Switch the model; 409 "context_too_large" if the context does not fit (then compactFirst). */
  setModel: (id: string, model: string, compactFirst = false) =>
    post<Chat>(`api/chats/${enc(id)}/model`, { model, compact_first: compactFirst }),
  setEffort: (id: string, level: string) => post<Chat>(`api/chats/${enc(id)}/effort`, { level }),
  /** Stop a running bash command; the agent gets "Command stopped by the user". */
  stopTool: (id: string, toolCallId: string) => post<{ ok: boolean }>(`api/chats/${enc(id)}/tools/${enc(toolCallId)}/stop`),
  /** Turn a running bash command into a background task; it keeps running. */
  backgroundTool: (id: string, toolCallId: string) =>
    post<BackgroundTask>(`api/chats/${enc(id)}/tools/${enc(toolCallId)}/background`),
  sendMessage: (id: string, text: string, attachments?: string[]) =>
    post<SendResult>(
      `api/chats/${enc(id)}/messages`,
      attachments && attachments.length > 0 ? { text, attachments } : { text },
    ),
  /** Remove a queued message; 409 if already handed over. */
  unqueue: (id: string, entry: string) =>
    request<{ ok: boolean }>(`api/chats/${enc(id)}/queue/${enc(entry)}`, { method: "DELETE" }),
  /** Hand over held-back messages now; 409 if the agent is working. */
  flushQueue: (id: string) => post<SendResult>(`api/chats/${enc(id)}/queue/send`),
  abort: (id: string) => post<Chat>(`api/chats/${enc(id)}/abort`),
  suspend: (id: string) => post<Chat>(`api/chats/${enc(id)}/suspend`),
  setInternet: (id: string, enabled: boolean) => post<Chat>(`api/chats/${enc(id)}/internet`, { enabled }),
  llmCalls: (id: string) => request<LLMCall[]>(`api/chats/${enc(id)}/llm_calls`),
  toolExecutions: (id: string) => request<ToolExecutionsResponse>(`api/chats/${enc(id)}/tool_executions`),
  background: (id: string) => request<BackgroundTask[]>(`api/chats/${enc(id)}/background`),
  /** End a running background task; 409 if it is not (or no longer) running. */
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
  /** Display image of a response (path in the sandbox, ID of the response), see lib/images. */
  image: (id: string, path: string, msg: string) => `api/chats/${enc(id)}/images?path=${enc(path)}&msg=${enc(msg)}`,
}
