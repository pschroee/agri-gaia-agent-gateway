import { useState } from "react"
import { DownloadIcon, HardDriveIcon, TriangleAlertIcon } from "lucide-react"
import { urls } from "@/api/client"
import type { Artifact, BackgroundTask, Delegation, LLMCall, SocketCall, WorkspaceBackup } from "@/api/types"
import { AgentOrigin } from "@/components/AgentOrigin"
import { BackgroundTasksPanel } from "@/components/BackgroundTasks"
import { ExecutionsPanel } from "@/components/ExecutionsPanel"
import { LlmCallsPanel } from "@/components/LlmCallsPanel"
import { AgentTaskTree } from "@/components/SubagentOverview"
import { TasksPanel } from "@/components/Tasks"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { runningCount } from "@/lib/background"
import { type Evidence, evidenceSummary } from "@/lib/evidence"
import { formatBytes, formatTime, shortHash, socketOpLabel, socketResultLabel } from "@/lib/format"
import type { SubagentRun } from "@/lib/subagents"
import { type Task, visibleTasks } from "@/lib/tasks"
import { cn } from "@/lib/utils"
import { workspaceSummary } from "@/lib/workspace"

type Props = {
  chatId: string
  chatTitle: string
  chatRunning: boolean
  /** Lauf der offenen Detailansicht; dann öffnet der Reiter „Subagenten“. */
  activeRunId?: string
  /** Nach einem Klick auf einen Lauf, etwa um das Seitenblatt zu schließen. */
  onNavigate?: () => void
  artifacts: Artifact[]
  socketCalls: SocketCall[]
  /** Übertragene Rechte des Chats (fehlt: ohne Delegation). */
  delegation?: Delegation
  runs: SubagentRun[]
  llmCalls: LLMCall[]
  modelName: (id: string) => string
  /** Sicherung von /workspace (Chat-Feld workspace). */
  workspace?: WorkspaceBackup
  /** Aufgabenliste des Agenten (aus den todo-Aufrufen). */
  tasks?: Task[]
  /** Abgleich der Werkzeugaufrufe mit dem Protokoll des Orchestrators (E9). */
  evidence?: Map<string, Evidence>
  /** Hintergrundaufgaben (bash mit run_in_background). */
  background?: BackgroundTask[]
  /** Ändert sich der Wert, öffnet der Reiter „Hintergrund“ (Zähler im Chatkopf, Karten im Verlauf). */
  backgroundFocus?: number
  /** Aufgabe, die dabei aufgeklappt und in den Blick gerückt wird. */
  backgroundFocusId?: string
  /**
   * Das Panel entsteht gerade wegen dieses Klicks (Seitenblatt auf schmalen Bildschirmen, das beim
   * Öffnen neu eingehängt wird): gleich mit dem Reiter „Hintergrund“ beginnen.
   */
  backgroundFocusFresh?: boolean
}

const trigger = "h-7 text-xs"
const noEvidence = new Map<string, Evidence>()
const noBackground: BackgroundTask[] = []

export function ChatSidePanel({
  chatId,
  chatTitle,
  chatRunning,
  activeRunId,
  onNavigate,
  artifacts,
  socketCalls,
  delegation,
  runs,
  llmCalls,
  modelName,
  workspace,
  tasks = [],
  evidence = noEvidence,
  background = noBackground,
  backgroundFocus = 0,
  backgroundFocusId,
  backgroundFocusFresh = false,
}: Props) {
  // Werkzeuge ohne Ausführung in der Sandbox (todo, subagent, MCP) zählen hier nicht.
  const executable = new Map([...evidence].filter(([, e]) => e.state !== "internal"))
  const execSummary = evidenceSummary(executable)
  const flagged = chatRunning ? 0 : execSummary.flagged
  const inputs = artifacts.filter((a) => a.kind === "input")
  const outputs = artifacts.filter((a) => a.kind !== "input")
  // Beim Öffnen eines Laufs springt der Reiter auf „Subagenten“; sonst bleibt die Wahl des Nutzers.
  const [tab, setTab] = useState(activeRunId ? "subagents" : backgroundFocusFresh ? "background" : "artifacts")
  const [prevRun, setPrevRun] = useState(activeRunId)
  if (prevRun !== activeRunId) {
    setPrevRun(activeRunId)
    if (activeRunId) setTab("subagents")
  }
  const [prevBgFocus, setPrevBgFocus] = useState(backgroundFocus)
  if (prevBgFocus !== backgroundFocus) {
    setPrevBgFocus(backgroundFocus)
    setTab("background")
  }
  const bgRunning = runningCount(background)
  return (
    <Tabs value={tab} onValueChange={setTab} className="flex h-full min-h-0 flex-col gap-0">
      <TabsList className="m-2 grid w-[calc(100%-1rem)] grid-cols-2 gap-0.5 group-data-horizontal/tabs:h-auto">
        <TabsTrigger className={trigger} value="tasks">Aufgaben ({visibleTasks(tasks).length})</TabsTrigger>
        <TabsTrigger className={trigger} value="subagents">Subagenten ({runs.length})</TabsTrigger>
        <TabsTrigger className={trigger} value="artifacts">Artefakte ({artifacts.length})</TabsTrigger>
        <TabsTrigger className={trigger} value="llm">Modellaufrufe ({llmCalls.length})</TabsTrigger>
        <TabsTrigger
          className={cn(trigger, flagged > 0 && "text-red-700")}
          value="exec"
          title="Werkzeugausführungen durch den Orchestrator, abgeglichen mit den Anforderungen am Proxy"
        >
          Ausführungen ({execSummary.total}){flagged > 0 ? ` · ${flagged}!` : ""}
        </TabsTrigger>
        <TabsTrigger className={trigger} value="socket">
          Socket-Protokoll ({socketCalls.length})
        </TabsTrigger>
        <TabsTrigger
          className={cn(trigger, bgRunning > 0 && "text-sky-700 dark:text-sky-400")}
          value="background"
          title="Hintergrundaufgaben (bash mit run_in_background)"
        >
          Hintergrund ({bgRunning > 0 ? `${bgRunning}/${background.length}` : background.length})
        </TabsTrigger>
      </TabsList>
      <TabsContent value="background" className="min-h-0 flex-1 overflow-y-auto px-3 pb-3">
        <BackgroundTasksPanel chatId={chatId} tasks={background} focus={{ n: backgroundFocus, id: backgroundFocusId }} />
      </TabsContent>
      <TabsContent value="exec" className="min-h-0 flex-1 overflow-y-auto px-3 pb-3">
        <ExecutionsPanel evidence={executable} summary={execSummary} settled={!chatRunning} />
      </TabsContent>
      <TabsContent value="artifacts" className="min-h-0 flex-1 overflow-y-auto px-3 pb-3">
        <ArtifactGroup title="Ergebnisse (vom Agenten)" chatId={chatId} list={outputs} empty="Noch keine Ergebnisse." />
        <ArtifactGroup
          title="Eingaben (vom Nutzer)"
          chatId={chatId}
          list={inputs}
          empty="Keine Eingaben. Hochgeladene Dateien liegen in der Sandbox unter /workspace/inputs/."
        />
        <WorkspaceNote workspace={workspace} />
      </TabsContent>
      <TabsContent value="socket" className="min-h-0 flex-1 overflow-y-auto px-3 pb-3">
        <DelegationCard delegation={delegation} />
        <SocketLog calls={socketCalls} chatId={chatId} runs={runs} />
      </TabsContent>
      <TabsContent value="subagents" className="min-h-0 flex-1 overflow-y-auto px-3 pb-3">
        <AgentTaskTree
          chatId={chatId}
          tree={{ chatTitle, chatRunning, runs, llmCalls }}
          activeRunId={activeRunId}
          onNavigate={onNavigate}
        />
      </TabsContent>
      <TabsContent value="llm" className="min-h-0 flex-1 overflow-y-auto px-3 pb-3">
        <LlmCallsPanel calls={llmCalls} modelName={modelName} />
      </TabsContent>
      <TabsContent value="tasks" className="min-h-0 flex-1 overflow-y-auto px-3 pb-3">
        <TasksPanel tasks={tasks} />
      </TabsContent>
    </Tabs>
  )
}

function ArtifactGroup({ title, chatId, list, empty }: { title: string; chatId: string; list: Artifact[]; empty: string }) {
  return (
    <section className="mb-4">
      <h3 className="mb-1.5 text-xs font-semibold tracking-wide text-muted-foreground uppercase">{title}</h3>
      {list.length === 0 ? (
        <p className="text-xs text-muted-foreground">{empty}</p>
      ) : (
        <ul className="flex flex-col gap-1.5">
          {list.map((a) => (
            <li key={`${a.kind}/${a.name}`} className="rounded-md border p-2 text-xs">
              <a
                className="flex items-center gap-1.5 font-mono font-medium break-all text-sky-700 hover:underline"
                href={urls.artifact(chatId, a.name, a.kind)}
                download={a.name}
              >
                <DownloadIcon className="size-3.5 shrink-0" />
                {a.name}
              </a>
              <div className="mt-0.5 text-muted-foreground">
                {formatBytes(a.size)} · {a.content_type} · <span className="uppercase">{a.via}</span>
              </div>
              <div className="text-muted-foreground">
                <span className="font-mono" title={a.sha256}>
                  {shortHash(a.sha256)}
                </span>{" "}
                · {formatTime(a.created_at)}
              </div>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

/** Dezenter Hinweis, ob /workspace gesichert ist (übersteht das Ruhen des Chats). */
function WorkspaceNote({ workspace }: { workspace?: WorkspaceBackup }) {
  const { text, warning } = workspaceSummary(workspace)
  return (
    <section className="mb-4 text-xs text-muted-foreground">
      <h3 className="mb-1.5 font-semibold tracking-wide uppercase">Arbeitsbereich</h3>
      <p
        className="flex items-center gap-1.5"
        title="/workspace ohne inputs/, node_modules, .venv, __pycache__ und .cache; wird nach jeder Antwort und beim Ruhen gesichert und beim Fortsetzen wiederhergestellt"
      >
        <HardDriveIcon className="size-3.5 shrink-0" />
        {text}
      </p>
      {warning && (
        <p className="mt-1 flex items-start gap-1.5 text-amber-700 dark:text-amber-500">
          <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
          {warning}
        </p>
      )}
    </section>
  )
}

const idLabel = (id: string) => (id === "*" ? "alle" : id === "own" ? "selbst angelegte" : id)

/** Übertragene Rechte über dem Socket-Protokoll: gegen sie prüft der Autorisierungsdienst jeden Aufruf. */
function DelegationCard({ delegation }: { delegation?: Delegation }) {
  if (!delegation) {
    return (
      <p className="mb-2 rounded-md border border-dashed p-2 text-xs text-muted-foreground">
        Ohne Delegation: Lesen geht ohne Rückfrage, Schreiben braucht eine Bestätigung.
      </p>
    )
  }
  const enforce = delegation.enforce !== false
  return (
    <div className={cn("mb-2 rounded-md border p-2 text-xs", enforce ? "border-emerald-300 bg-emerald-50/60" : "border-amber-300 bg-amber-50/60")}>
      <div className="font-medium">
        Übertragene Rechte · {enforce ? "Übergriffe werden abgewiesen" : "Übergriffe werden nur protokolliert"}
      </div>
      {delegation.expires_at && <div className="text-muted-foreground">gültig bis {formatTime(delegation.expires_at)}</div>}
      <ul className="mt-1 font-mono">
        {delegation.rules.length === 0 && <li>keine Rechte</li>}
        {delegation.rules.map((r, i) => (
          <li key={i}>
            {r.action} {r.resource}
            {r.ids && r.ids.length > 0 ? `: ${r.ids.map(idLabel).join(", ")}` : " (ohne Objekt)"}
          </li>
        ))}
      </ul>
    </div>
  )
}

function SocketLog({ calls, chatId, runs }: { calls: SocketCall[]; chatId: string; runs: SubagentRun[] }) {
  if (calls.length === 0) return <p className="text-xs text-muted-foreground">Noch keine Aufrufe über den Socket.</p>
  const sorted = [...calls].sort((a, b) => b.id - a.id)
  return (
    <ul className="flex flex-col gap-1.5">
      {sorted.map((c) => (
        <li
          key={c.id}
          className={cn(
            "rounded-md border p-2 font-mono text-xs",
            (c.op === "agent_limit" || c.op === "subagent_limit") && "border-amber-300 bg-amber-50/60",
            /übergriff/i.test(c.result ?? "") && "border-red-300 bg-red-50/60",
          )}
        >
          <div className="flex items-center gap-1.5">
            <span className="text-muted-foreground">#{c.id}</span>
            <span className="uppercase">{c.via}</span>
            <span className="font-semibold" title={c.op}>
              {socketOpLabel(c.op)}
            </span>
            <AgentOrigin chatId={chatId} session={c.session} runs={runs} />
            <span className="ml-auto font-sans text-muted-foreground">{formatTime(c.created_at)}</span>
          </div>
          {c.detail && <div className="mt-0.5 break-all">{c.detail}</div>}
          {c.result && (
            <div
              className={
                /fehl|error|denied|reject|abgelehnt|abgewiesen|abgebrochen|expired|übergriff/i.test(c.result)
                  ? "text-red-700"
                  : "text-muted-foreground"
              }
              title={c.result}
            >
              → {socketResultLabel(c.result)}
            </div>
          )}
        </li>
      ))}
    </ul>
  )
}
