import { useMemo } from "react"
import type { LLMCall } from "@/api/types"
import { useNow } from "@/hooks/useNow"
import { buildAgentTree, type AgentNode } from "@/lib/subagent-overview"
import type { SubagentRun } from "@/lib/subagents"

export type AgentTreeInput = { chatTitle: string; chatRunning: boolean; runs: SubagentRun[]; llmCalls: LLMCall[] }

/** Tree of the agents; running durations are updated every 5 s. */
export function useAgentTree({ chatTitle, chatRunning, runs, llmCalls }: AgentTreeInput): AgentNode {
  const now = useNow(5000)
  return useMemo(
    () => buildAgentTree({ chatTitle, chatRunning, runs, llmCalls, now }),
    [chatTitle, chatRunning, runs, llmCalls, now],
  )
}
