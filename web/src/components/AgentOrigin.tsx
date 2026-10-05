// Origin of a socket call or an approval: main agent or subagent (session from PI_AGW_SESSION,
// set by the orchestrator). Display only; the agent could change the value.
import { BotIcon } from "lucide-react"
import { subagentHref } from "@/hooks/useHashRoute"
import { isSubagentSession, runName, shortRunId, type SubagentRun } from "@/lib/subagents"

export function AgentOrigin({ chatId, session, runs }: { chatId: string; session?: string; runs?: SubagentRun[] }) {
  if (!isSubagentSession(session)) return null
  const run = runs?.find((r) => r.runId === session)
  return (
    <a
      href={subagentHref(chatId, session)}
      title={run?.task ? `Task: ${run.task}` : `Run ${session}`}
      className="inline-flex max-w-full items-center gap-1 rounded bg-violet-100 px-1.5 py-0.5 font-sans text-[11px] font-medium text-violet-800 hover:underline dark:bg-violet-950 dark:text-violet-200"
    >
      <BotIcon className="size-3 shrink-0" />
      <span className="truncate">Subagent {run ? runName(run) : shortRunId(session)}</span>
    </a>
  )
}
