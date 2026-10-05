import { describe, expect, it } from "vitest"
import type { LLMCall, SubagentEntry } from "@/api/types"
import {
  buildAgentTree,
  containsRun,
  flattenAgentTree,
  formatSpan,
  formatTokensShort,
  groupAgentNodes,
  runDuration,
  runMetrics,
  runSubtitle,
  runTitle,
  statusCounts,
  statusCountsLabel,
  subagentCountLabel,
  type AgentNode,
} from "./subagent-overview"
import { groupRuns } from "./subagents"

const entry = (p: Partial<SubagentEntry> & Pick<SubagentEntry, "run_id" | "entry_id">): SubagentEntry => ({
  chat_id: "c",
  agent: "scout",
  kind: "text",
  payload: {},
  confirmed: false,
  created_at: "2026-09-29T10:00:00Z",
  ...p,
})
const iso = (s: string) => `2026-09-29T${s}Z`
const t = (s: string) => Date.parse(iso(s))

const call = (p: Partial<LLMCall>): LLMCall => ({
  id: 1,
  slot_id: "s",
  source_ip: "",
  model: "m",
  response_id: "",
  status: 200,
  input: 0,
  output: 0,
  cache_read: 0,
  cache_write: 0,
  cost: 0,
  peak: false,
  tool_calls: [],
  started_at: iso("10:00:00"),
  duration_ms: 0,
  main: false,
  ...p,
})

describe("runTitle", () => {
  it("lässt die Vorsilbe „Task:“ von pi-subagents weg", () => {
    expect(runTitle({ task: "Task: Führe `node --version` aus", agent: "", runId: "a" })).toBe("Führe `node --version` aus")
    expect(runTitle({ task: "task:   Zähle Dateien", agent: "", runId: "a" })).toBe("Zähle Dateien")
    expect(runTitle({ task: "Tasks: bleibt", agent: "", runId: "a" })).toBe("Tasks: bleibt")
  })
  it("nimmt die erste nicht leere Zeile des Auftrags", () => {
    expect(runTitle({ task: "\n  Suche die Datensätze\nund fasse zusammen", agent: "scout", runId: "abc" })).toBe(
      "Suche die Datensätze",
    )
  })

  it("kürzt lange Zeilen mit Auslassungszeichen", () => {
    const title = runTitle({ task: "x".repeat(200), agent: "scout", runId: "abc" }, 20)
    expect(title).toBe(`${"x".repeat(19)}…`)
    expect(title.length).toBe(20)
  })

  it("fällt ohne Auftrag auf den Agentennamen zurück", () => {
    expect(runTitle({ task: undefined, agent: "scout", runId: "abc" })).toBe("Subagent scout")
    expect(runTitle({ task: "  ", agent: "", runId: "abc" })).toBe("Subagent")
  })
})

describe("runSubtitle", () => {
  it("nennt Agent und kurze Laufkennung", () => {
    expect(runSubtitle({ agent: "scout", runId: "abcdef123#2" })).toBe("scout · Lauf abcdef#2")
    expect(runSubtitle({ agent: "", runId: "abc" })).toBe("Subagent · Lauf abc")
  })
})

describe("runDuration", () => {
  const run = { start: t("10:00:00"), end: t("10:00:30") }
  it("misst bei beendeten Läufen vom ersten bis zum letzten Eintrag", () => {
    expect(runDuration(run, "done", t("10:05:00"))).toBe(30_000)
    expect(runDuration(run, "stopped", t("10:05:00"))).toBe(30_000)
  })

  it("zählt bei laufenden oder stillen Läufen bis jetzt", () => {
    expect(runDuration(run, "running", t("10:01:00"))).toBe(60_000)
    expect(runDuration(run, "idle", t("10:02:00"))).toBe(120_000)
  })

  it("wird nie negativ", () => {
    expect(runDuration(run, "running", t("09:00:00"))).toBe(30_000)
  })
})

describe("formatSpan", () => {
  it("formatiert Sekunden, Minuten und Stunden", () => {
    expect(formatSpan(0)).toBe("0 s")
    expect(formatSpan(42_400)).toBe("42 s")
    expect(formatSpan(125_000)).toBe("2 min 5 s")
    expect(formatSpan(3_725_000)).toBe("1 h 2 min")
    expect(formatSpan(-5)).toBe("0 s")
  })
})

describe("formatTokensShort", () => {
  it("kürzt Tausender und Millionen", () => {
    expect(formatTokensShort(999)).toBe("999")
    expect(formatTokensShort(1234)).toBe("1,2k")
    expect(formatTokensShort(12_345)).toBe("12k")
    expect(formatTokensShort(2_500_000)).toBe("2,5M")
  })
})

describe("runMetrics", () => {
  const runs = groupRuns([
    entry({ run_id: "a", entry_id: "1", kind: "task", payload: { text: "t" } }),
    entry({ run_id: "a", entry_id: "2", kind: "tool_call", response_id: "r1" }),
    entry({ run_id: "a", entry_id: "3", kind: "tool_result", payload: { is_error: true } }),
    entry({ run_id: "a", entry_id: "4", kind: "text", response_id: "r2" }),
    entry({ run_id: "a", entry_id: "5", kind: "text", response_id: "r2" }),
  ])

  it("summiert die am Proxy erfassten Aufrufe, deren Antwort im Lauf vorkommt", () => {
    const m = runMetrics(runs[0], [
      call({ id: 1, response_id: "r1", input: 100, output: 20, cache_read: 50, cost: 0.001 }),
      call({ id: 2, response_id: "r2", input: 200, output: 30, cost: 0.002 }),
      call({ id: 3, response_id: "fremd", input: 999, output: 999, cost: 1 }),
    ])
    expect(m).toEqual({
      llmCalls: 2,
      input: 300,
      output: 50,
      cacheRead: 50,
      tokens: 350,
      cost: 0.003,
      toolCalls: 1,
      errors: 1,
    })
  })

  it("lässt Tokens und Kosten weg, wenn kein Aufruf zugeordnet ist", () => {
    const m = runMetrics(runs[0], [])
    expect(m.llmCalls).toBe(0)
    expect(m.tokens).toBeUndefined()
    expect(m.cost).toBeUndefined()
    expect(m.toolCalls).toBe(1)
  })
})

describe("statusCounts", () => {
  it("zählt je Status und beschriftet in fester Reihenfolge", () => {
    const c = statusCounts(["done", "running", "done", "done", "done", "stopped"])
    expect(c).toEqual({ running: 1, idle: 0, done: 4, stopped: 1 })
    expect(statusCountsLabel(c)).toBe("1 läuft · 4 fertig · 1 ohne Antwort")
  })

  it("lässt leere Zähler weg", () => {
    expect(statusCountsLabel(statusCounts(["done", "done"]))).toBe("2 fertig")
    expect(statusCountsLabel(statusCounts([]))).toBe("")
  })
})

describe("subagentCountLabel", () => {
  it("unterscheidet Einzahl und Mehrzahl", () => {
    expect(subagentCountLabel(1)).toBe("1 Subagent")
    expect(subagentCountLabel(3)).toBe("3 Subagenten")
  })
})

describe("buildAgentTree", () => {
  const entries = [
    entry({ run_id: "a", entry_id: "1", kind: "task", payload: { text: "Erster Auftrag" }, created_at: iso("10:00:00") }),
    entry({ run_id: "a", entry_id: "2", kind: "text", created_at: iso("10:00:20") }),
    entry({ run_id: "p#1", entry_id: "1", agent: "w", kind: "task", payload: { text: "Teil 1" }, created_at: iso("10:01:00") }),
    entry({ run_id: "p#1", entry_id: "2", agent: "w", kind: "tool_call", created_at: iso("10:01:05") }),
    entry({ run_id: "p#0", entry_id: "1", agent: "w", kind: "task", payload: { text: "Teil 0" }, created_at: iso("10:01:01") }),
    entry({ run_id: "p#0", entry_id: "2", agent: "w", kind: "text", created_at: iso("10:01:10") }),
  ]
  const runs = groupRuns(entries)

  it("setzt den Hauptagenten als Wurzel mit Chattitel und Status", () => {
    const root = buildAgentTree({ chatTitle: "Mein Chat", chatRunning: true, runs, llmCalls: [], now: t("10:01:10") })
    expect(root.kind).toBe("main")
    expect(root.title).toBe("Mein Chat")
    expect(root.status).toBe("running")
    const idle = buildAgentTree({ chatTitle: "", chatRunning: false, runs: [], llmCalls: [], now: 0 })
    expect(idle.title).toBe("Ohne Titel")
    expect(idle.status).toBe("done")
    expect(idle.children).toEqual([])
  })

  it("hängt Einzelläufe direkt an und fasst parallele Läufe ohne Basis-Lauf zu einem Knoten", () => {
    const root = buildAgentTree({ chatTitle: "c", chatRunning: true, runs, llmCalls: [], now: t("10:01:10") })
    expect(root.children.map((n) => [n.kind, n.id])).toEqual([
      ["run", "a"],
      ["parallel", "p"],
    ])
    const a = root.children[0]
    expect(a.runId).toBe("a")
    expect(a.title).toBe("Erster Auftrag")
    expect(a.status).toBe("done")
    expect(a.durationMs).toBe(20_000)
    const p = root.children[1]
    expect(p.runId).toBeUndefined()
    expect(p.title).toBe("Parallele Läufe (2)")
    expect(p.children.map((c) => c.runId)).toEqual(["p#0", "p#1"])
    // Ein Kind läuft noch, also läuft die Gruppe.
    expect(p.children.map((c) => c.status)).toEqual(["done", "running"])
    expect(p.status).toBe("running")
  })

  it("summiert Kennzahlen eines Gruppenknotens aus den Kindern", () => {
    const withCalls = groupRuns([
      entry({ run_id: "q#0", entry_id: "1", response_id: "x", created_at: iso("10:00:00") }),
      entry({ run_id: "q#1", entry_id: "1", response_id: "y", created_at: iso("10:00:05") }),
    ])
    const root = buildAgentTree({
      chatTitle: "c",
      chatRunning: false,
      runs: withCalls,
      llmCalls: [
        call({ response_id: "x", input: 10, output: 1, cost: 0.5 }),
        call({ response_id: "y", input: 5, output: 2, cost: 0.25 }),
      ],
      now: t("10:10:00"),
    })
    const q = root.children[0]
    expect(q.metrics.tokens).toBe(18)
    expect(q.metrics.cost).toBe(0.75)
    expect(q.durationMs).toBe(5_000)
    expect(q.status).toBe("done")
  })

  it("hängt parallele Läufe unter einen vorhandenen Basis-Lauf", () => {
    const r = groupRuns([
      entry({ run_id: "b", entry_id: "1", kind: "text", created_at: iso("10:00:00") }),
      entry({ run_id: "b#0", entry_id: "1", kind: "text", created_at: iso("10:00:01") }),
    ])
    const root = buildAgentTree({ chatTitle: "c", chatRunning: false, runs: r, llmCalls: [], now: 0 })
    expect(root.children).toHaveLength(1)
    expect(root.children[0].kind).toBe("run")
    expect(root.children[0].children.map((c) => c.runId)).toEqual(["b#0"])
  })
})

describe("flattenAgentTree", () => {
  it("liefert die Läufe in Baumreihenfolge mit Tiefe, ohne Wurzel und Gruppenknoten", () => {
    const leaf = (id: string, children: AgentNode[] = []): AgentNode => ({
      kind: "run",
      id,
      runId: id,
      title: id,
      subtitle: "",
      status: "done",
      metrics: { llmCalls: 0, input: 0, output: 0, cacheRead: 0, toolCalls: 0, errors: 0 },
      durationMs: 0,
      start: 0,
      children,
    })
    const root: AgentNode = {
      ...leaf("main", [
        leaf("a", [leaf("a#0")]),
        { ...leaf("p", [leaf("p#0"), leaf("p#1")]), kind: "parallel", runId: undefined },
      ]),
      kind: "main",
      runId: undefined,
    }
    expect(flattenAgentTree(root).map((x) => [x.node.id, x.depth])).toEqual([
      ["a", 0],
      ["a#0", 1],
      ["p#0", 1],
      ["p#1", 1],
    ])
  })
})

describe("groupAgentNodes", () => {
  const n = (id: string, status: AgentNode["status"] = "done"): AgentNode => ({
    kind: "run",
    id,
    runId: id,
    title: id,
    subtitle: "",
    status,
    metrics: { llmCalls: 0, input: 0, output: 0, cacheRead: 0, toolCalls: 0, errors: 0 },
    durationMs: 0,
    start: 0,
    children: [],
  })

  it("lässt bis zur Grenze alle Knoten einzeln stehen", () => {
    const list = [n("a"), n("b"), n("c")]
    expect(groupAgentNodes(list, 3)).toEqual({ type: "nodes", nodes: list })
  })

  it("fasst oberhalb der Grenze alle Knoten zu einer Gruppe mit Zählern zusammen", () => {
    const list = [n("a"), n("b"), n("c", "running"), n("d"), n("e")]
    const g = groupAgentNodes(list, 3)
    expect(g).toEqual({ type: "group", nodes: list, counts: { running: 1, idle: 0, done: 4, stopped: 0 } })
  })
})

describe("containsRun", () => {
  it("sucht einen Lauf auch in tieferen Ebenen", () => {
    const root = buildAgentTree({
      chatTitle: "c",
      chatRunning: false,
      runs: groupRuns([entry({ run_id: "p#0", entry_id: "1" }), entry({ run_id: "p#1", entry_id: "1" })]),
      llmCalls: [],
      now: 0,
    })
    expect(containsRun(root.children, "p#1")).toBe(true)
    expect(containsRun(root.children, "p")).toBe(false)
    expect(containsRun(root.children, undefined)).toBe(false)
  })
})
